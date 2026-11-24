package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// telegramTextLimit 은 Telegram sendMessage 의 text 필드 상한(문자 수)이다.
const telegramTextLimit = 4096

// telegramChannel 은 Telegram Bot API 를 구현한다.
//
// 플랫폼 특성:
//   - 인증은 모두 URL path 에 있으며(/bot<token>/sendMessage), 서명이 필요 없다.
//   - MarkdownV2 대신 HTML 파싱 모드를 쓴다: MarkdownV2 는 `_*[]()~`>#+-=|{}.!`
//     총 18 개 문자의 이스케이프를 요구하며 하나만 빠져도 메시지 전체가 거부된다. HTML 은 & < > 세 개만 이스케이프하면 된다.
//   - 비즈니스 오류도 HTTP 200 안에 숨어 있으며, ok 필드로 판단한다.
type telegramChannel struct{}

func (telegramChannel) Kind() string { return KindTelegram }

// Telegram 은 1:1 대화 약 초당 1 건, 그룹은 분당 20 건이다. 보수적인 값을 쓴다.
func (telegramChannel) DefaultRatePerMin() int { return 20 }

// Bot Token 은 완전한 자격 증명이다. chat_id 는 수신자일 뿐 비밀이 아니다(그것을 얻어도 Token 없이는 메시지를 보낼 수 없음).
func (telegramChannel) SecretKeys() []string { return []string{"bot_token"} }

// base_url 은 Token 이 어느 API 엔드포인트(예: 자체 리버스 프록시)로 보내질지 결정하므로, 이를 바꾸면 Token 을 다시 지정해야 한다.
func (telegramChannel) DestinationKeys() []string { return []string{"base_url"} }

func (telegramChannel) Validate(cfg map[string]any) error {
	if cfgString(cfg, "bot_token") == "" {
		return errors.New("Bot Token 이 없습니다")
	}
	if cfgString(cfg, "chat_id") == "" {
		return errors.New("Chat ID 가 없습니다")
	}
	if base := cfgString(cfg, "base_url"); base != "" {
		if err := validateHTTPURL(base); err != nil {
			return fmt.Errorf("API 주소가 유효하지 않습니다: %w", err)
		}
	}
	return nil
}

func (c telegramChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	endpoint, err := telegramEndpoint(cfg)
	if err != nil {
		return 0, Permanent(err)
	}
	text, kept := telegramHTML(m)
	payload := map[string]any{
		"chat_id":                  cfgString(cfg, "chat_id"),
		"text":                     text,
		"parse_mode":               "HTML",
		"disable_web_page_preview": false,
	}
	raw, err := doJSON(ctx, "POST", endpoint, nil, payload)
	if err != nil {
		return 0, err
	}
	var res struct {
		OK          bool   `json:"ok"`
		ErrorCode   int    `json:"error_code"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("Telegram 응답 파싱 실패: %w (%s)", err, snippet(raw))
	}
	if res.OK {
		return kept, nil
	}
	// 429 는 속도 제한이라 백오프 후 재시도가 효과가 있다. 나머지(400 파라미터 오류, 401 token 오류, 403 차단됨,
	// 404 chat 없음)는 모두 설정 문제라 재시도해도 스스로 복구되지 않는다.
	if res.ErrorCode == 429 {
		return 0, fmt.Errorf("Telegram 속도 제한: %s", res.Description)
	}
	return 0, Permanent(fmt.Errorf("Telegram 이 오류를 반환함 %d: %s", res.ErrorCode, res.Description))
}

// telegramEndpoint 는 sendMessage 주소를 만든다. base_url 이 비어 있으면 공식 API 를 쓰고,
// 비어 있지 않으면 자체 Bot API 리버스 프록시에 쓴다(네트워크 제한 환경에서 흔한 요구).
func telegramEndpoint(cfg map[string]any) (string, error) {
	base := cfgString(cfg, "base_url")
	if base == "" {
		base = "https://api.telegram.org"
	}
	base = strings.TrimSuffix(base, "/")
	token := cfgString(cfg, "bot_token")
	raw := base + "/bot" + token + "/sendMessage"
	u, err := url.Parse(raw)
	if err != nil {
		// err 를 그대로 전달하지 않는다: 주소에 Bot Token 이 들어 있으며, 이때는 addr 조차 되돌려 보이면 안 된다.
		return "", fmt.Errorf("API 주소 조합 실패(API 주소: %s)", redactRequestTarget(base))
	}
	return u.String(), nil
}

// telegramHTML 은 HTML 본문을 렌더링하고, 본문과 실제로 쓴 항목 수를 반환한다(Channel.Send 참고).
func telegramHTML(m Message) (string, int) {
	var b strings.Builder
	b.WriteString("<b>" + telegramEscape(markdownTitle(m)) + "</b>\n")
	if m.Batch {
		// Telegram 의 상한은 **문자 수**이므로 담기도 문자 기준으로 계량한다(runeSize).
		footer := ""
		if m.HomeURL != "" {
			footer = fmt.Sprintf("\n\n<a href=\"%s\">플랫폼에서 전체 보기</a>", telegramEscapeAttr(m.HomeURL))
		}
		kept := packItemCount(m.Items, telegramTextLimit, telegramReservedRunes, footer, runeSize, func(it Item, idx int) string {
			return telegramBatchLine(it, idx+1)
		})
		items := m.Items[:kept]
		b.Reset()
		b.WriteString("<b>" + telegramEscape(telegramBatchTitle(m, items, len(m.Items))) + "</b>")
		for i, it := range items {
			b.WriteString("\n" + telegramEscape(telegramBatchLine(it, i+1)))
		}
		b.WriteString(footer)
		return TruncateHTML(b.String(), telegramTextLimit), kept
	}
	if len(m.Items) == 0 {
		return b.String(), 0
	}
	it := m.Items[0]
	if it.IsStatusChange() {
		b.WriteString(fmt.Sprintf("\n<b>상태 변경</b>: %s → %s",
			telegramEscape(StatusLabel(it.FromStatus)), telegramEscape(StatusLabel(it.ToStatus))))
	}
	if it.VulnClass != "" && it.VulnClass != it.Title() {
		b.WriteString("\n<b>유형</b>: " + telegramEscape(it.VulnClass))
	}
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		b.WriteString("\n<b>자산</b>: " + telegramEscape(a))
	}
	if s := OneLine(it.Summary, maxSummaryRunes); s != "" {
		b.WriteString("\n<b>요약</b>: " + telegramEscape(s))
	}
	if it.DetailURL != "" {
		b.WriteString(fmt.Sprintf("\n\n<a href=\"%s\">상세 보기</a>", telegramEscapeAttr(it.DetailURL)))
	}
	return TruncateHTML(b.String(), telegramTextLimit), 1
}

// telegramReservedRunes 는 메시지 제목과 나타날 수 있는 잘림 안내를 위해 남겨 둔다(문자 기준).
const telegramReservedRunes = 160

// telegramBatchLine 은 요약의 한 항목을 렌더링한다(이스케이프하지 않음, 호출자가 한꺼번에 이스케이프).
func telegramBatchLine(it Item, idx int) string {
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		return fmt.Sprintf("%d. %s · %s — %s", idx, SeverityLabel(it.Severity), it.Title(), a)
	}
	return fmt.Sprintf("%d. %s · %s", idx, SeverityLabel(it.Severity), it.Title())
}

// telegramBatchTitle 은 요약 메시지의 제목 줄을 렌더링한다. 건수는 이번 배치 총수가 아니라 **이 메시지에 실제로 포함된**
// 건수를 쓴다——그렇지 않으면 읽는 사람은 머리에 적힌 숫자가 전부라고 생각한다.
func telegramBatchTitle(m Message, items []Item, total int) string {
	title := fmt.Sprintf("취약점 요약 · 총 %d 건", total)
	if extra := total - len(items); extra > 0 {
		title += fmt.Sprintf("(앞의 %d 건만 표시, 나머지 %d 건은 다음 메시지에서 이어짐)", len(items), extra)
	}
	if m.WindowMinutes > 0 {
		title = fmt.Sprintf("최근 %d 분 · %s", m.WindowMinutes, title)
	}
	return title
}

// telegramEscape 는 HTML 텍스트 내용을 이스케이프한다.
// Telegram 은 이 세 엔티티만 인식하며, 이스케이프하면 &amp; 같은 기존 엔티티는 두 번 이스케이프된다——이것이 바로
// 원하는 동작이다: 우리가 보여 주려는 것은 원래 문자이지, 사용자가 HTML 을 주입하게 하려는 것이 아니다.
func telegramEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// telegramEscapeAttr 는 HTML 속성 값을 이스케이프한다. 텍스트 이스케이프에 더해 따옴표도 처리해야 한다——
// URL 에 따옴표가 있으면 href 속성을 일찍 닫아 뒤의 내용이 주입 지점이 된다.
func telegramEscapeAttr(s string) string {
	s = telegramEscape(s)
	s = strings.ReplaceAll(s, "\"", "&quot;")
	return s
}
