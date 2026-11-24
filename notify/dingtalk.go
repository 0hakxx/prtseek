package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"time"
)

// dingTalkChannel 은 딩톡(DingTalk) 사용자 정의 봇을 구현한다.
//
// 플랫폼 특성(여기 구현의 절충을 결정함):
//   - 봇 하나당 분당 20 건으로 속도가 제한되며, 초과하면 조용히 버려진다(HTTP 는 여전히 200 일 수 있음).
//     그래서 속도 제한은 반드시 클라이언트에서 해야 한다. DefaultRatePerMin 참고.
//   - 보안 설정은 셋 중 하나: 서명 / 사용자 정의 키워드 / IP 화이트리스트. 서명만이 메시지 내용에
//     의존하지 않는 방식이므로 서명만 지원한다(셋 다 켜지 않은 순수 webhook 도 지원).
//   - 성공/실패 모두 HTTP 200 을 반환하고 body 의 errcode 로 구분한다——errcode 를 확인하지 않으면
//     전달 실패를 성공으로 기록하게 된다.
type dingTalkChannel struct{}

func (dingTalkChannel) Kind() string { return KindDingTalk }

func (dingTalkChannel) DefaultRatePerMin() int { return 20 }

// 딩톡의 Webhook 주소에는 access_token 이 들어 있어 그 자체가 자격 증명이므로 전체를 마스킹한다.
func (dingTalkChannel) SecretKeys() []string { return []string{"webhook", "secret"} }

// 대상은 딩톡의 Webhook 주소 자체다. 주소를 바꾸면 새 주소에 대해 서명 키도 반드시 다시 지정해야 한다.
func (dingTalkChannel) DestinationKeys() []string { return []string{"webhook"} }

func (dingTalkChannel) Validate(cfg map[string]any) error {
	hook := cfgString(cfg, "webhook")
	if hook == "" {
		return errors.New("Webhook 주소가 없습니다")
	}
	if err := validateHTTPURL(hook); err != nil {
		return fmt.Errorf("Webhook 주소가 유효하지 않습니다: %w", err)
	}
	return nil
}

// Send 는 메시지를 한 번 전달한다. 링크가 있고 단건이면 ActionCard(버튼 포함)를, 아니면 markdown 을 쓴다.
func (c dingTalkChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	hook := cfgString(cfg, "webhook")
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	endpoint, err := dingTalkSignedURL(hook, cfgString(cfg, "secret"), time.Now())
	if err != nil {
		return 0, Permanent(err)
	}

	title := markdownTitle(m)
	// 딩톡 markdown 본문에는 명확한 바이트 상한이 없지만, 증거 필드가 비정상적으로 부푸는 것을 막으려고 상한 보호를 한다.
	text, kept := markdownBody(m, 20000)

	var payload any
	if !m.Batch && len(m.Items) == 1 && m.Items[0].DetailURL != "" {
		payload = map[string]any{
			"msgtype": "actionCard",
			"actionCard": map[string]any{
				"title":          title,
				"text":           text,
				"btnOrientation": "0",
				"singleTitle":    "상세 보기",
				"singleURL":      m.Items[0].DetailURL,
			},
		}
	} else {
		payload = map[string]any{
			"msgtype":  "markdown",
			"markdown": map[string]any{"title": title, "text": text},
		}
	}

	raw, err := doJSON(ctx, "POST", endpoint, nil, payload)
	if err != nil {
		return 0, err
	}
	// 딩톡은 비즈니스 오류를 200 응답 안에 숨긴다.
	var res struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("딩톡 응답 파싱 실패: %w (%s)", err, snippet(raw))
	}
	if res.ErrCode != 0 {
		// 301000 은 서명 검증 실패, 310000 은 키워드 불일치——둘 다 설정 오류이므로
		// 재시도해도 스스로 복구되지 않는다.
		return 0, Permanent(fmt.Errorf("딩톡이 오류를 반환함 %d: %s", res.ErrCode, res.ErrMsg))
	}
	return kept, nil
}

// dingTalkSignedURL 은 공식 서명 규칙에 따라 webhook 에 timestamp 와 sign 파라미터를 덧붙인다.
//
// 규칙: 서명할 문자열 = timestamp + "\n" + secret, HMAC-SHA256 의 **키도 secret** 이며,
// 결과를 base64 한 뒤 URL 인코딩한다. timestamp 는 밀리초다. secret 이 비어 있으면 그대로 반환해,
// 서명을 켜지 않은 봇을 지원한다.
func dingTalkSignedURL(hook, secret string, now time.Time) (string, error) {
	if secret == "" {
		return hook, nil
	}
	ts := strconv.FormatInt(now.UnixMilli(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "\n" + secret))
	sign := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	u, err := url.Parse(hook)
	if err != nil {
		// err 를 그대로 전달하지 않는다: url.Parse 의 오류 텍스트에는 전체 주소(access_token 포함)가 들어 있다.
		return "", fmt.Errorf("Webhook 주소 파싱 실패: %s", redactRequestTarget(hook))
	}
	q := u.Query()
	q.Set("timestamp", ts)
	q.Set("sign", sign)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// validateHTTPURL 은 주소가 사용 가능하고 프로토콜이 지원되는지 검증하며, 리터럴 IP 대상에 대해 내부망 판단을 한다.
//
// 두 가지 주의점:
//
//  1. **오류 메시지는 반드시 민감 정보를 제거해야 한다**. url.Parse 자체가 반환하는 것은 *url.Error 이며, 그 Error() 에는
//     **전체 원본 주소**가 들어 있는데, 이 기능의 여러 서비스 주소에는 자격 증명이 박혀 있다(딩톡 access_token,
//     위챗 워크 key, Telegram 의 bot token, 페이슈 hook id). 예전에는 여기서 바로 `return err` 했기 때문에,
//     「주소 형식이 잘못됨」 오류가 자격 증명을 실어 날라, 테스트 인터페이스의 400 응답,
//     전달마다 저장되는 last_error, 서버 로그, 전달 기록 인터페이스로 흘러갔다.
//
//  2. **리터럴 IP 는 바로 내부망 여부를 판단**하고, 도메인은 다이얼 단계에서 판단한다(blockInternalDial 이 최종
//     적용 지점이며 DNS 리바인딩도 막는다). 여기서 한 번 하는 것은 설정을 저장할 때 바로 안내를 받게 하기 위함이며,
//     첫 전달이 실패할 때까지 기다리지 않게 하기 위함이다.
//
// 프로토콜 제한은 방어적이다: file:///gopher:// 같은 것은 http.Client 에 예상치 못한
// 동작을 일으킨다(이미 scheme 검사로 막히지만, 이 표면을 열어 둘 이유가 없다).
func validateHTTPURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("주소를 파싱할 수 없습니다(%s)", redactRequestTarget(raw))
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("http/https 만 지원합니다. 받은 값 %q", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("호스트명이 없습니다")
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && isBlockedDialIP(ip) && !allowLocalTargets() {
		return fmt.Errorf("로컬/링크 로컬 주소 %s 로의 전달을 거부합니다(정말 로컬 서비스로 보내야 한다면 %s=1 을 설정하세요)", ip, AllowLocalTargetsEnv)
	}
	return nil
}
