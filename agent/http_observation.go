package agent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
	"golang.org/x/net/html"
)

const httpBodyLimit = 2 << 20

type httpRedirect struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Status int    `json:"status"`
}
type httpObservation struct {
	OriginalURL   string         `json:"original_url"`
	FinalURL      string         `json:"final_url"`
	Status        int            `json:"status"`
	Redirects     []httpRedirect `json:"redirects"`
	ElapsedMS     int64          `json:"elapsed_ms"`
	BodyBytes     int            `json:"observed_body_bytes"`
	SHA256        string         `json:"observed_body_sha256"`
	BodyTruncated bool           `json:"body_truncated"`
	TextTruncated bool           `json:"text_truncated"`
	ContentType   string         `json:"content_type"`
	Route         string         `json:"route"`
	Artifact      string         `json:"body_file,omitempty"`
	Note          string         `json:"note,omitempty"`
	Content       string         `json:"content"`
}
type observedWebFetch struct {
	actool.CoreTool
	proxy, ca string
}

func newObservedWebFetch(proxy, ca string) actool.CoreTool {
	return &observedWebFetch{CoreTool: actool.NewWebFetch(actool.WebFetchConfig{Proxy: proxy, CACert: ca}), proxy: proxy, ca: ca}
}
func (t *observedWebFetch) Description() string {
	return "HTTP(S) 페이지/API 를 읽는다. 리디렉션 체인, 최종 URL, 상태, 소요 시간, 읽은 응답 길이/SHA256, 제한된 본문을 반환한다. 기본적으로 HTML 내비게이션/스크립트 잡음을 제거하며, 원본 응답은 이 대화의 .research/http 에 저장된다. extract=true 면 폼, 링크, 스크립트 참조, 주석을 유지한다. HTTP 200 이 목표 달성을 뜻하지는 않는다. 실패해도 출구를 조용히 바꾸지 않는다."
}
func (t *observedWebFetch) Prompt() string { return t.Description() }
func (t *observedWebFetch) InputSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{
		"url": map[string]any{"type": "string"}, "extract": map[string]any{"type": "boolean"},
		"max_chars": map[string]any{"type": "integer", "minimum": 500, "maximum": 12000, "description": "본문 글자 수 예산, 기본 8000. 초과분은 body_file 에서 읽는다"},
	}, "required": []string{"url"}}
}
func (t *observedWebFetch) client() (*http.Client, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil // Explicit direct/proxy route, matching Bash's configured route.
	if t.proxy != "" {
		u, err := url.Parse(t.proxy)
		if err != nil || u.Host == "" {
			return nil, fmt.Errorf("invalid configured HTTP proxy")
		}
		tr.Proxy = http.ProxyURL(u)
	}
	if t.ca != "" {
		pem, err := os.ReadFile(t.ca)
		if err != nil {
			return nil, fmt.Errorf("설정된 CA 를 읽지 못했습니다: %w", err)
		}
		pool, _ := x509.SystemCertPool()
		if pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("설정된 CA 에 유효한 PEM 인증서가 없습니다")
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &http.Client{Transport: tr, Timeout: 30 * time.Second}, nil
}
func (t *observedWebFetch) Call(ctx context.Context, raw json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	var in struct {
		URL      string `json:"url"`
		Extract  bool   `json:"extract"`
		MaxChars int    `json:"max_chars"`
	}
	if json.Unmarshal(raw, &in) != nil {
		return actool.Errorf("invalid WebFetch input"), nil
	}
	u, err := url.Parse(in.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return actool.Errorf("사용자 이름/비밀번호가 포함되지 않은 HTTP(S) URL 이 필요합니다"), nil
	}
	if in.MaxChars == 0 {
		in.MaxChars = 8000
	}
	in.MaxChars = max(500, min(in.MaxChars, 12000))
	client, err := t.client()
	if err != nil {
		return actool.Errorf(fmt.Sprintf("HTTP 설정 오류: %v", err)), nil
	}
	defer client.CloseIdleConnections()
	o := httpObservation{OriginalURL: in.URL, FinalURL: in.URL, Redirects: []httpRedirect{}, Route: "direct"}
	if t.proxy != "" {
		o.Route = "configured_proxy"
	}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
			return fmt.Errorf("unsupported redirect scheme")
		}
		if req.URL.User != nil {
			return fmt.Errorf("redirect contains credentials")
		}
		if len(via) >= 10 {
			return fmt.Errorf("redirect limit (10)")
		}
		previous := via[len(via)-1]
		if req.URL.Host != previous.URL.Host || req.URL.Scheme != previous.URL.Scheme {
			req.Header.Del("Authorization")
			req.Header.Del("Cookie")
			req.Header.Del("Proxy-Authorization")
		}
		o.Redirects = append(o.Redirects, httpRedirect{From: previous.URL.String(), To: req.URL.String(), Status: req.Response.StatusCode})
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, in.URL, nil)
	if err != nil {
		return actool.Errorf(err.Error()), nil
	}
	req.Header.Set("User-Agent", "prtseek/1.2 HTTP observation")
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return actool.Errorf(fmt.Sprintf("HTTP transport/TLS error; route=%s; elapsed_ms=%d; no alternate-route retry: %v", o.Route, time.Since(start).Milliseconds(), err)), nil
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, httpBodyLimit+1))
	o.ElapsedMS = time.Since(start).Milliseconds()
	o.FinalURL = resp.Request.URL.String()
	o.Status = resp.StatusCode
	o.ContentType = resp.Header.Get("Content-Type")
	if len(body) > httpBodyLimit {
		o.BodyTruncated = true
		body = body[:httpBodyLimit]
	}
	if readErr != nil {
		o.BodyTruncated = true
		o.Note = "응답 읽기 중단: " + readErr.Error()
	}
	o.BodyBytes = len(body)
	digest := sha256.Sum256(body)
	o.SHA256 = hex.EncodeToString(digest[:])
	if tc != nil && tc.WorkingDir != "" {
		root, openErr := os.OpenRoot(tc.WorkingDir)
		if openErr == nil {
			defer root.Close()
			openErr = root.MkdirAll(filepath.Join(".research", "http"), 0700)
			if openErr == nil {
				path := filepath.Join(".research", "http", rand.Text()+".body")
				var f *os.File
				f, openErr = root.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
				if openErr == nil {
					_, openErr = f.Write(body)
					closeErr := f.Close()
					if openErr == nil {
						openErr = closeErr
					}
					if openErr == nil {
						o.Artifact = filepath.ToSlash(path)
					}
				}
			}
		}
		if openErr != nil {
			o.Note += " 원본 응답 저장 실패: " + openErr.Error()
		}
	}
	text := string(body)
	if strings.Contains(strings.ToLower(o.ContentType), "html") {
		text = focusedHTML(text, in.Extract)
	}
	runes := []rune(text)
	if len(runes) > in.MaxChars {
		o.TextTruncated = true
		text = string(runes[:in.MaxChars])
	}
	o.Content = text
	if strings.TrimSpace(text) == "" {
		o.Note += " 본문이 비어 있음. HTTP 상태를 성공 증거로 삼을 수 없다."
	}
	lower := strings.ToLower(text)
	if strings.Contains(lower, "captcha") || strings.Contains(lower, "verify you are human") || strings.Contains(lower, "unusual traffic") {
		o.Note += " 인증/속도 제한 페이지일 수 있다. 최종 URL 과 본문을 확인할 것. 검색에 유효한 결과가 없으면 더 정확한 키워드나 다른 출처로 바꾸고, 같은 페이지를 반복해서 읽지 않는다."
	}
	if o.BodyTruncated {
		o.Note += " 길이와 SHA256 은 저장된 앞부분에만 해당하며 전체 응답이 아니다."
	}
	encoded, _ := json.Marshal(o)
	return actool.Result{Content: []llm.ContentBlock{llm.TextBlock(string(encoded))}, IsError: resp.StatusCode >= 400 || readErr != nil}, nil
}

func focusedHTML(source string, extract bool) string {
	doc, err := html.Parse(strings.NewReader(source))
	if err != nil {
		return source
	}
	var main *html.Node
	var find func(*html.Node)
	find = func(n *html.Node) {
		if main == nil && n.Type == html.ElementNode && (n.Data == "main" || n.Data == "article") {
			main = n
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			find(c)
		}
	}
	find(doc)
	if main == nil {
		main = doc
	}
	var lines []string
	seen := map[string]bool{}
	add := func(s string) {
		s = strings.Join(strings.Fields(s), " ")
		if s != "" && !seen[s] {
			seen[s] = true
			lines = append(lines, s)
		}
	}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "script", "style", "nav", "header", "footer", "noscript", "svg", "head":
				return
			}
		}
		if n.Type == html.TextNode {
			add(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
		if n.Type == html.ElementNode && n.Data == "a" {
			for _, a := range n.Attr {
				if a.Key == "href" {
					add("link: " + a.Val)
				}
			}
		}
	}
	walk(main)
	if extract {
		add("--- HTML evidence (untrusted page data) ---")
		var scan func(*html.Node)
		scan = func(n *html.Node) {
			if n.Type == html.CommentNode {
				add("comment: " + n.Data)
			}
			if n.Type == html.ElementNode {
				switch n.Data {
				case "form", "input", "script", "meta", "link":
					var attrs []string
					for _, a := range n.Attr {
						attrs = append(attrs, a.Key+"="+a.Val)
					}
					add(n.Data + " " + strings.Join(attrs, " "))
				}
			}
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				scan(c)
			}
		}
		scan(doc)
	}
	return strings.Join(lines, "\n")
}
