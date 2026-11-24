package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

type ctfEvidenceRef struct {
	ConversationID int64  `json:"conversation_id"`
	ID             string `json:"id"`
}

const ctfTreeQuery = `WITH RECURSIVE tree(id) AS (SELECT $1 UNION SELECT d.child_id FROM conversation_delegations d JOIN tree ON d.parent_id=tree.id)`

type ctfOutcome struct {
	State              string           `json:"state"`
	Summary            string           `json:"summary,omitempty"`
	Criterion          string           `json:"criterion,omitempty"`
	Verification       string           `json:"verification,omitempty"`
	Evidence           []ctfEvidenceRef `json:"evidence,omitempty"`
	Artifacts          []string         `json:"artifacts,omitempty"`
	VerificationLevel  string           `json:"verification_level,omitempty"`
	ExperimentID       string           `json:"experiment_id,omitempty"`
	BaselineEvidence   *ctfEvidenceRef  `json:"baseline_evidence,omitempty"`
	ResultEvidence     *ctfEvidenceRef  `json:"result_evidence,omitempty"`
	AcceptanceEvidence *ctfEvidenceRef  `json:"acceptance_evidence,omitempty"`
}

func (s *Server) readCTFOutcome(id int64) ctfOutcome {
	o := ctfOutcome{State: "unverified"}
	var raw string
	if err := s.m.pg.QueryRow(`SELECT report_json FROM conversation_outcomes WHERE conversation_id=$1`, id).Scan(&raw); err == nil {
		_ = json.Unmarshal([]byte(raw), &o)
	}
	return o
}

func (s *Server) ctfTreeIDs(id int64) ([]int64, error) {
	rows, err := s.m.pg.Query(ctfTreeQuery+` SELECT id FROM tree`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var v int64
		if err = rows.Scan(&v); err != nil {
			return nil, err
		}
		ids = append(ids, v)
	}
	return ids, rows.Err()
}

func (s *Server) ctfOutcomeTool(c *db.Conversation) actool.CoreTool {
	ref := objSchema(map[string]any{"conversation_id": map[string]any{"type": "integer", "description": "증거가 속한 대화 번호. 자신의 증거는 생략 가능"}, "id": strParam("실제 도구 증거 t... 번호")}, "id")
	return wrTool("report_outcome", "이번 목표 상태를 기록하며, 실행이 끝났는지와는 독립적이다. verified 는 Agent 가 criterion 에 따라 실제로 검증했다는 뜻이며, verification 을 반드시 채우고 다시 확인할 수 있는 성공 도구 증거를 인용해야 한다. 파일이 존재하는 것과 후보 flag 모두 성공을 증명하기에는 부족하다.", objSchema(map[string]any{
		"state":   map[string]any{"type": "string", "enum": []string{"unverified", "partial", "blocked", "verified"}},
		"summary": strParam("결론 또는 실제 차단 지점"), "criterion": strParam("이 하위 작업에서 검증 가능한 검수 조건"), "verification": strParam("실제 검증 방식과 관측 결과. 추론을 검증으로 쓰면 안 됨"),
		"evidence":           map[string]any{"type": "array", "maxItems": 16, "items": ref},
		"artifacts":          map[string]any{"type": "array", "maxItems": 16, "items": map[string]any{"type": "string"}, "description": "자신의 작업 디렉터리에 있는 실제 산출물의 상대 경로"},
		"verification_level": map[string]any{"type": "string", "enum": []string{"inference", "observed", "reproduced", "accepted"}, "description": "추론/실제 관찰/격리 실험 독립 재현/플랫폼 수락. 이것은 Agent 가 보고하는 증거 등급이지 자동 의미 판정이 아니다."},
		"experiment_id":      strParam("독립 재현에는 research_experiment 가 할당한 실제 번호를 써야 하며, 초기 상태와 고유 산출물을 설명해야 한다"),
		"baseline_evidence":  ref, "result_evidence": ref, "acceptance_evidence": ref,
	}, "state", "summary", "criterion"), func(ctx context.Context, raw json.RawMessage) (actool.Result, error) {
		if ctx.Err() != nil {
			return actool.Errorf("run cancelled"), nil
		}
		var outcome ctfOutcome
		if len(raw) > 16384 || json.Unmarshal(raw, &outcome) != nil {
			return actool.Errorf("invalid outcome"), nil
		}
		if err := s.validateCTFOutcome(c.ID, &outcome); err != nil {
			return actool.Errorf(err.Error()), nil
		}
		body, _ := json.Marshal(outcome)
		if _, err := s.m.pg.Exec(`INSERT INTO conversation_outcomes(conversation_id,report_json) VALUES($1,$2) ON CONFLICT(conversation_id) DO UPDATE SET report_json=excluded.report_json`, c.ID, string(body)); err != nil {
			return actool.Errorf("목표 상태를 저장할 수 없습니다"), nil
		}
		return jsonResult(outcome)
	})
}

func (s *Server) validateCTFOutcome(id int64, o *ctfOutcome) error {
	if o.VerificationLevel == "" {
		o.VerificationLevel = "inference"
		if o.State == "verified" {
			o.VerificationLevel = "observed"
		}
	}
	switch o.VerificationLevel {
	case "inference", "observed", "reproduced", "accepted":
	default:
		return fmt.Errorf("invalid verification_level")
	}
	if o.State == "verified" && o.VerificationLevel == "inference" {
		return fmt.Errorf("추론만으로는 verified 로 표시할 수 없습니다")
	}
	if o.VerificationLevel != "inference" && (len(o.Evidence) == 0 || strings.TrimSpace(o.Verification) == "") {
		return fmt.Errorf("observed 이상 등급에는 검증 설명과 도구 증거가 필요합니다")
	}
	switch o.State {
	case "unverified", "partial", "blocked", "verified":
	default:
		return fmt.Errorf("invalid outcome state")
	}
	if strings.TrimSpace(o.Summary) == "" || strings.TrimSpace(o.Criterion) == "" || len(o.Summary) > 4000 || len(o.Criterion) > 2000 || len(o.Verification) > 4000 || len(o.Evidence) > 16 || len(o.Artifacts) > 16 {
		return fmt.Errorf("짧은 결론과 검수 조건을 채우세요. 증거와 산출물은 각각 최대 16 개입니다")
	}
	if o.State == "verified" && (strings.TrimSpace(o.Verification) == "" || len(o.Evidence) == 0) {
		return fmt.Errorf("verified 에는 실제 검증 설명과 성공 도구 증거가 필요합니다. 증거가 부족하면 partial/unverified 를 쓰세요")
	}
	ids, err := s.ctfTreeIDs(id)
	if err != nil {
		return err
	}
	allowed := map[int64]bool{}
	for _, v := range ids {
		allowed[v] = true
	}
	success := false
	for i, ref := range o.Evidence {
		if ref.ConversationID == 0 {
			ref.ConversationID = id
			o.Evidence[i] = ref
		}
		if !allowed[ref.ConversationID] {
			return fmt.Errorf("증거 대화 #%d 가 현재 작업에 속하지 않습니다", ref.ConversationID)
		}
		memory, err := agent.NewResearchMemory(s.conversationWorkDir(ref.ConversationID))
		if err != nil {
			return fmt.Errorf("증거 디렉터리를 읽을 수 없습니다")
		}
		state, err := memory.Snapshot()
		if err != nil {
			return err
		}
		found := false
		for _, obs := range state.Observations {
			if obs.ID != ref.ID {
				continue
			}
			root, err := os.OpenRoot(memory.EvidenceRoot())
			if err != nil {
				return err
			}
			f, err := root.Open(obs.Path)
			root.Close()
			if err != nil {
				return fmt.Errorf("증거 %s 를 더 이상 읽을 수 없습니다", ref.ID)
			}
			h := sha256.New()
			n, copyErr := io.Copy(h, io.LimitReader(f, 64<<10))
			f.Close()
			if copyErr != nil || n != obs.Bytes || hex.EncodeToString(h.Sum(nil)) != obs.SHA256 {
				return fmt.Errorf("증거 %s 의 내용이 변경되었습니다", ref.ID)
			}
			found = true
			// Reading a prior claim or a delegation receipt does not independently
			// establish success. The verification must cite an actual observation.
			if !obs.IsError && !strings.HasPrefix(obs.Tool, "research_") && obs.Tool != "report_outcome" && !strings.HasPrefix(obs.Tool, "delegate_") && obs.Tool != "wait_agents" && obs.Tool != "cancel_agent" {
				success = true
			}
			break
		}
		if !found {
			return fmt.Errorf("증거 %s 가 없습니다. research_recall 로 실제 번호를 조회하세요", ref.ID)
		}
	}
	if o.State == "verified" && !success {
		return fmt.Errorf("verified 에는 성공한 실제 도구 관찰이 필요하며, 분배나 보고만 인용할 수 없습니다")
	}
	if o.VerificationLevel == "reproduced" || o.VerificationLevel == "accepted" {
		if err := s.validateVerificationLevel(id, o); err != nil {
			return err
		}
	}
	for _, path := range o.Artifacts {
		if !filepath.IsLocal(path) || strings.Contains(path, "\\") {
			return fmt.Errorf("산출물은 작업 디렉터리 안의 상대 경로를 써야 합니다")
		}
		root, err := os.OpenRoot(s.conversationWorkDir(id))
		if err != nil {
			return err
		}
		info, err := root.Stat(path)
		root.Close()
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("산출물 %s 가 없거나 일반 파일이 아닙니다", path)
		}
	}
	o.Summary = agent.RedactResearchText(o.Summary)
	o.Criterion = agent.RedactResearchText(o.Criterion)
	o.Verification = agent.RedactResearchText(o.Verification)
	return nil
}

// Validate evidence identity, chronology and experiment isolation. The report
// remains the agent's claim: byte integrity does not establish its semantics.
func (s *Server) validateVerificationLevel(id int64, o *ctfOutcome) error {
	refs := []*ctfEvidenceRef{o.AcceptanceEvidence}
	if o.VerificationLevel == "reproduced" {
		refs = []*ctfEvidenceRef{o.BaselineEvidence, o.ResultEvidence}
	}
	for _, ref := range refs {
		if ref == nil || ref.ID == "" {
			return fmt.Errorf("독립 재현에는 baseline_evidence/result_evidence 가, 플랫폼 수락에는 acceptance_evidence 가 필요합니다")
		}
		if ref.ConversationID == 0 {
			ref.ConversationID = id
		}
		found := false
		for _, e := range o.Evidence {
			cid := e.ConversationID
			if cid == 0 {
				cid = id
			}
			if cid == ref.ConversationID && e.ID == ref.ID {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("등급 검증 증거는 evidence 에도 반드시 함께 넣어야 합니다")
		}
		memory, err := agent.NewResearchMemory(s.conversationWorkDir(ref.ConversationID))
		if err != nil {
			return err
		}
		state, err := memory.Snapshot()
		if err != nil {
			return err
		}
		success := false
		for _, obs := range state.Observations {
			if obs.ID == ref.ID && !obs.IsError && !strings.HasPrefix(obs.Tool, "research_") && obs.Tool != "report_outcome" && !strings.HasPrefix(obs.Tool, "delegate_") && obs.Tool != "wait_agents" && obs.Tool != "cancel_agent" {
				success = true
			}
		}
		if !success {
			return fmt.Errorf("등급 검증은 성공한 실제 도구 관찰을 인용해야 합니다")
		}
	}
	if o.VerificationLevel == "reproduced" {
		if o.BaselineEvidence.ConversationID != id || o.ResultEvidence.ConversationID != id || o.BaselineEvidence.ID == o.ResultEvidence.ID {
			return fmt.Errorf("독립 재현에는 이 대화에서 서로 다른 기준선 관찰과 결과 관찰 두 번이 필요합니다")
		}
		memory, err := agent.NewResearchMemory(s.conversationWorkDir(id))
		if err != nil {
			return err
		}
		state, err := memory.Snapshot()
		if err != nil {
			return err
		}
		baseline, result := -1, -1
		for i, obs := range state.Observations {
			if obs.ID == o.BaselineEvidence.ID {
				baseline = i
			}
			if obs.ID == o.ResultEvidence.ID {
				result = i
			}
		}
		if baseline < 0 || result <= baseline {
			return fmt.Errorf("기준선 관찰은 결과 검증보다 먼저여야 합니다")
		}
		found := false
		for _, entry := range state.Entries {
			if entry.Scope == o.ExperimentID && strings.HasPrefix(o.ExperimentID, "exp-") {
				found = true
			}
		}
		if !found || strings.ContainsAny(o.ExperimentID, "/\\.") {
			return fmt.Errorf("독립 재현에는 research_experiment 로 만든 실험 번호가 필요합니다")
		}
		root, err := os.OpenRoot(s.conversationWorkDir(id))
		if err != nil {
			return err
		}
		defer root.Close()
		info, err := root.Stat(filepath.Join(".research", "experiments", o.ExperimentID))
		if err != nil || !info.IsDir() {
			return fmt.Errorf("격리 실험 디렉터리를 사용할 수 없습니다")
		}
	}
	return nil
}
