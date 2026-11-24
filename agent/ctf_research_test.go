package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/compaction"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

type ctfStreamingFixture struct{ *researchRuntimeProvider }

func (p *ctfStreamingFixture) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		msg, stop, _, err := p.Complete(ctx, req)
		if err != nil {
			yield(llm.StreamEvent{}, err)
			return
		}
		events := []llm.StreamEvent{{Type: llm.SEMessageStart}}
		for _, b := range msg.Content {
			if b.Type == llm.BlockToolUse {
				events = append(events, llm.StreamEvent{Type: llm.SEToolUseStart, ToolID: b.ID, ToolName: b.Name}, llm.StreamEvent{Type: llm.SEToolInputJSON, Text: string(b.Input)})
			} else {
				events = append(events, llm.StreamEvent{Type: llm.SETextDelta, Text: b.Text})
			}
		}
		events = append(events, llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: stop}, llm.StreamEvent{Type: llm.SEMessageStop})
		for _, e := range events {
			if !yield(e, nil) {
				return
			}
		}
	}
}

func TestCTFResearchStreamingCompletesAtExactBudget(t *testing.T) {
	p := &ctfStreamingFixture{&researchRuntimeProvider{}}
	opts, run := ctfTestOptions(t, p, 2)
	opts.NonStreaming = false
	calls := 0
	p.complete = func(llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		calls++
		if calls == 1 {
			return ctfToolReply("inspect_fixture", `{}`, calls)
		}
		state, _ := run.memory.Snapshot()
		id := state.Observations[0].ID
		return ctfToolReply("research_status", fmt.Sprintf(`{"status":"completed","summary":"Verified assigned fixture","evidence_ids":[%q]}`, id), calls)
	}
	s := agentcore.NewSession(opts)
	defer s.Close()
	text, err := captureCTFResearch(t.Context(), s, run, "inspect fixture", "solver", nil)
	if err != nil || calls != 2 || run.snapshot().Requests != 2 || text != "Verified assigned fixture" {
		t.Fatalf("calls=%d state=%+v text=%s err=%v", calls, run.snapshot(), text, err)
	}
	// A tool run after the status decision invalidates that decision.
	researchInvoke(t, &opts, "inspect_fixture", `{"extra":true}`)
	if run.snapshot().Status != "active" {
		t.Fatal("late evidence did not invalidate sealed status")
	}
}

func ctfTestOptions(t *testing.T, p llm.Provider, max int) (agentcore.Options, *ctfResearchRun) {
	t.Helper()
	root := t.TempDir()
	calls := 0
	opts := agentcore.Options{Provider: p, WorkingDir: root, NonStreaming: true, PermissionMode: permission.ModeBypass,
		DisableBackgroundTasks: true, Tools: []actool.CoreTool{researchFixtureTool("inspect_fixture", false, &calls)}}
	if err := AttachResearchRuntime(WithResearchOptimization(t.Context(), true), &opts, "fixture", root, "Inspect the local challenge"); err != nil {
		t.Fatal(err)
	}
	r, err := attachCTFResearch(t.Context(), &opts, "solver", "Inspect the local challenge", max)
	if err != nil {
		t.Fatal(err)
	}
	return opts, r
}

func ctfToolReply(name, input string, n int) (llm.Message, string, llm.Usage, error) {
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: fmt.Sprint("call-", n), Name: name, Input: json.RawMessage(input)}}}, "tool_use", llm.Usage{}, nil
}

func TestCTFResearchCannotSealWithUncollectedChildren(t *testing.T) {
	opts, run := ctfTestOptions(t, &researchRuntimeProvider{}, 0)
	pending := true
	run.completionCheck = func() string {
		if pending {
			return "collect pending child first"
		}
		return ""
	}
	status := `{"status":"blocked","summary":"Missing fixture","blocker":"missing_input","resume_when":"Supply fixture"}`
	researchInvoke(t, &opts, "research_status", status)
	if run.snapshot().Status != "active" {
		t.Fatal("sealed before collecting children")
	}
	pending = false
	researchInvoke(t, &opts, "research_status", status)
	if run.snapshot().Status != "blocked" {
		t.Fatal("failed to seal after collection")
	}
}
func ctfTextReply(text string) (llm.Message, string, llm.Usage, error) {
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock(text)}}, "end_turn", llm.Usage{}, nil
}

func TestCTFResearchContinuesAcrossThreeSimulatedHours(t *testing.T) {
	p := &researchRuntimeProvider{}
	opts, run := ctfTestOptions(t, p, 0)
	now := time.Now()
	start := now
	deadline := now.Add(4 * time.Hour)
	run.state.Deadline = &deadline
	run.now = func() time.Time { return now }
	requests := 0
	p.complete = func(req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		requests++
		now = now.Add(time.Minute)
		if requests <= 180 {
			if requests%2 == 1 {
				return ctfToolReply("inspect_fixture", fmt.Sprintf(`{"hypothesis":%d}`, requests), requests)
			}
			return ctfTextReply("No flag yet. Here is a partial summary.")
		}
		if requests == 181 {
			state, _ := run.memory.Snapshot()
			id := state.Observations[len(state.Observations)-1].ID
			return ctfToolReply("research_status", fmt.Sprintf(`{"status":"completed","summary":"Assigned local inspection verified; no global flag claim","evidence_ids":[%q]}`, id), requests)
		}
		return ctfTextReply("Local inspection complete with evidence")
	}
	s := agentcore.NewSession(opts)
	defer s.Close()
	results := 0
	_, err := captureCTFResearch(t.Context(), s, run, "Inspect local fixture", "solver", func(a db.Activity) {
		if a.Kind == "result" {
			results++
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	state := run.snapshot()
	if now.Sub(start) < 3*time.Hour || state.Status != "completed" || state.Continuations < 80 || requests != 181 || results != 1 {
		t.Fatalf("elapsed=%s requests=%d results=%d state=%+v", now.Sub(start), requests, results, state)
	}
	var persisted CTFResearchState
	b, err := os.ReadFile(filepath.Join(run.memory.root, ".research", "run.json"))
	if err != nil || json.Unmarshal(b, &persisted) != nil || persisted.Status != "completed" {
		t.Fatal("durable completion missing", err)
	}
}

func TestCTFResearchRejectsUnverifiedCompletionAndOldEvidence(t *testing.T) {
	opts, run := ctfTestOptions(t, &researchRuntimeProvider{}, 0)
	for _, input := range []string{`{"status":"completed","summary":"flag guessed"}`, `{"status":"completed","summary":"invented evidence","evidence_ids":["t999999"]}`, `{"status":"blocked","summary":"no flag"}`, `{"status":"blocked","summary":"network down","blocker":"environment","resume_when":"network returns"}`} {
		if res := researchInvoke(t, &opts, "research_status", input); !res.IsError {
			t.Fatalf("accepted %s", input)
		}
	}
	researchInvoke(t, &opts, "inspect_fixture", `{}`)
	state, _ := run.memory.Snapshot()
	id := state.Observations[0].ID
	// A fresh run cannot complete solely on a previous run's successful output.
	run.startEvidence = state.NextID
	if res := researchInvoke(t, &opts, "research_status", fmt.Sprintf(`{"status":"completed","summary":"old result","evidence_ids":[%q]}`, id)); !res.IsError {
		t.Fatal("old evidence accepted")
	}
	if res := researchInvoke(t, &opts, "research_status", `{"status":"blocked","summary":"challenge attachment is absent","blocker":"missing_input","resume_when":"challenge attachment supplied"}`); res.IsError {
		t.Fatal(res.Flatten())
	}
	if run.snapshot().Status != "blocked" {
		t.Fatal("blocker lost")
	}
}

func TestCTFResearchNoWorkAndBudgetsAreNotSuccess(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		max        int
		advance    bool
	}{{"idle", "stalled", 0, false}, {"round-budget", "budget_exhausted", 1, false}, {"time-budget", "budget_exhausted", 0, true}} {
		t.Run(tc.name, func(t *testing.T) {
			p := &researchRuntimeProvider{}
			opts, run := ctfTestOptions(t, p, tc.max)
			now := time.Now()
			deadline := now.Add(4 * time.Hour)
			run.state.Deadline = &deadline
			run.now = func() time.Time { return now }
			calls := 0
			p.complete = func(llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
				calls++
				if tc.advance {
					now = now.Add(5 * time.Hour)
					return ctfToolReply("inspect_fixture", `{}`, calls)
				}
				return ctfTextReply("stopping without evidence")
			}
			s := agentcore.NewSession(opts)
			defer s.Close()
			_, err := captureCTFResearch(t.Context(), s, run, "local test", "solver", nil)
			var incomplete *ResearchIncompleteError
			if !errors.As(err, &incomplete) || incomplete.Status != tc.want || run.snapshot().Status != tc.want {
				t.Fatalf("state=%+v err=%v", run.snapshot(), err)
			}
			if calls > 3 {
				t.Fatalf("text-only loop spun %d requests", calls)
			}
		})
	}
}

func TestCTFResearchContextFreshAfterActualCompaction(t *testing.T) {
	p := &researchRuntimeProvider{}
	opts, run := ctfTestOptions(t, p, 0)
	old := "obsolete-next-step"
	next := []string{old}
	id, err := run.memory.ObserveTool("inspect_fixture", json.RawMessage(`{}`), "missing specific precondition", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run.memory.Record(t.Context(), ResearchRecordInput{NextSteps: &next, Entry: &ResearchEntryInput{Kind: "failure", Summary: "early-route-failed-on-specific-precondition", EvidenceIDs: []string{id}}}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		_, err := run.memory.Record(t.Context(), ResearchRecordInput{Entry: &ResearchEntryInput{Kind: "hypothesis", Summary: fmt.Sprint("later clue ", i)}})
		if err != nil {
			t.Fatal(err)
		}
	}
	next = []string{"new-discriminating-experiment"}
	_, _ = run.memory.Record(t.Context(), ResearchRecordInput{NextSteps: &next})
	c := compaction.New(compaction.Config{ContextWindow: 32000, KeepRecent: 2}, func(context.Context, []llm.Message) (string, error) { return "lossy old conversation summary", nil })
	history := []llm.Message{}
	for i := 0; i < 30; i++ {
		history = append(history, llm.UserText(strings.Repeat("old trace ", 3000)))
	}
	compacted, ok := c.Reactive(t.Context(), history)
	if !ok {
		t.Fatal("real compaction did not run")
	}
	p.complete = func(req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		prompt := strings.Join(req.System, "\n")
		if !strings.Contains(prompt, next[0]) || !strings.Contains(prompt, "early-route-failed-on-specific-precondition") || strings.Contains(prompt, old) {
			t.Fatalf("current plan or old failure lost after compaction: %s", prompt)
		}
		return ctfTextReply("checked")
	}
	_, _, _, err = opts.Provider.Complete(t.Context(), llm.CompletionRequest{System: opts.SystemPrompt, Messages: llm.MessagesForAPI(compacted), Tools: []llm.ToolSchema{{Name: "inspect_fixture"}}})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCTFResearchManualCancelNeverContinues(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	p := &researchRuntimeProvider{}
	opts, run := ctfTestOptions(t, p, 0)
	calls := 0
	p.complete = func(llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		calls++
		cancel()
		return ctfTextReply("partial")
	}
	s := agentcore.NewSession(opts)
	defer s.Close()
	_, err := captureCTFResearch(ctx, s, run, "test", "solver", nil)
	if err == nil || run.snapshot().Status != "cancelled" || calls != 1 {
		t.Fatalf("calls=%d state=%+v err=%v", calls, run.snapshot(), err)
	}
}

func TestCTFResearchProductionChatAndResume(t *testing.T) {
	root := t.TempDir()
	calls := 0
	p := &researchRuntimeProvider{}
	p.complete = func(llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		calls++
		return ctfTextReply("No flag yet")
	}
	tx := transcript.NewStore(filepath.Join(root, "transcripts"))
	chat := NewChatAgent(p, "fixture", root, tx, 32000)
	chat.SetNonStreaming(func() bool { return true })
	_, err := chat.Chat(t.Context(), "solver", "resume-local", "Analyze local attachment", 0, time.Minute, false, nil)
	var incomplete *ResearchIncompleteError
	if !errors.As(err, &incomplete) || incomplete.Status != "stalled" {
		t.Fatal(err)
	}
	p.complete = func(req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		calls++
		if !strings.Contains(strings.Join(req.System, "\n"), "Analyze local attachment") {
			t.Fatal("original goal lost on resume")
		}
		for _, msg := range req.Messages {
			for _, b := range msg.Content {
				if b.Type == llm.BlockToolResult && strings.Contains(blocksText(b.Content), "연구 상태를 저장했습니다: blocked") {
					return ctfTextReply("Please supply attachment")
				}
			}
		}
		return ctfToolReply("research_status", `{"status":"blocked","summary":"attachment not supplied","blocker":"missing_input","resume_when":"supply attachment"}`, calls)
	}
	_, err = chat.Chat(t.Context(), "solver", "resume-local", "Continue", 0, time.Minute, false, nil)
	if !errors.As(err, &incomplete) || incomplete.Status != "blocked" {
		t.Fatal(err)
	}
}
