package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// feishuChannel 은 페이슈(Lark 포함) 사용자 정의 봇을 구현하며, 대화형 카드를 쓴다.
//
// 플랫폼 특성:
//   - 서명 알고리즘이 딩톡과 **다르고** 아주 틀리기 쉽다. feishuSign 주석 참고.
//   - 딩톡과 마찬가지로 비즈니스 오류를 HTTP 200 의 body 에 넣는다(code != 0).
//   - 카드 header 는 색상 템플릿을 지원하므로 등급별로 색을 매핑해 메시지 목록에서 심각도를 한눈에 알아보게 한다.
type feishuChannel struct{}

func (feishuChannel) Kind() string { return KindFeishu }

// 페이슈 사용자 정의 봇은 초당 약 5회, 분당 100회로 환산된다.
func (feishuChannel) DefaultRatePerMin() int { return 100 }

// Webhook 주소의 마지막 부분이 봇의 고유 식별자이므로 자격 증명에 해당한다.
func (feishuChannel) SecretKeys() []string { return []string{"webhook", "secret"} }

// 마찬가지로: Webhook 주소를 바꾸면 새 주소에 대해 서명 키를 반드시 다시 지정해야 한다.
func (feishuChannel) DestinationKeys() []string { return []string{"webhook"} }

func (feishuChannel) Validate(cfg map[string]any) error {
	hook := cfgString(cfg, "webhook")
	if hook == "" {
		return errors.New("Webhook 주소가 없습니다")
	}
	if err := validateHTTPURL(hook); err != nil {
		return fmt.Errorf("Webhook 주소가 유효하지 않습니다: %w", err)
	}
	return nil
}

func (c feishuChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	card, kept := feishuCard(m)
	payload := map[string]any{
		"msg_type": "interactive",
		"card":     card,
	}
	// 서명 파라미터는 메시지와 같은 층에 있으며, secret 이 설정되었을 때만 나타난다.
	if secret := cfgString(cfg, "secret"); secret != "" {
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		payload["timestamp"] = ts
		payload["sign"] = feishuSign(ts, secret)
	}
	raw, err := doJSON(ctx, "POST", cfgString(cfg, "webhook"), nil, payload)
	if err != nil {
		return 0, err
	}
	var res struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		// 일부 버전의 페이슈 hook 은 이 필드 이름을 쓰므로 함께 호환한다.
		StatusCode    int    `json:"StatusCode"`
		StatusMessage string `json:"StatusMessage"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("페이슈 응답 파싱 실패: %w (%s)", err, snippet(raw))
	}
	if res.Code != 0 {
		return 0, Permanent(fmt.Errorf("페이슈가 오류를 반환함 %d: %s", res.Code, res.Msg))
	}
	if res.StatusCode != 0 {
		return 0, Permanent(fmt.Errorf("페이슈가 오류를 반환함 %d: %s", res.StatusCode, res.StatusMessage))
	}
	return kept, nil
}

// feishuSign 은 페이슈 공식 규칙에 따라 서명을 계산한다.
//
// 여기는 특히 함정에 빠지기 쉽다: 공식 예시는
//
//	hmac.new(string_to_sign.encode(), digestmod=sha256)
//
// 즉 **key = timestamp + "\n" + secret, message 는 비어 있음** 이며, 직관적인
// 「key=secret, message=stringToSign」 가 아니다——그것은 바로 딩톡의 알고리즘이다. 두 알고리즘은 정확히 반대이므로,
// 다른 쪽 구현을 보고 쓰면 반드시 서명 검증에 실패한다(19021 오류).
func feishuSign(timestamp, secret string) string {
	stringToSign := timestamp + "\n" + secret
	mac := hmac.New(sha256.New, []byte(stringToSign))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// feishuSeverityTemplate 은 취약점 등급을 카드 header 색상 템플릿으로 매핑한다.
// 알 수 없는 등급은 grey 를 쓴다——low 와 혼동되지 않도록 blue 는 쓰지 않는다.
func feishuSeverityTemplate(severity string) string {
	switch severity {
	case "critical":
		return "red"
	case "high":
		return "orange"
	case "medium":
		return "yellow"
	case "low":
		return "blue"
	default:
		return "grey"
	}
}

// feishuMaxCardBytes 는 카드 내용의 보수적인 상한이다. 페이슈는 카드 크기를 제한하며, 넘으면 통째로 거부된다.
// 공식 상한보다 확실히 낮은 값을 잡고, JSON 포장 오버헤드도 계산에 넣는다.
const feishuMaxCardBytes = 24000

// feishuCard 는 대화형 카드를 만들고, 카드와 **실제로 쓴 항목 수**를 반환한다.
// kept 의 용도는 markdownBody 와 같다: 실제로 카드에 들어간 항목만 전달됨으로 표시해야 한다.
func feishuCard(m Message) (map[string]any, int) {
	elements := []any{}
	kept := 0
	if m.Batch {
		// 항목 전체를 먼저 담은 뒤 헤더를 붙인다: 헤더에 「나머지 N 건은 다음 메시지에서 이어짐」을 써야 하므로,
		// N 은 실제로 담긴 건수에서 나와야 한다.
		kept = packItemCount(m.Items, feishuMaxCardBytes, markdownReservedBytes, "", byteSize, func(it Item, idx int) string {
			return feishuBatchLine(it, idx+1)
		})
		items := m.Items[:kept]
		elements = append(elements, feishuMarkdownDiv(markdownBatchIntro(m, items, len(m.Items))))
		for i, it := range items {
			elements = append(elements, feishuMarkdownDiv(feishuBatchLine(it, i+1)))
		}
		if m.HomeURL != "" {
			elements = append(elements, feishuButton("플랫폼에서 전체 보기", m.HomeURL))
		}
	} else if len(m.Items) > 0 {
		kept = 1
		it := m.Items[0]
		elements = append(elements, feishuMarkdownDiv(feishuItemLines(it)))
		if it.DetailURL != "" {
			elements = append(elements, feishuButton("상세 보기", it.DetailURL))
		}
	}

	card := map[string]any{
		"config":   map[string]any{"wide_screen_mode": true},
		"header":   map[string]any{"title": map[string]any{"tag": "plain_text", "content": markdownTitle(m)}},
		"elements": elements,
	}
	if len(m.Items) > 0 {
		card["header"].(map[string]any)["template"] = feishuSeverityTemplate(m.Items[0].Severity)
	}
	return card, kept
}

func feishuMarkdownDiv(content string) map[string]any {
	return map[string]any{"tag": "div", "text": map[string]any{"tag": "lark_md", "content": content}}
}

func feishuButton(label, url string) map[string]any {
	return map[string]any{
		"tag": "action",
		"actions": []any{map[string]any{
			"tag":  "button",
			"text": map[string]any{"tag": "lark_md", "content": label},
			"url":  url,
			"type": "primary",
		}},
	}
}

// feishuItemLines 는 취약점 하나의 lark_md 본문을 렌더링한다.
//
// lark_md 는 markdown 과 같은 계열의 텍스트 형식이라 링크와 강조도 똑같이 해석하므로, 외부에서 온
// 필드는 모두 markdownText(한 줄화 + 이스케이프)를 거친다——그렇지 않으면 취약점 제목 하나가
// 페이슈에서 클릭 가능한 외부 링크가 된다.
func feishuItemLines(it Item) string {
	out := fmt.Sprintf("**%s · %s**", SeverityLabel(it.Severity), markdownText(it.Title(), 0))
	if it.IsStatusChange() {
		out += fmt.Sprintf("\n**상태 변경**: %s → %s",
			markdownText(StatusLabel(it.FromStatus), 0), markdownText(StatusLabel(it.ToStatus), 0))
	}
	if it.VulnClass != "" && it.VulnClass != it.Title() {
		out += fmt.Sprintf("\n**유형**: %s", markdownText(it.VulnClass, 0))
	}
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		out += fmt.Sprintf("\n**자산**: %s", markdownText(a, 0))
	}
	if it.Summary != "" {
		if s := markdownText(it.Summary, maxSummaryRunes); s != "" {
			out += fmt.Sprintf("\n**요약**: %s", s)
		}
	}
	return out
}

// feishuBatchLine 은 요약 카드의 한 항목을 렌더링한다.
func feishuBatchLine(it Item, index int) string {
	line := fmt.Sprintf("**%d. %s · %s**", index, SeverityLabel(it.Severity), markdownText(it.Title(), 0))
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		line += " — " + markdownText(a, 0)
	}
	return line
}
