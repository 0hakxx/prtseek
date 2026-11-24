package server

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/Autumn-27/norma/llm"
)

// 공회전 턴(생각만, 본문도 도구도 없음)의 인식과 속행, steerHooks.Stop 참고.

func assistantThinking(text string) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		{Type: llm.BlockThinking, Thinking: text, Signature: "sig"},
	}}
}

func TestIsThinkingOnlyTurn(t *testing.T) {
	toolUse := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		{Type: llm.BlockThinking, Thinking: "먼저 포트를 스캔"},
		{Type: llm.BlockToolUse, ID: "t1", Name: "run_nuclei"},
	}}
	cases := []struct {
		name string
		msgs []llm.Message
		want bool
	}{
		{"생각만", []llm.Message{llm.UserText("시작"), assistantThinking("생각해 보자")}, true},
		{"생각+도구", []llm.Message{llm.UserText("시작"), toolUse}, false},
		{"생각+본문", []llm.Message{assistantThinking("생각해 보자"), {
			Role:    llm.RoleAssistant,
			Content: []llm.ContentBlock{{Type: llm.BlockThinking, Thinking: "x"}, llm.TextBlock("결론")},
		}}, false},
		{"본문이 공백 문자뿐", []llm.Message{{
			Role:    llm.RoleAssistant,
			Content: []llm.ContentBlock{{Type: llm.BlockThinking, Thinking: "x"}, llm.TextBlock("  \n ")},
		}}, true},
		{"완전히 빈 assistant 턴", []llm.Message{{Role: llm.RoleAssistant}}, true},
		// 도구 결과는 user 역할이라, 판정은 반드시 그 앞의 assistant 로 거슬러 가야 하며 가까운 것을 오판하면 안 된다.
		{"마지막이 도구 결과", []llm.Message{toolUse, {
			Role:    llm.RoleUser,
			Content: []llm.ContentBlock{{Type: llm.BlockToolResult, ToolUseID: "t1"}},
		}}, false},
		{"assistant 메시지 없음", []llm.Message{llm.UserText("시작")}, false},
		{"빈 이력", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isThinkingOnlyTurn(c.msgs); got != c.want {
				t.Fatalf("isThinkingOnlyTurn = %v, want %v", got, c.want)
			}
		})
	}
}

// fakeHooks 는 프로그래밍 가능한 inner HookRunner 로, steerHooks 가 inner 의 결정을 존중하는지 검증하는 데 쓴다.
type fakeHooks struct {
	prevent  bool
	blocking []string
	msg      string
}

func (f fakeHooks) PreToolUse(context.Context, string, []byte) (bool, string, []byte) {
	return false, "", nil
}
func (f fakeHooks) PostToolUse(context.Context, string, []byte, []byte, bool) {}
func (f fakeHooks) Stop(context.Context, []llm.Message) (bool, []string, string) {
	return f.prevent, f.blocking, f.msg
}

func TestSteerHooksStopNudgesEmptyTurn(t *testing.T) {
	empty := []llm.Message{assistantThinking("먼저 서브도메인을 열거해야겠다")}

	t.Run("공회전 턴에 속행 지시 주입", func(t *testing.T) {
		h := steerHooks{nudges: &atomic.Int64{}, limit: defaultEmptyTurnNudges, label: "worker-1 · #1"}
		prevent, blocking, _ := h.Stop(context.Background(), empty)
		if prevent {
			t.Fatal("공회전 턴은 강제 중지하면 안 됨")
		}
		if len(blocking) != 1 || blocking[0] != emptyTurnNudge {
			t.Fatalf("blocking = %v, want [emptyTurnNudge]", blocking)
		}
	})

	t.Run("본문이나 도구가 있으면 개입 안 함", func(t *testing.T) {
		h := steerHooks{nudges: &atomic.Int64{}, limit: defaultEmptyTurnNudges}
		normal := []llm.Message{{
			Role:    llm.RoleAssistant,
			Content: []llm.ContentBlock{llm.TextBlock("스캔 완료, 열린 포트 없음")},
		}}
		if _, blocking, _ := h.Stop(context.Background(), normal); blocking != nil {
			t.Fatalf("정상 마무리를 공회전으로 오판: %v", blocking)
		}
		if n := h.nudges.Load(); n != 0 {
			t.Fatalf("개입하지 않을 때는 카운트하면 안 됨, got %d", n)
		}
	})

	t.Run("상한 도달 후 마무리 허용", func(t *testing.T) {
		const limit = 5 // 사용자가 「빈 응답 재시도 횟수」를 5 로 설정
		h := steerHooks{nudges: &atomic.Int64{}, limit: limit}
		for i := 1; i <= limit; i++ {
			if _, blocking, _ := h.Stop(context.Background(), empty); len(blocking) != 1 {
				t.Fatalf("%d 번째는 아직 할당량 내여야 함, blocking = %v", i, blocking)
			}
		}
		if _, blocking, _ := h.Stop(context.Background(), empty); blocking != nil {
			t.Fatalf("상한을 넘었는데 아직 주입 중: %v", blocking)
		}
	})

	// 「빈 응답 재시도 횟수」를 -1 로 설정 = 이 계층을 끔, emptyTurnNudgeLimit 은 0 으로 파싱됨.
	t.Run("설정으로 끄면 개입 안 함", func(t *testing.T) {
		h := steerHooks{nudges: &atomic.Int64{}, limit: 0}
		if _, blocking, _ := h.Stop(context.Background(), empty); blocking != nil {
			t.Fatalf("껐는데 아직 주입 중: %v", blocking)
		}
	})

	t.Run("inner 가 강제 중지를 결정하면 덮어쓰지 않음", func(t *testing.T) {
		h := steerHooks{inner: fakeHooks{prevent: true, msg: "guard 가 마무리 거부"}, nudges: &atomic.Int64{}, limit: defaultEmptyTurnNudges}
		prevent, blocking, msg := h.Stop(context.Background(), empty)
		if !prevent || msg != "guard 가 마무리 거부" || blocking != nil {
			t.Fatalf("inner 의 강제 중지가 덮어써짐: prevent=%v blocking=%v msg=%q", prevent, blocking, msg)
		}
		if n := h.nudges.Load(); n != 0 {
			t.Fatalf("inner 에 양보할 때는 할당량을 소비하면 안 됨, got %d", n)
		}
	})

	t.Run("inner 가 이미 속행을 요청하면 덮어쓰지 않음", func(t *testing.T) {
		h := steerHooks{inner: fakeHooks{blocking: []string{"guard 의 속행 이유"}}, nudges: &atomic.Int64{}, limit: defaultEmptyTurnNudges}
		_, blocking, _ := h.Stop(context.Background(), empty)
		if len(blocking) != 1 || blocking[0] != "guard 의 속행 이유" {
			t.Fatalf("inner 의 속행 메시지가 덮어써짐: %v", blocking)
		}
	})

	t.Run("카운터가 없을 때 동작 불변", func(t *testing.T) {
		h := steerHooks{limit: defaultEmptyTurnNudges} // 예: 향후 다른 호출 지점이 nudges 전달을 잊음
		if _, blocking, _ := h.Stop(context.Background(), empty); blocking != nil {
			t.Fatalf("카운터가 없으면 주입하면 안 됨: %v", blocking)
		}
	})
}
