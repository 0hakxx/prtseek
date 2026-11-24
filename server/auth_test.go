package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// GetSetting 은 "키 없음"과 "읽기 오류"의 반환값이 error 하나만 다르다: 두 경우 모두 value 가
// 빈 문자열이다. 비밀번호 관련 handler 가 error 를 "아직 비밀번호 미설정"으로 여기면, 데이터베이스가
// 흔들리는 동안 초기화 입구를 열어 버린다——authInit 은 인증되지 않은 요청이 기존 관리자 비밀번호를
// 덮어쓰도록 통과시키고, authStatus 는 프런트엔드를 바로 /setup 으로 보내 그 일을 그대로 하게 한다.
//
// 이 두 테스트는 handler 의 커넥션 풀을 닫아 읽기 실패를 만들고, 두 곳 모두 fail closed 됨을 단언한다.
func TestAuthStatusFailsClosedWhenDataSourceUnavailable(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatalf("SQLite unavailable (%v)", err)
	}
	defer m.Close()
	// 풀을 닫아, 이후 GetSetting 이 sql.ErrNoRows 가 아니라 error 를 반환하게 한다.
	if err := m.pg.Close(); err != nil {
		t.Fatal(err)
	}

	s := &Server{m: m}
	w := httptest.NewRecorder()
	s.authStatus(w, httptest.NewRequest("GET", "/api/auth/status", nil))

	if w.Code != 503 {
		t.Fatalf("status=%d want 503 (읽기 실패를 미초기화로 여기면 사용자를 /setup 으로 보내 비밀번호를 덮어씀); body=%s", w.Code, w.Body.String())
	}
	var payload struct {
		Initialized *bool `json:"initialized"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err == nil && payload.Initialized != nil {
		t.Fatalf("읽기 실패 시 initialized 로 답하면 안 되는데 %v 얻음", *payload.Initialized)
	}
}

func TestAuthInitFailsClosedWhenDataSourceUnavailable(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatalf("SQLite unavailable (%v)", err)
	}
	defer m.Close()
	if err := m.pg.Close(); err != nil {
		t.Fatal(err)
	}

	s := &Server{m: m}
	w := httptest.NewRecorder()
	body := strings.NewReader(`{"password":"correct horse battery"}`)
	s.authInit(w, httptest.NewRequest("POST", "/api/auth/init", body))

	if w.Code != 503 {
		t.Fatalf("status=%d want 503 (읽기 실패 시 통과시키면 인증되지 않은 요청이 기존 비밀번호를 덮어씀); body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "token") {
		t.Fatalf("읽기 실패 시 token 을 발급하면 안 됨: %s", w.Body.String())
	}
}

func TestValidatePassword(t *testing.T) {
	for _, tc := range []struct {
		name, pw string
		wantErr  bool
	}{
		{"빈", "", true},
		{"7 자리", "1234567", true},
		{"8 자리", "12345678", false},
		{"한글 8 자는 바이트가 아니라 문자 수로 셈", "가나다라마바사아", false},
		{"한글 3 자는 9 바이트지만 문자는 3 개뿐", "가나다", true},
		{"72 바이트", strings.Repeat("a", 72), false},
		{"73 바이트는 bcrypt 상한 초과", strings.Repeat("a", 73), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := validatePassword(tc.pw); (got != "") != tc.wantErr {
				t.Fatalf("validatePassword(%q)=%q, wantErr=%v", tc.pw, got, tc.wantErr)
			}
		})
	}
}
