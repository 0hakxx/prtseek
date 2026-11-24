package notify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// 이 파일은 불변량 테스트다: 채널 구현에서 나오는 **어떤** 오류 텍스트도 자격 증명을 담으면 안 된다.
//
// 왜 파일을 따로 두는가: 최초의 채널 테스트는 성공 경로와 플랫폼 업무 오류만 다루고,
// 전송 계층 실패는 전혀 보지 않았다. 그런데 바로 전송 계층 오류(연결 거부/DNS 실패/시간 초과)가 가장 위험하다——
// http.Client.Do 가 반환하는 *url.Error 는 **전체 URL** 을 오류 텍스트에 찍는데, 이 기능의
// 몇몇 채널은 자격 증명이 URL 안에 있다. 자격 증명이 이 문자열을 타고 네 출구로 흘러간다:
//
//	notification_deliveries.last_error  → 평문으로 DB 저장
//	GET /api/notify/deliveries 응답     → 채널 설정의 마스킹을 우회해 브라우저로 에코
//	서버 로그                          → 종종 외부로 유출/보존됨
//	테스트 전송 인터페이스의 502 응답   → 프런트엔드에 바로 뜸
//
// 그래서 여기서는 함수 하나만 테스트하지 않고, 채널마다 반드시 실패하는 요청을 실제로 한 번 보내 오류 텍스트에서
// 그 자격 증명을 찾을 수 없음을 단언한다.

// credentialCases 는 「자격 증명이 URL 에 있는」 모든 채널 형태를 다룬다:
// 딩톡/위챗 워크는 query, 페이슈는 경로 끝, Telegram 은 경로 중간.
var credentialCases = []struct {
	name   string
	ch     Channel
	cfg    map[string]any
	secret string
}{
	{
		name:   "딩톡 access_token 이 query 에",
		ch:     dingTalkChannel{},
		cfg:    map[string]any{"webhook": "http://127.0.0.1:1/robot/send?access_token=" + leakProbeToken},
		secret: leakProbeToken,
	},
	{
		name:   "위챗 워크 key 가 query 에",
		ch:     weComChannel{},
		cfg:    map[string]any{"webhook": "http://127.0.0.1:1/cgi-bin/webhook/send?key=" + leakProbeToken},
		secret: leakProbeToken,
	},
	{
		name:   "페이슈 hook id 가 경로 끝에",
		ch:     feishuChannel{},
		cfg:    map[string]any{"webhook": "http://127.0.0.1:1/open-apis/bot/v2/hook/" + leakProbeToken},
		secret: leakProbeToken,
	},
	{
		name:   "Telegram bot token 이 경로 중간에",
		ch:     telegramChannel{},
		cfg:    map[string]any{"bot_token": leakProbeToken, "chat_id": "1", "base_url": "http://127.0.0.1:1"},
		secret: leakProbeToken,
	},
	{
		name:   "딩톡 서명 키",
		ch:     dingTalkChannel{},
		cfg:    map[string]any{"webhook": "http://127.0.0.1:1/robot/send", "secret": leakProbeToken},
		secret: leakProbeToken,
	},
}

// leakProbeToken 은 절대 실제 자격 증명일 수 없는 센티넬 값으로, 오류 텍스트에서 이를 검색하는 데 쓴다.
const leakProbeToken = "LEAKPROBE0123456789abcdef"

// TestChannelErrorsNeverLeakCredentials 는 핵심 불변량이다.
func TestChannelErrorsNeverLeakCredentials(t *testing.T) {
	for _, tc := range credentialCases {
		t.Run(tc.name, func(t *testing.T) {
			// 반드시 실패하는 상대: 127.0.0.1:1 은 아무도 수신하지 않아 연결 거부 경로를 탄다.
			_, err := tc.ch.Send(context.Background(), tc.cfg, Message{
				Items: []Item{{FindingID: 1, Severity: "high", Name: "유출 탐침"}},
			})
			if err == nil {
				t.Fatal("도달 불가 주소는 오류를 내야 함")
			}
			assertNoSecret(t, err.Error(), tc.secret)
		})
	}
}

// TestChannelErrorsNeverLeakCredentialsInPermanentPath 는 영구 실패 분기를 다룬다:
// URL 검증 실패, 플랫폼 업무 오류 등도 오류 텍스트를 외부로 전달하므로 마찬가지로 자격 증명을 담으면 안 된다.
func TestChannelErrorsNeverLeakCredentialsInPermanentPath(t *testing.T) {
	cases := []struct {
		name string
		ch   Channel
		cfg  map[string]any
	}{
		// 주소에 자격 증명이 있지만 형식이 잘못됨 → validateHTTPURL / url.Parse 분기 유발.
		{"딩톡 주소 잘못됨", dingTalkChannel{}, map[string]any{"webhook": "file:///" + leakProbeToken}},
		{"위챗 워크 주소 잘못됨", weComChannel{}, map[string]any{"webhook": "gopher://" + leakProbeToken}},
		{"페이슈 주소 잘못됨", feishuChannel{}, map[string]any{"webhook": "ftp://" + leakProbeToken + "/hook"}},
		{"Telegram API 주소 잘못됨", telegramChannel{}, map[string]any{"bot_token": "tok", "chat_id": "1", "base_url": "file://" + leakProbeToken}},
		{"범용 Webhook 주소 잘못됨", webhookChannel{}, map[string]any{"url": "javascript:" + leakProbeToken}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.ch.Send(context.Background(), tc.cfg, Message{Items: []Item{{Severity: "high"}}})
			if err == nil {
				t.Fatal("잘못된 설정은 오류를 내야 함")
			}
			assertNoSecret(t, err.Error(), leakProbeToken)
		})
	}
}

func assertNoSecret(t *testing.T, text, secret string) {
	t.Helper()
	if strings.Contains(text, secret) {
		t.Fatalf("오류 텍스트가 자격 증명 %q 를 유출함:\n    %s", secret, text)
	}
}

func TestRedactRequestTargetKeepsOnlySchemeAndHost(t *testing.T) {
	cases := map[string]string{
		"https://oapi.dingtalk.com/robot/send?access_token=S1":    "https://oapi.dingtalk.com/…",
		"https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=S2": "https://qyapi.weixin.qq.com/…",
		"https://open.feishu.cn/open-apis/bot/v2/hook/S3":         "https://open.feishu.cn/…",
		"https://api.telegram.org/botS4/sendMessage":              "https://api.telegram.org/…",
		"http://10.0.0.5:8080/hook":                               "http://10.0.0.5:8080/…",
	}
	for in, want := range cases {
		got := redactRequestTarget(in)
		if got != want {
			t.Errorf("redactRequestTarget(%q) = %q, 기대 %q", in, got, want)
		}
		// 탈민 결과 자체에 원래 주소의 어떤 경로/쿼리 조각도 다시 담으면 안 된다.
		if parts := strings.SplitN(in, "://", 2); len(parts) == 2 {
			if hostAndRest := strings.SplitN(parts[1], "/", 2); len(hostAndRest) == 2 && hostAndRest[1] != "" {
				if strings.Contains(got, hostAndRest[1]) {
					t.Errorf("탈민 후에도 경로/쿼리 조각 %q 가 남음: %q", hostAndRest[1], got)
				}
			}
		}
	}
	// 파싱 불가한 입력은 절대 원래 문자열을 에코하지 않는다.
	for _, bad := range []string{"", "://", "not a url", "http://"} {
		if got := redactRequestTarget(bad); strings.Contains(got, bad) && bad != "" {
			t.Errorf("파싱 불가 입력 %q 가 %q 로 에코됨", bad, got)
		}
	}
}

// TestRedactTransportErrorStripsURL 은 *url.Error 라는 구체 타입을 직접 겨냥한다:
// 이것은 http.Client.Do 의 반환 타입이자 유출의 첫 현장이다.
func TestRedactTransportErrorStripsURL(t *testing.T) {
	inner := errors.New("dial tcp 127.0.0.1:1: connect: connection refused")
	uerr := &url.Error{
		Op:  "Post",
		URL: "https://api.telegram.org/bot" + leakProbeToken + "/sendMessage",
		Err: inner,
	}
	got := redactTransportError(uerr)
	assertNoSecret(t, got, leakProbeToken)
	if !strings.Contains(got, "api.telegram.org") {
		t.Errorf("조사를 위해 host 는 유지해야 하는데 %q 얻음", got)
	}
	if !strings.Contains(got, "connection refused") {
		t.Errorf("조사를 위해 하위 원인은 유지해야 하는데 %q 얻음", got)
	}
	// Op 도 유지해야 함(POST 인지 GET 인지가 조사에 의미 있음).
	if !strings.Contains(got, "Post") {
		t.Errorf("조사를 위해 작업명은 유지해야 하는데 %q 얻음", got)
	}
}

// TestRedactURLsInTextHandlesFallback 은 대비 경로다: *url.Error 가 아닌 사용자 정의 오류
// (예: 리다이렉트 정책이 반환한 오류) 안의 주소도 마찬가지로 제거되어야 한다.
func TestRedactURLsInTextHandlesFallback(t *testing.T) {
	in := fmt.Sprintf("교차 호스트 리다이렉트 거부(a.example → http://b.example/bot%s/send)", leakProbeToken)
	got := redactURLsInText(in)
	assertNoSecret(t, got, leakProbeToken)
	if !strings.Contains(got, "http://b.example/…") {
		t.Errorf("주소를 탈민 형태로 바꿔야 하는데 %q 얻음", got)
	}
	// 주소가 없는 텍스트는 그대로 유지한다.
	if plain := "dial tcp: connection refused"; redactURLsInText(plain) != plain {
		t.Error("주소가 없는 텍스트는 바뀌면 안 됨")
	}
}

// TestCrossHostRedirectRefused 는 「자격 증명이 URL 에 있음 + 교차 호스트 점프 따라감 = 자격 증명을 넘김」을 다룬다.
// httptest 의 두 서버는 127.0.0.1 의 다른 포트에서 수신하며, 포트가 다르면 Host 가 다르므로
// 정확히 교차 호스트 점프를 이룬다.
func TestCrossHostRedirectRefused(t *testing.T) {
	var hit bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/robot/send?access_token="+leakProbeToken, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	_, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": redirector.URL + "/robot/send?access_token=" + leakProbeToken},
		Message{Items: []Item{{Severity: "high"}}})
	if err == nil {
		t.Fatal("교차 호스트 리다이렉트는 거부되어야 함")
	}
	if hit {
		t.Fatal("점프 대상이 접근됨——자격 증명이 리다이렉트를 따라 유출됨")
	}
	assertNoSecret(t, err.Error(), leakProbeToken)
}

// TestSameHostRedirectAllowed 는 역방향 테스트다: 동일 호스트 점프(예: 끝에 슬래시 보충)는 여전히 쓸 수 있어야 함.
// 그러지 않으면 정상 워크플로까지 함께 막힌다.
func TestSameHostRedirectAllowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robot/send" {
			// 동일 호스트, 동일 포트의 점프.
			http.Redirect(w, r, "/robot/send/", http.StatusTemporaryRedirect)
			return
		}
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer srv.Close()

	if _, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": srv.URL + "/robot/send"},
		Message{Items: []Item{{Severity: "high"}}}); err != nil {
		t.Fatalf("동일 호스트 리다이렉트는 거부되면 안 됨: %v", err)
	}
}
