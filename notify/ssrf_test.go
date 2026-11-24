package notify

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

// 이 파일은 관련된 두 가지 강화를 다룬다:
//   ① 전달 주소가 서버를 발판으로 내부망 / 클라우드 메타데이터를 때리면 안 됨(SSRF)
//   ② 주소 검증의 오류 메시지가 주소 안의 자격 증명을 내보내면 안 됨
//
// 테스트 환경에 관해: 본 패키지의 많은 테스트가 127.0.0.1 의 httptest 가짜 수신자를 쓰며, 가드는 기본적으로
// 그것들을 차단한다. 그래서 TestMain 에서 일괄로 AllowLocalTargetsEnv 를 켜고, 아래 각 SSRF 테스트는
// 명시적으로 그것을 비워 **기본 거부** 동작을 단언한다.

func TestMain(m *testing.M) {
	// 일반 테스트가 로컬 가짜 수신자에 연결되게 함. SSRF 테스트는 스스로 임시로 비운다.
	_ = os.Setenv(AllowLocalTargetsEnv, "1")
	os.Exit(m.Run())
}

// TestDialGuardRejectsLoopbackByDefault 는 SSRF 보호의 핵심 단언이다:
// 기본 설정에서 루프백 주소로의 전달은 반드시 **연결 계층**에서 거부되어야 한다.
func TestDialGuardRejectsLoopbackByDefault(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		_, _ = io.WriteString(w, `{"errcode":0}`)
	}))
	defer srv.Close()

	t.Setenv(AllowLocalTargetsEnv, "") // 탈출구 끄기 = 기본 동작
	_, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": srv.URL + "/robot/send"}, Message{Items: []Item{{Severity: "high"}}})
	if err == nil {
		t.Fatal("기본값은 루프백 주소로의 전달을 허용하면 안 됨")
	}
	if hit {
		t.Fatal("요청이 이미 로컬 서비스에 도달함——가드가 작동하지 않음")
	}
	// 오류 메시지는 사용자가 어떻게 풀지 안내해야 함(로컬 SMTP 중계는 올바른 설정).
	if !strings.Contains(err.Error(), AllowLocalTargetsEnv) {
		t.Errorf("거부 메시지는 명시적으로 어떻게 풀지 설명해야 함: %v", err)
	}
}

// TestDialGuardAllowsLoopbackWhenOptedIn 은 역방향 테스트다: 명시적으로 켠 뒤에는 반드시 쓸 수 있어야 함.
// 그러지 않으면 로컬 postfix / 내부망 중계 같은 올바른 배포가 일률적으로 폐기된다.
func TestDialGuardAllowsLoopbackWhenOptedIn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer srv.Close()

	t.Setenv(AllowLocalTargetsEnv, "1")
	if _, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": srv.URL + "/robot/send"}, Message{Items: []Item{{Severity: "high"}}}); err != nil {
		t.Fatalf("명시적으로 푼 뒤에는 전달 가능해야 함: %v", err)
	}
}

func TestIsBlockedDialIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "127.1.2.3", "::1",
		"169.254.169.254", // 클라우드 메타데이터 엔드포인트——이 함수가 존재하는 주된 이유
		"169.254.1.1", "fe80::1",
		"0.0.0.0", "::",
		"224.0.0.1", "ff02::1",
		"::ffff:127.0.0.1", // IPv4-mapped 형태는 반드시 복원한 뒤 판정해야 함. 그러지 않으면 우회 구멍이 됨
		"",
	}
	for _, s := range blocked {
		if !isBlockedDialIP(net.ParseIP(s)) {
			t.Errorf("%s 는 거부되어야 함", s)
		}
	}
	// RFC1918 사설망은 **의도적으로 통과**: 내부망 자체 구축 Mattermost / SMTP 중계는 흔한 올바른 용법이다.
	// 이 단언은 그 취사선택을 고정한다——향후 누군가 무심코 사설망 판정을 추가하면 여기서 실패해,
	// 의식적인 결정을 이끌어 낸다(일부 배포를 조용히 폐기하는 대신).
	allowed := []string{"10.0.0.5", "172.16.3.4", "192.168.1.10", "8.8.8.8", "2606:4700::1111"}
	for _, s := range allowed {
		if isBlockedDialIP(net.ParseIP(s)) {
			t.Errorf("%s 는 통과시켜야 함(사설망은 흔한 올바른 전달 대상)", s)
		}
	}
}

// TestValidateHTTPURLRejectsLiteralPrivateTargets 는 설정 단계의 사전 안내를 다룬다:
// 리터럴 IP 는 저장할 때 거부되어야 하며, 첫 전달 실패를 기다리면 안 된다.
func TestValidateHTTPURLRejectsLiteralPrivateTargets(t *testing.T) {
	t.Setenv(AllowLocalTargetsEnv, "")
	for _, raw := range []string{
		"http://127.0.0.1:8080/hook",
		"http://169.254.169.254/latest/meta-data/",
		"http://[::1]:8080/hook",
	} {
		if err := validateHTTPURL(raw); err == nil {
			t.Errorf("%s 는 설정 단계에서 거부되어야 함", raw)
		}
	}
	// 공인 주소와 사설 주소는 평소처럼 통과한다(사설망은 다이얼 단계로 넘기며, 거기서는 막지 않음).
	for _, raw := range []string{"https://oapi.dingtalk.com/robot/send", "http://10.0.0.9/hook"} {
		if err := validateHTTPURL(raw); err != nil {
			t.Errorf("%s 는 검증을 통과해야 함: %v", raw, err)
		}
	}
}

// TestValidateHTTPURLErrorNeverLeaksCredentials 는 감사가 내 지난번에서 지적한 누락 분기다.
//
// url.Parse 가 **실패**하면 *url.Error 를 반환하며, 그 Error() 는 전체 원본 주소를 담는다. 지난번에 나는
// http.Client.Do 의 반환 오류만 탈민하고 여기를 놓쳤다. 그런데 당시 보충한 「영구 실패 경로」 테스트
// (file://, gopher://, ftp://)는 사실 모두 url.Parse 로 성공 파싱되어 scheme 분기를 타므로,
// 전부 초록이어도 이 경로가 안전함을 증명하지 못한다——가짜 보증이다.
func TestValidateHTTPURLErrorNeverLeaksCredentials(t *testing.T) {
	cases := []string{
		"http://127.0.0.1/%zz?access_token=" + leakProbeToken,         // 잘못된 퍼센트 이스케이프
		"https://a.example.com:port/x?access_token=" + leakProbeToken, // 포트가 숫자가 아님
		"http://[::1?access_token=" + leakProbeToken,                  // 괄호가 짝이 안 맞음
	}
	for _, raw := range cases {
		// 먼저 이 입력이 **정말로** url.Parse 를 실패시키는지 확인한다. 이 단계를 안 하면 테스트가
		// 전혀 모르는 사이에 다른 분기를 탈 수 있다(지난번의 가짜 보증이 바로 이렇게 생겼다).
		if _, err := url.Parse(raw); err == nil {
			t.Errorf("%q 는 파싱에 실패해야 함, 그러지 않으면 이 테스트가 목표 분기를 다루지 못함", raw)
			continue
		}
		err := validateHTTPURL(raw)
		if err == nil {
			t.Errorf("%q 는 검증에 실패해야 함", raw)
			continue
		}
		assertNoSecret(t, err.Error(), leakProbeToken)
	}
	// 채널 계층의 래핑도 주소를 내보내지 않는지 확인한다.
	t.Setenv(AllowLocalTargetsEnv, "")
	err := (dingTalkChannel{}).Validate(map[string]any{"webhook": cases[0]})
	if err == nil {
		t.Fatal("잘못된 주소는 검증에 실패해야 함")
	}
	assertNoSecret(t, err.Error(), leakProbeToken)
}

// TestEmailDialGuardRejectsLoopbackByDefault 는 SMTP 채널의 다이얼 가드를 다룬다.
//
// 메일 채널은 예전에 벌거벗은 net.Dialer 를 썼고, 이는 SSRF 보호 전체에서 유일한 구멍이었다: host 를
// 169.254.169.254 나 127.0.0.1 로 넣으면 바로 연결되고, smtp.NewClient 핸드셰이크가 실패하면
// 상대가 반환한 그 한 줄을 오류에 담아 last_error 를 거쳐 전달 이력 인터페이스로 에코한다——바로 다른 채널이
// 이미 닫아 둔 반맹목 읽기 원시 기능이고, 「연결 거부 vs 시간 초과」의 소요 차이로 포트도 탐지할 수 있다.
//
// 본 패키지의 TestMain 은 AllowLocalTargetsEnv 를 전역으로 켰으므로(많은 테스트가 127.0.0.1 의
// 가짜 수신자를 씀), 이 테스트는 반드시 스스로 그것을 비워야 한다——그러지 않으면 가드가 있든 없든 통과하며,
// 그것이 바로 이 구멍이 처음에 어떤 테스트에도 발견되지 않은 이유다.
func TestEmailDialGuardRejectsLoopbackByDefault(t *testing.T) {
	f := newFakeSMTP(t)
	cfg := emailCfg(t, f, nil)

	t.Setenv(AllowLocalTargetsEnv, "") // 탈출구 끄기 = 기본 동작
	_, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil {
		t.Fatal("기본값은 메일을 루프백 주소로 전달하는 것을 허용하면 안 됨")
	}
	// 연결 자체가 수립되면 안 됨: 가드가 Control 훅에서 차단하므로 EHLO 는 영원히 나가지 못한다.
	if f.sawCommand("EHLO") || f.sawCommand("HELO") {
		t.Fatal("SMTP 세션이 이미 수립됨——가드가 작동하지 않음")
	}
	// 오류 메시지는 사용자가 어떻게 풀지 안내해야 함(로컬 postfix 중계는 올바른 설정).
	if !strings.Contains(err.Error(), AllowLocalTargetsEnv) {
		t.Errorf("거부 메시지는 명시적으로 어떻게 풀지 설명해야 함: %v", err)
	}
}

// TestEmailDialGuardAllowsLoopbackWhenOptedIn 은 짝이 되는 역방향 테스트다: 명시적으로 켠 뒤에는
// 반드시 정상 전달되어야 함. 내부망 자체 구축 SMTP / 로컬 중계는 매우 흔한 배포라 가드가 일률적으로 막으면 안 된다.
func TestEmailDialGuardAllowsLoopbackWhenOptedIn(t *testing.T) {
	f := newFakeSMTP(t)
	cfg := emailCfg(t, f, nil)

	t.Setenv(AllowLocalTargetsEnv, "1")
	if _, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("명시적으로 푼 뒤에는 로컬 SMTP 로 전달 가능해야 함: %v", err)
	}
	if !f.sawCommand("EHLO") {
		t.Fatal("EHLO 를 보지 못함——세션이 실제로 수립되지 않음")
	}
}
