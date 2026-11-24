package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// weComMarkdownLimit 은 위챗 워크 그룹 봇 markdown content 의 절대 상한(문자가 아니라 바이트)이다.
// 여섯 채널 중 가장 빡빡한 제한이며, TruncateBytes 가 존재하는 주된 이유이기도 하다.
const weComMarkdownLimit = 4096

// weComChannel 은 위챗 워크(WeCom) 그룹 봇을 구현한다.
//
// 플랫폼 특성:
//   - URL 의 key 로만 인증하며 서명을 지원하지 않는다——그래서 webhook 주소 자체가 자격 증명의 전부다.
//   - markdown content 상한은 4096 **바이트**이며, 넘으면 통째로 거부된다(잘리지 않음). 한글은 글자당 3 바이트이므로,
//     본문에 쓸 수 있는 것은 천여 자뿐이라 반드시 클라이언트에서 잘라야 한다.
//   - 속도 제한은 분당 20 건이며, 이것도 클라이언트 속도 제한으로 막는다.
type weComChannel struct{}

func (weComChannel) Kind() string { return KindWeCom }

func (weComChannel) DefaultRatePerMin() int { return 20 }

// 위챗 워크에는 자격 증명이 Webhook 하나뿐이며(URL 의 key), 서명을 지원하지 않는다——
// 주소 전체가 자격 증명의 전부이므로 마스킹할 다른 필드가 없다.
func (weComChannel) SecretKeys() []string { return []string{"webhook"} }

// 위챗 워크에는 Webhook 필드 하나뿐이며, 그것이 대상이자 자격 증명이므로 「주소를 바꾼 뒤 남는 자격 증명」이라는 것이 없다.
func (weComChannel) DestinationKeys() []string { return []string{"webhook"} }

func (weComChannel) Validate(cfg map[string]any) error {
	hook := cfgString(cfg, "webhook")
	if hook == "" {
		return errors.New("Webhook 주소가 없습니다")
	}
	if err := validateHTTPURL(hook); err != nil {
		return fmt.Errorf("Webhook 주소가 유효하지 않습니다: %w", err)
	}
	return nil
}

func (c weComChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	// 요약 배치는 매우 길 수 있어(50 건 × 건마다 한 줄 + 접두사) 4096 바이트를 쉽게 넘는다.
	// 플랫폼 오류에 맡기지 않고 여기서 자른다: 거부되면 이 배치를 통째로 잃지만, 자르면 적어도 앞의 몇 건은 전달된다.
	content, kept := markdownBody(m, weComMarkdownLimit)
	payload := map[string]any{
		"msgtype":  "markdown",
		"markdown": map[string]any{"content": content},
	}
	raw, err := doJSON(ctx, "POST", cfgString(cfg, "webhook"), nil, payload)
	if err != nil {
		return 0, err
	}
	var res struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("위챗 워크 응답 파싱 실패: %w (%s)", err, snippet(raw))
	}
	if res.ErrCode != 0 {
		// 45009 는 인터페이스 호출이 제한을 넘었다는 뜻이다——플랫폼의 속도 제한 창은 굴러가므로 백오프 후 재시도가 효과가 있어,
		// 명시적으로 재시도 가능으로 분류한다. 여기까지 왔다면 클라이언트의 rate_per_min 이 너무 공격적으로 설정된 것이므로,
		// 재시도는 대비책일 뿐이고 진짜 해결책은 그 채널의 속도 제한 값을 낮추는 것이다.
		if res.ErrCode == 45009 {
			return 0, fmt.Errorf("위챗 워크 속도 제한 %d: %s", res.ErrCode, res.ErrMsg)
		}
		// 93000 은 webhook key 가 유효하지 않다는 뜻이다——영구 실패이며 재시도해도 스스로 복구되지 않는다.
		return 0, Permanent(fmt.Errorf("위챗 워크가 오류를 반환함 %d: %s", res.ErrCode, res.ErrMsg))
	}
	return kept, nil
}
