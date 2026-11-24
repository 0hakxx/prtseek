package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"log"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/llmrec"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
)

type taskAgentBundle struct {
	runtime        *taskLLMRuntime // goal decomposition runtime
	plannerRuntime *taskLLMRuntime
	workerRuntime  *taskLLMRuntime
	mainRuntime    *taskLLMRuntime
	pl             *agent.Planner
	wk             *agent.Worker
	main           *agent.MainAgent
	pinnedWorkers  map[int64]*agent.Worker
}

type llmAuditProfile struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Format string `json:"format"`
	Model  string `json:"model"`
}

type llmTransitionAudit struct {
	Mode     string           `json:"mode"` // automatic | manual | exhausted
	Reason   string           `json:"reason"`
	Previous *llmAuditProfile `json:"previous,omitempty"`
	Next     *llmAuditProfile `json:"next,omitempty"`
}

type llmActivityMetadata struct {
	LLMTransition llmTransitionAudit `json:"llm_transition"`
}

type taskLLMRuntime struct {
	s            *Server
	taskID       string
	agentKey     string
	pinProfileID *int64
}

type taskLLMError struct {
	taskID         string
	chainExhausted bool
	cause          error
}

func (e *taskLLMError) Error() string {
	if e.chainExhausted {
		return fmt.Sprintf("task %s LLM profile chain exhausted: %v", e.taskID, e.cause)
	}
	return e.cause.Error()
}

func (e *taskLLMError) Unwrap() error { return e.cause }

func isTaskLLMRuntimeError(err error) bool {
	var target *taskLLMError
	return errors.As(err, &target)
}

func isTaskLLMChainExhausted(err error) bool {
	var target *taskLLMError
	return errors.As(err, &target) && target.chainExhausted
}

// isQuotaExhaustedError is intentionally strict. A generic 429, auth error,
// network failure, or 5xx does not rotate providers; the response must explicitly
// identify quota, credits, billing balance, or payment exhaustion.
func isQuotaExhaustedError(err error) bool {
	return err != nil && agent.IsQuotaExhaustedMessage(err.Error())
}

type taskLLMSelection struct {
	task      *Task
	profileID int64
	revision  int64
	provider  llm.Provider
	// retry 는 이번에 선택된 설정에서 해석한 재시도 파라미터다(profile 재정의 → 전역 정책 → 내장 기본값).
	// 같은 provider 안전 구간 재시도는 이를 따르므로, profile 을 바꾸면 재시도 리듬도 바뀐다.
	retry agent.RetryConfig
}

type taskLLMStreamHooks struct {
	current    func() (taskLLMSelection, error)
	exhaust    func(taskLLMSelection, error) (db.TaskLLMTransition, error)
	transition func(taskLLMSelection, db.TaskLLMTransition, error)
}

// current resolves the provider this role runs on. Pinned workers use only their
// assigned profile; legacy task agents use Agent 바인딩 → 작업 LLM 설정 체인 → 전역/환경 설정.
// 바인딩이 작업 체인보다 우선한다: 어떤 역할에 모델을 명시적으로 지정하면 계속 그 모델에서 실행한다. 바인딩이 없거나
// 구성에 실패할 때만 작업 체인으로 강등되고, 작업 체인이 비어 있으면 전역 설정으로 강등된다.
// 반환되는 profile id 는 작업 체인을 탈 때만 0 이 아니다 —— streamTaskLLM 은 이것으로 할당량 오류가
// 작업의 장애 조치 상태를 진행시켜야 하는지 판단한다(바인딩/전역 경로는 작업 체인 상태를 바꾸지 않고 기존 의미를 따름).
func (r *taskLLMRuntime) current() (taskLLMSelection, error) {
	if cfg, linked, err := r.s.personalTaskConfig(r.taskID); linked {
		if err != nil {
			return taskLLMSelection{}, err
		}
		var profileID int64
		if err := r.s.m.pg.QueryRow(`SELECT (config_snapshot->>'profile_id') FROM conversation_task_runs WHERE task_id=$1`, r.taskID).Scan(&profileID); err != nil {
			return taskLLMSelection{}, err
		}
		prov, err := cfg.NewProviderWithLimiter(r.s.requestLimiterForProfile(profileID))
		if err != nil {
			return taskLLMSelection{}, err
		}
		prov = llmrec.Wrap(prov, r.s.m.Database(), cfg.Model, "personal-background", cfg.ThinkingType, cfg.ReasoningEffort, r.s.m.LLMRecordEnabled)
		t, _ := r.s.m.Task(r.taskID)
		return taskLLMSelection{task: t, provider: prov, retry: cfg.Retry}, nil
	}
	if r.pinProfileID != nil {
		// Pinned worker runtimes are exclusive: resolve their profile before
		// touching bindings or the task failover state. A missing or invalid
		// profile is an error, never an invitation to use another model.
		prov, cfg, ok := r.s.providerForProfile(*r.pinProfileID)
		if !ok {
			return taskLLMSelection{}, fmt.Errorf("LLM profile #%d is missing or invalid", *r.pinProfileID)
		}
		t, _ := r.s.m.Task(r.taskID)
		return taskLLMSelection{task: t, provider: prov, retry: cfg.Retry}, nil
	}
	taskNum, err := parseTaskID(r.taskID)
	if err != nil {
		return taskLLMSelection{}, err
	}
	pt, err := r.s.m.pg.GetTask(taskNum)
	if err != nil {
		return taskLLMSelection{}, err
	}
	if pt == nil {
		return taskLLMSelection{}, fmt.Errorf("task %s not found", r.taskID)
	}
	r.s.syncTaskLLMState(pt)
	t, _ := r.s.m.Task(r.taskID)
	sel := taskLLMSelection{task: t, revision: pt.LLMChainRevision}
	if prov, cfg, ok := r.s.agentBindingProvider(r.agentKey); ok {
		sel.provider, sel.retry = prov, cfg.Retry
		return sel, nil
	}
	if len(pt.LLMProfileIDs) > 0 {
		if pt.ActiveLLMProfileID == nil {
			return sel, &taskLLMError{taskID: r.taskID, chainExhausted: true, cause: errors.New("all selected profiles are quota exhausted")}
		}
		sel.profileID = *pt.ActiveLLMProfileID
		prov, cfg, ok := r.s.providerForProfile(sel.profileID)
		if !ok {
			return sel, fmt.Errorf("LLM profile #%d is missing or invalid", sel.profileID)
		}
		sel.provider, sel.retry = prov, cfg.Retry
		return sel, nil
	}
	prov, cfg, ok := r.s.globalProvider()
	if !ok {
		return sel, fmt.Errorf("task %s has no available fallback LLM provider", r.taskID)
	}
	sel.provider, sel.retry = prov, cfg.Retry
	return sel, nil
}

// activeCfg resolves the task's currently-active LLM config, mirroring current()'s
// source precedence (agent binding → active chain profile → global). Read-only and
// best-effort: ok=false when nothing resolves, leaving the per-setting fallback to
// the caller. If failover switches profiles, the change takes effect on the next
// agent run (a fresh Session is built per run in captureRun).
func (r *taskLLMRuntime) activeCfg() (agent.Config, bool) {
	if cfg, linked, err := r.s.personalTaskConfig(r.taskID); linked {
		return cfg, err == nil
	}
	if r.pinProfileID != nil {
		_, cfg, ok := r.s.providerForProfile(*r.pinProfileID)
		return cfg, ok
	}
	taskNum, err := parseTaskID(r.taskID)
	if err != nil {
		return agent.Config{}, false
	}
	pt, err := r.s.m.pg.GetTask(taskNum)
	if err != nil || pt == nil {
		return agent.Config{}, false
	}
	if _, cfg, ok := r.s.agentBindingProvider(r.agentKey); ok {
		return cfg, true
	}
	if len(pt.LLMProfileIDs) > 0 && pt.ActiveLLMProfileID != nil {
		if _, cfg, ok := r.s.providerForProfile(*pt.ActiveLLMProfileID); ok {
			return cfg, true
		}
	}
	if _, cfg, ok := r.s.globalProvider(); ok {
		return cfg, true
	}
	return agent.Config{}, false
}

// nonStreaming reports whether the task's currently-active LLM source is set to
// non-streaming. Unresolvable → streaming (false), the safe default.
func (r *taskLLMRuntime) nonStreaming() bool {
	cfg, ok := r.activeCfg()
	return ok && !cfg.Stream
}

// ResearchModel resolves the real model behind the task-router label for each
// run, using the same snapshot and profile precedence as actual model calls.
func (r *taskLLMRuntime) ResearchModel() string {
	cfg, _ := r.activeCfg()
	return cfg.Model
}

// maxTokens returns the currently-active source's per-reply output cap.
// Unresolvable → 0, i.e. send no cap, matching the pre-setting behaviour.
func (r *taskLLMRuntime) maxTokens() int {
	cfg, _ := r.activeCfg() // 설정을 해석하지 못하면 0 값 0
	return cfg.MaxTokens
}

func parseTaskID(id string) (int64, error) {
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid task id %q", id)
	}
	return n, nil
}

// streamHooks builds the failover/exhaustion callbacks shared by Stream and
// Complete: how to read the current profile selection, how to mark it quota
// exhausted, and how to emit a failover transition.
func (r *taskLLMRuntime) streamHooks() taskLLMStreamHooks {
	return taskLLMStreamHooks{
		current: r.current,
		exhaust: func(selection taskLLMSelection, cause error) (db.TaskLLMTransition, error) {
			taskNum, _ := parseTaskID(r.taskID)
			transition, err := r.s.m.pg.MarkTaskLLMProfileQuotaExhaustedAtRevision(taskNum, selection.profileID, selection.revision, cause.Error())
			if err != nil {
				return transition, err
			}
			if pt, getErr := r.s.m.pg.GetTask(taskNum); getErr == nil && pt != nil {
				r.s.syncTaskLLMState(pt)
			}
			return transition, nil
		},
		transition: func(selection taskLLMSelection, transition db.TaskLLMTransition, cause error) {
			r.s.emitTaskLLMTransition(selection.task, transition, cause)
		},
	}
}

func (r *taskLLMRuntime) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	ctx = llmrec.WithTaskID(ctx, r.taskID)
	return streamTaskLLM(ctx, r.taskID, req, r.streamHooks())
}

// Complete is the non-streaming counterpart of Stream. A non-streaming call is
// atomic — it never delivers partial output — so every failure is safe to retry
// on the same provider or fail over to the next profile without risking
// duplicated model output or tool execution (the "committed" bookkeeping the
// streaming path needs is unnecessary here).
func (r *taskLLMRuntime) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	ctx = llmrec.WithTaskID(ctx, r.taskID)
	return completeTaskLLM(ctx, r.taskID, req, r.streamHooks())
}

func completeTaskLLM(ctx context.Context, taskID string, req llm.CompletionRequest, hooks taskLLMStreamHooks) (llm.Message, string, llm.Usage, error) {
	for {
		selection, err := hooks.current()
		if err != nil {
			return llm.Message{}, "", llm.Usage{}, err
		}
		var (
			msg     llm.Message
			sr      string
			usage   llm.Usage
			callErr error
		)
		// 같은 provider 안전 구간 재시도: 비스트리밍 호출은 전체 성공 아니면 전체 실패라
		// 도중에 출력을 이미 넘긴 문제가 없으므로, 어떤 일시적 실패든 그대로 재시도할 수 있다.
		retries, backoffOf := sameProviderRetryPolicy(selection.retry)
		for attempt := 0; ; attempt++ {
			msg, sr, usage, callErr = selection.provider.Complete(ctx, req)
			if callErr != nil && ctx.Err() == nil &&
				attempt < retries && isRetryableStreamError(callErr) {
				backoff := backoffOf(attempt)
				log.Printf("[task-llm] task %s 비스트리밍 호출 실패, %v 후 같은 provider 로 재시도 (%d/%d): %v",
					taskID, backoff, attempt+1, retries, callErr)
				if sleepCtx(ctx, backoff) {
					break // 백오프 중에 ctx 취소 → 재시도 중지
				}
				continue
			}
			break
		}
		if callErr == nil {
			return msg, sr, usage, nil
		}
		// profileID=0: 명시적 체인이 이미 비워짐. 할당량 오류가 아님: 그대로 전달. 둘 다 작업 failover 상태를 바꾸지 않는다.
		if selection.profileID == 0 || !isQuotaExhaustedError(callErr) {
			return llm.Message{}, "", llm.Usage{}, callErr
		}
		transition, markErr := hooks.exhaust(selection, callErr)
		if markErr != nil {
			return llm.Message{}, "", llm.Usage{}, fmt.Errorf("mark profile quota exhausted after %v: %w", callErr, markErr)
		}
		if transition.Advanced && !transition.Stale && hooks.transition != nil {
			hooks.transition(selection, transition, callErr)
		}
		if !transition.Stale && transition.NextProfileID == nil {
			return llm.Message{}, "", llm.Usage{}, &taskLLMError{taskID: taskID, chainExhausted: transition.ChainExhausted, cause: callErr}
		}
		// 호출자에게 어떤 출력도 넘기지 않았으므로, 다음 profile 로 같은 논리 요청을 다시 보내는 것은 안전하다.
	}
}

func streamTaskLLM(ctx context.Context, taskID string, req llm.CompletionRequest, hooks taskLLMStreamHooks) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		for {
			selection, err := hooks.current()
			if err != nil {
				yield(llm.StreamEvent{}, err)
				return
			}
			committed := false
			var pending []llm.StreamEvent
			var streamErr error
			retries, backoffOf := sameProviderRetryPolicy(selection.retry)
			// 같은 provider 안전 구간 재시도: committed 전(아직 호출자에게 어떤 출력도 넘기지 않음)
			// 의 일시적 실패는 그대로 재생할 수 있어 모델 출력이나 도구 실행을 중복하지 않는다. committed 후,
			// ctx 취소, 또는 확정적/할당량 오류면 빠져나가 아래의 기존 전달/장애 조치 로직에 넘긴다.
			for attempt := 0; ; attempt++ {
				committed = false
				pending = nil
				streamErr = nil
				for event, err := range selection.provider.Stream(ctx, req) {
					if err != nil {
						streamErr = err
						break
					}
					if !committed && !streamEventCommitsOutput(event) {
						pending = append(pending, event)
						continue
					}
					if !committed {
						for _, buffered := range pending {
							if !yield(buffered, nil) {
								return
							}
						}
						pending = nil
						committed = true
					}
					if !yield(event, nil) {
						return
					}
				}
				if streamErr != nil && !committed && ctx.Err() == nil &&
					attempt < retries && isRetryableStreamError(streamErr) {
					backoff := backoffOf(attempt)
					log.Printf("[task-llm] task %s 제출 전 스트림 실패, %v 후 같은 provider 로 재시도 (%d/%d): %v",
						taskID, backoff, attempt+1, retries, streamErr)
					if sleepCtx(ctx, backoff) {
						break // 백오프 중에 ctx 취소 → 재시도 중지
					}
					continue
				}
				break
			}
			if streamErr == nil {
				for _, buffered := range pending {
					if !yield(buffered, nil) {
						return
					}
				}
				return
			}
			// profileID=0 means the explicit chain was cleared while this stable task
			// bundle was still in use. Agent/global fallback errors follow the legacy
			// behavior and never mutate task failover state.
			if selection.profileID == 0 || !isQuotaExhaustedError(streamErr) {
				for _, buffered := range pending {
					if !yield(buffered, nil) {
						return
					}
				}
				if committed {
					// Partial output can already have started a tool. This applies to
					// transport/overload errors too, not only quota failover failures.
					yield(llm.StreamEvent{}, &taskLLMError{taskID: taskID, cause: streamErr})
				} else {
					yield(llm.StreamEvent{}, streamErr)
				}
				return
			}
			transition, markErr := hooks.exhaust(selection, streamErr)
			if markErr != nil {
				cause := fmt.Errorf("mark profile quota exhausted after %v: %w", streamErr, markErr)
				if committed {
					// Output may already have driven tool execution. Report the persistence
					// failure, but classify it as router-handled so the worker does not
					// replay the entire intent and duplicate those side effects.
					yield(llm.StreamEvent{}, &taskLLMError{taskID: taskID, cause: cause})
				} else {
					yield(llm.StreamEvent{}, cause)
				}
				return
			}
			if transition.Advanced && !transition.Stale && hooks.transition != nil {
				hooks.transition(selection, transition, streamErr)
			}
			if committed || (!transition.Stale && transition.NextProfileID == nil) {
				yield(llm.StreamEvent{}, &taskLLMError{taskID: taskID, chainExhausted: transition.ChainExhausted, cause: streamErr})
				return
			}
			// No event reached the caller, so replaying the same logical request on
			// the next profile cannot duplicate model output or tool execution.
		}
	}
}

func streamEventCommitsOutput(event llm.StreamEvent) bool {
	switch event.Type {
	case llm.SETextDelta, llm.SEThinkingDelta, llm.SEToolInputJSON:
		return event.Text != ""
	case llm.SEToolUseStart, llm.SEMessageDelta, llm.SEMessageStop:
		return true
	default:
		return false
	}
}

// 제출 전 안전 구간 안에서 같은 provider 에 대한 【기본】 재시도 횟수. SDK 의 doStream 은 연결
// 단계만 재시도한다(200 을 받기 전까지). 스트림이 한 번 시작되면 중간 끊김 / overloaded / 스트림 내 429 같은 일시적 장애가 바로
// model_error 로 올라와 재시도가 0 이다. token 을 하나도 호출자에게 넘기지 않았다면(!committed), 완전히 같은
// 요청을 재생해도 모델 출력이나 도구 부수 효과를 중복하지 않으므로, 여기서 같은 provider 백오프 재시도 계층을 하나 더해
// 이런 흔들림을 의도 전체 재실행 이전에 막는다. LLM 설정의 재시도로 재정의/전역 재시도 정책으로 덮어쓸 수 있다.
const sameProviderStreamRetries = 2

// sameProviderRetryBackoff 는 attempt 번째 재시도 전의 【기본】 백오프(0.5s, 1s…, 상한 4s)로,
// SDK 의 지수 단계와 같은 방식이지만 상한이 더 작아 worker 의 마무리/취소 응답을 붙잡지 않는다.
// 변수 형태로 노출해, 테스트가 백오프를 0 으로 둘 수 있게 한다.
var sameProviderRetryBackoff = func(attempt int) time.Duration {
	return min(500*time.Millisecond*(1<<attempt), 4*time.Second)
}

// sameProviderRetryPolicy 는 이번 호출에 어떤 같은 provider 재시도 파라미터를 쓸지 해석한다: 설정에 횟수가 있으면
// 설정 값을 쓰고(음수 = 이 계층 재시도 끔), 간격이 있으면 지수 백오프를 고정 간격으로 바꾸며, 둘 다 없으면
// 설정 가능하게 바뀌기 전과 바이트 단위로 같다.
func sameProviderRetryPolicy(r agent.RetryConfig) (retries int, backoff func(int) time.Duration) {
	retries, backoff = sameProviderStreamRetries, sameProviderRetryBackoff
	if r.StreamAttempts != 0 {
		retries = max(r.StreamAttempts, 0)
	}
	if r.StreamInterval > 0 {
		d := r.StreamInterval
		backoff = func(int) time.Duration { return d }
	}
	return retries, backoff
}

// isRetryableStreamError 는 「제출 전 스트림 실패」가 같은 provider 에서 재생할 가치가 있는지 판단한다.
// 일시적인 전송 끊김 / 공급자 과부하 / 속도 제한은 스스로 회복하므로 안전하게 재시도할 수 있다. 다음 세 종류는 재시도하지 않는다:
//   - 할당량 소진: profile 장애 조치에 맡기고, 여기서 재시도를 헛되이 태우지 않는다
//   - 컨텍스트 과다: 같은 요청을 재생해도 소용없으므로 harness 의 reactive 압축에 맡긴다
//   - 4xx 확정적 거부(400/401/403/404/422): 어느 provider 로 가도 똑같이 실패한다
func isRetryableStreamError(err error) bool {
	if err == nil {
		return false
	}
	var protocolErr *agent.FlashProtocolError
	if errors.As(err, &protocolErr) {
		return false
	}
	if isQuotaExhaustedError(err) {
		return false
	}
	s := strings.ToLower(err.Error())
	if strings.Contains(s, "too long") || strings.Contains(s, "context length") ||
		strings.Contains(s, "context_length") || strings.Contains(s, "maximum context") ||
		strings.Contains(s, "status 413") {
		return false
	}
	for _, code := range []string{"status 400", "status 401", "status 403", "status 404", "status 422"} {
		if strings.Contains(s, code) {
			return false
		}
	}
	// 나머지(전송 reset/EOF/timeout, 408/429/5xx, anthropic overloaded_error 같은 스트림 내 error 이벤트
	// 등)는 모두 일시적으로 보고 재시도를 허용한다.
	return true
}

// CompactionWindow mirrors current()'s precedence so the context window always
// matches the provider the role will actually stream on.
func (r *taskLLMRuntime) CompactionWindow() int {
	if cfg, linked, err := r.s.personalTaskConfig(r.taskID); linked {
		if err != nil {
			return (agent.Config{}).CompactionWindow()
		}
		return cfg.CompactionWindow()
	}
	if r.pinProfileID != nil {
		if _, cfg, ok := r.s.providerForProfile(*r.pinProfileID); ok {
			return cfg.CompactionWindow()
		}
		return (agent.Config{}).CompactionWindow()
	}
	if _, cfg, ok := r.s.agentBindingProvider(r.agentKey); ok {
		return cfg.CompactionWindow()
	}
	taskNum, err := parseTaskID(r.taskID)
	if err != nil {
		return (agent.Config{}).CompactionWindow()
	}
	chain, err := r.s.m.pg.TaskLLMProfiles(taskNum)
	if err != nil {
		return (agent.Config{}).CompactionWindow()
	}
	if len(chain) == 0 {
		if _, cfg, ok := r.s.globalProvider(); ok {
			return cfg.CompactionWindow()
		}
		return (agent.Config{}).CompactionWindow()
	}
	minimum := 0
	for _, entry := range chain {
		if cfg, ok := r.s.loadProfileConfig(entry.ProfileID); ok {
			window := cfg.CompactionWindow()
			if minimum == 0 || window < minimum {
				minimum = window
			}
		}
	}
	if minimum == 0 {
		return (agent.Config{}).CompactionWindow()
	}
	return minimum
}

// agentBindingProvider resolves the profile a role is explicitly bound to
// (agents.llm_profile_id) — the highest-precedence level for task agents. ok=false
// when the role has no binding or the bound profile no longer builds, so callers
// fall through to the task chain. A bound profile stays exclusive unless
// llm_pool_bind_fallback is on, which is what poolForBinding encodes.
func (s *Server) agentBindingProvider(agentKey string) (llm.Provider, agent.Config, bool) {
	id := s.effectiveProfileForAgent(agentKey, nil)
	if id == nil {
		return nil, agent.Config{}, false
	}
	prov, cfg, ok := s.providerForProfile(*id)
	if !ok {
		return nil, agent.Config{}, false
	}
	return s.poolForBinding(*id, prov, cfg), cfg, true
}

// globalProvider returns the process-wide provider (persisted active profile or
// environment config) — the last resort once a role has neither a binding nor a
// task chain.
func (s *Server) globalProvider() (llm.Provider, agent.Config, bool) {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	if !s.llmOn || s.llmProv == nil {
		return nil, agent.Config{}, false
	}
	return s.llmProv, s.llmCfg, true
}

// taskRuntimeAvailable reports whether every listed role can resolve a provider
// under the runtime precedence in current(): the role's own binding first, then
// the task chain, then global. An exhausted chain is a hard stop for unbound
// roles rather than a silent fall through to global — same as current().
func (s *Server) taskRuntimeAvailable(t *Task, agentKeys ...string) bool {
	if t == nil || len(agentKeys) == 0 {
		return false
	}
	if cfg, linked, err := s.personalTaskConfig(t.ID); linked {
		return err == nil && cfg.APIKey != ""
	}
	state := t.llmStateSnapshot()
	unboundReady := false
	if len(state.ProfileIDs) > 0 {
		if state.ActiveID != nil {
			_, _, unboundReady = s.providerForProfile(*state.ActiveID)
		}
	} else {
		_, _, unboundReady = s.globalProvider()
	}
	for _, key := range agentKeys {
		if _, _, ok := s.agentBindingProvider(key); ok {
			continue
		}
		if !unboundReady {
			return false
		}
	}
	return true
}

func (s *Server) invalidateTaskAgents() {
	s.taskAgentMu.Lock()
	s.taskAgents = map[string]*taskAgentBundle{}
	s.taskAgentMu.Unlock()
}

func (s *Server) agentsForTask(t *Task) *taskAgentBundle {
	s.taskAgentMu.Lock()
	defer s.taskAgentMu.Unlock()
	if bundle := s.taskAgents[t.ID]; bundle != nil {
		return bundle
	}
	goalRuntime := &taskLLMRuntime{s: s, taskID: t.ID, agentKey: "goals"}
	plannerRuntime := &taskLLMRuntime{s: s, taskID: t.ID, agentKey: "planner"}
	workerRuntime := &taskLLMRuntime{s: s, taskID: t.ID, agentKey: "worker"}
	mainRuntime := &taskLLMRuntime{s: s, taskID: t.ID, agentKey: "mainagent"}
	tx := transcript.NewStore(filepath.Join(s.m.dir, "transcripts"))
	window := workerRuntime.CompactionWindow()
	wk := agent.NewWorker(workerRuntime, "task-router", s.m.dir, tx, window, s.agentMaxTurns("worker"))
	wk.SetFindingRecorder(s.evidenceStore())
	wk.SetCompactionWindowResolver(workerRuntime.CompactionWindow)
	wk.SetNonStreaming(workerRuntime.nonStreaming) // 작업의 현재 활성 profile 의 스트리밍 스위치에 따름(매 턴 읽음)
	wk.SetMaxTokens(workerRuntime.maxTokens)       // 위와 같음, 출력 상한도 현재 활성 profile 을 따름
	wk.SetNoaEnabled(s.m.NoaCompactionEnabled)     // 실험 기능: noa 컨텍스트 압축(플랫폼 단위 스위치, run 마다 읽음)
	wk.SetRunTimeout(time.Duration(s.agentRunSeconds("worker")) * time.Second)
	wk.SetProxy(s.m.ProxyAddr(), s.m.ProxyCACert())
	wk.SetWebSearch(s.webSearchFor("worker"))
	wk.SetConstraintInject(s.constraintInjectWorker) // 작업 제약을 worker 에 주입(설정 가능, 기본 켜짐)
	pl := agent.NewPlanner(plannerRuntime, "task-router", s.m.dir, tx, plannerRuntime.CompactionWindow(), s.agentMaxTurns("planner"))
	pl.SetFindingRecorder(s.evidenceStore())
	pl.SetCompactionWindowResolver(plannerRuntime.CompactionWindow)
	pl.SetNonStreaming(plannerRuntime.nonStreaming)
	pl.SetMaxTokens(plannerRuntime.maxTokens)
	pl.SetNoaEnabled(s.m.NoaCompactionEnabled) // 실험 기능: noa 컨텍스트 압축(플랫폼 단위 스위치, run 마다 읽음)
	pl.SetKillWork(s.engine.KillWork)
	pl.SetSteerWork(s.engine.SteerWork)
	pl.SetProxy(s.m.ProxyAddr(), s.m.ProxyCACert())
	pl.SetWebSearch(s.webSearchFor("planner"))
	pl.SetConstraintInject(s.constraintInjectPlanner) // 작업 제약을 planner 에 주입(설정 가능, 기본 켜짐)
	// cold-digest §7: 차가운 노드 백그라운드 압축. 엔진이 권위 있는 해석기를 통해 실제로 구동하는 것이 바로 이 per-task planner
	// (agentsForTask)이므로 Compactor 를 여기에 연결해야 한다. 작업 라우팅을 타는 planner provider(§4: agent 와
	// 같은 모델이며 작업 LLM 체인을 따라 해석됨)를 쓰며, 압축은 Complete 로 body 를 한 번에 생성한다.
	pl.SetCompactor(agent.NewCompactor(plannerRuntime, "task-router"))
	main := agent.NewMainAgent(mainRuntime, "task-router", s.m.dir, tx, mainRuntime.CompactionWindow(), s.agentMaxTurns("mainagent"))
	main.SetFindingRecorder(s.evidenceStore())
	main.SetCompactionWindowResolver(mainRuntime.CompactionWindow)
	main.SetNonStreaming(mainRuntime.nonStreaming)
	main.SetMaxTokens(mainRuntime.maxTokens)
	main.SetNoaEnabled(s.m.NoaCompactionEnabled) // 실험 기능: noa 컨텍스트 압축(플랫폼 단위 스위치, run 마다 읽음)
	main.SetProxy(s.m.ProxyAddr(), s.m.ProxyCACert())
	main.SetWebSearch(s.webSearchFor("mainagent"))
	main.SetSteerWork(s.engine.SteerWork) // steer_work: 사람이 실행 중인 work 를 실시간으로 교정
	bundle := &taskAgentBundle{
		runtime: goalRuntime, plannerRuntime: plannerRuntime, workerRuntime: workerRuntime,
		mainRuntime: mainRuntime, pl: pl, wk: wk, main: main, pinnedWorkers: map[int64]*agent.Worker{},
	}
	s.taskAgents[t.ID] = bundle
	return bundle
}

func (s *Server) workerForTaskSlot(t *Task, slot WorkerSlot) (*agent.Worker, error) {
	if slot.ProfileID <= 0 {
		return nil, errors.New("worker slot is not pinned")
	}
	_, cfg, ok := s.providerForProfile(slot.ProfileID)
	if !ok {
		return nil, fmt.Errorf("LLM profile #%d is missing or invalid", slot.ProfileID)
	}
	s.taskAgentMu.Lock()
	bundle := s.taskAgents[t.ID]
	s.taskAgentMu.Unlock()
	if bundle == nil {
		bundle = s.agentsForTask(t)
	}
	s.taskAgentMu.Lock()
	defer s.taskAgentMu.Unlock()
	if current := s.taskAgents[t.ID]; current != nil {
		bundle = current
	}
	if bundle.pinnedWorkers == nil {
		bundle.pinnedWorkers = make(map[int64]*agent.Worker)
	}
	if worker := bundle.pinnedWorkers[slot.ProfileID]; worker != nil {
		return worker, nil
	}
	id := slot.ProfileID
	runtime := &taskLLMRuntime{s: s, taskID: t.ID, agentKey: "worker", pinProfileID: &id}
	tx := transcript.NewStore(filepath.Join(s.m.dir, "transcripts"))
	worker := agent.NewWorker(runtime, cfg.Model, s.m.dir, tx, runtime.CompactionWindow(), s.agentMaxTurns("worker"))
	worker.SetFindingRecorder(s.evidenceStore())
	worker.SetCompactionWindowResolver(runtime.CompactionWindow)
	worker.SetNonStreaming(runtime.nonStreaming)
	worker.SetMaxTokens(runtime.maxTokens)
	worker.SetNoaEnabled(s.m.NoaCompactionEnabled)
	worker.SetRunTimeout(time.Duration(s.agentRunSeconds("worker")) * time.Second)
	worker.SetProxy(s.m.ProxyAddr(), s.m.ProxyCACert())
	worker.SetWebSearch(s.webSearchFor("worker"))
	worker.SetConstraintInject(s.constraintInjectWorker)
	bundle.pinnedWorkers[id] = worker
	return worker, nil
}

func (s *Server) syncTaskLLMState(pt *db.Task) {
	if pt == nil {
		return
	}
	id := fmt.Sprintf("%d", pt.ID)
	s.m.mu.Lock()
	if task := s.m.tasks[id]; task != nil {
		task.setLLMState(pt.LLMProfileID, pt.ActiveLLMProfileID, pt.LLMProfileIDs, pt.LLMChainRevision, pt.LLMFailoverState, pt.LLMFailoverReason)
	}
	s.m.mu.Unlock()
}

func (s *Server) emitTaskLLMTransition(t *Task, transition db.TaskLLMTransition, cause error) {
	if t == nil {
		return
	}
	previous := s.llmAuditProfile(transition.PreviousProfileID)
	var next *llmAuditProfile
	if transition.NextProfileID != nil {
		next = s.llmAuditProfile(*transition.NextProfileID)
	}
	mode := "automatic"
	kind := "llm_switch"
	summary := fmt.Sprintf("%s 할당량 부족", llmAuditProfileLabel(previous))
	if transition.NextProfileID != nil {
		summary += fmt.Sprintf(", 이후 호출을 %s 로 전환함", llmAuditProfileLabel(next))
	} else {
		mode = "exhausted"
		kind = "llm_failover"
		summary += ", 설정 체인이 소진됨"
	}
	metadata, _ := json.Marshal(llmActivityMetadata{LLMTransition: llmTransitionAudit{
		Mode: mode, Reason: cause.Error(), Previous: previous, Next: next,
	}})
	s.engine.emitActivity(t, db.Activity{Worker: "system", Kind: kind, IsError: transition.ChainExhausted, Summary: summary, Detail: cause.Error(), Metadata: metadata})
	log.Printf("[llm-failover] task %s: %s", t.ID, summary)
}

func (s *Server) llmAuditProfile(id int64) *llmAuditProfile {
	if id <= 0 || s.m == nil || s.m.pg == nil {
		return nil
	}
	p, err := s.m.pg.ProfileByID(id)
	if err != nil || p == nil {
		return &llmAuditProfile{ID: id, Name: fmt.Sprintf("설정 #%d", id)}
	}
	return &llmAuditProfile{ID: p.ID, Name: p.Name, Format: p.Format, Model: p.Model}
}

func llmAuditProfileLabel(profile *llmAuditProfile) string {
	if profile == nil {
		return "기본 설정"
	}
	name := profile.Name
	if name == "" {
		name = fmt.Sprintf("설정 #%d", profile.ID)
	}
	detail := []string{}
	if profile.Format != "" {
		detail = append(detail, profile.Format)
	}
	if profile.Model != "" {
		detail = append(detail, profile.Model)
	}
	if len(detail) == 0 {
		return name
	}
	return fmt.Sprintf("%s（%s）", name, strings.Join(detail, " / "))
}

func sameOptionalID(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func (s *Server) emitManualTaskLLMSwitch(t *Task, previousID, nextID *int64) db.Activity {
	previous := (*llmAuditProfile)(nil)
	next := (*llmAuditProfile)(nil)
	if previousID != nil {
		previous = s.llmAuditProfile(*previousID)
	}
	if nextID != nil {
		next = s.llmAuditProfile(*nextID)
	}
	summary := fmt.Sprintf("사용자가 작업 LLM 을 %s 에서 %s 로 수동 전환함", llmAuditProfileLabel(previous), llmAuditProfileLabel(next))
	metadata, _ := json.Marshal(llmActivityMetadata{LLMTransition: llmTransitionAudit{
		Mode: "manual", Reason: "사용자가 작업 LLM 을 수동 전환", Previous: previous, Next: next,
	}})
	return s.engine.emitActivity(t, db.Activity{Worker: "system", Kind: "llm_switch", Summary: summary, Detail: summary, Metadata: metadata})
}
