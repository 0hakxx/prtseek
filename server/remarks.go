package server

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/Autumn-27/artex/db"
)

const maxRemarkRunes = db.MaxTaskRemarkRunes

func normalizeRemark(value string) (string, error) {
	value = strings.TrimSpace(value)
	if utf8.RuneCountInString(value) > maxRemarkRunes {
		return "", fmt.Errorf("메모는 최대 %d 자입니다", maxRemarkRunes)
	}
	return value, nil
}

func (s *Server) updateScheduleMetadata(w http.ResponseWriter, r *http.Request) {
	id, err := parseScheduleID(r)
	if err != nil || id <= 0 {
		writeErr(w, 400, "예약 번호가 유효하지 않습니다")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxTaskMetadataRequestSize)
	var request struct {
		Name   *string `json:"name"`
		Remark *string `json:"remark"`
	}
	if err := decode(r, &request); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeErr(w, 413, "요청 본문이 너무 큽니다")
		} else {
			writeErr(w, 400, err.Error())
		}
		return
	}
	if request.Name == nil && request.Remark == nil {
		writeErr(w, 400, "name 또는 remark 를 제공해야 합니다")
		return
	}
	if request.Name != nil {
		name := strings.TrimSpace(*request.Name)
		if name == "" || utf8.RuneCountInString(name) > maxTaskNameRunes {
			writeErr(w, 400, fmt.Sprintf("예약 이름은 1 에서 %d 자여야 합니다", maxTaskNameRunes))
			return
		}
		request.Name = &name
	}
	if request.Remark != nil {
		remark, err := normalizeRemark(*request.Remark)
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		request.Remark = &remark
	}
	item, err := s.m.pg.UpdateTaskScheduleMetadata(id, db.TaskSchedulePatch{Name: request.Name, Remark: request.Remark})
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if item == nil {
		writeErr(w, 404, "예약이 존재하지 않습니다")
		return
	}
	writeJSON(w, 200, item)
}
