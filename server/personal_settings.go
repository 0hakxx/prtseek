package server

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/llmrec"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
)

func (s *Server) initPersonalSettings() error {
	if s.m.pg == nil {
		return nil
	}
	if err := s.m.pg.EnsurePersonalSettings(); err != nil {
		return err
	}
	_, err := s.m.pg.Exec(`INSERT INTO agents(key,name,description,role,builtin,enabled,max_turns,run_seconds)
	 VALUES('personal','개인 도우미','현재 대화에서 작업을 직접 처리하며, 지속 실행이 필요하면 백그라운드 전환 버튼을 사용한다','assistant',true,true,40,900)
	 ON CONFLICT(key) DO NOTHING`)
	if err != nil {
		return err
	}
	a, err := s.m.pg.GetAgentByKey("personal")
	if err != nil {
		return err
	}
	if err := s.m.pg.SeedPromptIfEmpty(a.ID, agent.PersonalDefaultPrompt); err != nil {
		return err
	}
	agent.TaskResponseLanguage = func(taskID int64) string {
		if taskID <= 0 {
			return ""
		}
		var raw []byte
		if s.m.pg.QueryRow(`SELECT config_snapshot FROM conversation_task_runs WHERE task_id=$1`, taskID).Scan(&raw) != nil {
			return ""
		}
		var snap personalRunSnapshot
		_ = json.Unmarshal(raw, &snap)
		return snap.ResponseLanguage
	}
	agent.TaskResearchRoot = func(taskID int64) string {
		var conversationID int64
		if s.m.pg.QueryRow(`SELECT conversation_id FROM conversation_task_runs WHERE task_id=$1`, taskID).Scan(&conversationID) != nil {
			return ""
		}
		return filepath.Join(s.m.dir, "sessions", s.convBusyKey(conversationID))
	}
	return nil
}

func (s *Server) registerPersonalSettingsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/personal/onboarding", s.personalOnboarding)
	mux.HandleFunc("PATCH /api/personal/onboarding", s.personalOnboarding)
	mux.HandleFunc("GET /api/personal/model-capabilities", s.personalModelCapabilities)
	mux.HandleFunc("POST /api/personal/model-capabilities", s.personalModelCapabilities)
	mux.HandleFunc("GET /api/conversations/{id}/research", s.personalResearchState)
}

type personalOnboardingState struct {
	Model string `json:"model"`
	Tools string `json:"tools"`
}

func (s *Server) personalOnboarding(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	state := personalOnboardingState{Model: "pending", Tools: "pending"}
	if value, _, _ := pg.GetSetting("personal_onboarding_v1"); value != "" {
		_ = json.Unmarshal([]byte(value), &state)
	}
	if state.Model == "pending" {
		if p, _ := pg.ActiveProfile(); p != nil && p.APIKey != "" {
			state.Model = "completed"
		}
	}
	if r.Method == http.MethodPatch {
		var req struct {
			Model *string `json:"model"`
			Tools *string `json:"tools"`
		}
		if !decodeConversationRequest(w, r, &req) {
			return
		}
		valid := func(v string) bool { return v == "pending" || v == "skipped" || v == "completed" }
		if req.Model != nil {
			if !valid(*req.Model) {
				writeErr(w, 400, "유효하지 않은 모델 안내 상태")
				return
			}
			state.Model = *req.Model
		}
		if req.Tools != nil {
			if !valid(*req.Tools) {
				writeErr(w, 400, "유효하지 않은 도구 안내 상태")
				return
			}
			state.Tools = *req.Tools
		}
		b, _ := json.Marshal(state)
		if err := pg.SetSetting("personal_onboarding_v1", string(b)); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	writeJSON(w, 200, state)
}

// Candidate values are adapter hints, not proof that a remote model obeys them.
// A chosen value must pass the explicit request probe before it is persisted.
func reasoningCandidates(cfg agent.Config) []string {
	m := strings.ToLower(strings.TrimSpace(cfg.Model))
	// DeepSeek's current models default to thinking enabled. Expose its three
	// distinct effort levels, not aliases such as medium (which maps to high).
	// https://api-docs.deepseek.com/guides/thinking_mode
	if agent.IsDeepSeekFlash(m) || m == "deepseek-v4-pro" {
		return []string{"low", "high", "max"}
	}
	if strings.HasPrefix(m, "gpt-5") || strings.HasPrefix(m, "gpt-6") || strings.HasPrefix(m, "o3") || strings.HasPrefix(m, "o4") {
		return []string{"low", "medium", "high"}
	}
	if strings.HasPrefix(m, "claude-opus-4") || strings.HasPrefix(m, "claude-sonnet-4") {
		return []string{"low", "medium", "high"}
	}
	return []string{}
}

func capabilityKey(id int64, cfg agent.Config, effort string) string {
	b := fmt.Sprintf("%d\x00%s\x00%s\x00%s\x00%s\x00%s", id, cfg.Format, cfg.BaseURL, cfg.Model, cfg.APIKey, effort)
	h := sha256.Sum256([]byte(b))
	return "personal_effort_" + hex.EncodeToString(h[:])
}

func validEffort(v string) bool {
	switch v {
	case "", "minimal", "low", "medium", "high", "xhigh", "max":
		return true
	}
	return false
}

func (s *Server) personalModelCapabilities(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	id, _ := strconv.ParseInt(r.URL.Query().Get("profile_id"), 10, 64)
	effort := ""
	if r.Method == http.MethodPost {
		var req struct {
			ProfileID       int64  `json:"profile_id"`
			ReasoningEffort string `json:"reasoning_effort"`
		}
		if !decodeConversationRequest(w, r, &req) {
			return
		}
		id, effort = req.ProfileID, req.ReasoningEffort
	}
	if id == 0 {
		if p, _ := s.m.pg.ActiveProfile(); p != nil {
			id = p.ID
		}
	}
	cfg, ok := s.loadProfileConfig(id)
	if !ok {
		if r.Method == http.MethodPost {
			writeErr(w, 400, "먼저 사용 가능한 모델 설정을 저장하세요")
			return
		}
		writeJSON(w, 200, map[string]any{"reasoning_efforts": []string{}, "verified": false, "source": "unknown"})
		return
	}
	values := reasoningCandidates(cfg)
	verified := false
	if r.Method == http.MethodPost {
		if !validEffort(effort) {
			writeErr(w, 400, "유효하지 않은 사고 강도")
			return
		}
		if effort != "" {
			cfg.ReasoningEffort = effort
		}
		if _, _, err := agent.TestConnection(r.Context(), cfg); err != nil {
			writeErr(w, 400, "이 모델 설정이 이 강도의 요청 검증을 통과하지 못했습니다: "+redactPersonalError(err, cfg.APIKey))
			return
		}
		if err := s.m.pg.SetSetting(capabilityKey(id, cfg, effort), "true"); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		verified = true
	}
	writeJSON(w, 200, map[string]any{"reasoning_efforts": values, "verified": verified, "source": "adapter", "profile_id": id, "model": cfg.Model, "research_optimized": agent.IsDeepSeekFlash(cfg.Model), "personal_version": "1.2.0"})
}

func redactPersonalError(err error, key string) string {
	msg := err.Error()
	if key != "" {
		msg = strings.ReplaceAll(msg, key, "[REDACTED]")
	}
	return msg
}

func (s *Server) validatePersonalEffort(c *db.Conversation, effort string) error {
	if !validEffort(effort) {
		return fmt.Errorf("유효하지 않은 사고 강도")
	}
	if effort == "" {
		return nil
	}
	cfg, id, ok := s.personalProfileConfig(c)
	if !ok {
		return fmt.Errorf("먼저 사용 가능한 모델 설정을 저장하세요")
	}
	verified, _, err := s.m.pg.GetSetting(capabilityKey(id, cfg, effort))
	if err != nil {
		return err
	}
	if verified != "true" {
		return fmt.Errorf("먼저 이 모델 설정의 사고 강도를 검증한 뒤 선택을 저장하세요")
	}
	return nil
}

func (s *Server) personalProfileConfig(c *db.Conversation) (agent.Config, int64, bool) {
	if c.LLMProfileID != nil {
		cfg, ok := s.loadProfileConfig(*c.LLMProfileID)
		return cfg, *c.LLMProfileID, ok
	}
	// Legacy specialised conversations keep their existing default role binding.
	// An explicit conversation selection always wins above, while personal chats
	// deliberately have no hidden role-specific override.
	if c.AgentKey != "personal" {
		if id := s.effectiveProfileForAgent(c.AgentKey, nil); id != nil {
			if cfg, ok := s.loadProfileConfig(*id); ok {
				return cfg, *id, true
			}
		}
	}
	if p, _ := s.m.pg.ActiveProfile(); p != nil {
		cfg, ok := s.loadProfileConfig(p.ID)
		return cfg, p.ID, ok
	}
	_, cfg, ok := s.globalProvider()
	return cfg, 0, ok
}

func (s *Server) personalConversationSettings(id int64) db.PersonalConversationSettings {
	out, _ := s.m.pg.PersonalConversationSettings(id)
	return out
}

type personalConversationView struct {
	*db.Conversation
	Mode                     string `json:"mode"`
	ReasoningEffort          string `json:"reasoning_effort"`
	ResponseLanguage         string `json:"response_language"`
	EffectiveModel           string `json:"effective_model"`
	EffectiveProfileID       int64  `json:"effective_profile_id"`
	EffectiveReasoningEffort string `json:"effective_reasoning_effort"`
}

func (s *Server) personalConversationView(c *db.Conversation) personalConversationView {
	p := s.personalConversationSettings(c.ID)
	cfg, id, _ := s.personalProfileConfig(c)
	effort := p.ReasoningEffort
	if effort == "" {
		effort = cfg.ReasoningEffort
	}
	return personalConversationView{Conversation: c, Mode: workflowMode(c.AgentKey), ReasoningEffort: p.ReasoningEffort, ResponseLanguage: p.ResponseLanguage, EffectiveModel: cfg.Model, EffectiveProfileID: id, EffectiveReasoningEffort: effort}
}

func (s *Server) personalChatAgent(c *db.Conversation) *agent.ChatAgent {
	cfg, id, ok := s.personalProfileConfig(c)
	if !ok {
		return nil
	}
	settings := s.personalConversationSettings(c.ID)
	if settings.ReasoningEffort != "" {
		cfg.ReasoningEffort = settings.ReasoningEffort
	}
	prov, err := cfg.NewProviderWithLimiter(s.requestLimiterForProfile(id))
	if err != nil {
		return nil
	}
	prov = llmrec.Wrap(prov, s.m.Database(), cfg.Model, "personal", cfg.ThinkingType, cfg.ReasoningEffort, s.m.LLMRecordEnabled)
	ca := agent.NewChatAgent(prov, cfg.Model, s.m.dir, transcript.NewStore(filepath.Join(s.m.dir, "transcripts")), cfg.CompactionWindow())
	ca.SetProxy(s.m.ProxyAddr(), s.m.ProxyCACert())
	ca.SetWebSearch(s.m.WebSearchOpts())
	ca.SetGuard(s.chatGuard())
	ca.SetNonStreaming(nonStreamingResolver(cfg))
	ca.SetMaxTokens(maxTokensResolver(cfg))
	ca.SetNoaEnabled(s.m.NoaCompactionEnabled)
	return ca
}

func inferPersonalLanguage(message, previous string) string {
	low := strings.ToLower(message)
	if strings.Contains(low, "用英文") || strings.Contains(low, "영어로") || strings.Contains(low, "reply in english") || strings.Contains(low, "respond in english") {
		return "en"
	}
	if strings.Contains(low, "用中文") || strings.Contains(low, "중국어로") || strings.Contains(low, "reply in chinese") || strings.Contains(low, "respond in chinese") {
		return "zh"
	}
	if previous != "" {
		return previous
	}
	han, letters := 0, 0
	for _, r := range message {
		if unicode.Is(unicode.Han, r) {
			han++
		}
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
			letters++
		}
	}
	if han > 0 {
		return "zh"
	}
	if letters >= 3 {
		return "en"
	}
	return "zh"
}

// The snapshot deliberately contains no API key. Credentials stay referenced by
// profile ID; model, endpoint and inference settings are frozen for this run.
type personalRunSnapshot struct {
	ProfileID         int64             `json:"profile_id"`
	Model             string            `json:"model"`
	BaseURL           string            `json:"base_url"`
	Format            string            `json:"format"`
	ReasoningEffort   string            `json:"reasoning_effort"`
	ThinkingType      string            `json:"thinking_type,omitempty"`
	ResponseLanguage  string            `json:"response_language"`
	Streaming         bool              `json:"streaming"`
	ContextWindowK    int               `json:"context_window_k"`
	MaxTokens         int               `json:"max_tokens"`
	MaxTokensField    string            `json:"max_tokens_field"`
	CredentialBinding string            `json:"credential_binding"`
	ConnectionBinding string            `json:"connection_binding,omitempty"`
	RatePerSecond     float64           `json:"rate_per_second"`
	RatePerMinute     float64           `json:"rate_per_minute"`
	Retry             agent.RetryConfig `json:"retry"`
	Version           int               `json:"version"`
}

func personalCredentialBinding(cfg agent.Config) string {
	h := sha256.Sum256([]byte(string(cfg.Format) + "\x00" + cfg.BaseURL + "\x00" + cfg.APIKey))
	return hex.EncodeToString(h[:])
}

func makePersonalRunSnapshot(cfg agent.Config, id int64, effort, language string) personalRunSnapshot {
	if effort == "" {
		effort = cfg.ReasoningEffort
	}
	return personalRunSnapshot{ProfileID: id, Model: cfg.Model, BaseURL: cfg.BaseURL, Format: string(cfg.Format), ReasoningEffort: effort, ThinkingType: cfg.ThinkingType, ResponseLanguage: language, Streaming: cfg.Stream, ContextWindowK: cfg.ContextWindowK, MaxTokens: cfg.MaxTokens, MaxTokensField: cfg.MaxTokensField, CredentialBinding: personalCredentialBinding(cfg), ConnectionBinding: personalConnectionBinding(cfg), RatePerSecond: cfg.RatePerSecond, RatePerMinute: cfg.RatePerMinute, Retry: cfg.Retry, Version: 4}
}

func personalConnectionBinding(cfg agent.Config) string {
	h := sha256.Sum256([]byte(cfg.Proxy + "\x00" + cfg.SessionHeaderKey))
	return hex.EncodeToString(h[:])
}

func (s *Server) personalBackgroundSnapshot(c *db.Conversation) (json.RawMessage, error) {
	cfg, id, ok := s.personalProfileConfig(c)
	if !ok || id == 0 {
		return nil, fmt.Errorf("먼저 사용 가능한 모델 설정을 저장하고 선택하세요")
	}
	p := s.personalConversationSettings(c.ID)
	if err := s.validatePersonalEffort(c, p.ReasoningEffort); err != nil {
		return nil, err
	}
	return json.Marshal(makePersonalRunSnapshot(cfg, id, p.ReasoningEffort, p.ResponseLanguage))
}

func (s *Server) personalApplyBackgroundSettings(t *Task, run *db.PersonalBackgroundRun) error {
	var snap personalRunSnapshot
	if err := json.Unmarshal(run.ConfigSnapshot, &snap); err != nil {
		return err
	}
	if snap.ProfileID <= 0 {
		return fmt.Errorf("백그라운드 작업에 모델 스냅샷이 없습니다")
	}
	if _, ok := s.loadProfileConfig(snap.ProfileID); !ok {
		return fmt.Errorf("백그라운드 작업의 모델 설정을 사용할 수 없습니다")
	}
	return nil
}

func (s *Server) personalTaskConfig(taskID string) (agent.Config, bool, error) {
	if s.m.pg == nil {
		return agent.Config{}, false, nil
	}
	var raw []byte
	err := s.m.pg.QueryRow(`SELECT config_snapshot FROM conversation_task_runs WHERE task_id=$1`, taskID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return agent.Config{}, false, nil
	}
	if err != nil {
		return agent.Config{}, true, err
	}
	var snap personalRunSnapshot
	if err = json.Unmarshal(raw, &snap); err != nil {
		return agent.Config{}, true, err
	}
	cfg, ok := s.loadProfileConfig(snap.ProfileID)
	if !ok {
		return cfg, true, fmt.Errorf("백그라운드 작업 모델이 삭제되었거나 사용할 수 없습니다. 일시 중지한 뒤 다시 설정하세요")
	}
	if snap.CredentialBinding == "" || snap.CredentialBinding != personalCredentialBinding(cfg) {
		return agent.Config{}, true, fmt.Errorf("백그라운드 작업의 모델 자격 증명이나 서비스 주소가 바뀌었습니다. 일시 중지한 뒤 모델을 다시 선택하고 계속하세요")
	}
	if snap.Version >= 4 && snap.ConnectionBinding != personalConnectionBinding(cfg) {
		return agent.Config{}, true, fmt.Errorf("백그라운드 작업 연결 파라미터가 바뀌었습니다. 일시 중지한 뒤 모델을 다시 선택하고 계속하세요")
	}
	if snap.Version < 4 {
		cfg.Proxy, cfg.SessionHeaderKey = "", ""
	}
	cfg.Model = snap.Model
	cfg.BaseURL = snap.BaseURL
	cfg.Format = llm.Format(snap.Format)
	cfg.ReasoningEffort = snap.ReasoningEffort
	if snap.Version >= 3 {
		cfg.ThinkingType = snap.ThinkingType
	} else {
		cfg.ThinkingType = ""
	}
	cfg.Stream = snap.Streaming
	cfg.ContextWindowK = snap.ContextWindowK
	cfg.MaxTokens = snap.MaxTokens
	cfg.MaxTokensField = snap.MaxTokensField
	cfg.RatePerSecond, cfg.RatePerMinute = snap.RatePerSecond, snap.RatePerMinute
	cfg.Retry = snap.Retry
	return cfg, true, nil
}
