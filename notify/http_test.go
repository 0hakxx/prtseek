package notify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// 이 파일은 doJSON 의 HTTP 계층 오류 분류를 다룬다.
//
// 왜 따로 테스트하는가: 각 채널 어댑터는 플랫폼 자체의 업무 오류 코드만 다루는데(딩톡 errcode,
// 페이슈 code, Telegram ok 필드), **HTTP 계층**의 분류는 doJSON 이 통합해서 하므로
// 둘은 독립된 두 방어선이다. 이 방어선이 없으면 503 을 반환하는 중계 게이트웨이가 영구 실패로 취급되어
// 재시도를 바로 포기하고, 403 은 재시도 가능으로 취급되어 세 번이나 헛되이 백오프한다.

func replyServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDoJSONClassifiesHTTPStatus(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		permanent bool
	}{
		{"200 성공은 오류가 아님", 200, false},
		{"429 속도 제한은 재시도 가능", 429, false},
		{"408 요청 시간 초과는 재시도 가능", 408, false},
		{"500 서버 오류는 재시도 가능", 500, false},
		{"502 게이트웨이 오류는 재시도 가능", 502, false},
		{"503 서비스 불가는 재시도 가능", 503, false},
		{"400 파라미터 오류는 영구 실패", 400, true},
		{"401 인증 실패는 영구 실패", 401, true},
		{"403 접근 금지는 영구 실패", 403, true},
		{"404 주소 없음은 영구 실패", 404, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := replyServer(t, tc.status, `{"detail":"upstream says no"}`)
			_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
			if tc.status < 300 {
				if err != nil {
					t.Fatalf("2xx 는 오류를 내면 안 됨: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("2xx 가 아니면 오류를 내야 함")
			}
			if got := IsPermanent(err); got != tc.permanent {
				t.Fatalf("HTTP %d 의 permanent 판정 오류: 기대 %v 얻음 %v (%v)",
					tc.status, tc.permanent, got, err)
			}
			// 상태 코드는 반드시 오류에 나타나야 한다. 그러지 않으면 사용자가 자기 설정이 틀렸는지 상대가 죽었는지 판단할 수 없다.
			// Go 의 영어 StatusText 가 아니라 숫자를 단언한다: 본 패키지의 문구는 한국어지만
			// (프로젝트 다른 부분과 일치), 숫자가 언어 독립적이라 안정적으로 단언할 수 있는 부분이다.
			if !strings.Contains(err.Error(), strconv.Itoa(tc.status)) {
				t.Errorf("오류 메시지에 HTTP 상태 코드 %d 가 있어야 하는데 %v 얻음", tc.status, err)
			}
		})
	}
}

// TestDoJSONIncludesResponseSnippet 는 snippet 을 다룬다: 상대가 반환한 오류 설명을 가져와야 한다.
// 그러지 않으면 사용자는 「실패함」만 알 뿐 상대가 왜 거부했는지 모른다.
func TestDoJSONIncludesResponseSnippet(t *testing.T) {
	srv := replyServer(t, 400, `{"error":"invalid webhook token"}`)
	_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
	if err == nil {
		t.Fatal("오류를 내야 함")
	}
	if !strings.Contains(err.Error(), "invalid webhook token") {
		t.Errorf("오류 메시지에 상대의 설명을 가져와야 하는데 %v 얻음", err)
	}
}

// TestDoJSONSnippetIsSingleLineAndBounded 는 snippet 의 형태를 제약한다:
// 상대 응답이 그대로 last_error 열과 프런트엔드 표에 들어가므로, 여러 줄/초과 길이는 레이아웃과 페이로드를 깨뜨린다.
func TestDoJSONSnippetIsSingleLineAndBounded(t *testing.T) {
	// 줄바꿈, 탭, 5000 자 초과 길이 내용이 든 응답.
	long := strings.Repeat("x", 5000)
	srv := replyServer(t, 500, "line1\nline2\r\n\tline3 "+long)
	_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
	if err == nil {
		t.Fatal("오류를 내야 함")
	}
	msg := err.Error()
	if strings.ContainsAny(msg, "\r\n\t") {
		t.Errorf("오류 메시지는 단일 행으로 압축되어야 하는데 %q 얻음", msg)
	}
	// snippet 상한 200 자 + 고정 접두사라, 총량은 반드시 원본 응답보다 훨씬 작아야 한다.
	if len(msg) > 400 {
		t.Errorf("오류 메시지가 너무 긺(%d 바이트), snippet 으로 잘려야 함: %q", len(msg), msg)
	}
}

// TestDoJSONRejectsOversizedResponse 는 읽기에 상한이 있음을 확인한다: 상대가 비정상적으로 초대형 내용을 반환할 때
// 응답 전체를 메모리로 읽으면 안 된다(전달 이력의 모든 건이 last_error 를 하나씩 저장함).
func TestDoJSONRejectsOversizedResponse(t *testing.T) {
	huge := strings.Repeat("A", 1<<20) // 1 MiB
	srv := replyServer(t, 400, huge)
	_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
	if err == nil {
		t.Fatal("오류를 내야 함")
	}
	if len(err.Error()) > 400 {
		t.Errorf("초대형 응답은 길이 제한 읽기로 잘려야 하는데 오류 메시지 길이 %d", len(err.Error()))
	}
}
