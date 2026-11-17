package agent

// A model's end_turn is a checkpoint, not proof that a research goal is done.
// This controller owns the run budget and the explicit, evidence-linked exit.
import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

const ctfResearchPrompt = `竞赛收敛与持续研究协议：目标未达到时，一次模型文字结尾只是阶段检查点；目标已被可靠工具结果满足时，立即给出flag/答案和最短步骤，禁止为凑流程继续复核、派生角色或落盘。report_outcome(verified)通过宿主证据检查后就完成，不需要再research_status确认；没有report_outcome工具时才用research_status(completed)。证据完整可回查即可，observed不必升级为reproduced或accepted；不要求为同一题的续跑重复生成已有证据。不要把子任务局部完成当成整题完成，不把示例/猜测当真实flag。
工具原始证据由宿主自动保存；research_record只在重要决策、可复用失败路线或长任务阶段转换时简短更新，不为每个工具输出再写一遍。难题继续选择有信息增益的实验，用互补子任务并行推进，压缩后按现有证据与失败路线继续，不从头重复。blocked只用于外部缺失材料/环境/权限并给恢复条件；未拿到flag本身不是外部阻塞。用户停止、实际预算和授权边界始终生效。`

// ResearchIncompleteError prevents callers from labelling a budget/stall/block as success.
type ResearchIncompleteError struct{ Status, Detail string }

func (e *ResearchIncompleteError) Error() string {
	return "研究未完成（" + e.Status + "）：" + e.Detail
}

type CTFResearchState struct {
	Status        string     `json:"status"`
	Objective     string     `json:"objective"`
	Summary       string     `json:"summary,omitempty"`
	EvidenceIDs   []string   `json:"evidence_ids,omitempty"`
	Blocker       string     `json:"blocker,omitempty"`
	ResumeWhen    string     `json:"resume_when,omitempty"`
	StartedAt     time.Time  `json:"started_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	Requests      int        `json:"requests"`
	Continuations int        `json:"continuations"`
	MaxRequests   int        `json:"max_requests"`
	Deadline      *time.Time `json:"deadline,omitempty"`
}

// RunState exposes the host checkpoint without accepting a caller-supplied path.
func (m *ResearchMemory) RunState() (*CTFResearchState, error) {
	p, err := m.safeResearchPath(".research/run.json", false)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 16001))
	if err != nil {
		return nil, err
	}
	var s CTFResearchState
	if len(b) > 16000 || json.Unmarshal(b, &s) != nil || researchContainsSecret(string(b)) {
		return nil, errors.New("research run checkpoint is invalid")
	}
	return &s, nil
}

type ctfResearchRun struct {
	mu                          sync.Mutex
	memory                      *ResearchMemory
	state                       CTFResearchState
	startEvidence               uint64
	novel, lastNovel, idleStops int
	now                         func() time.Time
	completionCheck             func() string
	verifiedCompletion          func() (string, bool)
}

// The server validates report_outcome once. Reuse that decision instead of
// demanding a second model round and another independent evidence journal.
func (r *ctfResearchRun) acceptVerifiedCompletion() (string, bool) {
	if r.verifiedCompletion == nil {
		return "", false
	}
	summary, ok := r.verifiedCompletion()
	if !ok || strings.TrimSpace(summary) == "" {
		return "", false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state.Status == "completed" {
		return r.state.Summary, true
	}
	r.state.Status, r.state.Summary = "completed", researchClip(RedactResearchText(summary), 4000)
	// A local checkpoint failure must not turn already validated success into
	// more remote work. The authoritative outcome is independently saved by host.
	if err := r.persist(); err != nil {
		r.state.Summary += "\n（目标已验证；运行检查点保存失败：" + err.Error() + "）"
	}
	return r.state.Summary, true
}

func attachCTFResearch(ctx context.Context, opts *agentcore.Options, role, task string, maxRequests int) (*ctfResearchRun, error) {
	if !IsCTFSolveRole(role) {
		return nil, nil
	}
	rt, ok := opts.Hooks.(*researchRuntime)
	if !ok {
		return nil, errors.New("CTF research memory unavailable; existing evidence preserved")
	}
	state, err := rt.memory.Snapshot()
	if err != nil {
		return nil, err
	}
	if state.Objective == "" {
		objective := researchClip(RedactResearchText(task), 2800)
		if _, err := rt.memory.Record(ctx, ResearchRecordInput{Objective: &objective}); err != nil {
			return nil, err
		}
	}
	r := &ctfResearchRun{memory: rt.memory, startEvidence: state.NextID, now: time.Now,
		state: CTFResearchState{Status: "active", Objective: researchClip(RedactResearchText(task), 3000), StartedAt: time.Now().UTC(), MaxRequests: maxRequests}}
	if deadline, ok := ctx.Deadline(); ok {
		r.state.Deadline = &deadline
	}
	if err := r.persist(); err != nil {
		return nil, err
	}
	rt.supervisor = r
	opts.Tools = append(opts.Tools, r.statusTool())
	// Replace the startup snapshot with a request-time snapshot, not two
	// contradictory copies of old and new plans in the system prompt.
	if n := len(opts.SystemPrompt); n > 0 && strings.HasPrefix(opts.SystemPrompt[n-1], "\n研究记忆根目录：") {
		opts.SystemPrompt = opts.SystemPrompt[:n-1]
	}
	opts.SystemPrompt = append(opts.SystemPrompt, ctfResearchPrompt)
	// Count all main model requests across continuations. Summary-only requests
	// are separate and don't reset or consume the configured research round cap.
	opts.MaxTurns, opts.MaxDuration, opts.Settlement = 0, 0, nil
	opts.Provider = &researchContextProvider{Provider: opts.Provider, memory: rt.memory, run: r}
	return r, nil
}

// persist uses the same contained path and cross-process lock as the evidence
// journal. A restart can inspect the checkpoint; it never replays side effects.
func (r *ctfResearchRun) persist() error {
	unlock, err := r.memory.lock(context.Background())
	if err != nil {
		return err
	}
	defer unlock()
	r.state.UpdatedAt = r.now().UTC()
	b, err := json.MarshalIndent(r.state, "", "  ")
	if err != nil {
		return err
	}
	p, err := r.memory.safeResearchPath(".research/run.json", false)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(r.memory.dir, ".run-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	for attempt := 0; attempt < 12; attempt++ {
		err = os.Rename(tmp, p)
		if err == nil || !researchTransientFileError(err) {
			return err
		}
		time.Sleep(5 * time.Millisecond)
	}
	return err
}

func (r *ctfResearchRun) snapshot() CTFResearchState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}

func (r *ctfResearchRun) end(status, detail string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state.Status, r.state.Summary = status, researchClip(RedactResearchText(detail), 2400)
	if err := r.persist(); err != nil {
		return fmt.Errorf("%s; checkpoint save failed: %w", detail, err)
	}
	if status == "completed" {
		return nil
	}
	return &ResearchIncompleteError{Status: status, Detail: detail}
}

func (r *ctfResearchRun) request(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state.Deadline != nil && !r.now().Before(*r.state.Deadline) {
		return &ResearchIncompleteError{Status: "budget_exhausted", Detail: "运行时间预算已耗尽，证据与下一步已保留"}
	}
	if r.state.MaxRequests > 0 && r.state.Requests >= r.state.MaxRequests {
		return &ResearchIncompleteError{Status: "budget_exhausted", Detail: "模型轮次预算已耗尽，证据与下一步已保留"}
	}
	r.state.Requests++
	return r.persist()
}

func (r *ctfResearchRun) observed(novel bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	invalidated := r.state.Status == "completed" || r.state.Status == "blocked"
	if invalidated {
		// A later substantive tool in the same batch can change the evidence.
		// Require a fresh end decision instead of reusing the earlier seal.
		r.state.Status = "active"
	}
	if novel {
		r.novel++
	}
	if invalidated {
		return r.persist()
	}
	return nil
}

func (r *ctfResearchRun) continuation() (string, error) {
	if _, ok := r.acceptVerifiedCompletion(); ok {
		return "", nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state.Status != "active" {
		return "", nil
	}
	if r.novel == r.lastNovel {
		r.idleStops++
	} else {
		r.idleStops = 0
	}
	r.lastNovel = r.novel
	r.state.Continuations++
	if r.idleStops >= 3 {
		r.state.Status, r.state.Summary = "stalled", "连续三次结束检查没有新增工具证据，模型未执行续研要求；保留检查点，未判定解题完成"
	}
	if err := r.persist(); err != nil {
		return "", err
	}
	if r.state.Status != "active" {
		return "", &ResearchIncompleteError{Status: r.state.Status, Detail: r.state.Summary}
	}
	return "先判断目标是否已由现有成功证据满足：若是，立即report_outcome(verified)交付（无此工具才research_status(completed)），不得为了结束手续再次复测或落盘。若未完成，按现有失败条件选择有信息增益的新步骤；真实外部缺口才用blocked说明恢复条件。\n" + r.memory.ContextSummary(), nil
}

func (r *ctfResearchRun) statusTool() actool.CoreTool {
	str := func() map[string]any { return map[string]any{"type": "string"} }
	return actool.Build(actool.Spec{Name: "research_status", Description: "无report_outcome时提交分配目标的完成/外部阻塞检查。有report_outcome则优先用verified一次完成，不双重确认。completed引用本题可回查成功证据并解释如何满足目标，续跑不必重做已成功操作。blocked必须给恢复条件。",
		Schema: map[string]any{"type": "object", "additionalProperties": false, "required": []string{"status", "summary"}, "properties": map[string]any{
			"status": map[string]any{"type": "string", "enum": []string{"active", "completed", "blocked"}}, "summary": str(),
			"evidence_ids": map[string]any{"type": "array", "maxItems": 8, "items": str()},
			"blocker":      map[string]any{"type": "string", "enum": []string{"missing_input", "environment", "permission"}}, "resume_when": str()}},
		Concurrent: func(json.RawMessage) bool { return false },
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Decision{Behavior: permission.Allow}
		},
		Run: func(ctx context.Context, raw json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			var in struct {
				Status      string   `json:"status"`
				Summary     string   `json:"summary"`
				EvidenceIDs []string `json:"evidence_ids"`
				Blocker     string   `json:"blocker"`
				ResumeWhen  string   `json:"resume_when"`
			}
			if len(raw) > 16000 || json.Unmarshal(raw, &in) != nil || researchContainsSecret(string(raw)) {
				return actool.Errorf("invalid research status or possible credentials"), nil
			}
			if err := ctx.Err(); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if in.Status != "active" && in.Status != "completed" && in.Status != "blocked" {
				return actool.Errorf("status must be active, completed or blocked"), nil
			}
			if strings.TrimSpace(in.Summary) == "" || len(in.Summary) > 2400 || len(in.EvidenceIDs) > 8 || len(in.ResumeWhen) > 1200 {
				return actool.Errorf("provide a bounded, concrete summary"), nil
			}
			if in.Status != "active" && r.completionCheck != nil {
				if pending := r.completionCheck(); pending != "" {
					return actool.Errorf(pending), nil
				}
			}
			state, err := r.memory.Snapshot()
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			success, fresh := false, false
			for _, id := range in.EvidenceIDs {
				found := false
				for _, ev := range state.Observations {
					if ev.ID == id {
						found = true
						if r.memory.evidenceHealth(ev.Path, ev.SHA256) != "current" {
							return actool.Errorf("evidence changed or missing"), nil
						}
						// IDs are monotonically allocated by ResearchMemory, across entries and observations.
						var n uint64
						_, _ = fmt.Sscanf(id, "t%d", &n)
						if n >= r.startEvidence {
							fresh = true
						}
						success = success || !ev.IsError
					}
				}
				if !found {
					return actool.Errorf("unknown evidence ID; use research_recall"), nil
				}
			}
			if in.Status == "completed" && !success {
				return actool.Errorf("completion needs intact successful evidence from this task and an explanation of how it meets the assigned goal"), nil
			}
			if in.Status == "blocked" {
				if strings.TrimSpace(in.ResumeWhen) == "" || (in.Blocker != "missing_input" && in.Blocker != "environment" && in.Blocker != "permission") {
					return actool.Errorf("blocked needs blocker and concrete resume_when; an unsolved hypothesis is not an external blocker"), nil
				}
				if in.Blocker == "environment" && !fresh {
					return actool.Errorf("environment blocker needs current tool evidence"), nil
				}
			}
			r.mu.Lock()
			defer r.mu.Unlock()
			r.state.Status, r.state.Summary, r.state.EvidenceIDs, r.state.Blocker, r.state.ResumeWhen = in.Status, in.Summary, in.EvidenceIDs, in.Blocker, in.ResumeWhen
			if err := r.persist(); err != nil {
				r.state.Status = "active"
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text("研究状态已保存：" + in.Status + "。证据链接已检查，结论仍须由真实验证支撑。"), nil
		}})
}

// Inject current bounded state at the request boundary, AFTER SDK compaction.
// This covers both built-in and noa context views and doesn't edit tool pairs.
type researchContextProvider struct {
	llm.Provider
	memory *ResearchMemory
	run    *ctfResearchRun
}

func (p *researchContextProvider) prepare(ctx context.Context, req llm.CompletionRequest) (llm.CompletionRequest, error) {
	if len(req.Tools) == 0 {
		return req, nil
	} // don't pollute the summarizer's task
	if p.run != nil {
		if err := p.run.request(ctx); err != nil {
			return req, err
		}
	}
	req.System = append(append([]string{}, req.System...), p.memory.ContextSummary())
	if p.run != nil {
		b, _ := json.Marshal(p.run.snapshot())
		req.System = append(req.System, "当前运行状态（宿主数据）："+string(b))
	}
	return req, nil
}
func (p *researchContextProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	if text, ok := p.sealedReply(ctx, req); ok {
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock(text)}}, "end_turn", llm.Usage{}, nil
	}
	req, err := p.prepare(ctx, req)
	if err != nil {
		return llm.Message{}, "", llm.Usage{}, err
	}
	return p.Provider.Complete(ctx, req)
}
func (p *researchContextProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		if text, ok := p.sealedReply(ctx, req); ok {
			for _, ev := range []llm.StreamEvent{{Type: llm.SEMessageStart}, {Type: llm.SETextDelta, Text: text}, {Type: llm.SEMessageDelta, StopReason: "end_turn"}, {Type: llm.SEMessageStop}} {
				if !yield(ev, nil) {
					return
				}
			}
			return
		}
		req, err := p.prepare(ctx, req)
		if err != nil {
			yield(llm.StreamEvent{}, err)
			return
		}
		for ev, err := range p.Provider.Stream(ctx, req) {
			if !yield(ev, err) {
				return
			}
		}
	}
}

// A status tool has already supplied the evidence-linked final summary. Seal
// locally instead of spending another request (or losing success at the cap).
func (p *researchContextProvider) sealedReply(ctx context.Context, req llm.CompletionRequest) (string, bool) {
	if ctx.Err() != nil || p.run == nil || len(req.Tools) == 0 {
		return "", false
	}
	if text, ok := p.run.acceptVerifiedCompletion(); ok {
		return text, true
	}
	s := p.run.snapshot()
	return s.Summary, s.Status == "completed" || s.Status == "blocked"
}

func captureCTFResearch(ctx context.Context, session *agentcore.Session, run *ctfResearchRun, input, role string, emit func(db.Activity)) (text string, err error) {
	var lastResult db.Activity
	terminalReason := harness.ReasonCompleted
	defer func() {
		state := run.snapshot()
		if err != nil {
			status := "failed"
			detail := err.Error()
			var incomplete *ResearchIncompleteError
			if errors.As(err, &incomplete) {
				status = incomplete.Status
				detail = incomplete.Detail
			}
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				status = "budget_exhausted"
			}
			if errors.Is(ctx.Err(), context.Canceled) {
				status = "cancelled"
			}
			if errors.Is(err, ErrIncompleteRun) {
				status = "interrupted"
				terminalReason = "incomplete_stream"
			}
			if saveErr := run.end(status, detail); saveErr != nil {
				err = saveErr
			}
		} else if state.Status != "completed" {
			detail := state.Summary
			if state.Status == "blocked" && state.ResumeWhen != "" {
				detail += "；恢复条件：" + state.ResumeWhen
			}
			err = run.end(state.Status, detail)
		}
		if emit != nil {
			lastResult.Worker, lastResult.Kind = role, "result"
			if err != nil {
				lastResult.IsError = true
				lastResult.Detail = err.Error()
			} else {
				lastResult.Detail = text
			}
			lastResult.Summary = firstLine(lastResult.Detail, 400)
			emit(lastResult)
		}
		if observer, ok := ctx.Value(modelObserverKey{}).(*ModelObserver); ok && observer.Terminal != nil {
			state := run.snapshot()
			if state.Status != "completed" && terminalReason == harness.ReasonCompleted {
				terminalReason = harness.TerminalReason(state.Status)
			}
			observer.Terminal(terminalReason)
		}
	}()
	for {
		var reason harness.TerminalReason
		text, reason, err = captureRunSession(ctx, session, input, func(rec db.Activity) {
			rec.Worker = role
			if rec.Kind == "result" {
				lastResult = rec
				// An intermediate model final remains visible as a checkpoint.
				if run.snapshot().Status != "active" || rec.IsError {
					return
				}
				rec.Kind = "text"
			}
			if emit != nil {
				emit(rec)
			}
		})
		terminalReason = reason
		if err != nil {
			return text, err
		}
		if ctx.Err() != nil {
			return text, ctx.Err()
		}
		if run.snapshot().Status != "active" {
			return text, nil
		}
		if reason != harness.ReasonCompleted {
			return text, &ResearchIncompleteError{Status: "interrupted", Detail: "Agent loop ended: " + string(reason)}
		}
		// The SDK deliberately bypasses a repeatedly blocking Stop hook after a
		// text-only response. The host checks again instead of declaring success.
		input, err = run.continuation()
		if err != nil {
			return text, err
		}
	}
}
