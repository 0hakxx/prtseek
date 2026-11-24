package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Autumn-27/artex/intercept"
	actool "github.com/Autumn-27/norma/tool"
)

type findingReviewDecision struct {
	FindingID         int64  `json:"finding_id"`
	Verdict           string `json:"verdict"`
	Reason            string `json:"reason"`
	RootCauseEvidence string `json:"root_cause_evidence"`
}
type findingReviewConclusion struct {
	Summary   string                  `json:"summary"`
	Decisions []findingReviewDecision `json:"decisions"`
}

func (s *Server) isHistoricalFindingReview(conv int64) (bool, error) {
	var exists bool
	err := s.m.pg.QueryRow(`SELECT EXISTS(SELECT 1 FROM finding_case_review_runs WHERE conversation_id=$1)`, conv).Scan(&exists)
	return exists, err
}

func (s *Server) toolCompleteFindingCaseReview() actool.CoreTool {
	decision := objSchema(map[string]any{
		"finding_id":          idParam("선택한 독립 취약점 기록 ID"),
		"verdict":             map[string]any{"type": "string", "enum": []string{"independent", "merged", "suggested"}},
		"reason":              strParam("판단 이유. 증거화된 공통점이나 서로 다른 코드 경로를 인용할 것. 유형, 제목, 자산만 같다고 공통 근본 원인을 증명할 수 없음"),
		"root_cause_evidence": strParam("원래 보고/증거에서 실제 공통 또는 서로 다른 근본 원인을 뒷받침하는 구체적 자료. suggested 는 구체적인 공통점, 부족한 핵심 증거, 저장된 그룹을 설명해야 함. 구체적인 공통점이 없으면 independent 로 판정할 것")}, "finding_id", "verdict", "reason", "root_cause_evidence")
	return wrTool("complete_finding_case_review", "과거 정리를 끝내기 전에 선택한 기록마다 근본 원인 판단을 제출하고 검수한다: 모든 기록을 반드시 읽었어야 하고, 의심에는 확인 대기 그룹 제안이 있어야 한다. 병합은 활성 폴더에 있기만 하면 되고 통합 보고서는 나중에 보충할 수 있다. 검수에 실패하면 완료를 선언할 수 없다.", objSchema(map[string]any{"summary": strParam("정리 결과 요약"), "decisions": map[string]any{"type": "array", "items": decision}}, "summary", "decisions"), func(ctx context.Context, raw json.RawMessage) (actool.Result, error) {
		var a findingReviewConclusion
		if err := json.Unmarshal(raw, &a); err != nil {
			return actool.Errorf(err.Error()), nil
		}
		conv := intercept.ConvIDFromContext(ctx)
		if err := s.saveFindingReviewConclusion(ctx, conv, a); err != nil {
			return actool.Errorf(err.Error()), nil
		}
		return actool.Text("과거 정리 검수를 통과했습니다. 원래 기록과 보고는 보존했고, 기록별 근본 원인 판단을 저장했습니다."), nil
	})
}

func (s *Server) saveFindingReviewConclusion(ctx context.Context, conv int64, a findingReviewConclusion) error {
	if conv <= 0 {
		return errors.New("컨텍스트가 있는 과거 정리 대화에만 쓸 수 있습니다")
	}
	return s.m.pg.WithEvidenceTx(ctx, func(tx *sql.Tx) error {
		var idsRaw, readsRaw json.RawMessage
		var state string
		if err := tx.QueryRow(`SELECT finding_ids,reviewed_ids,state FROM finding_case_review_runs WHERE conversation_id=$1`, conv).Scan(&idsRaw, &readsRaw, &state); err != nil {
			return err
		}
		if state != "running" {
			return errors.New("정리 대화가 실행 중이 아니어서 검수를 제출할 수 없습니다")
		}
		var ids, reads []int64
		if err := json.Unmarshal(idsRaw, &ids); err != nil {
			return err
		}
		if err := json.Unmarshal(readsRaw, &reads); err != nil {
			return err
		}
		read := map[int64]bool{}
		for _, id := range reads {
			read[id] = true
		}
		decisions := map[int64]findingReviewDecision{}
		for _, d := range a.Decisions {
			if _, ok := decisions[d.FindingID]; ok {
				return errors.New("중복된 판단 번호")
			}
			decisions[d.FindingID] = d
		}
		if strings.TrimSpace(a.Summary) == "" || len(ids) != len(decisions) {
			return errors.New("요약과, 선택한 모든 기록을 포괄하는 기록별 판단을 제공해야 합니다")
		}
		for _, id := range ids {
			d, ok := decisions[id]
			if !ok || !read[id] {
				return fmt.Errorf("먼저 get_finding_record 로 선택한 기록 #%d 를 읽고 판단하세요", id)
			}
			if strings.TrimSpace(d.Reason) == "" || strings.TrimSpace(d.RootCauseEvidence) == "" {
				return fmt.Errorf("기록 #%d 에 판단 이유나 근본 원인 증거가 없습니다", id)
			}
			var cid sql.NullInt64
			if err := tx.QueryRow(`SELECT m.case_id FROM findings f LEFT JOIN finding_case_members m ON m.finding_id=f.id WHERE f.id=$1`, id).Scan(&cid); err != nil {
				return err
			}
			switch d.Verdict {
			case "merged":
				if !cid.Valid {
					return fmt.Errorf("기록 #%d 가 아직 병합되지 않았습니다", id)
				}
			case "suggested":
				var pending bool
				if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM finding_case_suggestions s
                    JOIN finding_case_suggestion_members sm ON sm.suggestion_id=s.id
                    WHERE s.state='pending' AND sm.finding_id=$1)`, id).Scan(&pending); err != nil {
					return err
				}
				if !pending {
					return fmt.Errorf("기록 #%d 는 먼저 확인 대기 제안을 저장해야 합니다", id)
				}
			case "independent":
				if cid.Valid {
					return fmt.Errorf("기록 #%d 가 이미 폴더에 있어 독립으로 선언할 수 없습니다", id)
				}
			default:
				return errors.New("유효하지 않은 판단 유형")
			}
			if cid.Valid {
				var active bool
				if err := tx.QueryRow(`SELECT active FROM finding_cases WHERE id=$1`, cid.Int64).Scan(&active); err != nil {
					return err
				}
				if !active {
					return fmt.Errorf("폴더 #%d 가 이미 닫혔습니다", cid.Int64)
				}
			}
		}
		raw, err := json.Marshal(a)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE finding_case_review_runs SET conclusion=$2 WHERE conversation_id=$1`, conv, string(raw))
		return err
	})
}

func (s *Server) validateFindingReviewCompletion(conv int64) error {
	var raw json.RawMessage
	if err := s.m.pg.QueryRow(`SELECT conclusion FROM finding_case_review_runs WHERE conversation_id=$1`, conv).Scan(&raw); err != nil {
		return err
	}
	var a findingReviewConclusion
	if err := json.Unmarshal(raw, &a); err != nil {
		return err
	}
	if len(a.Decisions) == 0 {
		return errors.New("정리가 검수를 통과하지 못했습니다: complete_finding_case_review 기록별 근본 원인 판단을 제출하지 않았습니다. 실행 기록을 확인하세요")
	}
	// Recheck after the last model turn; concurrent evidence changes invalidate completion.
	return s.saveFindingReviewConclusion(context.Background(), conv, a)
}

// Require substantive sections and balanced fenced blocks. This validates report
// completeness, not the truth of a model's root-cause claim or a live exploit.
func validateUnifiedFindingReport(report string) error {
	aliases := [][]string{{"개요", "overview", "summary"}, {"영향", "impact"}, {"전제", "prerequisite", "precondition"}, {"재현", "reproduction", "reproduce"}, {"증거", "evidence"}, {"poc", "완전한 스크립트", "검증 스크립트", "proof of concept"}, {"근본 원인", "root cause"}, {"수정", "remediation", "recommendation", "fix"}}
	names := []string{"개요", "영향", "전제 조건", "재현 절차", "증거", "PoC", "근본 원인 분석", "수정 권고"}
	found := make([]bool, len(aliases))
	current := -1
	hasContent := false
	fence := ""
	finish := func() {
		if current >= 0 && hasContent {
			found[current] = true
		}
	}
	for _, line := range strings.Split(report, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			marker := t[:3]
			if fence == "" {
				fence = marker
			} else if fence == marker {
				fence = ""
			}
			continue
		}
		if fence != "" {
			if t != "" {
				hasContent = true
			}
			continue
		}
		if strings.HasPrefix(t, "#") {
			heading := strings.TrimLeft(t, "#")
			if len(heading) == len(t) || !strings.HasPrefix(heading, " ") {
				continue
			}
			// Subheadings belong to the current section unless they start another required section.
			next := -1
			lower := strings.ToLower(heading)
			for i, words := range aliases {
				for _, word := range words {
					if strings.Contains(lower, word) {
						next = i
						break
					}
				}
				if next >= 0 {
					break
				}
			}
			if next >= 0 {
				finish()
				current = next
				hasContent = false
			}
			continue
		}
		if t != "" && t != "---" && t != "***" && t != ">" {
			hasContent = true
		}
	}
	finish()
	if fence != "" {
		return errors.New("통합 보고서의 코드 블록이 닫히지 않아 잘렸을 수 있습니다. 보완한 뒤 다시 저장하세요")
	}
	missing := []string{}
	for i, ok := range found {
		if !ok {
			missing = append(missing, names[i])
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("통합 보고서에 내용이 있는 장이 없습니다: %s. 보완한 뒤 저장하세요", strings.Join(missing, ", "))
	}
	return nil
}

func (s *Server) historicalFindingReviewSelection(ctx context.Context) ([]int64, error) {
	conv := intercept.ConvIDFromContext(ctx)
	if conv == 0 {
		return nil, nil
	}
	var raw json.RawMessage
	err := s.m.pg.QueryRow(`SELECT finding_ids FROM finding_case_review_runs WHERE conversation_id=$1`, conv).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	ids := []int64{}
	err = json.Unmarshal(raw, &ids)
	return ids, err
}
