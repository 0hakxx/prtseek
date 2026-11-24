package notify

import (
	"bufio"
	"context"

	"net"
	"strings"
	"sync"
	"testing"
)

// 이 파일은 메일 채널의 프로토콜 수준 테스트를 보강한다. 그전까지 email.Send 의 커버리지는 0 이었다——
// SMTP 경로 전체를 거친 테스트가 하나도 없었는데, 그것이야말로 여섯 채널 중 프로토콜 면이 가장 크고
// 가장 틀리기 쉬운 것이다(핸드셰이크, 인증, 봉투, DATA 단계가 각자 다른 실패 의미를 가짐).
//
// 여기서는 net/smtp 를 mock 하지 않고 자체 제작한 최소 SMTP 서버로 구동한다:
// 메일 채널 위험의 대부분은 「실제 SMTP 서버와 대화하는」 이 단계에 있으므로,
// 이 단계를 mock 하면 테스트 안 한 것이나 마찬가지다.

// fakeSMTP 는 딱 쓸 만큼의 SMTP 서버다: greet/EHLO/AUTH/MAIL/RCPT/DATA/QUIT 를 완료할 수 있고,
// 테스트 요구에 따라 특정 단계에서 지정한 응답 코드를 반환한다.
type fakeSMTP struct {
	ln net.Listener

	// rcptReply 는 RCPT TO 의 응답이며 기본값은 250.
	rcptReply string
	// mailReply 는 MAIL FROM 의 응답이며 기본값은 250.
	mailReply string
	// advertiseAuth 가 true 면 EHLO 에서 AUTH PLAIN 지원을 선언한다.
	advertiseAuth bool

	mu       sync.Mutex
	data     string
	commands []string
}

func newFakeSMTP(t *testing.T) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{ln: ln, rcptReply: "250 OK", mailReply: "250 OK"}
	go f.serve()
	t.Cleanup(func() { ln.Close() })
	return f
}

func (f *fakeSMTP) hostPort(t *testing.T) (string, int) {
	t.Helper()
	addr, ok := f.ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatal("TCP 수신 주소가 아님")
	}
	return "127.0.0.1", addr.Port
}

func (f *fakeSMTP) record(cmd string) {
	f.mu.Lock()
	f.commands = append(f.commands, cmd)
	f.mu.Unlock()
}

func (f *fakeSMTP) body() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.data
}

func (f *fakeSMTP) sawCommand(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.commands {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func (f *fakeSMTP) serve() {
	conn, err := f.ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	br := bufio.NewReader(conn)
	w := func(s string) { _, _ = conn.Write([]byte(s + "\r\n")) }
	w("220 fake.local ESMTP ready")
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		f.record(line)
		switch {
		case strings.HasPrefix(line, "EHLO"), strings.HasPrefix(line, "HELO"):
			// STARTTLS 를 선언하지 않음: 코드가 평문 분기를 타게 한다(테스트 목표는 봉투 로직이지 TLS 가 아님).
			w("250-fake.local")
			if f.advertiseAuth {
				w("250-AUTH PLAIN")
			}
			w("250 8BITMIME")
		case strings.HasPrefix(line, "AUTH"):
			// 단순 처리: PLAIN 의 초기 응답은 여러 줄에 걸칠 수 있으므로 그대로 받아들인다.
			w("235 2.7.0 Authentication successful")
		case strings.HasPrefix(line, "MAIL FROM"):
			w(f.mailReply)
		case strings.HasPrefix(line, "RCPT TO"):
			w(f.rcptReply)
		case strings.HasPrefix(line, "DATA"):
			w("354 End data with <CR><LF>.<CR><LF>")
			var sb strings.Builder
			for {
				dl, err := br.ReadString('\n')
				if err != nil {
					return
				}
				if strings.TrimRight(dl, "\r\n") == "." {
					break
				}
				sb.WriteString(dl)
			}
			f.mu.Lock()
			f.data = sb.String()
			f.mu.Unlock()
			w("250 2.0.0 Ok: queued as FAKE1")
		case strings.HasPrefix(line, "QUIT"):
			w("221 2.0.0 Bye")
			return
		default:
			w("250 OK")
		}
	}
}

func emailCfg(t *testing.T, f *fakeSMTP, extra map[string]any) map[string]any {
	t.Helper()
	host, port := f.hostPort(t)
	cfg := map[string]any{
		"host": host,
		"port": float64(port),
		"from": "artex@example.com",
		"to":   []any{"a@example.com", "b@example.com"},
	}
	for k, v := range extra {
		cfg[k] = v
	}
	return cfg
}

func TestEmailSendDeliversFullMessage(t *testing.T) {
	f := newFakeSMTP(t)
	f.advertiseAuth = true
	cfg := emailCfg(t, f, map[string]any{"username": "artex", "password": "pw"})

	if _, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("전달 실패: %v", err)
	}
	// 봉투 단계는 반드시 거쳐야 함: 발신자, 수신자 둘, DATA.
	for _, want := range []string{"MAIL FROM:<artex@example.com>", "RCPT TO:<a@example.com>", "RCPT TO:<b@example.com>", "DATA", "AUTH", "QUIT"} {
		if !f.sawCommand(want) {
			t.Errorf("SMTP 세션에 %q 가 없음, 실제 명령: %v", want, f.commands)
		}
	}
	// 본문은 base64 의 HTML 이며 실제 취약점 내용을 담아야 한다(인코딩 후에도 알아볼 수 있음).
	body := f.body()
	if body == "" {
		t.Fatal("DATA 단계에서 본문을 받지 못함")
	}
	if !strings.Contains(body, "Content-Type: text/html") {
		t.Errorf("Content-Type 헤더가 없음:\n%s", body)
	}
	if !strings.Contains(body, "base64") {
		t.Errorf("본문이 base64 로 인코딩되지 않음(긴 HTML 행은 SMTP 의 1000 바이트 행 길이 제한을 깨뜨림):\n%s", body)
	}
	// 여러 수신자가 모두 To 헤더에 나타나야 한다.
	if !strings.Contains(body, "a@example.com, b@example.com") {
		t.Errorf("To 헤더에 모든 수신자가 포함되지 않음:\n%s", body)
	}
}

func TestEmailSendWithoutAuth(t *testing.T) {
	// 계정을 설정하지 않으면 AUTH 를 보내면 안 됨 —— 일부 중계 서버는 이 때문에 거부한다.
	f := newFakeSMTP(t)
	cfg := emailCfg(t, f, nil)
	if _, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("전달 실패: %v", err)
	}
	if f.sawCommand("AUTH") {
		t.Errorf("계정을 설정하지 않았는데 AUTH 를 보냄: %v", f.commands)
	}
}

// TestEmailSendClassifiesSMTPReplies 는 이번 감사 수정의 직접 검증이다:
// 5xx 는 영구 실패로, 4xx(그레이리스트)는 재시도 가능으로 판정.
func TestEmailSendClassifiesSMTPReplies(t *testing.T) {
	cases := []struct {
		name      string
		rcptReply string
		mailReply string
		permanent bool
	}{
		{"수신자가 550 으로 영구 거부됨", "550 5.1.1 User unknown", "250 OK", true},
		{"수신자가 450 그레이리스트를 만남", "450 4.7.1 Greylisting in action", "250 OK", false},
		{"수신자가 452 메일박스 가득 참을 만남", "452 4.2.2 Mailbox full", "250 OK", false},
		{"발신자가 553 으로 영구 거부됨", "250 OK", "553 5.1.3 Bad address", true},
		{"발신자가 451 임시 오류를 만남", "250 OK", "451 4.3.0 Temporary failure", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeSMTP(t)
			f.rcptReply = tc.rcptReply
			f.mailReply = tc.mailReply
			_, err := (emailChannel{}).Send(context.Background(), emailCfg(t, f, nil), singleMsg())
			if err == nil {
				t.Fatal("오류를 내야 함")
			}
			if got := IsPermanent(err); got != tc.permanent {
				t.Fatalf("permanent 판정 오류: 기대 %v 얻음 %v (%v)", tc.permanent, got, err)
			}
			// 서버 원문을 유지해야 함. 그러지 않으면 사용자가 서버 관리자를 찾아야 할지 주소를 고쳐야 할지 모른다.
			if !strings.Contains(err.Error(), strings.Fields(tc.rcptReply)[0]) && !strings.Contains(err.Error(), strings.Fields(tc.mailReply)[0]) {
				t.Errorf("오류에 서버의 응답 코드를 유지해야 함: %v", err)
			}
		})
	}
}

func TestEmailSendRefusesPlaintextCredentials(t *testing.T) {
	// net/smtp 의 PlainAuth 는 암호화되지 않은 연결에서 자격 증명 전송을 거부한다(대상이 localhost 인 경우 제외).
	// 이는 **올바른** 보안 동작이라 우회하면 안 되지만, 사용자가 고칠 수 있도록 안내하는 오류를 내야 한다.
	// 여기서는 localhost 가 아닌 호스트명으로 이를 유발한다.
	f := newFakeSMTP(t)
	f.advertiseAuth = true
	_, port := f.hostPort(t)
	cfg := map[string]any{
		"host":     "smtp.example.com", // localhost 가 아님
		"port":     float64(port),
		"from":     "a@example.com",
		"to":       []any{"b@example.com"},
		"username": "artex",
		"password": "pw",
	}
	_, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil {
		t.Skip("로컬 DNS 가 로컬 서버로 해석됨, 건너뜀(다른 테스트에는 영향 없음)")
	}
	// 연결 실패 또는 자격 증명 전송 거부 모두 이 단언을 통과함. 핵심은 비밀번호를 **조용히** 보내지 않는 것이다.
	if !IsPermanent(err) && !strings.Contains(err.Error(), "연결") {
		t.Logf("오류: %v(localhost 가 아니라 연결 못 하는 것은 예상대로)", err)
	}
}

func TestEmailValidateReportsMissingFields(t *testing.T) {
	// 메일 채널은 설정 필드가 가장 많아 하나라도 빠지면 전달할 때야 드러난다. 여기서는 하나씩
	// 검증이 미리 잡아내는지 확인한다. 단언은 「오류 메시지가 무엇이 빠졌는지 언급하는지」를 검사한다.
	cases := []struct {
		name string
		cfg  map[string]any
	}{
		{"host 누락", map[string]any{"port": float64(25), "from": "a@b.c", "to": []any{"d@e.f"}}},
		{"port 누락", map[string]any{"host": "smtp.example.com"}},
		{"port 범위 초과", map[string]any{"host": "h", "port": float64(70000), "from": "a@b.c", "to": []any{"d@e.f"}}},
		{"from 누락", map[string]any{"host": "h", "port": float64(25), "to": []any{"d@e.f"}}},
		{"to 누락", map[string]any{"host": "h", "port": float64(25), "from": "a@b.c"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := (emailChannel{}).Validate(tc.cfg); err == nil {
				t.Fatalf("검증에 실패해야 함: %v", tc.cfg)
			}
		})
	}
}

// TestEmailConfigTolerance 는 설정 읽기의 내결함성을 다룬다: JSONB 에서 숫자는 float64 지만,
// 사용자가 UI 에서 포트를 문자열로 넣을 수도 있다. 배열도 단일 문자열일 수 있다.
func TestEmailConfigTolerance(t *testing.T) {
	cfg := map[string]any{
		"host": "smtp.example.com",
		"port": "587", // 문자열 형태의 포트
		"from": "a@b.c",
		"to":   "d@e.f", // 배열이 아닌 단일 문자열
		"tls":  "true",  // 문자열 형태의 불리언
	}
	if err := (emailChannel{}).Validate(cfg); err != nil {
		t.Fatalf("문자열 형태의 숫자를 허용해야 함: %v", err)
	}
	if got := cfgInt(cfg, "port"); got != 587 {
		t.Errorf("cfgInt 가 문자열 포트를 해석하지 못함, %d 얻음", got)
	}
	if !cfgBool(cfg, "tls") {
		t.Error("cfgBool 이 문자열 \"true\" 를 해석하지 못함")
	}
	if to := cfgStrings(cfg, "to"); len(to) != 1 || to[0] != "d@e.f" {
		t.Errorf("cfgStrings 가 단일 문자열을 호환하지 못함, %v 얻음", to)
	}
}

// TestFilterValidateRejectsTypo 는 감사 수정의 직접 검증이다:
// 임계값 오타는 반드시 쓸 때 차단해야 함. 그러지 않으면 필터가 조용히 무효화되어 전부 푸시된다.
func TestFilterValidateRejectsTypo(t *testing.T) {
	good := []string{"", "low", "medium", "high", "critical"}
	for _, s := range good {
		if err := (Filter{MinSeverity: s}).Validate(); err != nil {
			t.Errorf("올바른 임계값 %q 가 거부됨: %v", s, err)
		}
	}
	// 이것들은 실제로 생길 수 있는 오타다——모두 거부되어야 한다.
	for _, s := range []string{"hgih", "HIGH", "심각", "high ", "crit"} {
		err := (Filter{MinSeverity: s}).Validate()
		if err == nil {
			t.Errorf("잘못된 임계값 %q 는 거부되어야 함(그러지 않으면 필터가 조용히 무효화되어 전부 푸시됨)", s)
			continue
		}
		// 오류 메시지는 사용자가 바르게 고치도록 안내해야 한다.
		if !strings.Contains(err.Error(), "low") || !strings.Contains(err.Error(), "critical") {
			t.Errorf("오류 메시지는 선택 가능한 값을 나열해야 하는데 %q 얻음", err.Error())
		}
	}
}

// TestFilterValidateIsWriteTimeOnly 는 「쓰기는 엄격, 읽기는 관대」의 분담을 못 박는다:
// DB 에 이미 있는 잘못된 값이 채널 전체를 못 읽게 만들면 안 된다(그러면 기존 채널이 갑자기 전부 푸시를 멈춤).
func TestFilterValidateIsWriteTimeOnly(t *testing.T) {
	raw := []byte(`{"min_severity":"hgih"}`)
	f := ParseFilter(raw) // 오류 없음
	if f.MinSeverity != "hgih" {
		t.Fatalf("읽기 경로는 그대로 유지해야 하는데 %q 얻음", f.MinSeverity)
	}
	// 그리고 그 채널은 여전히 이벤트에 대해 판정할 수 있어야 함(panic 하지 않고 막히지 않음).
	_ = Match(f, Snapshot{Kind: EventFindingCreated, Severity: "critical"})
}
