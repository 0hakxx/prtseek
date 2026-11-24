package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	actool "github.com/Autumn-27/norma/tool"
)

type findingCaseRequest struct {
	TaskID         json.RawMessage   `json:"task_id"`
	FindingID      json.RawMessage   `json:"finding_id"`
	IDs            []json.RawMessage `json:"finding_ids"`
	CaseID         json.RawMessage   `json:"case_id"`
	Version        int64             `json:"version"`
	Title          string            `json:"title"`
	Reason         string            `json:"reason"`
	Report         string            `json:"report"`
	Severity       string            `json:"severity"`
	SeverityReason string            `json:"severity_reason"`
	Page           int               `json:"page"`
}

func (a findingCaseRequest) ids() []int64 {
	out := []int64{}
	for _, raw := range a.IDs {
		out = append(out, parseProfileID(raw))
	}
	return out
}
func caseHTTPError(w http.ResponseWriter, err error) {
	code := 400
	if errors.Is(err, db.ErrFindingCaseConflict) || errors.Is(err, db.ErrFindingCaseReviewBusy) {
		code = 409
	}
	writeErr(w, code, err.Error())
}
func decodeCaseRequest(w http.ResponseWriter, r *http.Request, a any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(a); err != nil {
		writeErr(w, 400, "유효하지 않은 요청 본문")
		return false
	}
	return true
}
func (s *Server) caseFindingDTO(f *db.DBFinding) FindingDTO {
	return findingFromDB(f, s.resolveAssetIDs(f.AssetIDs))
}
func (s *Server) listFindingCases(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page := findingPaginationParam(q.Get("page"), 1, 0)
	size := findingPaginationParam(q.Get("limit"), 20, 100)
	rows, total, err := s.m.pg.ListFindingCases(findingFilterFromQuery(q), page, size)
	if err != nil {
		caseHTTPError(w, err)
		return
	}
	caseIDs := []int64{}
	for _, row := range rows {
		if row.Case != nil {
			caseIDs = append(caseIDs, row.Case.ID)
		}
	}
	contexts, err := s.m.pg.FindingCaseListContexts(caseIDs)
	if err != nil {
		caseHTTPError(w, err)
		return
	}
	assetIDs := []int64{}
	for _, c := range contexts {
		assetIDs = append(assetIDs, c.AssetIDs...)
	}
	assets := s.resolveAssetIDs(assetIDs)
	items := []map[string]any{}
	for _, row := range rows {
		v := map[string]any{"case": row.Case, "matched_ids": row.MatchedIDs}
		if row.Case != nil {
			c := contexts[row.Case.ID]
			v["task_name"], v["task_description"], v["last_found_at"], v["asset_count"] = c.TaskName, c.TaskDescription, c.LastFoundAt, c.AssetCount
			v["assets"] = append([]FindingAssetDTO{}, findingFromDB(&db.DBFinding{AssetIDs: c.AssetIDs}, assets).Assets...)
		}
		if row.FindingID > 0 {
			f, err := s.m.pg.FindingCaseSummary(row.FindingID, q.Get("original_rows") == "1")
			if err != nil {
				caseHTTPError(w, err)
				return
			}
			if f == nil {
				continue
			}
			f.Report = ""
			v["finding"] = s.caseFindingDTO(f)
		}
		items = append(items, v)
	}
	stats, err := s.m.pg.DistinctFindingStats(q.Get("task_id"))
	if err != nil {
		caseHTTPError(w, err)
		return
	}
	matchingReports, err := s.m.pg.MatchingFindingReportCount(findingFilterFromQuery(q))
	if err != nil {
		caseHTTPError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"items": items, "total": total, "matching_reports": matchingReports, "page": page, "page_size": size, "stats": stats})
}
func (s *Server) findingCaseAccess(w http.ResponseWriter, r *http.Request, mutation bool) (*db.FindingCase, bool) {
	id := int64(atoiDefault(r.PathValue("id"), 0))
	c, err := s.m.pg.GetFindingCase(id)
	if err != nil {
		caseHTTPError(w, err)
		return nil, false
	}
	if c == nil {
		writeErr(w, 404, "취약점 그룹이 존재하지 않습니다")
		return nil, false
	}
	if ctx := r.URL.Query().Get("context_task"); ctx != "" {
		t := s.m.ResolveTask(ctx)
		_, inherited, allowed := findingProvenanceInTask(t, c.TaskID)
		if !allowed {
			writeErr(w, 404, "작업에 해당 취약점 그룹이 없습니다")
			return nil, false
		}
		if mutation && inherited {
			writeErr(w, 403, "상속된 취약점 그룹은 읽기 전용입니다")
			return nil, false
		}
	}
	return c, true
}
func (s *Server) getFindingCase(w http.ResponseWriter, r *http.Request) {
	c, ok := s.findingCaseAccess(w, r, false)
	if !ok {
		return
	}
	events, err := s.m.pg.FindingCaseEvents(c.ID)
	if err != nil {
		caseHTTPError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"case": c, "events": events})
}
func (s *Server) getFindingCaseMembers(w http.ResponseWriter, r *http.Request) {
	c, ok := s.findingCaseAccess(w, r, false)
	if !ok {
		return
	}
	page := findingPaginationParam(r.URL.Query().Get("page"), 1, 0)
	size := findingPaginationParam(r.URL.Query().Get("limit"), 20, 100)
	fs, total, err := s.m.pg.FindingCaseMembers(c.ID, page, size)
	if err != nil {
		caseHTTPError(w, err)
		return
	}
	assets := s.resolveFindingAssets(fs)
	items := []FindingDTO{}
	for _, f := range fs {
		dto := findingFromDB(f, assets)
		if ctx := r.URL.Query().Get("context_task"); ctx != "" {
			dto.SourceTaskID, dto.Inherited, _ = findingProvenanceInTask(s.m.ResolveTask(ctx), f.TaskID)
		}
		items = append(items, dto)
	}
	writeJSON(w, 200, map[string]any{"items": items, "total": total, "page": page, "page_size": size})
}
func (s *Server) removeFindingCaseMember(w http.ResponseWriter, r *http.Request) {
	c, ok := s.findingCaseAccess(w, r, true)
	if !ok {
		return
	}
	fid := int64(atoiDefault(r.PathValue("fid"), 0))
	var body struct {
		Reason string `json:"reason"`
	}
	if !decodeCaseRequest(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Reason) == "" {
		body.Reason = "수동으로 병합 그룹에서 제외"
	}
	if err := s.m.pg.RemoveFindingCaseMember(r.Context(), c.ID, fid, body.Reason); err != nil {
		caseHTTPError(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func (s *Server) caseSuggestions(w http.ResponseWriter, r *http.Request) {
	items, err := s.m.pg.FindingCaseSuggestions(int64(atoiDefault(r.URL.Query().Get("task_id"), 0)))
	if err != nil {
		caseHTTPError(w, err)
		return
	}
	writeJSON(w, 200, items)
}
func (s *Server) resolveCaseSuggestion(w http.ResponseWriter, r *http.Request) {
	var a struct {
		Accept     bool     `json:"accept"`
		ExcludeIDs []string `json:"exclude_ids"`
	}
	if !decodeCaseRequest(w, r, &a) {
		return
	}
	exclude := make([]int64, 0, len(a.ExcludeIDs))
	for _, raw := range a.ExcludeIDs {
		id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil || id <= 0 {
			caseHTTPError(w, errors.New("유효하지 않은 제외 기록 번호"))
			return
		}
		exclude = append(exclude, id)
	}
	id, err := s.m.pg.ResolveFindingCaseSuggestion(r.Context(), int64(atoiDefault(r.PathValue("id"), 0)), a.Accept, exclude)
	if err != nil {
		caseHTTPError(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"case_id": i64s(id)})
}
func (s *Server) mergeFindingCase(w http.ResponseWriter, r *http.Request) {
	var a findingCaseRequest
	if !decodeCaseRequest(w, r, &a) {
		return
	}
	id, err := s.m.pg.MergeFindingCase(r.Context(), parseProfileID(a.TaskID), a.ids(), a.Title, a.Reason, "human")
	if err != nil {
		caseHTTPError(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"case_id": i64s(id)})
}
func (s *Server) updateCaseReport(w http.ResponseWriter, r *http.Request) {
	c, ok := s.findingCaseAccess(w, r, true)
	if !ok {
		return
	}
	var a findingCaseRequest
	if !decodeCaseRequest(w, r, &a) {
		return
	}
	if err := validateUnifiedFindingReport(a.Report); err != nil {
		caseHTTPError(w, err)
		return
	}
	if err := s.m.pg.UpdateFindingCaseReport(r.Context(), c.ID, a.Version, a.Title, a.Report, a.Severity, a.SeverityReason); err != nil {
		caseHTTPError(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) findingCaseTools() []actool.CoreTool {
	props := map[string]any{"finding_id": idParam("독립 findings 기록 ID, 탐색 노드 ID 아님"), "finding_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "독립 취약점 기록 ID 목록"}, "task_id": idParam("같은 출처 작업 ID"), "case_id": idParam("병합 그룹 ID"), "version": idParam("get_finding_case 가 실제로 읽은 version"), "page": idParam("페이지, 1 부터, 페이지당 20 건"), "title": strParam("같은 실제 결함의 이름"), "reason": strParam("구체적인 증거와 병합 이유. 유형만 같다는 것은 안 됨"), "report": strParam("모든 구성원 증거를 담은 완전한 통합 Markdown 보고서"), "severity": strParam("critical|high|medium|low"), "severity_reason": strParam("검증된 영향에 근거한 등급 근거. 최고 등급을 그대로 가져오지 않음")}
	specs := []struct {
		name, desc string
		required   []string
	}{
		{"search_finding_duplicates", "같은 작업의 후보 요약과 자산을 읽기 전용으로 검색하고, 점수순으로 정렬해 score, tier, hit_reasons 를 반환한다: strong=취약점 유형이 같고 자산이 겹침, weak=같은 취약점 유형과 자산 겹침을 동시에 갖추지 않음이니 hit_reasons 와 함께 확인하며 참고용으로만. 등급이 달라도 중복일 수 있다. 페이지를 지원하며 필요에 따라 상세를 읽는다.", []string{"finding_id"}},
		{"get_finding_record", "독립 취약점의 완전한 원본 증거와 보고를 읽고, 실행 기록을 추적할 수 있도록 탐색 노드 번호를 반환한다.", []string{"finding_id"}},
		{"merge_finding_records", "증거가 같은 결함임을 명확히 증명할 때 병합한다. 모든 원본 기록을 보존한다. 작업을 넘나들지 않고, 원래 등급을 바꾸지 않으며, 수동 기각을 우회할 수 없다.", []string{"task_id", "finding_ids", "title", "reason"}},
		{"suggest_finding_merge", "증거에 구체적인 공통점이 있지만 핵심 증거가 부족할 때, 중복 의심 그룹 하나를 제출한다(같은 작업 기록 2..20 건, 한 번에 모든 의심 구성원을 포괄하며 둘씩 쪼개 제안하지 않음). 사람이 확인하기를 기다리며 자동 병합이 아니다. 유형/자산만 같으면 제출하지 않는다.", []string{"task_id", "finding_ids", "title", "reason"}},
		{"get_finding_case", "병합 그룹과 구성원을 페이지 단위로 읽는다. 먼저 version 을 기록한 뒤 구성원의 원래 보고, 증거, 트래픽을 읽는다. 버전이 바뀌면 반드시 다시 읽어야 한다.", []string{"case_id"}},
		{"update_finding_case_report", "통합 보고서와 등급 이유를 저장한다. version 은 반드시 실제로 읽은 것에서 와야 한다. 각 원본 보고를 보존하고 새 취약점을 만들지 않는다.", []string{"case_id", "version", "title", "report", "severity", "severity_reason"}},
	}
	out := []actool.CoreTool{}
	for _, spec := range specs {
		spec := spec
		handler := func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var a findingCaseRequest
			if err := json.Unmarshal(in, &a); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			v, err := s.performFindingCaseTool(ctx, spec.name, a)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			raw, _ := json.Marshal(v)
			return actool.Text(string(raw)), nil
		}
		schemaProps := map[string]any{}
		for _, key := range spec.required {
			schemaProps[key] = props[key]
		}
		if spec.name == "search_finding_duplicates" || spec.name == "get_finding_case" {
			schemaProps["page"] = props["page"]
		}
		schema := objSchema(schemaProps, spec.required...)
		if strings.HasPrefix(spec.name, "get_") || strings.HasPrefix(spec.name, "search_") {
			out = append(out, roTool(spec.name, spec.desc, schema, handler))
		} else {
			out = append(out, wrTool(spec.name, spec.desc, schema, handler))
		}
	}
	out = append(out, s.toolCompleteFindingCaseReview())
	return out
}
func (s *Server) performFindingCaseTool(ctx context.Context, name string, a findingCaseRequest) (any, error) {
	if err := s.checkCaseReviewScope(ctx, name, a); err != nil {
		return nil, err
	}
	fid := parseProfileID(a.FindingID)
	cid := parseProfileID(a.CaseID)
	page := a.Page
	if page < 1 {
		page = 1
	}
	switch name {
	case "search_finding_duplicates":
		scope, err := s.historicalFindingReviewSelection(ctx)
		if err != nil {
			return nil, err
		}
		var scopes [][]int64
		if scope != nil {
			scopes = append(scopes, scope)
		}
		items, total, strongTotal, err := s.m.pg.FindingCaseCandidates(fid, page, 20, scopes...)
		return map[string]any{"items": items, "total": total, "strong_total": strongTotal, "page": page, "page_size": 20,
			"note": "점수순 정렬. weak 는 같은 취약점 유형과 자산 겹침을 동시에 갖추지 않음이니 hit_reasons 와 함께 확인하며 참고용으로만 쓰고, 보통 이를 근거로 병합을 제안하면 안 됨"}, err
	case "get_finding_record":
		f, err := s.m.pg.GetFinding(fid)
		if err != nil {
			return nil, err
		}
		if f == nil {
			return nil, errors.New("취약점이 존재하지 않습니다")
		}
		dto := s.caseFindingDTO(f)
		cid, err := s.m.pg.FindingCaseID(f.ID)
		if err != nil {
			return nil, err
		}
		if cid > 0 {
			dto.CaseID = i64s(cid)
		}
		if conv := intercept.ConvIDFromContext(ctx); conv > 0 {
			if _, err := s.m.pg.Exec(`UPDATE finding_case_review_runs SET reviewed_ids=CASE WHEN EXISTS(SELECT 1 FROM json_each(reviewed_ids) WHERE value=$2) THEN reviewed_ids ELSE collection_append(reviewed_ids,$2) END WHERE conversation_id=$1 AND EXISTS(SELECT 1 FROM json_each(finding_ids) WHERE value=$2)`, conv, f.ID); err != nil {
				return nil, err
			}
		}
		return map[string]any{"finding": dto, "finding_node_id": f.NodeID}, nil
	case "merge_finding_records":
		id, err := s.m.pg.MergeFindingCase(ctx, parseProfileID(a.TaskID), a.ids(), a.Title, a.Reason, "reporter")
		return map[string]string{"case_id": i64s(id)}, err
	case "suggest_finding_merge":
		ids := a.ids()
		if len(ids) < 2 || len(ids) > 20 {
			return nil, errors.New("중복 의심 그룹에는 기록이 2 에서 20 건 필요합니다")
		}
		id, err := s.m.pg.SuggestFindingCase(ctx, parseProfileID(a.TaskID), ids, a.Title, a.Reason)
		return map[string]any{"suggestion_id": i64s(id), "finding_ids": ids}, err
	case "get_finding_case":
		c, err := s.m.pg.GetFindingCase(cid)
		if err != nil {
			return nil, err
		}
		if c == nil {
			return nil, errors.New("취약점 그룹이 존재하지 않습니다")
		}
		fs, total, err := s.m.pg.FindingCaseMembers(cid, page, 20)
		members := []FindingDTO{}
		for _, f := range fs {
			members = append(members, s.caseFindingDTO(f))
		}
		return map[string]any{"case": c, "members": members, "total": total, "page": page, "page_size": 20}, err
	case "update_finding_case_report":
		if err := validateUnifiedFindingReport(a.Report); err != nil {
			return nil, err
		}
		err := s.m.pg.UpdateFindingCaseReport(ctx, cid, a.Version, a.Title, a.Report, a.Severity, a.SeverityReason)
		return map[string]bool{"ok": err == nil}, err
	}
	return nil, errors.New("알 수 없는 병합 도구")
}
func idParam(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}
func (s *Server) seedFindingCaseTools() {
	_, _ = s.m.pg.Exec(`UPDATE finding_case_review_runs SET state='failed',error='서비스 재시작으로 중단되었습니다. 정리를 다시 제출하세요' WHERE state IN ('queued','running')`)
	a, err := s.m.pg.GetAgentByKey("reporter")
	if err != nil || a == nil {
		return
	}
	const flag = "finding_case_tools_v3_review"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	names := []string{}
	previous, _, _ := s.m.pg.GetSetting("finding_case_tools_v1")
	// v3 refreshes descriptions and schemas without repeating the v2 binding migration.
	previousReview, _, _ := s.m.pg.GetSetting("finding_case_tools_v2_review")
	for _, t := range s.findingCaseTools() {
		schema, _ := json.Marshal(t.InputSchema())
		if err := s.m.pg.RefreshToolDefaults(t.Name(), t.Description(), schema); err != nil {
			log.Printf("[finding-cases] refresh %s schema failed: %v", t.Name(), err)
		}
		if previousReview == "true" {
			continue
		}
		if err := s.m.pg.SeedTool(t.Name(), t.Description(), schema, json.RawMessage(`["reporter"]`)); err != nil {
			return
		}
		if previous != "true" || t.Name() == "complete_finding_case_review" {
			names = append(names, t.Name())
		}
	}
	if previousReview != "true" {
		if err := s.m.pg.AddAgentToToolBinding("reporter", names); err != nil {
			return
		}
		if err := s.m.pg.SetSetting("finding_case_tools_v2_review", "true"); err != nil {
			return
		}
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

const findingCaseReviewTaskMessage = "수동 선택 정리(원본 기록 읽기 전용): 이 작업에서 선택된 finding_ids=%s. 기존 보고를 하나씩 읽고, 같은 실제 근본 원인인지만 판단한다. 근본 원인이 명확히 같으면 바로 병합한다. 증거가 같은 엔드포인트, 핸들러/백엔드 컴포넌트, 파라미터, 누락된 검사 같은 구체적 공통점을 가리키지만 공통 근본 원인을 확정할 핵심 증거가 부족할 때만, 같은 작업 의심 구성원 2..20 건을 포괄하는 그룹을 제출하며 둘씩 쪼개 제안하지 않는다. 유형이나 자산만 같으면 제안하기에 부족하고, 구체적 공통점이 없으면 independent 를 유지하며 불확실하다는 이유로 제안하지 않는다. 증거가 서로 다른 코드 경로임을 보여 주면 independent 로 판정할 수 있다. 기존 또는 새 그룹은 통합 보고서를 최선을 다해 갱신한다. 통합 보고서가 잠시 없거나 갱신 대기 중이어도 complete_finding_case_review 를 막지 않으며, 이후 실행이나 사람이 보충할 수 있다. 원본 보고를 생성하거나 덮어쓰거나, 등급·상태를 바꾸거나, 증거를 보충 바인딩하는 것이 금지되며, 선택되지 않은 기록을 병합하지 않고, 대상을 탐지하지 않으며, 수동 기각을 지킨다. 끝내기 전에 complete_finding_case_review 를 호출해 기록별 판단을 제출하고 검수를 통과한다."

func (s *Server) reviewFindingCases(w http.ResponseWriter, r *http.Request) {
	var a findingCaseRequest
	if !decodeCaseRequest(w, r, &a) {
		return
	}
	ids := a.ids()
	if cid := parseProfileID(a.CaseID); cid > 0 {
		c, err := s.m.pg.GetFindingCase(cid)
		if err != nil || c == nil || !c.Active {
			writeErr(w, 400, "취약점 그룹을 사용할 수 없습니다")
			return
		}
		if ctx := r.URL.Query().Get("context_task"); ctx != "" && (c.TaskID == nil || ctx != i64s(*c.TaskID)) {
			writeErr(w, 403, "상속된 취약점 그룹은 읽기 전용입니다")
			return
		}
		fs, _, err := s.m.pg.FindingCaseMembers(cid, 1, 100000)
		if err != nil {
			caseHTTPError(w, err)
			return
		}
		ids = nil
		for _, f := range fs {
			ids = append(ids, f.ID)
		}
	}
	if len(ids) == 0 || (len(a.CaseID) == 0 && len(ids) > 200) {
		writeErr(w, 400, "원본 보고를 1 에서 200 건 선택하세요")
		return
	}
	reporter, err := s.m.pg.GetAgentByKey("reporter")
	if err != nil || reporter == nil || !reporter.Enabled || s.chatAgentRef() == nil {
		writeErr(w, 409, "보고 Agent 가 활성화되지 않았거나 모델을 사용할 수 없습니다")
		return
	}
	batches := map[int64][]int64{}
	for _, fid := range ids {
		f, err := s.m.pg.FindingCaseSummary(fid)
		if err != nil || f == nil || f.TaskID == nil {
			writeErr(w, 400, "선택한 취약점의 출처 작업을 사용할 수 없습니다")
			return
		}
		batches[*f.TaskID] = append(batches[*f.TaskID], fid)
	}
	// Validate and snapshot every source before starting any batch.
	taskContexts := map[int64]triggeredRun{}
	for taskID := range batches {
		t, ok := s.m.Task(i64s(taskID))
		if !ok {
			writeErr(w, 409, "작업을 사용할 수 없습니다")
			return
		}

		taskContexts[taskID] = triggeredRun{taskDesc: t.Description, taskGoal: t.Goal}
	}
	reservations, err := s.m.pg.CreateFindingCaseReviews(r.Context(), batches)
	if err != nil {
		caseHTTPError(w, err)
		return
	}
	runs := []map[string]any{}
	for _, reservation := range reservations {
		taskID, fids := reservation.TaskID, reservation.FindingIDs
		taskContext := taskContexts[taskID]
		raw, _ := json.Marshal(fids)
		msg := fmt.Sprintf(findingCaseReviewTaskMessage, raw)
		item := triggeredRun{agentKey: "reporter", title: "취약점 정리 · task#" + i64s(taskID), message: msg, taskID: taskID, taskDesc: taskContext.taskDesc, taskGoal: taskContext.taskGoal, conversationID: reservation.ConversationID}
		cfg := s.readTriggerBehavior("reporter")
		s.queueMu.Lock()
		s.triggerCfg["reporter"] = cfg
		s.triggerQ["reporter"] = append(s.triggerQ["reporter"], item)
		s.pumpLocked("reporter")
		s.queueMu.Unlock()
		runs = append(runs, map[string]any{"conversation_id": reservation.ConversationID, "task_id": i64s(taskID)})
	}
	writeJSON(w, 202, map[string]any{"runs": runs})
}
func (s *Server) caseReviewRuns(w http.ResponseWriter, r *http.Request) {
	rows, err := s.m.pg.Query(`SELECT conversation_id,task_id,state,error FROM finding_case_review_runs ORDER BY conversation_id DESC LIMIT 50`)
	if err != nil {
		caseHTTPError(w, err)
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var conv int64
		var task sql.NullInt64
		var state, msg string
		if err := rows.Scan(&conv, &task, &state, &msg); err != nil {
			caseHTTPError(w, err)
			return
		}
		items = append(items, map[string]any{"conversation_id": conv, "task_id": i64s(task.Int64), "state": state, "error": msg})
	}
	if err := rows.Err(); err != nil {
		caseHTTPError(w, err)
		return
	}
	writeJSON(w, 200, items)
}

// Manual reviews cannot mutate a finding outside the explicit selection.
func (s *Server) checkCaseReviewScope(ctx context.Context, name string, a findingCaseRequest) error {
	conv := intercept.ConvIDFromContext(ctx)
	if conv == 0 {
		return nil
	}
	var raw, conclusion json.RawMessage
	err := s.m.pg.QueryRow(`SELECT finding_ids,conclusion FROM finding_case_review_runs WHERE conversation_id=$1`, conv).Scan(&raw, &conclusion)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	var ids []int64
	if err := json.Unmarshal(raw, &ids); err != nil {
		return err
	}
	if string(conclusion) != "{}" && (name == "merge_finding_records" || name == "suggest_finding_merge" || name == "update_finding_case_report") {
		return errors.New("정리 판단을 이미 제출해 검수 후에는 수정할 수 없습니다")
	}
	allowed := map[int64]bool{}
	for _, id := range ids {
		allowed[id] = true
	}
	switch name {
	case "search_finding_duplicates":
		if !allowed[parseProfileID(a.FindingID)] {
			return errors.New("과거 정리는 선택한 기록의 후보만 검색할 수 있습니다")
		}
	case "update_finding_report", "bind_finding_traffic", "report_finding":
		return errors.New("과거 정리는 원본 보고, 상고, 증거 수정이 금지됩니다. 병합 관계와 통합 보고서만 쓸 수 있습니다")
	case "merge_finding_records", "suggest_finding_merge":
		checkIDs := a.ids()
		for _, id := range checkIDs {
			if !allowed[id] {
				return errors.New("수동 정리는 선택한 기록만 병합할 수 있습니다")
			}
		}
	case "update_finding_case_report":
		fs, _, err := s.m.pg.FindingCaseMembers(parseProfileID(a.CaseID), 1, 100000)
		if err != nil {
			return err
		}
		found := false
		for _, f := range fs {
			if allowed[f.ID] {
				found = true
			}
		}
		if !found {
			return errors.New("이 취약점 그룹은 사람이 선택한 범위에 속하지 않습니다")
		}
	}
	return nil
}
