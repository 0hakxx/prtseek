package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

func TestDelegationHonorsHourBudgetsAndParentDeadline(t *testing.T) {
	s, parent := newPersonalBackgroundTestServer(t)
	if err := s.initAgentWorkflows(); err != nil {
		t.Fatal(err)
	}
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"fixture","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"No flag yet"},"finish_reason":"stop"}]}`))
	}))
	defer endpoint.Close()
	id, err := s.m.pg.SaveProfile(&db.LLMProfile{Name: "hour-budget-fixture", Format: "openai", BaseURL: endpoint.URL + "/v1", APIKey: "fixture-only", Model: "fixture", Streaming: false})
	if err != nil {
		t.Fatal(err)
	}
	parent.AgentKey, parent.LLMProfileID = "command", &id
	a, err := s.m.pg.GetAgentByKey("solve")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.m.pg.SetAgentRunSeconds("solve", a.RunSecs) })
	for _, tc := range []struct {
		name               string
		seconds            int
		parentBudget, want time.Duration
	}{{"two hours", 7200, 3 * time.Hour, 2 * time.Hour}, {"unlimited child", 0, 3 * time.Hour, 3 * time.Hour}, {"parent remaining time", 14400, time.Hour, time.Hour}} {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.m.pg.SetAgentRunSeconds("solve", tc.seconds); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), tc.parentBudget)
			defer cancel()
			start := time.Now()
			res, err := s.delegateConversation(ctx, parent, "solver", "Inspect a local fixture; preserve unresolved status")
			if err != nil || res.IsError {
				t.Fatalf("delegation: %v %s", err, res.Flatten())
			}
			var result struct {
				ConversationID int64  `json:"conversation_id"`
				State          string `json:"state"`
			}
			if json.Unmarshal([]byte(res.Flatten()), &result) != nil || result.State != "stalled" {
				t.Fatal("partial result reported as success", res.Flatten())
			}
			root := filepath.Join(s.m.dir, "sessions", s.convBusyKey(result.ConversationID))
			memory, err := agent.NewResearchMemory(root)
			if err != nil {
				t.Fatal(err)
			}
			run, err := memory.RunState()
			if err != nil || run == nil || run.Deadline == nil {
				t.Fatal("deadline not retained", err)
			}
			if delta := run.Deadline.Sub(start) - tc.want; delta < -5*time.Second || delta > 5*time.Second {
				t.Fatalf("hidden cap or reset: deadline=%s want=%s", run.Deadline.Sub(start), tc.want)
			}
			var state string
			if err := s.m.pg.QueryRow(`SELECT state FROM conversation_delegations WHERE child_id=$1`, result.ConversationID).Scan(&state); err != nil || state != "stalled" {
				t.Fatalf("persisted outcome=%s err=%v", state, err)
			}
		})
	}
}

func TestWorkflowEntryDefaultsToSolveAndNormalizesLegacyRoles(t *testing.T) {
	for _, v := range [][3]string{{"", "", "solve"}, {"ctf", "command", "solve"}, {"ctf", "solver", "solve"}, {"ctf", "triage", "solve"}, {"ctf", "writeup", "writeup"}} {
		got, err := workflowEntry(v[0], v[1])
		if err != nil || got != v[2] {
			t.Fatalf("%v: %s %v", v, got, err)
		}
	}
	if _, err := workflowEntry("ctf", "custom_solver"); err == nil {
		t.Fatal("custom agent should be rejected")
	}
}

func TestCTFDelegationToolsAreExposed(t *testing.T) {
	s := &Server{}
	toolsFor := func(key string) map[string]bool {
		out := map[string]bool{}
		for _, tool := range s.conversationTools(&db.Conversation{AgentKey: key})(context.Background(), key) {
			out[tool.Name()] = true
		}
		return out
	}
	if !toolsFor("triage")["delegate_agent"] || !toolsFor("command")["delegate_agents"] || !toolsFor("solver")["delegate_agent"] {
		t.Fatal("CTF delegation tools missing")
	}
	if len(toolsFor("custom_solver")) != 0 {
		t.Fatal("custom agents should not expose delegation tools")
	}
	if !toolsFor("solve")["delegate_agents"] || !toolsFor("solver")["delegate_agents"] || toolsFor("writeup")["delegate_agents"] || toolsFor("writeup")["delegate_agent"] {
		t.Fatal("solve and legacy solve aliases should delegate; writeup should only read sources")
	}
}

func TestDelegatedConversationPersistsResult(t *testing.T) {
	s, parent := newPersonalBackgroundTestServer(t)
	if err := s.initAgentWorkflows(); err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&wire); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		fixtureResearchBlocked(w)
	}))
	defer endpoint.Close()
	id, err := s.m.pg.SaveProfile(&db.LLMProfile{Name: "delegation fixture", Format: "openai", BaseURL: endpoint.URL + "/v1", APIKey: "test-only", Model: "fixture", Streaming: false})
	if err != nil {
		t.Fatal(err)
	}
	parent.AgentKey = "command"
	parent.LLMProfileID = &id
	_, err = s.delegateConversation(context.Background(), parent, "solve", "Inspect a local fixture; do not use external targets.")
	if err != nil {
		t.Fatal(err)
	}
	var childID int64
	var state string
	if err = s.m.pg.QueryRow(`SELECT child_id,state FROM conversation_delegations WHERE parent_id=$1`, parent.ID).Scan(&childID, &state); err != nil {
		t.Fatal(err)
	}
	defer s.m.pg.DeleteConversation(childID)
	if state != "blocked" {
		t.Fatalf("delegation state %s", state)
	}
	child, err := s.m.pg.GetConversation(childID)
	if err != nil || child.LLMProfileID == nil || *child.LLMProfileID != id {
		t.Fatalf("child profile mismatch: %v %v", child, err)
	}
	if wire["model"] != "fixture" {
		t.Fatalf("model: %v", wire["model"])
	}
	messages, _ := json.Marshal(wire["messages"])
	wantWorkspace := filepath.Join(s.m.dir, "sessions", s.convBusyKey(parent.ID))
	encodedWorkspace, _ := json.Marshal(wantWorkspace)
	if !bytes.Contains(messages, encodedWorkspace[1:len(encodedWorkspace)-1]) {
		t.Fatalf("child did not receive parent evidence path %q", wantWorkspace)
	}
	data, _ := json.Marshal(wire["tools"])
	if bytes.Contains(data, []byte(`"delegate_agent"`)) || bytes.Contains(data, []byte(`"delegate_agents"`)) {
		t.Fatal("solve child should return evidence instead of delegating another layer")
	}
	if !bytes.Contains(data, []byte(`"research_recall"`)) {
		t.Fatal("non-DeepSeek CTF child lost research tools")
	}
	for _, unwanted := range []string{"Sleep", "TodoWrite", "TaskOutput", "TaskStop", "TaskList", "Monitor", "research_guide"} {
		quoted, _ := json.Marshal(unwanted)
		if bytes.Contains(data, quoted) {
			t.Fatalf("CTF child exposed unrelated SDK tool %s", unwanted)
		}
	}
}
func TestConversationDelegationsIncludesNestedWorkers(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	td := t.TempDir()
	s := New(context.Background(), m, td, td, td)
	root, err := m.pg.CreateConversation("triage", "root", nil)
	if err != nil {
		t.Fatal(err)
	}
	planner, err := m.pg.CreateConversation("command", "plan", nil)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := m.pg.CreateConversation("solver", "review", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]int64{{root.ID, planner.ID}, {planner.ID, worker.ID}} {
		if _, err := m.pg.Exec(`INSERT INTO conversation_delegations(parent_id,child_id,state) VALUES($1,$2,'completed')`, pair[0], pair[1]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.pg.AppendConvActivity(worker.ID, db.Activity{Worker: "solver", Kind: "tool_use", Tool: "Bash", Summary: "strings challenge.bin"}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/", nil)
	req.SetPathValue("id", fmt.Sprint(root.ID))
	rec := httptest.NewRecorder()
	s.conversationDelegations(rec, req)
	var body struct {
		Items []struct {
			ID         int64         `json:"id"`
			ParentID   int64         `json:"parent_id"`
			Activities []ActivityDTO `json:"activities"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if rec.Code != 200 || len(body.Items) != 2 || body.Items[1].ID != worker.ID || body.Items[1].ParentID != planner.ID || len(body.Items[1].Activities) != 1 || body.Items[1].Activities[0].Tool != "Bash" {
		t.Fatalf("unexpected tree: %s", rec.Body.String())
	}
}
