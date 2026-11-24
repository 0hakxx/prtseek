// Package agent wires real LLM-driven planner and work agents (on top of the
// agent-core SDK) to the dual SQLite graph. See docs/ARTEX-架构设计.md
// §4.3 (planner) and §4.4 (work agent).
//
// Provider configuration is read from the environment so the system runs with
// any Anthropic- or OpenAI-format endpoint. If no key is configured, FromEnv
// returns ok=false and the exploration engine stays idle (an LLM is required).
package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Autumn-27/artex/config"
	"github.com/Autumn-27/artex/llmrec"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/compaction"
	"github.com/Autumn-27/norma/llm"
	acperm "github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// Config describes the LLM backend resolved from the environment.
type Config struct {
	Format  llm.Format
	BaseURL string
	APIKey  string
	Model   string
	// Proxy routes all LLM requests through the given proxy URL (http/https/socks5,
	// optionally with user:pass@ credentials). Empty means direct — it does NOT
	// fall back to the standard *_PROXY environment variables.
	Proxy string
	// RatePerSecond / RatePerMinute cap the shared request rate across ALL agents
	// using the provider (0 = that window unlimited).
	RatePerSecond float64
	RatePerMinute float64
	// ContextWindowK is the model's context window in K tokens (user-configured),
	// used to size compaction thresholds. 0 = default; see CompactionWindow.
	ContextWindowK int
	// ThinkingType 은 사고 「켜기/끄기」 필드(thinking.type)를 독립적으로 제어한다:
	//   "" = 보내지 않음(기본값, 이 필드를 지원하지 않는 모델과 호환); "disabled" = 명시적으로 끔;
	//   "enabled" = 켬. ReasoningEffort 와 완전히 분리되어 있다——어떤 인터페이스는 thinking 필드가 없고
	//   강도 파라미터만으로 사고를 활성화하므로, 둘은 각각 따로 설정할 수 있다.
	ThinkingType string
	// ReasoningEffort 는 사고 「강도」 필드를 독립적으로 제어한다:
	//   "" = 보내지 않음(기본값); "low"/"medium"/"high"/"xhigh"/"max" = 해당 강도.
	//   OpenAI 는 최상위 reasoning_effort 로, Anthropic 은 output_config.effort 로 매핑된다.
	ReasoningEffort string
	// Stream 은 이 profile 이 스트리밍(SSE) 인터페이스를 쓸지 제어한다. true(기본값) = 스트리밍, false = 진짜
	// 비스트리밍(stream:false 를 보내 완전한 JSON 을 한 번에 받으며 Provider.Complete 사용). 비스트리밍은
	// 일부 게이트웨이의 형편없는 SSE 구현(빈 프레임, 사고 필드 프레임 유실)을 피할 수 있지만, 실행 중 실시간 진행/실시간
	// token 집계를 잃는다. agentcore.Options.NonStreaming = !Stream 으로 매핑된다.
	Stream bool
	// MaxTokens 는 한 번의 응답에 대한 출력 상한(token)이다. 0 = 이 필드를 보내지 않고 서버 기본값에 맡긴다
	// (기존 동작). ContextWindowK 와 다르다: 후자는 모델 전체 용량이며 로컬에서 압축 임계값 계산에만 쓰이고
	// 요청에는 나타나지 않는다. 이 값은 매 요청에 함께 보내진다. agentcore.Options.MaxTokens 로 매핑된다.
	MaxTokens int
	// MaxTokensField 는 MaxTokens 에 어떤 요청 필드 이름을 쓸지 고르며, format=openai 일 때만 적용된다:
	//   "" = max_tokens(기본값); "max_completion_tokens" = 새 필드.
	// OpenAI 추론 모델(o 시리즈/GPT-5)은 후자만 받으며, max_tokens 를 받으면 바로
	// unsupported_parameter 오류를 낸다. 반면 대부분의 호환 게이트웨이는 전자만 받으므로, 자동 추론하지 않고 사용자가 엔드포인트에 맞게 고른다.
	MaxTokensField string
	// SessionHeaderKey 가 비어 있지 않으면, 모든 LLM 요청에 사용자 정의 HTTP 헤더를 하나 붙인다. 헤더 이름은 이 값이고,
	// 헤더 값은 【현재 대화의 session id】다(chat 대화=conv-<id>, worker=exp<x>-worker-i<intent>
	// 등, WorkerSessionID 참고). session-id 헤더로 프롬프트 캐시/고정 라우팅을 하는 일부 게이트웨이용이다.
	// 비어 있음 = 보내지 않음. 값은 transcript.WithSessionID 가 요청 context 에 걸고, RoundTripper 가
	// 읽어 채우므로, 같은 공유 provider 도 대화마다 다른 헤더 값을 보낼 수 있다.
	SessionHeaderKey string
	// Retry 는 이 설정에서 해석된 재시도 파라미터다(profile 재정의 → 전역 정책 → 내장 기본값,
	// server 측에서 해석). 세 계층의 의미는 RetryConfig 참고. 0 값 = 내장 기본값을 그대로 사용.
	Retry RetryConfig
}

// RetryConfig 는 LLM 설정 하나에 따라붙는 재시도 파라미터다. 각 계층의 「횟수」는 같은 의미를 가진다:
// 0 = 내장 기본 횟수 사용; 음수 = 해당 계층 재시도 끔; >0 = 그 값 사용. 각 계층의 「간격」:
// 0 = 그 계층 원래의 지수 백오프 사용; >0 = 이 고정 간격으로 바꿔 사용.
type RetryConfig struct {
	// ConnectAttempts/ConnectInterval: SDK 연결 재시도(연결 재설정/시간 초과/429/5xx, 스트림 시작 전),
	// llm.Config.MaxRetries / RetryInterval 로 바로 매핑된다. 기본 3회, 0.5s 부터 지수 증가(최대 8s).
	ConnectAttempts int
	ConnectInterval time.Duration
	// EmptyAttempts/EmptyInterval: SDK 빈 응답 재시도(완료되었지만 content block 이 없음, openai
	// 형식만), llm.Config.EmptyResponseRetries / EmptyResponseInterval 로 매핑된다.
	// 기본 2회, 같은 지수 단계.
	EmptyAttempts int
	EmptyInterval time.Duration
	// StreamAttempts/StreamInterval: 같은 provider 의 안전 구간 재시도——이 프로젝트가 SDK 위에 덧붙인
	// 계층으로, 「아직 호출자에게 어떤 출력도 넘기지 않았을 때」만 스트림 끊김/과부하/스트림 내 429 를 재생한다. SDK 는 이를 모르며,
	// server/task_llm.go 가 사용한다. 기본 2회, 0.5s 부터 지수 증가(최대 4s).
	StreamAttempts int
	StreamInterval time.Duration
}

// compaction window resolution bounds (in K tokens). Below the floor the
// threshold math (window − summary reserve − buffer) would go non-positive and
// compaction would fire every turn; above the cap it would never fire.
const (
	defaultWindowK = 200  // unset → assume a 200K window (Claude default)
	minWindowK     = 32   // floor so effectiveWindow stays comfortably positive
	maxWindowK     = 1000 // cap at 1M tokens (user request)
)

// CompactionWindow returns the model context window in TOKENS for compaction
// thresholds, resolved from the user-configured size (ContextWindowK). 0/unset →
// a 200K default; otherwise clamped to [32K, 1M] so compaction stays effective.
func (c Config) CompactionWindow() int {
	k := c.ContextWindowK
	if k <= 0 {
		k = defaultWindowK
		if cap, ok := FlashModelCapabilities(c.Model); ok {
			k = cap.WorkingContextTokens / 1000
		}
	}
	if k < minWindowK {
		k = minWindowK
	}
	if k > maxWindowK {
		k = maxWindowK
	}
	return k * 1000
}

// compactionConfig builds the agent-core compaction config for a context window
// in tokens. agentcore.NewSession wires the summarizer (same provider) when this
// is set on Options.Compaction.
func compactionConfig(windowTokens int) *compaction.Config {
	if windowTokens <= 0 {
		windowTokens = defaultWindowK * 1000
	}
	return &compaction.Config{ContextWindow: windowTokens}
}

// FromEnv resolves the LLM provider config:
//
//	prtseek_LLM_PROVIDER = anthropic|openai (default: inferred from keys)
//	prtseek_LLM_MODEL    = model id        (default: per provider)
//	prtseek_LLM_BASE_URL = endpoint        (optional)
//	prtseek_LLM_PROXY    = proxy URL        (optional; http/https/socks5)
//	ANTHROPIC_API_KEY / OPENAI_API_KEY         = credentials
//
// Legacy ARTEX_* variables remain fallback aliases.
func FromEnv() (Config, bool) {
	prov := config.Env("LLM_PROVIDER")
	anthKey := os.Getenv("ANTHROPIC_API_KEY")
	oaiKey := os.Getenv("OPENAI_API_KEY")

	if prov == "" {
		switch {
		case anthKey != "":
			prov = "anthropic"
		case oaiKey != "":
			prov = "openai"
		default:
			return Config{}, false
		}
	}

	c := Config{
		BaseURL: config.Env("LLM_BASE_URL"),
		Model:   config.Env("LLM_MODEL"),
		Proxy:   strings.TrimSpace(config.Env("LLM_PROXY")),
		// 기본은 스트리밍. prtseek_LLM_STREAM=false/0/off 로 명시적으로 끄면 비스트리밍을 쓴다.
		Stream: !isFalsy(config.Env("LLM_STREAM")),
	}
	switch prov {
	case "openai":
		c.Format = llm.FormatOpenAI
		c.APIKey = oaiKey
		if c.Model == "" {
			c.Model = "gpt-4o"
		}
	case "openai-responses":
		c.Format = llm.FormatOpenAIResponses
		c.APIKey = oaiKey
		if c.Model == "" {
			c.Model = "gpt-5"
		}
	default:
		c.Format = llm.FormatAnthropic
		c.APIKey = anthKey
		if c.Model == "" {
			c.Model = "claude-opus-4-8"
		}
	}
	if c.APIKey == "" {
		return Config{}, false
	}
	return c, true
}

// ConfigFrom builds a Config from UI-provided strings (provider defaults to
// anthropic; model defaults per provider). Inputs are trimmed and the base URL
// is normalized to the API base the provider expects (the provider appends the
// endpoint path itself), so a full endpoint URL is tolerated.
func ConfigFrom(provider, model, baseURL, apiKey, proxy string) Config {
	c := Config{
		Model:   strings.TrimSpace(model),
		BaseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		APIKey:  strings.TrimSpace(apiKey),
		Proxy:   strings.TrimSpace(proxy),
		Stream:  true, // 기본은 스트리밍. 호출자가 profile 별로 재정의
	}
	switch strings.TrimSpace(provider) {
	case "openai":
		c.Format = llm.FormatOpenAI
		// provider appends "/chat/completions"; tolerate a full endpoint URL.
		c.BaseURL = strings.TrimRight(strings.TrimSuffix(c.BaseURL, "/chat/completions"), "/")
		if c.Model == "" {
			c.Model = "gpt-4o"
		}
	case "openai-responses":
		c.Format = llm.FormatOpenAIResponses
		// provider appends "/responses"; tolerate a full endpoint URL.
		c.BaseURL = strings.TrimRight(strings.TrimSuffix(c.BaseURL, "/responses"), "/")
		if c.Model == "" {
			c.Model = "gpt-5"
		}
	default:
		c.Format = llm.FormatAnthropic
		// provider appends "/v1/messages".
		c.BaseURL = strings.TrimRight(strings.TrimSuffix(c.BaseURL, "/v1/messages"), "/")
		if c.Model == "" {
			c.Model = "claude-opus-4-8"
		}
	}
	return c
}

// isFalsy reports whether an env-var string explicitly requests "off". Empty or
// unrecognized → false (so an unset var keeps the streaming default).
func isFalsy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "0", "false", "off", "no":
		return true
	}
	return false
}

// Provider returns the short provider name ("anthropic"/"openai").
func (c Config) Provider() string {
	switch c.Format {
	case llm.FormatOpenAI:
		return "openai"
	case llm.FormatOpenAIResponses:
		return "openai-responses"
	}
	return "anthropic"
}

// NewProvider builds a provider with its own HTTP request limiter. Every HTTP
// attempt, including SDK retries, counts toward both rolling rate windows.
func (c Config) NewProvider() (llm.Provider, error) {
	return c.NewProviderWithLimiter(&RequestLimiter{})
}

// NewProviderWithLimiter reuses quota history across provider rebuilds. Callers
// sharing a profile must retain the same limiter outside their provider cache.
func (c Config) NewProviderWithLimiter(limiter *RequestLimiter) (llm.Provider, error) {
	client, err := quotaAwareHTTPClient(c.Proxy, c.SessionHeaderKey)
	if err != nil {
		return nil, err
	}
	return c.newProvider(client, limiter)
}

func (c Config) newProvider(client *http.Client, limiter *RequestLimiter) (llm.Provider, error) {
	if limiter == nil {
		limiter = &RequestLimiter{}
	}
	if err := limiter.configure(c.RatePerSecond, c.RatePerMinute); err != nil {
		return nil, err
	}
	client.Transport = rateLimitedTransport{base: client.Transport, limiter: limiter}
	if c.Format == llm.FormatOpenAIResponses && IsDeepSeekFlash(c.Model) {
		return newFlashResponsesProvider(c, client)
	}
	flash := c.Format == llm.FormatOpenAI && IsDeepSeekFlash(c.Model)
	if flash {
		// Preserve the raw capture below this layer, then normalize indexed tool
		// deltas before Norma's index-less StreamEvent adapter sees them.
		client.Transport = flashStreamTransport{base: client.Transport}
	}
	lc := llm.Config{
		Format:     c.Format,
		BaseURL:    c.BaseURL,
		APIKey:     c.APIKey,
		Model:      c.Model,
		HTTPClient: client,
	}
	// 사고 켜기/끄기와 강도 두 필드는 각각 그대로 전달된다(비어 있음 = 해당 필드를 보내지 않음). 둘은 분리되어 있다:
	// thinking.type 만 보내거나, effort 만 보내거나, 둘 다 보내거나, 둘 다 보내지 않을 수 있다.
	lc.ThinkingType = c.ThinkingType
	lc.ReasoningEffort = c.ReasoningEffort
	// 출력 상한의 필드 이름 선택(비어 있음 = max_tokens 사용). 상한 「값」은 여기 없다: 매 턴
	// agentcore.Options.MaxTokens 로 전달되며, provider 는 어떤 키에 넣을지만 정한다.
	lc.MaxTokensField = c.MaxTokensField
	// 재시도 파라미터는 SDK 와 같은 의미이며(횟수 0=기본/음수=끔, 간격 0=지수 백오프/>0=고정), 그대로 전달한다.
	lc.MaxRetries = c.Retry.ConnectAttempts
	lc.RetryInterval = c.Retry.ConnectInterval
	lc.EmptyResponseRetries = c.Retry.EmptyAttempts
	lc.EmptyResponseInterval = c.Retry.EmptyInterval
	// Norma's limiter allows a full-window burst and excludes establishment
	// retries. Enforce limits on HTTP attempts instead; do not also enable it.
	provider, err := llm.NewProvider(lc)
	if err != nil || !flash {
		return provider, err
	}
	return newFlashProvider(provider, c), nil
}

// IsQuotaExhaustedMessage deliberately recognizes only explicit balance,
// billing, credit, or quota-exhaustion signals. Generic 429/rate-limit text,
// authentication failures, network errors, and server failures are excluded.
var nonFailoverHTTPStatus = regexp.MustCompile(`(?:status(?:\s+code)?|http(?:\s+status)?)\s*[=:]?\s*(?:401|403|5\d\d)\b`)
var transientQuotaLimit = regexp.MustCompile(`(?i)(?:\b(?:rpm|tpm|rpd|qps)\b|quota[_\s-]*metric|rate[_\s-]*limit|too many requests|(?:requests?|tokens?)\s+(?:per|/)\s*(?:second|minute)|(?:per|/)\s*(?:second|minute)\s+(?:requests?|tokens?)|generate[_\s-]*requests[_\s-]*per[_\s-]*(?:minute|second)|tokens?[_\s-]*per[_\s-]*(?:minute|second))`)

func IsQuotaExhaustedMessage(message string) bool {
	message = strings.ToLower(message)
	// Authentication/authorization and provider-side 5xx failures never rotate,
	// even when a gateway happens to echo a quota-looking phrase in the body.
	if nonFailoverHTTPStatus.MatchString(message) {
		return false
	}
	// Provider APIs frequently describe an ordinary rate limit as "quota
	// exceeded", especially Google-style responses containing a quota metric.
	// These limits recover with time and must stay on the current provider.
	if transientQuotaLimit.MatchString(message) {
		return false
	}
	markers := []string{
		"insufficient_quota", "quota_exceeded", "quota exceeded", "quota exhausted",
		"exceeded your current quota", "billing_hard_limit_reached",
		"billing hard limit", "billing_not_active", "credit balance", "insufficient credit",
		"insufficient balance", "balance is too low", "payment required", "status 402",
		"余额不足", "额度不足", "额度已用尽", "欠费",
		"잔액이 부족", "잔액 부족", "할당량 소진", "한도 초과", "잔고 부족",
	}
	for _, marker := range markers {
		if strings.Contains(message, marker) {
			return true
		}
	}
	// gRPC RESOURCE_EXHAUSTED is overloaded for both account quota and ordinary
	// request-rate limiting. Preserve it as an explicit exhaustion signal only
	// when the same error does not identify a transient rate limit.
	return strings.Contains(message, "resource_exhausted") &&
		!strings.Contains(message, "rate limit") &&
		!strings.Contains(message, "too many requests")
}

// quotaAwareTransport preserves Norma's normal retry behavior except for a 429
// whose body explicitly says the account quota/balance is exhausted. Norma's
// retry loop treats every 429 as transient; normalizing only that response to
// 402 lets a task router fail over immediately while retaining the original
// response body for provider-specific classification and audit logs.
type quotaAwareTransport struct {
	base http.RoundTripper
	// sessionHeaderKey, when non-empty, is the HTTP header name each request
	// carries; its value is the session id read from the request context. Empty
	// disables it. See Config.SessionHeaderKey.
	sessionHeaderKey string
}

func (t quotaAwareTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Custom session-id header: name is user-configured, value is THIS run's
	// session id (norma stashes it on the context via transcript.WithSessionID).
	// Stable across a session's turns and distinct across sessions — exactly what
	// a session-keyed prompt cache wants. Skipped when no session id is present.
	if t.sessionHeaderKey != "" {
		if sid := transcript.SessionIDFrom(req.Context()); sid != "" {
			req.Header.Set(t.sessionHeaderKey, sid)
		}
	}
	// When LLM recording is on, the Recorder puts a Capture on the context so the
	// raw wire bodies can be persisted. This is the only layer that still sees
	// them: norma builds the request body internally and decodes the SSE response
	// before either reaches the recorder.
	capt := llmrec.CaptureFrom(req.Context())
	capt.SetRequest(requestBodySnapshot(req))

	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil {
		return resp, err
	}
	// Tee rather than read: a 200 is an SSE stream that must keep streaming. The
	// 429 branch below reads through this wrapper, so its body lands in the
	// capture before being replaced.
	resp.Body = capt.TeeResponse(resp.StatusCode, resp.Body)

	if resp.StatusCode != http.StatusTooManyRequests {
		return resp, nil
	}
	body, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	if readErr != nil {
		return resp, nil
	}
	if IsQuotaExhaustedMessage(string(body)) {
		resp.StatusCode = http.StatusPaymentRequired
		resp.Status = "402 Payment Required"
	}
	return resp, nil
}

// requestBodySnapshot copies an outgoing request body without consuming it.
// norma builds every model request from a *bytes.Reader, so net/http populates
// GetBody and the copy has no effect on what gets sent.
func requestBodySnapshot(req *http.Request) string {
	if req.GetBody == nil {
		return ""
	}
	rc, err := req.GetBody()
	if err != nil {
		return ""
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		return ""
	}
	return string(b)
}

func quotaAwareHTTPClient(proxy, sessionHeaderKey string) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	proxy = strings.TrimSpace(proxy)
	if proxy == "" {
		transport.Proxy = nil // 비어 있음=직접 연결. HTTP_PROXY/HTTPS_PROXY 환경 변수로 대체하지 않음
	} else {
		proxyURL, err := url.Parse(proxy)
		if err != nil {
			return nil, fmt.Errorf("llm: invalid proxy %q: %w", proxy, err)
		}
		switch proxyURL.Scheme {
		case "http", "https", "socks5":
		case "":
			return nil, fmt.Errorf("llm: proxy %q missing scheme (use http://, https:// or socks5://)", proxy)
		default:
			return nil, fmt.Errorf("llm: unsupported proxy scheme %q (use http, https or socks5)", proxyURL.Scheme)
		}
		transport.Proxy = http.ProxyURL(proxyURL)
	}
	return &http.Client{Transport: quotaAwareTransport{base: transport, sessionHeaderKey: strings.TrimSpace(sessionHeaderKey)}}, nil
}

// logTestConnection prints the raw HTTP status code(s) and response body of a
// connection test to the server log, so "테스트 클릭" leaves a diagnosable trail of
// exactly what the gateway returned — 401 bodies, quota text, empty frames — not
// just the collapsed ok/err the UI shows. Bodies are clipped to keep a chatty
// SSE stream from flooding the log.
func logTestConnection(c Config, capt *llmrec.Capture) {
	safe := func(value string) string {
		if c.APIKey != "" {
			return strings.ReplaceAll(value, c.APIKey, "[REDACTED]")
		}
		return value
	}
	attempts := capt.Attempts()
	if len(attempts) == 0 {
		log.Printf("[llm-test] %s / %s @ %s — HTTP 요청을 하나도 보내지 않음(설정 해석이나 연결 단계에서 바로 실패)",
			c.Provider(), safe(c.Model), safe(c.BaseURL))
		return
	}
	for i, a := range attempts {
		log.Printf("[llm-test] %s / %s @ %s — 시도 %d/%d HTTP %d\n응답 본문: %s",
			c.Provider(), safe(c.Model), safe(c.BaseURL), i+1, len(attempts), a.Status, clipBody(safe(a.Body)))
	}
}

// clipBody trims a wire body for logging. 4K is plenty to show an error JSON or
// the head of an SSE stream while bounding a runaway response.
func clipBody(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "(비어 있음)"
	}
	const max = 4096
	if len(s) > max {
		return s[:max] + fmt.Sprintf("…(잘림, 전체 %d 바이트)", len(s))
	}
	return s
}

// TestConnection makes a minimal real completion to verify the provider/model/
// endpoint/key actually work. Returns the round-trip latency and the model's
// reply text.
func TestConnection(ctx context.Context, c Config) (time.Duration, string, error) {
	return testConnection(ctx, c, false)
}

// TestAgentConnection verifies a real tool-call/result round trip. The nonce
// is generated on the host and only revealed by an inert in-memory tool.
func TestAgentConnection(ctx context.Context, c Config) (time.Duration, string, error) {
	return testConnection(ctx, c, true)
}

func testConnection(ctx context.Context, c Config, testTools bool) (time.Duration, string, error) {
	prov, err := c.NewProvider()
	if err != nil {
		return 0, "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// 원본 wire 패킷 캡처: 연결 테스트에서 가장 봐야 할 것은 게이트웨이가 실제로 무엇을 돌려줬는지(상태 코드+응답 본문)인데,
	// norma 가 응답을 StreamEvent 로 디코딩하고 나면 이 정보가 사라진다. quotaAwareTransport 는
	// context 에서 이 Capture 를 찾아 매 HTTP 시도의 상태 코드와 body 를 채운다.
	ctx, capt := llmrec.NewCapture(ctx)
	defer logTestConnection(c, capt)
	// 연결 테스트는 단발 경로이며 agentcore 의 대화 루프를 거치지 않으므로, 아무도 context 에
	// session id 를 걸지 않는다. SessionHeaderKey 가 설정된 엔드포인트(예: opencode zen 은
	// x-opencode-session 헤더를 강제하며, 없으면 바로 400 MissingSessionID)에서는 "대화는 정상인데
	// 테스트 클릭은 400"이라는 차이가 생긴다. 여기서 일회용 무작위 session id 를 보충해, 테스트가 실제 대화와 같은
	// 헤더 전송 로직을 타게 한다. SessionHeaderKey 가 설정되지 않은 엔드포인트는 이를 읽지 않으므로 부작용이 없다.
	ctx = transcript.WithSessionID(ctx, "conntest-"+transcript.NewSessionID())
	start := time.Now()
	// MaxTokens 는 넉넉히 줘야 한다: 추론 모델(예: deepseek-v4-pro)은 답을 내기 전에 먼저 긴
	// 사고를 출력한다(실측으로 "ping" 한 마디에도 ~2900 token 을 쓴다). 32 만 주면 모델이 계속 "사고 단계"에 머물다
	// 출력 상한에 부딪혀(finish=length) 잘리며, 연결 테스트는 통과로 치더라도(err=nil)
	// "중단됨/length/resume" 이 뒤엉켜 표시된다. 예산을 넉넉히 줘서 OK 를 깔끔하게 내놓게 한다(finish=stop).
	// EscalateMaxTokens 는 false 로 둔다: 잘렸다고 상한을 올려 재시도하지 않아 resume 루프가 헛도는 것을 막는다.
	tools := []actool.CoreTool{}
	system, prompt, turns := "너는 연결 테스트다. 두 글자 OK 만 바로 출력한다. 생각하지 말고, 설명하지 말고, 다른 것은 아무것도 쓰지 않는다.", "ping", 1
	var called atomic.Bool
	nonce := ""
	if testTools {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return 0, "", err
		}
		nonce = "probe-" + hex.EncodeToString(random[:])
		tools = append(tools, actool.Build(actool.Spec{Name: "connection_probe", Description: "Return the verification receipt. No side effects.", Schema: map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}, Permissions: func(context.Context, json.RawMessage, acperm.Context) acperm.Decision { return acperm.Allowed() }, Run: func(context.Context, json.RawMessage, *actool.ToolContext) (actool.Result, error) {
			called.Store(true)
			return actool.Result{Content: []llm.ContentBlock{llm.TextBlock(nonce)}}, nil
		}}))
		system = "이것은 도구 기능 테스트다. 반드시 connection_probe 를 한 번 호출한 뒤, 도구가 반환한 receipt 를 그대로 답한다. 추측하지 말고, OK 만 답하지 않는다."
		prompt, turns = "Call connection_probe, then repeat its exact receipt.", 3
	}
	maxTokens := 8192
	if c.MaxTokens > 0 {
		maxTokens = min(maxTokens, c.MaxTokens)
	}
	reply, err := agentcore.Run(ctx, agentcore.Options{
		Provider:               prov,
		SystemPrompt:           []string{system},
		Tools:                  tools,
		DisableBackgroundTasks: true,
		PermissionMode:         acperm.ModeBypass,
		MaxTurns:               turns,
		MaxTokens:              maxTokens,
		NonStreaming:           !c.Stream, // 이 profile 의 실제 송수신 방식으로 연결 테스트
	}, prompt)
	lat := time.Since(start)
	if err != nil {
		return lat, "", err
	}
	// err==nil 만으로는 부족하다: 요청은 통했는데 모델이 한 글자도 내지 않는 경우가 실제로 있다(사고가 예산을 다 써 버림,
	// 본문이 안전 정책에 먹힘, 호환 계층이 content 를 버림). 이런 설정은 대화에서는 "응답 없음"인데
	// 테스트는 성공으로 보고한다——바로 이 항목이 없애려는 차이다. 보이는 본문이 없으면 무조건 실패로 판정한다.
	reply = strings.TrimSpace(reply)
	if reply == "" {
		return lat, "", fmt.Errorf("모델 응답 내용이 없습니다(요청은 통했지만 텍스트를 반환하지 않았습니다)")
	}
	if testTools && (!called.Load() || !strings.Contains(reply, nonce)) {
		return lat, "", fmt.Errorf("모델 텍스트 연결은 사용할 수 있지만 도구 호출/결과 전달 검증을 통과하지 못했습니다. 인터페이스의 도구 기능을 확인하세요")
	}
	return lat, reply, nil
}
