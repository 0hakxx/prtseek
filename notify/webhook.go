package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"text/template"
	"time"
)

// webhookChannel 은 일반 Webhook 어댑터다: 사용자가 URL, 메서드, 요청 헤더, JSON 템플릿을 직접 정한다.
// 이것이 있어 이 기능은 Slack / Mattermost / Discord / 자체 시스템마다 구현을 따로 쓸 필요가 없다——
// 그런 플랫폼은 설정 가능한 템플릿 하나로 모두 다룰 수 있다.
type webhookChannel struct{}

func (webhookChannel) Kind() string { return KindWebhook }

// 일반 Webhook 에는 공식 제한이 없으므로 0 을 반환해 기본적으로 속도 제한을 두지 않으며, 사용자가 상대의 능력에 맞게 정한다.
func (webhookChannel) DefaultRatePerMin() int { return 0 }

// url 과 headers 를 마스킹한다: 대상 주소 자체에 token 이 있는 경우가 많고, 사용자 정의 헤더에는 보통 인증 자격 증명이 들어 있으며,
// 둘 다 인터페이스 응답에 나타나므로 모두 막아야 한다.
// 대가는 편집할 때 헤더 하나만 바꾸려 해도 헤더 묶음 전체를 다시 입력해야 한다는 것이다(마스킹 값은 「원래 값 유지」로 해석됨)——
// 이 절충은 의도적이다: 한 번 더 입력하는 편이 자격 증명을 브라우저에 되돌려 보이는 것보다 낫다.
func (webhookChannel) SecretKeys() []string { return []string{"url", "headers"} }

// 대상은 url 이다. url 을 바꿀 때는 headers 를 반드시 다시 지정해야 한다 —— 그렇지 않으면 원래 Authorization 헤더가
// 새 주소로 그대로 보내지며, 이것이 바로 마스킹 우회의 주된 경로다.
func (webhookChannel) DestinationKeys() []string { return []string{"url"} }

// webhookDefaultTemplate 은 템플릿을 채우지 않았을 때의 대비용 요청 본문이다: 직관적인 JSON 구조로,
// 「JSON 하나를 받아 저장」하는 대부분의 자체 수신자를 다룬다.
const webhookDefaultTemplate = `{
  "title": {{json .Title}},
  "batch": {{.Batch}},
  "count": {{.Count}},
  "items": [
{{- range $i, $it := .Items}}
{{- if $i}},{{end}}
    {
      "finding_id": {{$it.FindingID}},
      "name": {{json $it.Name}},
      "vulnclass": {{json $it.VulnClass}},
      "severity": {{json $it.Severity}},
      "summary": {{json $it.Summary}},
      "assets": {{json $it.Assets}},
      "detail_url": {{json $it.DetailURL}}
    }
{{- end}}
  ]
}`

// webhookTemplateData 는 사용자 템플릿에 노출되는 컨텍스트다.
type webhookTemplateData struct {
	Title   string
	Batch   bool
	Count   int
	Items   []webhookItem
	HomeURL string
	// SentAt 은 이번 전달 시각(RFC3339)이며, 수신자가 기록하는 데 쓴다.
	SentAt string
}

type webhookItem struct {
	FindingID     int64
	Name          string
	VulnClass     string
	Severity      string
	SeverityLabel string
	Summary       string
	Assets        []string
	DetailURL     string
	FromStatus    string
	ToStatus      string
	// StatusLabel 은 상태 변경을 읽을 수 있게 설명한 것으로, 예: 「처리 대기 → 수정됨」. 상태 변경이 아니면 비어 있다.
	StatusLabel string
}

func (webhookChannel) Validate(cfg map[string]any) error {
	raw := cfgString(cfg, "url")
	if raw == "" {
		return errors.New("대상 URL 이 없습니다")
	}
	if err := validateHTTPURL(raw); err != nil {
		return fmt.Errorf("대상 URL 이 유효하지 않습니다: %w", err)
	}
	if m := strings.ToUpper(cfgString(cfg, "method")); m != "" && m != http.MethodGet && m != http.MethodPost && m != http.MethodPut && m != http.MethodPatch {
		return fmt.Errorf("지원하지 않는 메서드 %s (GET/POST/PUT/PATCH 사용 가능)", m)
	}
	if tpl := cfgString(cfg, "body_template"); tpl != "" {
		if _, err := parseWebhookTemplate(tpl); err != nil {
			return fmt.Errorf("요청 본문 템플릿 문법 오류: %w", err)
		}
	}
	return nil
}

func (c webhookChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	method := strings.ToUpper(cfgString(cfg, "method"))
	if method == "" {
		method = http.MethodPost
	}

	// GET 은 요청 본문을 싣지 않는다: 내용을 query 에 넣는 것은 템플릿 능력 범위를 넘고 GET 의 의미에도 맞지 않으므로,
	// GET 은 「적중하면 훅을 트리거」하는 수신자에만 적합하다.
	var payload any
	if method != http.MethodGet {
		body, err := renderWebhookBody(cfgString(cfg, "body_template"), m)
		if err != nil {
			return 0, Permanent(err)
		}
		// 템플릿 렌더링 결과는 문자열 형태의 JSON 이므로 여기서 json.RawMessage 로 바꿔 그대로 보낸다.
		// 두 번 이스케이프되어 사용자가 정성껏 만든 구조가 JSON 문자열 안에 갇히는 것을 막는다.
		if !json.Valid([]byte(body)) {
			return 0, Permanent(errors.New("요청 본문 템플릿 렌더링 결과가 올바른 JSON 이 아닙니다"))
		}
		payload = json.RawMessage(body)
	}

	headers := cfgMap(cfg, "headers")
	if ct := cfgString(cfg, "content_type"); ct != "" {
		// 덮어쓰기를 허용하지만 headers 다음에 적용해 명시적 설정이 우선하도록 보장한다.
		if headers == nil {
			headers = map[string]string{}
		}
		headers["Content-Type"] = ct
	}
	if _, err := doJSON(ctx, method, cfgString(cfg, "url"), headers, payload); err != nil {
		return 0, err
	}
	// 일반 Webhook 은 본문을 자르지 않으므로(수신자는 사용자 자신의 서비스이고 크기는 body_template 이 결정),
	// 배치 전체를 전달된 것으로 친다.
	return len(m.Items), nil
}

// renderWebhookBody 는 사용자 템플릿(또는 기본 템플릿)으로 요청 본문을 렌더링한다.
func renderWebhookBody(tpl string, m Message) (string, error) {
	if strings.TrimSpace(tpl) == "" {
		tpl = webhookDefaultTemplate
	}
	t, err := parseWebhookTemplate(tpl)
	if err != nil {
		return "", fmt.Errorf("요청 본문 템플릿 문법 오류: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, newWebhookTemplateData(m)); err != nil {
		return "", fmt.Errorf("요청 본문 템플릿 렌더링 실패: %w", err)
	}
	return buf.String(), nil
}

// parseWebhookTemplate 은 템플릿을 파싱한다.
//
// missingkey=zero 는 없는 map 키를 오류 대신 0 값으로 렌더링하게 한다——하지만 이 파일의 컨텍스트는 구조체이므로,
// 주된 역할은 .Items 가 비었을 때 range 가 오류를 내지 않게 하는 것이다. 정말 막아야 할 것은 .Items 가 nil 인 경우다.
func parseWebhookTemplate(tpl string) (*template.Template, error) {
	return template.New("body").Funcs(webhookTemplateFuncs).Option("missingkey=zero").Parse(tpl)
}

// webhookTemplateFuncs 는 템플릿에 노출되는 보조 함수다.
var webhookTemplateFuncs = template.FuncMap{
	// json 은 임의의 값을 JSON 으로 직렬화한다.
	//
	// 이 함수는 있으면 좋은 정도가 아니라 필수다: 이것이 없으면 사용자는 {{.Title}} 처럼 바로 값을 끼워 넣을 수밖에 없는데,
	// 취약점 제목에 따옴표나 줄바꿈이 하나만 있어도 요청 본문 전체가 올바른 JSON 이 아니게 된다——수신자는
	// 거부하고, 오류 메시지는 「JSON 파싱 실패」를 가리켜 제목에 따옴표가 있다는 것을 전혀 떠올릴 수 없다.
	"json": func(v any) (string, error) {
		raw, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		return string(raw), nil
	},
	// jsons 는 JSON 조각을 다른 JSON 문자열 값 안에 끼워 넣는 데 쓴다(문자열 이스케이프를 한 번 더 함).
	"jsons": func(v any) (string, error) {
		raw, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		quoted, err := json.Marshal(string(raw))
		if err != nil {
			return "", err
		}
		// 바깥 따옴표를 뗀다: 따옴표를 붙일지는 호출자가 정한다.
		return string(quoted[1 : len(quoted)-1]), nil
	},
}

func newWebhookTemplateData(m Message) webhookTemplateData {
	d := webhookTemplateData{
		Title:   markdownTitle(m),
		Batch:   m.Batch,
		Count:   len(m.Items),
		HomeURL: m.HomeURL,
		SentAt:  time.Now().Format(time.RFC3339),
		Items:   make([]webhookItem, 0, len(m.Items)),
	}
	for _, it := range m.Items {
		wi := webhookItem{
			FindingID:     it.FindingID,
			Name:          it.Name,
			VulnClass:     it.VulnClass,
			Severity:      it.Severity,
			SeverityLabel: SeverityLabel(it.Severity),
			Summary:       it.Summary,
			Assets:        append([]string{}, it.Assets...),
			DetailURL:     it.DetailURL,
			FromStatus:    it.FromStatus,
			ToStatus:      it.ToStatus,
		}
		if it.IsStatusChange() {
			wi.StatusLabel = StatusLabel(it.FromStatus) + " → " + StatusLabel(it.ToStatus)
		}
		d.Items = append(d.Items, wi)
	}
	return d
}
