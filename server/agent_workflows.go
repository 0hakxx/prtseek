package server

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/agent"
)

var defaultCTFAgents = map[string]bool{
	"solve":   true,
	"triage":  true,
	"command": true,
	"solver":  true,
	"writeup": true,
}

func isDefaultCTFAgent(key string) bool { return defaultCTFAgents[key] }
func isVisibleCTFAgent(key string) bool { return key == "solve" || key == "writeup" }

func (s *Server) initAgentWorkflows() error {
	pg := s.m.pg
	if pg == nil {
		return nil
	}
	if err := pg.EnsureAgentArchives(); err != nil {
		return err
	}
	if _, err := pg.Exec(`CREATE TABLE IF NOT EXISTS conversation_delegations (
	 parent_id BIGINT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
	 child_id BIGINT PRIMARY KEY REFERENCES conversations(id) ON DELETE CASCADE,
	 state TEXT NOT NULL DEFAULT 'running', created_at DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f','now')))`); err != nil {
		return err
	}
	// Interrupted children remain visible after restart; they are never silently replayed.
	_, _ = pg.Exec(`UPDATE conversation_delegations SET state='interrupted' WHERE state='running'`)
	if err := s.initCTFExecution(); err != nil {
		return err
	}
	// Retire the short-lived prefixed workflow without deleting its historical
	// conversations. Fresh operational roles use concise, user-editable keys.
	for _, legacy := range []string{"ctf_triage", "ctf_planner", "ctf_worker", "ctf_verifier", "ctf_writeup"} {
		if a, _ := pg.GetAgentByKey(legacy); a != nil && !pg.AgentArchived(legacy) {
			_ = pg.ArchiveAgent(legacy)
		}
	}
	for _, def := range []struct{ key, name, description, prompt string }{
		{"solve", "解题", "直接解题；难题按需并行研究，成功立即交付。", agent.CTFSolvePrompt},
		{"triage", "解题", "历史解题会话兼容角色。", agent.CTFSolvePrompt},
		{"command", "解题", "历史解题会话兼容角色。", agent.CTFSolvePrompt},
		{"solver", "解题", "历史解题会话兼容角色。", agent.CTFSolvePrompt},
		{"writeup", "写 WP", "只读引用历史会话与产物，在自己的工作区整理 WP。", agent.CTFWriteupPrompt},
	} {
		a, err := pg.GetAgentByKey(def.key)
		if err != nil {
			return err
		}
		if a == nil {
			a, err = pg.CreateAgent(def.key, def.name, def.description)
			if err != nil {
				return err
			}
			// New research roles default to four hours. Existing operator budgets
			// remain authoritative; zero continues to mean no time limit.
			seconds := 4 * 60 * 60
			if def.key == "writeup" {
				seconds = 30 * 60
			}
			if err = pg.SetAgentRunSeconds(a.Key, seconds); err != nil {
				return err
			}
			if def.key == "solve" {
				// Preserve the old entry's operational settings on the one-time move
				// to the single solve role; leave its history and prompt untouched.
				_, err = pg.Exec(`UPDATE agents SET (llm_profile_id,max_turns,run_seconds,web_search,interactive_shell)=(SELECT llm_profile_id,max_turns,run_seconds,web_search,interactive_shell FROM agents WHERE key='triage') WHERE key='solve' AND EXISTS(SELECT 1 FROM agents WHERE key='triage')`)
				if err != nil {
					return err
				}
				for _, statement := range []string{
					`INSERT INTO agent_visibility(agent_id,resource_kind,resource_id,mcp_tool_name,enabled) SELECT $1,v.resource_kind,v.resource_id,v.mcp_tool_name,v.enabled FROM agent_visibility v JOIN agents a ON a.id=v.agent_id WHERE a.key='triage' ON CONFLICT DO NOTHING`,
					`INSERT INTO agent_skill_visibility(agent_id,skill_name,enabled) SELECT $1,v.skill_name,v.enabled FROM agent_skill_visibility v JOIN agents a ON a.id=v.agent_id WHERE a.key='triage' ON CONFLICT DO NOTHING`,
					`UPDATE tools SET agents=collection_append(agents,'solve') WHERE EXISTS(SELECT 1 FROM json_each(agents) WHERE value='triage') AND NOT EXISTS(SELECT 1 FROM json_each(agents) WHERE value='solve')`,
				} {
					if _, err = pg.Exec(statement, a.ID); err != nil {
						return err
					}
				}
			}
		} else if a.Name != def.name || a.Description != def.description {
			if err = pg.UpdateAgentMeta(a.Key, def.name, def.description); err != nil {
				return err
			}
		}
		if err = pg.SeedPromptIfEmpty(a.ID, def.prompt); err != nil {
			return err
		}
		// Upgrade code-managed CTF prompts while preserving prompts edited in the UI.
		var current, updatedBy string
		if err = pg.QueryRow(`SELECT p.template_text,COALESCE(p.updated_by,'') FROM agents a JOIN agent_prompts p ON p.id=a.current_prompt_id WHERE a.id=$1`, a.ID).Scan(&current, &updatedBy); err != nil {
			return err
		}
		if current != def.prompt && updatedBy == "system" {
			if _, err = pg.SavePrompt(a.ID, def.prompt, "CTF solve/writeup roles v6", "system"); err != nil {
				return err
			}
		}
	}
	return nil
}

// workflowMode keeps the conversation surface CTF-only without imposing one
// rigid agent graph: the operator can choose any enabled CTF-role/custom agent.
func workflowMode(_ string) string { return "ctf" }

func workflowEntry(_, key string) (string, error) {
	if strings.TrimSpace(key) == "" {
		return "solve", nil
	}
	key = strings.TrimSpace(key)
	if agent.IsCTFSolveRole(key) {
		return "solve", nil
	}
	if key != "writeup" {
		return "", fmt.Errorf("请选择解题或写 WP")
	}
	return key, nil
}

func (s *Server) conversationDelegations(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, 400, "invalid conversation")
		return
	}
	rows, err := pg.Query(`WITH RECURSIVE descendants(child_id) AS (
	 SELECT child_id FROM conversation_delegations WHERE parent_id=$1
	 UNION
	 SELECT d.child_id FROM conversation_delegations d JOIN descendants p ON d.parent_id=p.child_id
	) SELECT c.id,c.agent_key,c.title,d.state,d.parent_id FROM descendants tree
	 JOIN conversation_delegations d ON d.child_id=tree.child_id
	 JOIN conversations c ON c.id=d.child_id ORDER BY c.id`, id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	type item struct {
		ID         int64         `json:"id"`
		AgentKey   string        `json:"agent_key"`
		Title      string        `json:"title"`
		State      string        `json:"state"`
		ParentID   int64         `json:"parent_id"`
		Running    bool          `json:"running"`
		Activities []ActivityDTO `json:"activities"`
		Outcome    ctfOutcome    `json:"outcome"`
	}
	items := []item{}
	for rows.Next() {
		var v item
		if err = rows.Scan(&v.ID, &v.AgentKey, &v.Title, &v.State, &v.ParentID); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		items = append(items, v)
	}
	if err = rows.Err(); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// The delegation list is also the live operator feed for a parent session.
	// A bounded tail keeps polling cheap while still exposing the latest worker
	// reasoning, tool calls and result messages without opening child tabs.
	s.chatMu.Lock()
	for i := range items {
		items[i].Running = s.chatBusy[s.convBusyKey(items[i].ID)]
		if items[i].Running {
			items[i].State = "running"
		}
	}
	s.chatMu.Unlock()
	for i := range items {
		items[i].Outcome = s.readCTFOutcome(items[i].ID)
		activity, _, activityErr := pg.ConvActivityPage(items[i].ID, 0, 12)
		if activityErr != nil {
			writeErr(w, 500, activityErr.Error())
			return
		}
		items[i].Activities = make([]ActivityDTO, 0, len(activity))
		for _, record := range activity {
			items[i].Activities = append(items[i].Activities, activityDTO(record))
		}
	}
	writeJSON(w, 200, map[string]any{"items": items})
}
