package notify

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
)

// Channel 은 알림 채널 하나의 어댑터다. 구현은 반드시 **상태가 없어야** 한다: 같은 인스턴스가 여러 채널
// 설정에서 동시에 재사용되며, 자격 증명은 모두 cfg 파라미터로 전달된다.
type Channel interface {
	// Kind 는 채널 유형 식별자를 반환하며, 등록표의 키와 일치해야 한다.
	Kind() string
	// Validate 는 설정을 저장할 때 호출되며 필수 필드와 형식을 검증한다. 반환된 오류는 설정한 사람에게 그대로
	// 보이므로, 막연한 「설정이 유효하지 않음」이 아니라 「어떤 필드가 빠졌는지」를 문구로 설명해야 한다.
	Validate(cfg map[string]any) error
	// Send 는 메시지를 한 번 전달하고, **실제로 전달된 항목 수**와 오류를 반환한다.
	//
	// 항목 수를 반환하는 이유: 플랫폼마다 메시지 길이 상한이 있어, 요약 메시지에 배치 전체가 들어가지 않으면 잘린다.
	// 호출자가 무조건 배치 전체를 전달됨으로 표시하면 잘린 항목들은 사라진다——메시지에서도 보이지 않고,
	// 전달 기록에도 성공으로 표시되어, 취약점이 한 번도 보내지지 않았다는 것을 어디서도 알 수 없다. kept 를 반환하면,
	// 호출자는 앞의 kept 건만 표시하고 나머지는 다음 배치로 남긴다.
	//
	// 오류를 반환하면 전달 실패를 뜻하며, 그중 *PermanentError 는 재시도하면 안 된다는 뜻이다.
	// 실패했을 때 kept 는 의미가 없으므로 호출자는 이를 무시해야 한다.
	Send(ctx context.Context, cfg map[string]any, m Message) (int, error)
	// DefaultRatePerMin 은 그 채널이 공식적으로 권장하는 분당 전달 상한을 반환하며, 새 채널 인스턴스를 만들 때
	// 기본 속도 제한 값으로 쓴다. 0 을 반환하면 알려진 제한이 없다는 뜻이다.
	DefaultRatePerMin() int
	// SecretKeys 는 그 채널 설정에서 자격 증명에 해당하는 키 이름을 반환한다. API 로 되돌려 보일 때 이 키들의 값은 마스킹되며,
	// 갱신할 때 마스킹 값을 받으면 DB 의 원래 값을 유지한다. 어떤 필드가 자격 증명인지는 구현만 분명히 알기 때문에
	// (위챗 워크는 Webhook 주소 전체가 자격 증명이고, 딩톡은 그중 secret 만 해당한다),
	// 이 지식은 반드시 채널이 제공해야 하며 상위 계층이 추측해서는 안 된다.
	SecretKeys() []string
	// DestinationKeys 는 그 채널 설정에서 「메시지를 어디로 보낼지」를 결정하는 키 이름을 반환한다.
	//
	// SecretKeys 와 마찬가지로 보안과 관련된 것이다: 대상 주소와 자격 증명은 서로 독립된 두 필드 묶음이므로,
	// 「주소만 바꾸고 자격 증명은 그대로 유지」를 허용하면 채널 설정을 바꿀 수 있는 누구나 DB 의 실제 자격 증명을
	// 자기가 제어하는 서버로 보낼 수 있어, 채널 설정의 마스킹이 완전히 의미를 잃는다.
	// 자세한 내용은 PrepareConfigUpdate 참고.
	DestinationKeys() []string
}

// registry 는 채널 등록표다. 일부러 init() 자기 등록 대신 명시적 리터럴을 쓴다: 그래야 「어떤 채널이 있는지」를
// 한곳에서 다 볼 수 있고, 새 채널을 추가할 때 빠뜨린 것이 런타임 부작용이 아니라 컴파일 시점에 드러난다.
var registry = map[string]Channel{
	KindDingTalk: dingTalkChannel{},
	KindFeishu:   feishuChannel{},
	KindWeCom:    weComChannel{},
	KindWebhook:  webhookChannel{},
	KindTelegram: telegramChannel{},
	KindEmail:    emailChannel{},
}

// Get 은 유형별로 채널 구현을 가져온다.
func Get(kind string) (Channel, bool) {
	c, ok := registry[kind]
	return c, ok
}

// ValidKind 는 kind 가 지원되는 채널 유형인지 보고한다.
func ValidKind(kind string) bool {
	_, ok := registry[kind]
	return ok
}

// Kinds 는 지원되는 모든 채널 유형을 사전순으로 반환한다(UI 드롭다운의 안정적인 표시용).
func Kinds() []string {
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// PermanentError 는 재시도하면 안 되는 전달 실패를 표시한다: 자격 증명 오류, 대상의 거부, 잘못된 요청 본문 등.
// 재시도는 일시적 장애(네트워크 흔들림, 속도 제한, 상대 쪽 5xx)에만 의미가 있다. 영구 실패를 반복해서 백오프 재시도하면
// 성공하지도 못하고 진짜 오류를 재시도 로그 속에 묻어 버린다.
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// Permanent 는 err 를 영구 실패로 표시한다. err 가 nil 이면 nil 을 반환하므로,
// `return Permanent(someCheck())` 처럼 쓰기 편하다.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &PermanentError{Err: err}
}

// IsPermanent 는 err 체인에 영구 실패 표시가 있는지 보고한다.
func IsPermanent(err error) bool {
	var pe *PermanentError
	return errors.As(err, &pe)
}

// ---- 설정 읽기 helper ----
//
// 채널 설정은 DB 의 JSONB 열에서 오며, encoding/json 으로 역직렬화하면 map[string]any 가 되고,
// 숫자는 모두 float64, 배열은 []any 다. 아래 helper 들이 이 변환을 통일하고, 사용자가
// UI 에서 비워 두어 생기는 타입 차이(예: 포트를 문자열로 입력)를 허용한다.

// cfgString 은 문자열 설정 항목을 가져오며 앞뒤 공백을 모두 잘라 낸다——웹 폼에서 복사해 붙여 넣으면 쉽게 붙어 오기 때문이다.
func cfgString(cfg map[string]any, key string) string {
	v, ok := cfg[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(s)
}

// cfgInt 는 정수 설정 항목을 가져오며 float64(JSON 기본)와 문자열 두 출처를 모두 받는다.
func cfgInt(cfg map[string]any, key string) int {
	switch v := cfg[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0
		}
		return n
	default:
		return 0
	}
}

// cfgBool 은 불리언 설정 항목을 가져오며 문자열 "true"/"1" 도 받는다.
func cfgBool(cfg map[string]any, key string) bool {
	switch v := cfg[key].(type) {
	case bool:
		return v
	case string:
		s := strings.ToLower(strings.TrimSpace(v))
		return s == "true" || s == "1" || s == "yes"
	default:
		return false
	}
}

// cfgStrings 는 문자열 배열 설정 항목을 가져오며, 공백을 자동으로 자르고 빈 문자열은 버린다.
func cfgStrings(cfg map[string]any, key string) []string {
	raw, ok := cfg[key].([]any)
	if !ok {
		// 단일 문자열도 받으므로 값이 하나뿐일 때 폼 제출이 편하다.
		if s := cfgString(cfg, key); s != "" {
			return []string{s}
		}
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, ok := v.(string)
		if !ok {
			continue
		}
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// cfgMap 은 문자열 맵 설정 항목(예: 사용자 정의 HTTP 헤더)을 가져오며, 키와 값의 공백을 모두 자르고 빈 키는 버린다.
func cfgMap(cfg map[string]any, key string) map[string]string {
	raw, ok := cfg[key].(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		s, ok := v.(string)
		if !ok {
			continue
		}
		out[k] = s
	}
	return out
}
