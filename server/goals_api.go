package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/db"
)

// 개요 「목표 관리」의 수동 CRUD 인터페이스. agent 측의 set_goals 도구와 같은 goal 노드 묶음에 쓰지만,
// 진입점은 사람이 UI 에서 직접 추가/삭제/수정하는 것이다. 추가/수정 후에는 「작업 되살리기」 로직을 재사용한다(admitTask resume:
// 최종 상태→running, 일시 중지 해제, 필요하면 대기열에 넣음). 삭제는 되살리지 않는다(제품 결정에 따름). 모든 변경 handler 는
// beginTaskOperation/decInflight 를 거쳐 작업 삭제와의 경쟁을 피한다(의도 CRUD 와 같음).

// listGoals 는 이 작업의 모든 목표를 반환한다(text/vulnclass/state 를 나눠서). 목표 관리 카드 렌더링에 쓴다.
func (s *Server) listGoals(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	goals, err := t.Store.ListByKind(db.KindGoal, 10000)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"goals": goalDTOs(goals)})
}

// addGoal 은 목표 하나를 수동으로 추가한다: DB 저장(작업 루트 spawns 아래에 걸음) → 「목표를 추가함」 트리거를 기록해
// planner 를 깨움 → 작업을 되살려, 플래너가 새 목표에 따라 달성 여부를 다시 판단하게 한다.
func (s *Server) addGoal(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, 409, "작업을 삭제하는 중이어서 목표를 추가할 수 없습니다")
		return
	}
	defer s.engine.decInflight(t.ID)

	var body struct {
		Text      string `json:"text"`
		VulnClass string `json:"vulnclass"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "invalid JSON")
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		writeErr(w, 400, "목표 내용은 비워 둘 수 없습니다")
		return
	}
	payload := map[string]any{"text": text}
	if vc := strings.TrimSpace(body.VulnClass); vc != "" {
		payload["vulnclass"] = vc
	}
	id, err := t.Store.AddGoal(payload, "human")
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if of, _ := t.Store.OriginFactID(); of > 0 && id > 0 {
		_ = t.Store.Link(of, db.RelSpawns, id) // goal descends from the task root (origin fact)
	}
	t.NotifyGoal([]string{text}) // 「사람이 목표를 추가함:…」 트리거를 기록하고 planner 를 깨움
	s.reviveTask(t)              // 완료/일시 중지된 작업을 실행 상태로 되돌려 계속 실행
	node, _ := t.Store.GetNode(id)
	if node == nil {
		writeErr(w, 500, "목표를 쓴 뒤 읽기에 실패했습니다")
		return
	}
	writeJSON(w, 200, goalDTO(node))
}

// editGoal 은 목표 텍스트(및 vulnclass)를 수동으로 수정한다: DB 수정 → 「사용자가 목표를 old 에서 new 로 수정함」을
// 기록해 planner 를 깨움 → 작업을 되살려, 플래너가 새 목표에 따라 방향을 조정하게 한다.
func (s *Server) editGoal(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, 409, "작업을 삭제하는 중이어서 목표를 수정할 수 없습니다")
		return
	}
	defer s.engine.decInflight(t.ID)

	gid, err := strconv.ParseInt(r.PathValue("gid"), 10, 64)
	if err != nil || gid <= 0 {
		writeErr(w, 400, "bad goal id")
		return
	}
	var body struct {
		Text      string `json:"text"`
		VulnClass string `json:"vulnclass"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "invalid JSON")
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		writeErr(w, 400, "목표 내용은 비워 둘 수 없습니다")
		return
	}
	node, err := t.Store.GetNode(gid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if node == nil || node.Kind != db.KindGoal {
		writeErr(w, 404, "목표가 존재하지 않습니다")
		return
	}
	oldText := goalDTO(node).Text
	if err := t.Store.UpdateGoalPayload(gid, text, strings.TrimSpace(body.VulnClass)); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	t.NotifyGoalEdited(oldText, text) // 「사람이 목표를 old 에서 new 로 수정함」 트리거를 기록하고 planner 를 깨움
	s.reviveTask(t)                   // 추가와 동일: 작업을 되살려 새 목표에 따라 다시 판단
	updated, _ := t.Store.GetNode(gid)
	if updated == nil {
		writeErr(w, 500, "목표를 갱신한 뒤 읽기에 실패했습니다")
		return
	}
	writeJSON(w, 200, goalDTO(updated))
}

// deleteGoal 은 목표 하나를 수동으로 삭제한다(하드 삭제, 간선/앵커 연쇄 삭제): DB 삭제 → 「사용자가 목표 X 를 삭제함」을
// 기록해 planner 를 깨워 남은 목표를 다시 판단하게 한다. 제품 결정에 따라 삭제는 작업을 되살리지 【않는다】.
func (s *Server) deleteGoal(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, 409, "작업을 삭제하는 중이어서 목표를 삭제할 수 없습니다")
		return
	}
	defer s.engine.decInflight(t.ID)

	gid, err := strconv.ParseInt(r.PathValue("gid"), 10, 64)
	if err != nil || gid <= 0 {
		writeErr(w, 400, "bad goal id")
		return
	}
	node, err := t.Store.GetNode(gid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if node == nil || node.Kind != db.KindGoal {
		writeErr(w, 404, "목표가 존재하지 않습니다")
		return
	}
	text := goalDTO(node).Text
	if err := t.Store.DeleteGoal(gid); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	t.NotifyGoalDeleted(text) // 「사람이 목표 X 를 삭제함:…」 트리거를 기록하고 planner 를 깨움(작업을 되살리지 않음)
	writeJSON(w, 200, map[string]bool{"ok": true})
}
