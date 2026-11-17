package server

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
)

func ctfCompletionEvidence(t *testing.T, s *Server, id int64) ctfEvidenceRef {
	t.Helper()
	if err := os.MkdirAll(s.conversationWorkDir(id), 0700); err != nil {
		t.Fatal(err)
	}
	memory, err := agent.NewResearchMemory(s.conversationWorkDir(id))
	if err != nil {
		t.Fatal(err)
	}
	evidenceID, err := memory.ObserveTool("Bash", json.RawMessage(`{"command":"local-fixture-check"}`), "local fixture: expected answer observed", false)
	if err != nil {
		t.Fatal(err)
	}
	return ctfEvidenceRef{ConversationID: id, ID: evidenceID}
}

func ctfCompletionReport(ref ctfEvidenceRef) ctfOutcome {
	return ctfOutcome{State: "verified", Summary: "local fixture answer", Criterion: "observe the expected local fixture answer", Verification: "the local check returned the expected answer", Evidence: []ctfEvidenceRef{ref}}
}

func TestCTFVerifiedFirstResultStopsOtherBranches(t *testing.T) {
	s, c := ctfRegressionServer(t)
	// Triage can directly fan out without an obligatory command conversation.
	c.AgentKey = "solve"
	if _, err := s.m.pg.Exec(`UPDATE conversations SET agent_key='solve' WHERE id=$1`, c.ID); err != nil {
		t.Fatal(err)
	}
	fastReport := make(chan ctfOutcome, 1)
	slowStarted := make(chan struct{})
	slowCancelled := make(chan struct{})
	var fastCalls, slowCalls atomic.Int32
	useCTFFixtureModel(t, s, c, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "slow-completion-fixture") {
			if slowCalls.Add(1) == 1 {
				close(slowStarted)
			}
			<-r.Context().Done()
			close(slowCancelled)
			return
		}
		if fastCalls.Add(1) != 1 {
			t.Error("verified child requested another model round")
			fixtureResearchBlocked(w)
			return
		}
		select {
		case outcome := <-fastReport:
			args, _ := json.Marshal(outcome)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "fixture", "choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "verified", "type": "function", "function": map[string]any{"name": "report_outcome", "arguments": string(args)}}}}, "finish_reason": "tool_calls"}}})
		case <-r.Context().Done():
		}
	})
	ctx, run := s.beginCTFWorkflow(t.Context(), c)
	defer s.closeCTFWorkflow(run)
	result, err := s.parallelCTFDelegationTool(c, ctx).Call(ctx, json.RawMessage(`{"tasks":[{"agent_key":"solver","task":"fast-completion-fixture"},{"agent_key":"solver","task":"slow-completion-fixture"}]}`), nil)
	if err != nil || result.IsError {
		t.Fatal(err, result.Flatten())
	}
	var started struct {
		Jobs []ctfJobView `json:"jobs"`
	}
	if err := json.Unmarshal([]byte(result.Flatten()), &started); err != nil || len(started.Jobs) != 2 {
		t.Fatal(err, result.Flatten())
	}
	select {
	case <-slowStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("slow branch did not start")
	}
	ref := ctfCompletionEvidence(t, s, started.Jobs[0].ID)
	fastReport <- ctfCompletionReport(ref)
	result, err = s.waitCTFAgents(ctx, c.ID, nil, 5*time.Second)
	if err != nil || result.IsError {
		t.Fatal(err, result.Flatten())
	}
	var waited struct {
		Jobs    []ctfJobView `json:"jobs"`
		Pending bool         `json:"pending"`
	}
	if err := json.Unmarshal([]byte(result.Flatten()), &waited); err != nil || !waited.Pending {
		t.Fatalf("first result did not return while slow branch was running: %v %s", err, result.Flatten())
	}
	var verified int
	for _, job := range waited.Jobs {
		if job.State == "completed" && job.Outcome.State == "verified" {
			verified++
		}
	}
	if verified != 1 {
		t.Fatal("first successful result missing", result.Flatten())
	}
	if _, ok := run.verifiedCompletion(c.ID); ok || run.completionCheck(c.ID) == "" {
		t.Fatal("child subgoal incorrectly completed parent goal")
	}
	select {
	case <-slowCancelled:
		t.Fatal("child subgoal cancelled its sibling")
	default:
	}
	// The parent accepts the existing child evidence without reproducing it.
	raw, _ := json.Marshal(ctfCompletionReport(ref))
	result, err = s.ctfOutcomeTool(c).Call(ctx, raw, nil)
	if err != nil || result.IsError {
		t.Fatal(err, result.Flatten())
	}
	if summary, ok := run.verifiedCompletion(c.ID); !ok || summary != "local fixture answer" || run.completionCheck(c.ID) != "" {
		t.Fatal("validated parent report did not seal completion", summary, ok)
	}
	if _, err := s.startCTFChild(ctx, c, "solver", "unnecessary verification"); err == nil {
		t.Fatal("successful parent dispatched more work")
	}
	select {
	case <-slowCancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("validated success did not cancel in-flight model request")
	}
	s.closeCTFWorkflow(run)
	run.mu.Lock()
	slow := run.jobs[started.Jobs[1].ID]
	state, delivered, active := slow.state, slow.delivered, run.active
	run.mu.Unlock()
	if state != "cancelled" || !delivered || active != 0 || fastCalls.Load() != 1 || slowCalls.Load() != 1 {
		t.Fatalf("workflow not drained: slow=%s delivered=%v active=%d calls=%d/%d", state, delivered, active, fastCalls.Load(), slowCalls.Load())
	}
}

func TestCTFInvalidAndPartialOutcomeDoNotStopResearch(t *testing.T) {
	s, c := ctfRegressionServer(t)
	started := make(chan struct{})
	cancelled := make(chan struct{})
	useCTFFixtureModel(t, s, c, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
		close(cancelled)
	})
	ctx, run := s.beginCTFWorkflow(t.Context(), c)
	defer s.closeCTFWorkflow(run)
	if _, err := s.startCTFChild(ctx, c, "solver", "unfinished local fixture"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("child did not start")
	}
	for _, state := range []string{"verified", "partial", "blocked"} {
		raw, _ := json.Marshal(ctfOutcome{State: state, Summary: "unproven claim", Criterion: "expected output"})
		result, err := s.ctfOutcomeTool(c).Call(ctx, raw, nil)
		if err != nil || result.IsError != (state == "verified") {
			t.Fatal(state, err, result.Flatten())
		}
		if _, ok := run.verifiedCompletion(c.ID); ok || run.completionCheck(c.ID) == "" {
			t.Fatal("unverified outcome bypassed unfinished children")
		}
		select {
		case <-cancelled:
			t.Fatal("unverified outcome cancelled useful research")
		default:
		}
	}
}

func TestCTFVerifiedCompletionDoesNotCarryIntoFreshWorkflow(t *testing.T) {
	s, c := ctfRegressionServer(t)
	ctx, first := s.beginCTFWorkflow(t.Context(), c)
	defer s.closeCTFWorkflow(first)
	raw, _ := json.Marshal(ctfCompletionReport(ctfCompletionEvidence(t, s, c.ID)))
	result, err := s.ctfOutcomeTool(c).Call(ctx, raw, nil)
	if err != nil || result.IsError {
		t.Fatal(err, result.Flatten())
	}
	s.closeCTFWorkflow(first)
	if s.readCTFOutcome(c.ID).State != "verified" {
		t.Fatal("saved report missing")
	}
	_, next := s.beginCTFWorkflow(t.Context(), c)
	defer s.closeCTFWorkflow(next)
	if _, ok := next.verifiedCompletion(c.ID); ok {
		t.Fatal("fresh workflow reused the previous turn's success")
	}
	if err := s.startCTFExecution(c.ID, c.ID); err != nil {
		t.Fatal(err)
	}
	if s.readCTFOutcome(c.ID).State != "unverified" {
		t.Fatal("new execution retained the previous report")
	}
}

func TestCTFParallelDelegationAvailableAtEntry(t *testing.T) {
	s, c := ctfRegressionServer(t)
	ctx, run := s.beginCTFWorkflow(t.Context(), c)
	defer s.closeCTFWorkflow(run)
	for _, key := range []string{"solve", "triage", "command", "solver", "writeup"} {
		found := false
		for _, tool := range s.conversationTools(c)(ctx, key) {
			if tool.Name() == "delegate_agents" {
				found = true
			}
		}
		if found != (key != "writeup") {
			t.Fatalf("parallel delegation exposure for %s: %v", key, found)
		}
	}
}
