package server

import (
	"net/http"
	"os"
	"path/filepath"

	"github.com/Autumn-27/artex/agent"
)

// The conversation ID is resolved through the authenticated conversation route;
// callers cannot supply filesystem paths or choose another task's memory root.
func (s *Server) personalResearchState(w http.ResponseWriter, r *http.Request) {
	_, c, ok := s.convByID(w, r)
	if !ok {
		return
	}
	if len(r.URL.Query().Get("q")) > 500 {
		writeErr(w, 400, "연구 기록 조회는 최대 500 바이트입니다")
		return
	}
	cfg, _, configured := s.personalProfileConfig(c)
	enabled := configured && (agent.IsDeepSeekFlash(cfg.Model) || isDefaultCTFAgent(c.AgentKey))
	root := filepath.Join(s.m.dir, "sessions", s.convBusyKey(c.ID))
	if _, err := os.Stat(root); os.IsNotExist(err) {
		writeJSON(w, 200, map[string]any{"enabled": enabled, "version": "1.2.0", "memory": nil})
		return
	}
	memory, err := agent.NewResearchMemory(root)
	if err != nil {
		writeErr(w, 500, "연구 기록을 지금은 읽을 수 없습니다: "+err.Error())
		return
	}
	state, err := memory.Recall(r.Context(), agent.ResearchRecallInput{Query: r.URL.Query().Get("q"), Limit: 20, VerifyEvidence: r.URL.Query().Get("verify") == "true"})
	if err != nil {
		writeErr(w, 500, "연구 기록 읽기 실패: "+err.Error())
		return
	}
	run, err := memory.RunState()
	if err != nil {
		writeErr(w, 500, "연구 실행 상태 읽기 실패: "+err.Error())
		return
	}
	if run != nil && run.Status == "active" {
		s.chatMu.Lock()
		running := s.chatBusy[s.convBusyKey(c.ID)]
		s.chatMu.Unlock()
		if !running {
			run.Status = "interrupted"
		}
	}
	writeJSON(w, 200, map[string]any{"enabled": enabled, "version": "1.2.0", "memory": state, "run": run})
}
