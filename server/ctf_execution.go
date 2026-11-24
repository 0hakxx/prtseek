package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	actool "github.com/Autumn-27/norma/tool"
)

// Keep a finite shared budget, but allow multi-hour installations to opt in to
// a larger limit without editing source. Invalid values retain the safe default.
func ctfModelCallLimit() int {
	n, err := strconv.Atoi(os.Getenv("prtseek_CTF_MODEL_CALL_LIMIT"))
	if err == nil && n >= 1 && n <= 100000 {
		return n
	}
	return ctfMaxModelCalls
}

const (
	ctfMaxActive     = 8
	ctfMaxJobs       = 24
	ctfMaxDepth      = 4
	ctfMaxModelCalls = 200
)

type ctfWorkflowKey struct{}
type ctfDepthKey struct{}
type ctfWorkflowRun struct {
	mu            sync.Mutex
	ctx           context.Context
	cancel        context.CancelFunc
	rootID        int64
	budget        *agent.RunBudget
	jobs          map[int64]*ctfJob
	active, total int
	closed        bool
	changed       chan struct{}
	all           sync.WaitGroup
}
type ctfJob struct {
	id, parentID                int64
	key, state, result, failure string
	done                        chan struct{}
	stop                        context.CancelCauseFunc
	delivered                   bool
}
type ctfJobView struct {
	ID       int64      `json:"conversation_id"`
	AgentKey string     `json:"agent_key"`
	State    string     `json:"state"`
	Result   string     `json:"result,omitempty"`
	Error    string     `json:"error,omitempty"`
	Outcome  ctfOutcome `json:"outcome"`
}

func (s *Server) initCTFExecution() error {
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS conversation_executions (conversation_id BIGINT PRIMARY KEY REFERENCES conversations(id) ON DELETE CASCADE, root_id BIGINT NOT NULL, state TEXT NOT NULL, started_ms BIGINT NOT NULL, finished_ms BIGINT NOT NULL DEFAULT 0, model_calls BIGINT NOT NULL DEFAULT 0)`,
		`CREATE TABLE IF NOT EXISTS conversation_outcomes (conversation_id BIGINT PRIMARY KEY REFERENCES conversations(id) ON DELETE CASCADE, report_json TEXT NOT NULL)`,
		`UPDATE conversation_executions SET state='interrupted', finished_ms=CAST(strftime('%s','now') AS INTEGER)*1000 WHERE state='running'`,
		`UPDATE conversation_turns SET state='interrupted',reason='service_restart',finished_ms=CAST(strftime('%s','now') AS INTEGER)*1000 WHERE state='running'`,
	} {
		if _, err := s.m.pg.Exec(statement); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) beginCTFWorkflow(ctx context.Context, c *db.Conversation) (context.Context, *ctfWorkflowRun) {
	if existing, ok := ctx.Value(ctfWorkflowKey{}).(*ctfWorkflowRun); ok {
		return ctx, existing
	}
	runCtx, cancel := context.WithCancel(ctx)
	w := &ctfWorkflowRun{ctx: runCtx, cancel: cancel, rootID: c.ID, budget: &agent.RunBudget{Limit: int64(ctfModelCallLimit())}, jobs: map[int64]*ctfJob{}, changed: make(chan struct{})}
	w.budget.OnCall = func(count int64) {
		_, _ = s.m.pg.Exec(`UPDATE conversation_executions SET model_calls=MAX(model_calls,$2) WHERE conversation_id=$1`, c.ID, count)
	}
	runCtx = context.WithValue(runCtx, ctfWorkflowKey{}, w)
	runCtx = agent.WithRunBudget(runCtx, w.budget)
	w.ctx = runCtx
	s.workflowMu.Lock()
	if s.workflowRuns == nil {
		s.workflowRuns = map[int64]*ctfWorkflowRun{}
	}
	s.workflowRuns[c.ID] = w
	s.workflowMu.Unlock()
	return runCtx, w
}

func (s *Server) closeCTFWorkflow(w *ctfWorkflowRun) {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		w.all.Wait()
		return
	}
	w.closed = true
	w.cancel()
	w.mu.Unlock()
	w.all.Wait() // a new user turn cannot race an old child's transcript writer
	_, _ = s.m.pg.Exec(`UPDATE conversation_executions SET model_calls=$2 WHERE conversation_id=$1`, w.rootID, w.budget.Calls())
	s.workflowMu.Lock()
	delete(s.workflowRuns, w.rootID)
	s.workflowMu.Unlock()
}

func (s *Server) startCTFExecution(id, root int64) error {
	if _, err := s.m.pg.Exec(`INSERT INTO conversation_executions(conversation_id,root_id,state,started_ms) VALUES($1,$2,'running',$3)
	 ON CONFLICT(conversation_id) DO UPDATE SET root_id=excluded.root_id,state='running',started_ms=excluded.started_ms,finished_ms=0,model_calls=0`, id, root, time.Now().UnixMilli()); err != nil {
		return err
	}
	_, err := s.m.pg.Exec(`DELETE FROM conversation_outcomes WHERE conversation_id=$1`, id)
	return err
}
func (s *Server) finishCTFExecution(id int64, state string) {
	_, _ = s.m.pg.Exec(`UPDATE conversation_executions SET state=$2,finished_ms=$3 WHERE conversation_id=$1`, id, state, time.Now().UnixMilli())
}

func (w *ctfWorkflowRun) signalLocked() { close(w.changed); w.changed = make(chan struct{}) }
func (w *ctfWorkflowRun) completionCheck(parentID int64) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var ids []int64
	for _, job := range w.jobs {
		if job.parentID == parentID && (job.state == "running" || !job.delivered) {
			ids = append(ids, job.id)
		}
	}
	if len(ids) == 0 {
		return ""
	}
	return fmt.Sprintf("하위 작업 %v 가 아직 실행 중이거나 결과를 회수하지 않았습니다. wait_agents 로 기다려 실제 결과를 읽으세요. 더 이상 필요 없는 분기는 cancel_agent 로 중지한 뒤 결과를 회수하세요. 결과를 넘기기 전에 실행 중인 하위 작업을 남겨서는 안 됩니다.", ids)
}

func (s *Server) conversationTools(parent *db.Conversation) func(context.Context, string) []actool.CoreTool {
	return func(ownerCtx context.Context, key string) []actool.CoreTool {
		if !isDefaultCTFAgent(key) {
			return nil
		}
		tools := []actool.CoreTool{
			wrTool("delegate_agent", "명확한 CTF 하위 작업 하나를 동기적으로 실행하고 결과를 반환한다. 쉬운 문제는 solver 를 하나만 분배하는 것을 우선한다.", objSchema(map[string]any{"agent_key": strParam("대상 역할"), "task": strParam("자료, 목표, 경계, 검수 조건")}, "agent_key", "task"), func(ctx context.Context, raw json.RawMessage) (actool.Result, error) {
				var req struct {
					AgentKey string `json:"agent_key"`
					Task     string `json:"task"`
				}
				if json.Unmarshal(raw, &req) != nil {
					return actool.Errorf("invalid delegation"), nil
				}
				return s.delegateConversation(ctx, parent, req.AgentKey, req.Task)
			}),
			s.ctfOutcomeTool(parent),
		}
		if key == "command" {
			tools = append(tools, s.parallelCTFDelegationTool(parent, ownerCtx))
		}
		if _, ok := ownerCtx.Value(ctfWorkflowKey{}).(*ctfWorkflowRun); ok {
			tools = append(tools,
				wrTool("wait_agents", "이번 턴의 직속 하위 작업을 기다린다. 새 결과 중 하나가 도착하면 바로 반환한다. 결과에는 독립된 실행 상태와 목표 상태가 들어 있다. ids 가 비어 있으면 이번 턴의 모든 직속 하위 작업을 뜻한다.", objSchema(map[string]any{"ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}}, "timeout_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 30}}), func(ctx context.Context, raw json.RawMessage) (actool.Result, error) {
					var req struct {
						IDs     []int64 `json:"ids"`
						Timeout int     `json:"timeout_seconds"`
					}
					if json.Unmarshal(raw, &req) != nil {
						return actool.Errorf("invalid wait"), nil
					}
					if req.Timeout == 0 {
						req.Timeout = 20
					}
					if req.Timeout < 1 || req.Timeout > 30 {
						return actool.Errorf("timeout_seconds must be 1..30"), nil
					}
					return s.waitCTFAgents(ctx, parent.ID, req.IDs, time.Duration(req.Timeout)*time.Second)
				}),
				wrTool("cancel_agent", "이번 턴의 직속 하위 작업 하나와 그 자손을 중지한다. 이어서 wait_agents 로 최종 상태를 회수한다.", objSchema(map[string]any{"id": map[string]any{"type": "integer"}}, "id"), func(ctx context.Context, raw json.RawMessage) (actool.Result, error) {
					var req struct {
						ID int64 `json:"id"`
					}
					if json.Unmarshal(raw, &req) != nil {
						return actool.Errorf("invalid cancellation"), nil
					}
					w := ownerCtx.Value(ctfWorkflowKey{}).(*ctfWorkflowRun)
					w.mu.Lock()
					job := w.jobs[req.ID]
					if job == nil || job.parentID != parent.ID {
						w.mu.Unlock()
						return actool.Errorf("not a child of this conversation"), nil
					}
					job.stop(agent.AbortChatStoppedByUser)
					w.mu.Unlock()
					return jsonResult(map[string]any{"conversation_id": req.ID, "status": "stopping"})
				}),
			)
		}
		return tools
	}
}

func (s *Server) parallelCTFDelegationTool(parent *db.Conversation, owner ...context.Context) actool.CoreTool {
	taskSchema := objSchema(map[string]any{"agent_key": strParam("solver"), "task": strParam("서로 보완하는 방향, 자료, 목표, 검수 기준")}, "agent_key", "task")
	return wrTool("delegate_agents", "서로 보완하는 Solver 를 1~6 개 시작하고 대화 번호를 즉시 반환한다. wait_agents 로 진행 상황을 하나씩 회수한다. 쉬운 문제는 하나만 시작한다.", objSchema(map[string]any{"tasks": map[string]any{"type": "array", "minItems": 1, "maxItems": 6, "items": taskSchema}}, "tasks"), func(ctx context.Context, raw json.RawMessage) (actool.Result, error) {
		var req struct {
			Tasks []struct {
				AgentKey string `json:"agent_key"`
				Task     string `json:"task"`
			} `json:"tasks"`
		}
		if json.Unmarshal(raw, &req) != nil || len(req.Tasks) < 1 || len(req.Tasks) > 6 {
			return actool.Errorf("tasks must contain 1 to 6 items"), nil
		}
		w, ok := ctx.Value(ctfWorkflowKey{}).(*ctfWorkflowRun)
		if !ok {
			return actool.Errorf("workflow runtime unavailable"), nil
		}
		seen := map[string]bool{}
		for _, task := range req.Tasks {
			q := strings.TrimSpace(task.Task)
			if task.AgentKey != "solver" || q == "" || len(q) > 64000 || seen[q] {
				return actool.Errorf("tasks must be distinct solver assignments, 1..64000 bytes each"), nil
			}
			seen[q] = true
		}
		w.mu.Lock()
		capacity := w.active+len(req.Tasks) <= ctfMaxActive && w.total+len(req.Tasks) <= ctfMaxJobs
		w.mu.Unlock()
		if !capacity {
			return actool.Errorf(fmt.Sprintf("이번 턴의 하위 작업 용량이 부족합니다. 먼저 기존 작업을 기다리세요. 총 작업 상한 %d, 동시 실행 상한 %d", ctfMaxJobs, ctfMaxActive)), nil
		}
		var jobs []ctfJobView
		childContext := ctx
		if len(owner) > 0 {
			childContext = owner[0]
		}
		for _, task := range req.Tasks {
			job, err := s.startCTFChild(childContext, parent, task.AgentKey, task.Task)
			if err != nil {
				return jsonResult(map[string]any{"jobs": jobs, "error": err.Error()})
			}
			jobs = append(jobs, ctfJobView{ID: job.id, AgentKey: job.key, State: "running"})
		}
		return jsonResult(map[string]any{"jobs": jobs, "next": "wait_agents"})
	})
}

func (s *Server) delegateConversation(ctx context.Context, parent *db.Conversation, key, task string) (actool.Result, error) {
	_, exists := ctx.Value(ctfWorkflowKey{}).(*ctfWorkflowRun)
	ctx, w := s.beginCTFWorkflow(ctx, parent)
	if !exists {
		defer s.closeCTFWorkflow(w)
	}
	job, err := s.startCTFChild(ctx, parent, key, task)
	if err != nil {
		return actool.Errorf(err.Error()), nil
	}
	<-job.done
	w.mu.Lock()
	job.delivered = true
	view := ctfJobView{ID: job.id, AgentKey: job.key, State: job.state, Result: job.result, Error: job.failure}
	w.mu.Unlock()
	view.Outcome = s.readCTFOutcome(job.id)
	result, err := jsonResult(view)
	result.IsError = view.State == "failed" || view.State == "timed_out"
	return result, err
}

func (s *Server) startCTFChild(ctx context.Context, parent *db.Conversation, key, task string) (*ctfJob, error) {
	w, ok := ctx.Value(ctfWorkflowKey{}).(*ctfWorkflowRun)
	if !ok {
		return nil, fmt.Errorf("workflow runtime unavailable")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	depth, _ := ctx.Value(ctfDepthKey{}).(int)
	if depth >= ctfMaxDepth {
		return nil, fmt.Errorf("분배 깊이가 %d 에 도달했습니다. 바로 분석하거나 기존 증거를 반환하세요", ctfMaxDepth)
	}
	if key == parent.AgentKey || !isDefaultCTFAgent(parent.AgentKey) || !isDefaultCTFAgent(key) {
		return nil, fmt.Errorf("자기 자신이나 기본 CTF 역할이 아닌 대상에는 분배할 수 없습니다")
	}
	if strings.TrimSpace(task) == "" || len(task) > 64000 {
		return nil, fmt.Errorf("task is empty or too long")
	}
	a, err := s.m.pg.GetAgentByKey(key)
	if err != nil || a == nil || !a.Enabled || s.m.pg.AgentArchived(key) {
		return nil, fmt.Errorf("target agent unavailable")
	}
	_, profile, ok := s.personalProfileConfig(parent)
	if !ok {
		return nil, fmt.Errorf("parent model unavailable")
	}
	w.mu.Lock()
	if w.closed || w.ctx.Err() != nil {
		w.mu.Unlock()
		return nil, fmt.Errorf("workflow cancelled")
	}
	if w.active >= ctfMaxActive || w.total >= ctfMaxJobs {
		w.mu.Unlock()
		return nil, fmt.Errorf("이번 턴 분배가 상한에 도달했습니다: 동시 %d, 총계 %d. 먼저 기존 작업을 기다리세요", ctfMaxActive, ctfMaxJobs)
	}
	w.active++
	w.total++
	w.all.Add(1)
	w.mu.Unlock()
	started := false
	defer func() {
		if !started {
			w.mu.Lock()
			w.active--
			w.total--
			w.mu.Unlock()
			w.all.Done()
		}
	}()
	var pin *int64
	if profile > 0 {
		pin = &profile
	}
	child, err := s.m.pg.CreateConversation(key, firstLine(task, 80), pin, s.personalConversationSettings(parent.ID))
	if err != nil {
		return nil, err
	}
	defer func() {
		if !started {
			_, _ = s.m.pg.Exec(`UPDATE conversation_delegations SET state='failed' WHERE child_id=$1`, child.ID)
			s.finishCTFExecution(child.ID, "failed")
		}
	}()
	if _, err = s.m.pg.Exec(`INSERT INTO conversation_delegations(parent_id,child_id) VALUES($1,$2)`, parent.ID, child.ID); err != nil {
		return nil, err
	}
	childTask := task + "\n\n부모 대화 자료 디렉터리(읽기 전용 참조, 산출물은 자신의 작업 디렉터리에 쓸 것): " + s.conversationWorkDir(parent.ID)
	childTask += "\n원본 문제 디렉터리: " + s.conversationWorkDir(w.rootID) + "\n먼저 실제 파일을 확인하세요. 끝내기 전에 report_outcome 으로 목표 상태, 검수 조건, 증거를 반환하세요. 정상적으로 답변을 마쳤다고 목표가 완료된 것은 아닙니다."
	ca := s.personalChatAgent(child)
	if ca == nil {
		_, _ = s.m.pg.Exec(`UPDATE conversation_delegations SET state='failed' WHERE child_id=$1`, child.ID)
		return nil, fmt.Errorf("child model unavailable")
	}
	budget := time.Duration(a.RunSecs) * time.Second
	runCtx, timeoutCancel := context.WithCancel(ctx)
	if budget > 0 {
		timeoutCancel()
		runCtx, timeoutCancel = context.WithTimeout(ctx, budget)
	}
	childCtx, stop := context.WithCancelCause(intercept.WithConvID(runCtx, child.ID))
	childCtx = context.WithValue(childCtx, ctfDepthKey{}, depth+1)
	childCtx = agent.WithResponseLanguage(childCtx, s.personalConversationSettings(parent.ID).ResponseLanguage)
	job := &ctfJob{id: child.ID, parentID: parent.ID, key: key, state: "running", done: make(chan struct{}), stop: stop}
	if err = s.startCTFExecution(child.ID, w.rootID); err != nil {
		timeoutCancel()
		stop(err)
		return nil, err
	}
	_, _ = s.m.pg.AppendConvActivity(child.ID, db.Activity{Worker: key, Kind: "user", Summary: firstLine(task, 200), Detail: childTask})
	_, _ = s.m.pg.AppendConvActivity(parent.ID, db.Activity{Worker: parent.AgentKey, Kind: "text", Summary: fmt.Sprintf("%s 에 분배함 · 대화 #%d", key, child.ID)})
	busyKey := s.convBusyKey(child.ID)
	s.chatMu.Lock()
	s.chatBusy[busyKey] = true
	s.chatCancel[busyKey] = stop
	s.chatMu.Unlock()
	w.mu.Lock()
	w.jobs[child.ID] = job
	w.signalLocked()
	w.mu.Unlock()
	started = true
	go func() {
		defer w.all.Done()
		defer timeoutCancel()
		defer stop(nil)
		ca = ca.WithExtraTools(s.conversationTools(child)).WithCompletionCheck(func() string { return w.completionCheck(child.ID) })
		result, callErr := s.runMeteredChat(childCtx, ca, child, busyKey, childTask, a.MaxTurns, budget, a.WebSearch, func(rec db.Activity) { _, _ = s.m.pg.AppendConvActivity(child.ID, rec) })
		// Drain descendants before releasing the conversation's busy slot.
		stop(agent.AbortChatTurnFinished)
		w.mu.Lock()
		var descendants []*ctfJob
		for _, other := range w.jobs {
			if other.parentID == child.ID {
				descendants = append(descendants, other)
			}
		}
		w.mu.Unlock()
		for _, other := range descendants {
			<-other.done
		}
		state, failure := "completed", ""
		if callErr != nil {
			state, failure = "failed", callErr.Error()
			var incomplete *agent.ResearchIncompleteError
			if errors.As(callErr, &incomplete) {
				state = incomplete.Status
			}
		}
		// Read the inherited/timeout context; our own cleanup cancellation above
		// must not turn an otherwise completed result into a cancellation.
		if runCtx.Err() != nil {
			state, failure = "cancelled", runCtx.Err().Error()
		}
		if cause := context.Cause(childCtx); cause != nil && cause != agent.AbortChatTurnFinished {
			state, failure = "cancelled", cause.Error()
		}
		if runCtx.Err() == context.DeadlineExceeded {
			state = "timed_out"
		}
		s.finishCTFExecution(child.ID, state)
		_, _ = s.m.pg.Exec(`UPDATE conversation_delegations SET state=$2 WHERE child_id=$1`, child.ID, state)
		s.chatMu.Lock()
		delete(s.chatBusy, busyKey)
		delete(s.chatCancel, busyKey)
		s.chatMu.Unlock()
		w.mu.Lock()
		job.state, job.result, job.failure = state, result, failure
		w.active--
		close(job.done)
		w.signalLocked()
		w.mu.Unlock()
	}()
	return job, nil
}

func (s *Server) waitCTFAgents(ctx context.Context, parentID int64, ids []int64, timeout time.Duration) (actool.Result, error) {
	w, ok := ctx.Value(ctfWorkflowKey{}).(*ctfWorkflowRun)
	if !ok {
		return actool.Errorf("workflow runtime unavailable"), nil
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	expired := false
	for {
		w.mu.Lock()
		selected := ids
		if len(selected) == 0 {
			for id, j := range w.jobs {
				if j.parentID == parentID {
					selected = append(selected, id)
				}
			}
		}
		ready, active := false, false
		for _, id := range selected {
			job := w.jobs[id]
			if job == nil || job.parentID != parentID {
				w.mu.Unlock()
				return actool.Errorf(fmt.Sprintf("unknown child conversation %d", id)), nil
			}
			if job.state == "running" {
				active = true
			} else if !job.delivered {
				ready = true
			}
		}
		if ready || !active || expired {
			views := make([]ctfJobView, 0, len(selected))
			for _, id := range selected {
				job := w.jobs[id]
				if job.state != "running" {
					job.delivered = true
				}
				views = append(views, ctfJobView{ID: id, AgentKey: job.key, State: job.state, Result: job.result, Error: job.failure})
			}
			w.mu.Unlock()
			for i := range views {
				views[i].Outcome = s.readCTFOutcome(views[i].ID)
			}
			return jsonResult(map[string]any{"jobs": views, "pending": active})
		}
		changed := w.changed
		w.mu.Unlock()
		select {
		case <-ctx.Done():
			return actool.Errorf("wait cancelled"), nil
		case <-timer.C:
			expired = true
		case <-changed:
		}
	}
}
