package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/intercept"
)

func TestFindingCaseReviewTaskMessagePolicy(t *testing.T) {
	message := fmt.Sprintf(findingCaseReviewTaskMessage, `[101,102,103]`)
	for _, want := range []string{
		"finding_ids=[101,102,103]",
		"근본 원인이 명확히 같으면 바로 병합",
		"같은 엔드포인트, 핸들러/백엔드 컴포넌트, 파라미터, 누락된 검사",
		"공통 근본 원인을 확정할 핵심 증거가 부족",
		"같은 작업 의심 구성원 2..20 건을 포괄하는 그룹",
		"둘씩 쪼개 제안하지 않는다",
		"유형이나 자산만 같으면 제안하기에 부족",
		"구체적 공통점이 없으면 independent 를 유지",
		"증거가 서로 다른 코드 경로임을 보여 주면 independent 로 판정",
		"통합 보고서를 최선을 다해 갱신",
		"통합 보고서가 잠시 없거나 갱신 대기 중이어도 complete_finding_case_review 를 막지 않으며",
		"선택되지 않은 기록을 병합하지 않고",
		"대상을 탐지하지 않으며",
		"수동 기각을 지킨다",
	} {
		if !strings.Contains(message, want) {
			t.Errorf("task message missing policy: %s", want)
		}
	}
	if strings.Contains(message, "불확실하면 제안을 제출") {
		t.Fatal("task message suggests solely because of uncertainty")
	}
	schema, err := json.Marshal((&Server{}).toolCompleteFindingCaseReview().InputSchema())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(schema), "구체적인 공통점이 없으면 independent") || strings.Contains(string(schema), "불확실하면 부족한 증거와 저장된 제안을 설명") {
		t.Fatal("completion schema must not require suggestions solely because of uncertainty")
	}
}

const completeFindingReviewReport = `# 동일 권한 결함
## 개요
동일 객체 권한 검사 누락.
## 영향
다른 계정의 주문을 읽을 수 있음을 검증함.
## 전제 조건
이미 존재하는 저권한 세션과 주문 번호.
## 재현 절차
자기 주문과 대조한 뒤 다른 계정 주문을 조회하면, 응답이 동일 검사 누락을 증명함.
## 증거
원 보고의 저장 응답과 명령 출력, 재탐지하지 않음.
## PoC
완전한 명령은 원본 증거에 있으며, 이 테스트는 격리된 자료를 사용함.
## 근본 원인 분석
동일 주문 handler 가 주문 소유자를 검증하지 않음.
## 수정 권고
요청자와 주문 소유자를 통일 검증하고, 객체 권한 검사를 회귀한다.
`

func TestUnifiedFindingReportRejectsIncompleteAndTruncated(t *testing.T) {
	if err := validateUnifiedFindingReport(completeFindingReviewReport); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{strings.Split(completeFindingReviewReport, "## 근본 원인 분석")[0], completeFindingReviewReport + "\n```bash\nunclosed", strings.Replace(completeFindingReviewReport, "요청자와 주문 소유자를 통일 검증하고, 객체 권한 검사를 회귀한다.", "", 1)} {
		if err := validateUnifiedFindingReport(s); err == nil {
			t.Fatal("incomplete report accepted")
		}
	}
	// Section labels appearing inside a script are not real report sections.
	if err := validateUnifiedFindingReport("## 개요\ntext\n```bash\n" + completeFindingReviewReport + "\n```"); err == nil {
		t.Fatal("script comments counted as sections")
	}
}

func TestFindingCaseReviewCompletionRequiresReadsNotFreshReport(t *testing.T) {
	s, task, ids := testCaseServer(t)
	runs, err := s.m.pg.CreateFindingCaseReviews(t.Context(), map[int64][]int64{task: ids})
	if err != nil {
		t.Fatal(err)
	}
	conv := runs[0].ConversationID
	defer s.m.pg.Exec(`DELETE FROM conversations WHERE id=$1`, conv)
	if _, err := s.m.pg.Exec(`UPDATE finding_case_review_runs SET state='running' WHERE conversation_id=$1`, conv); err != nil {
		t.Fatal(err)
	}
	ctx := intercept.WithConvID(context.Background(), conv)
	a := findingReviewConclusion{Summary: "객체 권한 검사의 구체적 증거에 근거해 판단"}
	for _, id := range ids {
		a.Decisions = append(a.Decisions, findingReviewDecision{FindingID: id, Verdict: "merged", Reason: "동일 handler", RootCauseEvidence: "각 기록의 baseline/proof 가 동일 객체 소유자 검증 누락을 가리킴"})
	}
	if err := s.saveFindingReviewConclusion(ctx, conv, a); err == nil {
		t.Fatal("unread findings accepted")
	}
	for _, id := range ids {
		if _, err := s.performFindingCaseTool(ctx, "get_finding_record", findingCaseRequest{FindingID: json.RawMessage(fmt.Sprint(id))}); err != nil {
			t.Fatal(err)
		}
	}
	cid, err := s.m.pg.MergeFindingCase(ctx, task, ids, "orders", "same concrete check", "reporter")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.saveFindingReviewConclusion(ctx, conv, a); err != nil {
		t.Fatal(err)
	}
	if err := s.validateFindingReviewCompletion(conv); err != nil {
		t.Fatal(err)
	}
	if err := s.checkCaseReviewScope(ctx, "update_finding_case_report", findingCaseRequest{CaseID: json.RawMessage(fmt.Sprint(cid))}); err == nil {
		t.Fatal("sealed review can still mutate")
	}
	if _, err := s.m.pg.Exec(`UPDATE findings SET evidence='new proof' WHERE id=$1`, ids[0]); err != nil {
		t.Fatal(err)
	}
	if err := s.validateFindingReviewCompletion(conv); err != nil {
		t.Fatalf("stale unified report should not block merged conclusion: %v", err)
	}
}

func TestFindingCaseReviewProtectsOriginalToolWrites(t *testing.T) {
	s, f, _ := trafficEvidenceServer(t)
	original, err := s.m.pg.GetFinding(f.FindingID)
	if err != nil {
		t.Fatal(err)
	}
	runs, err := s.m.pg.CreateFindingCaseReviews(t.Context(), map[int64][]int64{*original.TaskID: {original.ID}})
	if err != nil {
		t.Fatal(err)
	}
	conv := runs[0].ConversationID
	defer s.m.pg.Exec(`DELETE FROM conversations WHERE id=$1`, conv)
	ctx := intercept.WithConvID(context.Background(), conv)
	workflowCall(t, ctx, s.toolUpdateFindingReport(), map[string]any{"finding_id": f.NodeID, "report": "OVERWRITE", "evidence_version": original.EvidenceVersion}, true)
	if err := s.agentFindingTrafficAccess(ctx, f.FindingID, true); err == nil {
		t.Fatal("historical review can change evidence")
	}
	after, _ := s.m.pg.GetFinding(original.ID)
	if after.Report != original.Report || after.EvidenceVersion != original.EvidenceVersion || after.Status != original.Status || after.Severity != original.Severity {
		t.Fatal("original changed")
	}
	// Normal reporting is still able to write its own standalone report.
	workflowCall(t, context.Background(), s.toolUpdateFindingReport(), map[string]any{"finding_id": f.NodeID, "report": "normal report", "evidence_version": original.EvidenceVersion}, false)
}

func TestFindingCaseReviewSuggestionNeedsSavedSuggestion(t *testing.T) {
	s, task, ids := testCaseServer(t)
	runs, err := s.m.pg.CreateFindingCaseReviews(t.Context(), map[int64][]int64{task: ids[:2]})
	if err != nil {
		t.Fatal(err)
	}
	conv := runs[0].ConversationID
	defer s.m.pg.Exec(`DELETE FROM conversations WHERE id=$1`, conv)
	s.m.pg.Exec(`UPDATE finding_case_review_runs SET state='running' WHERE conversation_id=$1`, conv)
	ctx := intercept.WithConvID(context.Background(), conv)
	a := findingReviewConclusion{Summary: "공통 컴포넌트를 사람이 확인해야 함"}
	for _, id := range ids[:2] {
		s.performFindingCaseTool(ctx, "get_finding_record", findingCaseRequest{FindingID: json.RawMessage(fmt.Sprint(id))})
		a.Decisions = append(a.Decisions, findingReviewDecision{FindingID: id, Verdict: "suggested", Reason: "동일 경계이지만 근본 원인 미확인", RootCauseEvidence: "원 보고가 객체 권한 경계를 가리키지만 공통 컴포넌트 증거가 부족"})
	}
	if err := s.saveFindingReviewConclusion(ctx, conv, a); err == nil {
		t.Fatal("unpersisted suggestion accepted")
	}
	if _, err := s.m.pg.SuggestFindingCase(ctx, task, []int64{ids[0], ids[1]}, "orders", "common component unknown"); err != nil {
		t.Fatal(err)
	}
	if err := s.saveFindingReviewConclusion(ctx, conv, a); err != nil {
		t.Fatal(err)
	}
}

func TestFindingCaseReviewCandidatesStayWithinSelection(t *testing.T) {
	s, task, ids := testCaseServer(t)
	runs, err := s.m.pg.CreateFindingCaseReviews(t.Context(), map[int64][]int64{task: ids[:2]})
	if err != nil {
		t.Fatal(err)
	}
	conv := runs[0].ConversationID
	defer s.m.pg.Exec(`DELETE FROM conversations WHERE id=$1`, conv)
	ctx := intercept.WithConvID(context.Background(), conv)
	a := findingCaseRequest{FindingID: json.RawMessage(fmt.Sprint(ids[0]))}
	v, err := s.performFindingCaseTool(ctx, "search_finding_duplicates", a)
	if err != nil {
		t.Fatal(err)
	}
	if v.(map[string]any)["total"].(int) != 1 {
		t.Fatal("unselected candidates leaked", v)
	}
	v, err = s.performFindingCaseTool(context.Background(), "search_finding_duplicates", a)
	if err != nil || v.(map[string]any)["total"].(int) != 2 {
		t.Fatal("normal candidate search changed", v, err)
	}
}
