package server

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/Autumn-27/artex/db"
)

func intelligenceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, db.ErrIntelligenceInvalid):
		writeErr(w, 400, err.Error())
	case errors.Is(err, db.ErrIntelligenceNotFound):
		writeErr(w, 404, "정보가 없거나 현재 범위에 속하지 않습니다")
	default:
		writeErr(w, 500, err.Error())
	}
}
func (s *Server) intelligenceFilter(w http.ResponseWriter, r *http.Request) (db.IntelligenceFilter, bool) {
	f := db.IntelligenceFilter{}
	id, ok := pathInt(r, "id")
	if !ok || id <= 0 {
		writeErr(w, 400, "유효하지 않은 ID")
		return f, false
	}
	if r.PathValue("scope") == "assets" {
		f.AssetID = id
	} else {
		f.TaskID = id
	}
	var exists bool
	var err error
	if f.TaskID > 0 {
		err = s.m.pg.QueryRowContext(r.Context(), `SELECT EXISTS(SELECT 1 FROM tasks WHERE id=$1 AND deleted_at IS NULL)`, id).Scan(&exists)
	} else {
		err = s.m.pg.QueryRowContext(r.Context(), `SELECT EXISTS(SELECT 1 FROM assets WHERE id=$1)`, id).Scan(&exists)
	}
	if err != nil {
		intelligenceError(w, err)
		return f, false
	}
	if !exists {
		writeErr(w, 404, "작업이나 자산이 존재하지 않습니다")
		return f, false
	}
	q := r.URL.Query()
	f.Kind = q.Get("kind")
	f.Status = q.Get("status")
	f.Query = q.Get("q")
	f.Limit, _ = strconv.Atoi(q.Get("limit"))
	f.Offset, _ = strconv.Atoi(q.Get("offset"))
	if len(f.Query) > 500 {
		writeErr(w, 400, "검색 키워드가 너무 깁니다")
		return f, false
	}
	return f, true
}
func (s *Server) listIntelligence(w http.ResponseWriter, r *http.Request) {
	f, ok := s.intelligenceFilter(w, r)
	if !ok {
		return
	}
	page, err := s.m.pg.ListIntelligence(r.Context(), f)
	if err != nil {
		intelligenceError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, page)
}
func (s *Server) getIntelligence(w http.ResponseWriter, r *http.Request) {
	f, ok := s.intelligenceFilter(w, r)
	if !ok {
		return
	}
	id, ok := pathInt(r, "intelID")
	if !ok {
		writeErr(w, 400, "유효하지 않은 정보 ID")
		return
	}
	item, err := s.m.pg.GetIntelligence(r.Context(), id)
	if err != nil {
		intelligenceError(w, err)
		return
	}
	allowed := false
	if f.TaskID > 0 {
		err = s.m.pg.QueryRowContext(r.Context(), `SELECT $1=$2 OR EXISTS(SELECT 1 FROM task_relations WHERE task_id=$1 AND source_task_id=$2)`, f.TaskID, item.TaskID).Scan(&allowed)
	} else {
		for _, assetID := range item.AssetIDs {
			if assetID == f.AssetID {
				allowed = true
			}
		}
	}
	if err != nil {
		intelligenceError(w, err)
		return
	}
	if !allowed {
		intelligenceError(w, db.ErrIntelligenceNotFound)
		return
	}
	item.Inherited = f.TaskID > 0 && item.TaskID != f.TaskID
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, item)
}
func (s *Server) updateIntelligence(w http.ResponseWriter, r *http.Request) {
	taskID, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, 400, "유효하지 않은 작업 ID")
		return
	}
	id, ok := pathInt(r, "intelID")
	if !ok {
		writeErr(w, 400, "유효하지 않은 정보 ID")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var req struct {
		Status string `json:"status"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, "유효하지 않은 요청")
		return
	}
	if err := s.m.pg.UpdateIntelligenceStatus(r.Context(), taskID, id, req.Status); err != nil {
		intelligenceError(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"updated": true})
}
