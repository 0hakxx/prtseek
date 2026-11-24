package server

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/agent"
)

var defaultCTFAgents = map[string]bool{
	"triage":  true,
	"command": true,
	"solver":  true,
	"writeup": true,
}

func isDefaultCTFAgent(key string) bool { return defaultCTFAgents[key] }

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
		{"triage", "CTF Triage", "CTF 진입점과 자료 점검: 문제 유형, 첨부 파일, 권한 범위, flag 단서를 식별한다.", agent.CTFTriagePrompt},
		{"command", "CTF Command", "CTF 오케스트레이션과 작업 분해: 목표를 세우고 Solver 를 병렬로 분배하며 증거를 수렴한다.", agent.CTFPlannerPrompt},
		{"solver", "CTF Solver", "CTF 풀이 실행: 분배받은 자료를 분석하고 체인을 검증하며 재현 가능한 증거를 돌려준다.", agent.CTFWorkerPrompt},
		{"writeup", "CTF WriteUP", "CTF 결과물 정리: 실제 증거를 바탕으로 WRITEUP, EXP, 재현 절차를 정리한다.", agent.CTFWriteupPrompt},
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
		} else if a.Description == "" || a.Description == "CTF 模式专用角色，可编辑提示词与模型配置" || a.Description == "默认 CTF 编组角色；可配置提示词、模型与可见资源。" || a.Description == "CTF 모드 전용 역할이며 프롬프트와 모델 설정을 편집할 수 있음" || a.Description == "기본 CTF 편성 역할. 프롬프트, 모델, 보이는 리소스를 설정할 수 있음." {
			if err = pg.UpdateAgentMeta(a.Key, a.Name, def.description); err != nil {
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
			if _, err = pg.SavePrompt(a.ID, def.prompt, "CTF workflow v4", "system"); err != nil {
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
		return "triage", nil
	}
	key = strings.TrimSpace(key)
	if !isDefaultCTFAgent(key) {
		return "", fmt.Errorf("기본 CTF Agent 만 지원합니다")
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
