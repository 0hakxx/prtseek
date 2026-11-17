package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestCTFOnlyRootMayDelegate(t *testing.T) {
	s, c := ctfRegressionServer(t)
	ctx, run := s.beginCTFWorkflow(t.Context(), c)
	defer s.closeCTFWorkflow(run)
	child, err := s.m.pg.CreateConversation("solve", "synthetic subgoal", nil)
	if err != nil {
		t.Fatal(err)
	}
	childCtx := context.WithValue(ctx, ctfDepthKey{}, 1)
	for _, tool := range s.conversationTools(child)(childCtx, "solve") {
		if tool.Name() == "delegate_agent" || tool.Name() == "delegate_agents" {
			t.Fatal("child exposed delegation", tool.Name())
		}
	}
	for _, requestCtx := range []context.Context{ctx, childCtx} {
		if _, err := s.startCTFChild(requestCtx, child, "solve", "a forbidden nested subgoal"); err == nil {
			t.Fatal("server accepted nested delegation")
		}
	}
	if len(run.jobs) != 0 {
		t.Fatal("rejected nested work created jobs")
	}
}

func TestCTFSolveMigrationPreservesEntrySettings(t *testing.T) {
	s, _ := ctfRegressionServer(t)
	if _, err := s.m.pg.Exec(`DELETE FROM agents WHERE key='solve'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.m.pg.Exec(`UPDATE agents SET run_seconds=7777,max_turns=55,web_search=true,interactive_shell=true WHERE key='triage'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.m.pg.Exec(`INSERT INTO agent_skill_visibility(agent_id,skill_name,enabled) SELECT id,'local-fixture-skill',true FROM agents WHERE key='triage'`); err != nil {
		t.Fatal(err)
	}
	if err := s.initAgentWorkflows(); err != nil {
		t.Fatal(err)
	}
	a, err := s.m.pg.GetAgentByKey("solve")
	if err != nil || a.RunSecs != 7777 || a.MaxTurns != 55 || !a.WebSearch || !a.InteractiveShell || a.Name != "解题" {
		t.Fatal("entry settings lost", a, err)
	}
	var bound bool
	if err := s.m.pg.QueryRow(`SELECT EXISTS(SELECT 1 FROM agent_skill_visibility WHERE agent_id=$1 AND skill_name='local-fixture-skill' AND enabled)`, a.ID).Scan(&bound); err != nil || !bound {
		t.Fatal("skill binding lost", err)
	}
	if _, err := s.m.pg.Exec(`UPDATE agents SET run_seconds=8888 WHERE key='solve'`); err != nil {
		t.Fatal(err)
	}
	if err := s.initAgentWorkflows(); err != nil {
		t.Fatal(err)
	}
	a, _ = s.m.pg.GetAgentByKey("solve")
	if a.RunSecs != 8888 {
		t.Fatal("restart overwrote new solve settings")
	}
}

func TestCTFWriteupHasOnlySourceReadsAndOwnWorkspaceTools(t *testing.T) {
	s, c := ctfRegressionServer(t)
	c.AgentKey = "writeup"
	if _, err := s.m.pg.Exec(`UPDATE conversations SET agent_key='writeup' WHERE id=$1`, c.ID); err != nil {
		t.Fatal(err)
	}
	seen := make(chan map[string]bool, 1)
	useCTFFixtureModel(t, s, c, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Tools []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		names := map[string]bool{}
		for _, tool := range body.Tools {
			names[tool.Function.Name] = true
		}
		select {
		case seen <- names:
		default:
		}
		fixtureReply(w, "The supplied synthetic notes are ready to summarize.")
	})
	response := retestRequest(s.pgSendConversationMessage, http.MethodPost, c.ID, `{"message":"Summarize the supplied synthetic notes without executing anything."}`)
	if response.Code != 202 {
		t.Fatal(response.Code, response.Body)
	}
	waitCTFIdle(t, s, c.ID)
	var names map[string]bool
	select {
	case names = <-seen:
	default:
		t.Fatal("writeup model was not called")
	}
	for _, name := range []string{"Read", "Write", "writeup_list_conversations", "writeup_read_conversation", "writeup_read_activity", "writeup_list_artifacts", "writeup_read_artifact"} {
		if !names[name] {
			t.Fatal("missing writeup tool", name, names)
		}
	}
	for _, name := range []string{"Bash", "Edit", "WebFetch", "WebSearch", "Skill", "delegate_agent", "delegate_agents", "research_record", "research_status", "report_outcome", "SearchExtraTools", "ExecuteExtraTool"} {
		if names[name] {
			t.Fatal("writeup exposed an unrestricted or solve tool", name)
		}
	}
}
