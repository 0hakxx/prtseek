package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/guard"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// ChatAgent is the generic, task-independent conversational runner behind the chat
// page. It generalizes MainAgent.Chat: any agent (built-in OR a custom one, by
// key) can be chatted with, multi-turn history resumed from the transcript. It is
// a PURE ASSISTANT — base tools are the SDK DefaultTools (Bash/Read/Write/Edit/
// LS/Glob/Grep) plus whatever skills/MCP the key is made visible; NO pentest
// graph/task context is injected (that stays exclusive to MainAgent).
type ChatAgent struct {
	prov               llm.Provider
	model              string
	workDir            string
	tx                 *transcript.Store
	window             int
	proxyAddr          string
	proxyCACert        string
	webSearch          WebSearchOpts
	guard              *guard.Guard // optional; nil disables intercept hooks for chat
	nonStreamingFn     func() bool  // resolver: use non-streaming (Complete) path? (nil = streaming)
	noaEnabledFn       func() bool  // resolver: use experimental noa compaction? (nil = off)
	maxTokensFn        func() int   // resolver: per-reply output cap (nil/0 = send no cap)
	extraToolsFn       func(context.Context, string) []actool.CoreTool
	completionCheck    func() string
	verifiedCompletion func() (string, bool)
}

// WithVerifiedCompletion accepts only the host's current-run, evidence-validated
// outcome. A child's local success or a previous turn is not the caller's goal.
func (c *ChatAgent) WithVerifiedCompletion(check func() (string, bool)) *ChatAgent {
	copy := *c
	copy.verifiedCompletion = check
	return &copy
}

func (c *ChatAgent) WithCompletionCheck(check func() string) *ChatAgent {
	copy := *c
	copy.completionCheck = check
	return &copy
}

func (c *ChatAgent) WithExtraTools(fn func(context.Context, string) []actool.CoreTool) *ChatAgent {
	copy := *c
	copy.extraToolsFn = fn
	return &copy
}

func NewChatAgent(prov llm.Provider, model, workDir string, tx *transcript.Store, window int) *ChatAgent {
	return &ChatAgent{prov: prov, model: model, workDir: workDir, tx: tx, window: window}
}

// SetNonStreaming wires a resolver deciding whether chat runs use the
// non-streaming model path (true = non-streaming). nil/unset = streaming.
func (c *ChatAgent) SetNonStreaming(fn func() bool) { c.nonStreamingFn = fn }

func (c *ChatAgent) nonStreaming() bool { return c.nonStreamingFn != nil && c.nonStreamingFn() }

// SetNoaEnabled wires a resolver deciding whether chat runs use the experimental
// noa context-compression mechanism. nil/unset = off (built-in compaction). Read
// per run so the settings toggle takes effect without rebuilding the agent.
func (c *ChatAgent) SetNoaEnabled(fn func() bool) { c.noaEnabledFn = fn }

// SetMaxTokens wires a resolver for the per-reply output cap. nil/unset or 0 =
// send no cap and let the endpoint decide. Read per run, like nonStreaming.
func (c *ChatAgent) SetMaxTokens(fn func() int) { c.maxTokensFn = fn }

func (c *ChatAgent) maxTokens() int {
	if c.maxTokensFn == nil {
		return 0
	}
	return c.maxTokensFn()
}

// SetProxy points the chat agent's WebFetch/Bash at the recording proxy plus the
// CA cert it trusts (empty addr = direct). Kept for parity with the other agents.
func (c *ChatAgent) SetProxy(addr, caCert string) { c.proxyAddr, c.proxyCACert = addr, caCert }

// SetWebSearch selects the web_search backend for the chat agent (off by default).
func (c *ChatAgent) SetWebSearch(o WebSearchOpts) { c.webSearch = o }

// SetGuard attaches a guard (with user-configured intercept rules) to this chat
// agent. Must be called before Chat; safe to call multiple times.
func (c *ChatAgent) SetGuard(g *guard.Guard) { c.guard = g }

// chatWorkDirSpec returns a working-directory notice appended to every chat
// agent's system prompt. Mirrors artifactSpec but without pentest-specific
// wording ("payload", "抓响应体") that would be odd in a general assistant.
func chatWorkDirSpec(workDir string) string {
	return "\n\n**文件输出规约**：需要写文件时，一律写到工作目录 " + workDir + "（这是默认 CWD，相对路径即落在这里，也可用该绝对路径）——不要写 /tmp 或其他绝对路径。\n" + shellEnvironment()
}

// chatSystem renders the DB-managed prompt body for agentKey. Custom agents have
// no per-key in-code default, so DefaultAssistantPrompt is the render fallback.
func chatSystem(agentKey, dataDir, workDir string) string {
	prompt := renderSystem(agentKey, DefaultAssistantPrompt, chatVars{DataDir: dataDir, Now: nowStr()})
	if IsCTFSolveRole(agentKey) {
		prompt += "\n\n协作约定：只有一种解题角色。简单题直接做；难题用delegate_agents给solve分配2至4个互补未知点。收到首个足以完成目标的结果立即交付，宿主停止多余分支。不转交指挥角色，不自动派写WP。"
		prompt += "\n\n" + CTFEvidencePriority + "\n\n" + ctfRoleMethod(agentKey)
		prompt += "\n一次真实题目响应/本地求解结果的来源和验收条件清楚，且没有反证时，verification_level=observed已可交付，不要求反复请求或新开验证Agent。候选、示例或来源可疑的字符串才需一次有针对性的核对。只有隔离对照确实能消除歧义时才用research_experiment；不要把普通读取和已成功操作都套上基线/复测流程。reproduced与accepted是可选的更高证据等级，不是比赛交付门槛，平台接受必须有真实回执。"
		prompt += "\n\n目标达到后只调用一次report_outcome(state=verified)：summary先给flag/答案及最短使用步骤，criterion写本次真实目标，verification简述来源和判断，引用成功工具证据；子任务证据携带真实conversation_id。宿主会结束本轮并清理多余后代，无需再调用research_status、research_record、派writeup或重跑工具来证明流程完成。未达目标时用partial/unverified并继续有信息增益的工作。"
	} else if agentKey == "writeup" {
		prompt += "\n\n" + CTFWriteupPrompt + "\n\n" + CTFWriteupMethod
	}
	return prompt + chatWorkDirSpec(workDir)
}

func chatTodos(ctf bool) *actool.TodoStore {
	if ctf {
		return nil
	}
	return actool.NewTodoStore()
}

// chatVars carries the runtime variables a custom agent's prompt may reference.
// DataDir (server data root) + Now (server wall-clock, refreshed each turn) are the
// universal ones; any other {{.X}} fails to render and falls back to
// DefaultAssistantPrompt.
type chatVars struct{ DataDir, Now string }

// Chat runs ONE turn of a conversation with the agent identified by agentKey,
// resuming prior history keyed by sessionID. maxTurns is the per-turn agent step
// budget (0 = unlimited). maxDuration is the wall-clock run budget per turn
// (0 = unlimited); the timer resets each time Chat is called, so a new user
// message always starts a fresh countdown. webSearch gates network search for
// THIS agent (the global backend/key still come from the chat agent's config,
// but each agent decides on/off). emit receives each execution step (thinking /
// tool_use / tool_result / text / result), tagged with the agent key as the
// worker lane.
func (c *ChatAgent) Chat(ctx context.Context, agentKey, sessionID, message string, maxTurns int, maxDuration time.Duration, webSearch bool, emit func(db.Activity)) (string, error) {
	persistentGoal := agentKey == "personal" && PersonalPersistentGoal(message)
	if persistentGoal {
		// The user explicitly requested a persistent foreground goal. Keep the
		// existing cancellation and final-answer paths, but avoid settlement
		// solely because this run reached the usual step or time budget.
		maxTurns, maxDuration = 0, 0
	}
	// gate the global web-search opts by this agent's own flag.
	ws := c.webSearch
	if !webSearch {
		ws.Enabled = false
	}

	// Per-session working directory: <workDir>/sessions/<sessionID>/
	// Isolates file writes across conversations, mirroring how workers use i<intentID>/.
	sessionWorkDir := filepath.Join(c.workDir, "sessions", sessionID)
	_ = os.MkdirAll(sessionWorkDir, 0o755)
	ctx = intercept.WithReviewWorkingDirectory(ctx, sessionWorkDir)

	// Pure assistant: DefaultTools as the base; AugmentTools layers in the key's
	// visible skills/MCP and lets the DB tools table filter/override. DefaultTools
	// have no tools-table rows, so they always pass through.
	ctf := ctfRoleMethod(agentKey) != ""
	if ctf && maxDuration > 0 {
		// One deadline for the whole research run, including delegated work and
		// automatic continuations. Never reset it at a model's end_turn.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, maxDuration)
		defer cancel()
	}
	base := actool.DefaultTools()
	if ctf {
		// Delegation, not SDK background tasks, owns CTF parallelism. Sleep and
		// TodoWrite add competing control paths without helping solve a challenge.
		base = defaultToolsExcept("Sleep")
	}
	ctx = WithRunInfo(ctx, RunInfo{SessionID: sessionID})
	var tools []actool.CoreTool
	var def DeferredInfo
	cleanup := func() {}
	if agentKey == "writeup" {
		ws.Enabled = false
		tools = ctfWriteupWorkspaceTools(sessionWorkDir)
	} else {
		base = hostTools(base, c.proxyAddr, c.proxyCACert)
		tools, def, cleanup = AugmentTools(ctx, agentKey, base)
	}
	defer cleanup()
	if c.extraToolsFn != nil {
		tools = append(tools, c.extraToolsFn(ctx, agentKey)...)
	}

	system, boundary := deferredSystem(chatSystem(agentKey, c.workDir, sessionWorkDir)+responseLanguageBlock(ctx), def)
	opts := agentcore.Options{
		Provider:               budgetProvider(ctx, meterProvider(ctx, c.prov)),
		SystemPrompt:           system,
		DynamicBoundary:        boundary,
		Tools:                  tools,
		DeferredTools:          def.Deferred,
		UnlockSet:              def.Unlock,
		PermissionMode:         permission.ModeBypass,
		DisableBackgroundTasks: agentKey == "personal" || ctf,
		EnableWebFetch:         false, // Host adapter records redirects and bounded response evidence.
		WebFetchProxy:          c.proxyAddr,
		WebFetchCACert:         c.proxyCACert,
		// 联网搜索(可选)。ddgs 无需 key；brave-free 需 BraveKey；tavily 需 TavilyKey。
		// WebSearchProxy 是独立出口代理(http/https/socks5)，与记录流量的 MITM 代理无关；空则直连。
		EnableWebSearch:       ws.Enabled,
		WebSearchBackend:      ws.Backend,
		BraveSearchAPIKey:     ws.BraveKey,
		TavilySearchAPIKey:    ws.TavilyKey,
		DeepSeekSearchBaseURL: ws.DeepSeekBaseURL,
		DeepSeekSearchAPIKey:  ws.DeepSeekAPIKey,
		DeepSeekSearchModel:   ws.DeepSeekModel,
		WebSearchProxy:        ws.Proxy,
		BashEnv:               hostHTTPEnv(c.proxyAddr, c.proxyCACert), // Same explicit route/CA as WebFetch.
		WorkingDir:            sessionWorkDir,
		MaxTurns:              maxTurns,
		MaxDuration:           maxDuration,
		Compaction:            compactionConfig(c.window),
		Todos:                 chatTodos(ctf),
		// large tool output spills to cmd-output/ under the session dir.
		// 截断上限用 SDK 默认(tool.Capture 的 30000 字符)。
		ToolOutputDir: filepath.Join(sessionWorkDir, "cmd-output"),
		// 命中预算(步数)→ SDK 跑收尾:输出一句总结。Prompt 与收尾轮数按本 agent key 后台可编辑
		// (自定义 agent 各自一份;留空/0 用通用默认:10 轮)。
		Settlement:   wrapupSettlement(agentKey, nil),
		NonStreaming: c.nonStreaming(), // 该 profile 选非流式时走 Provider.Complete
		MaxTokens:    c.maxTokens(),    // 0 = 不发上限,由服务端默认值决定
	}
	if persistentGoal {
		opts.SystemPrompt = append(opts.SystemPrompt, "本轮用户明确要求持续推进目标。不要因尝试过几个方向、写出阶段总结或保存检查点就结束；继续依据新证据探索。只有取得可验证结果、用户中止，或明确说明环境/授权/可行路线的实际阻塞时才给最终答复。")
	}
	if c.guard != nil {
		opts.Hooks = c.guard.Hooks()
	}
	if c.tx != nil { // persist raw human↔AI conversation; one accumulating file per thread
		opts.Transcript = c.tx
		opts.SessionID = sessionID
	}
	// 实验功能:开启后由 noa 接管上下文压缩(归档集中在 <workDir>/noa/<SessionID> 下,持久)。
	enableNoa(&opts, c.noaEnabledFn, c.workDir, "chat-"+sessionID, noaWarn("chat-"+sessionID))
	ctx = attachSideCapture(ctx, &opts)
	if IsCTFRole(agentKey) {
		ctx = WithResearchOptimization(ctx, true)
		ctx = withoutResearchGuide(ctx)
	}
	if agentKey != "writeup" {
		if err := AttachResearchRuntime(ctx, &opts, c.model, sessionWorkDir, message); err != nil {
			return "", err
		}
	}
	research, err := attachCTFResearch(ctx, &opts, agentKey, message, maxTurns)
	if err != nil {
		return "", err
	}
	personalSessionOptions(ctx, &opts)
	if c.completionCheck != nil {
		opts.Hooks = &completionCheckHooks{HookRunner: opts.Hooks, check: c.completionCheck}
	}
	s := agentcore.NewSession(opts)
	defer s.Close()
	// reload prior conversation so the agent has context across turns (each Chat is
	// a fresh session). First turn: no file yet → Resume loads nothing and proceeds.
	if c.tx != nil {
		if err := resumeSession(s, sessionID); err != nil {
			return "", err
		}
	}
	// re-unlock skill-gated MCPs from prior Skill() calls in the reloaded history so
	// revealed tools stay callable across the fresh session.
	seedUnlockFromHistory(s.Messages(), def.UnlockSkill)
	if research != nil {
		research.completionCheck = c.completionCheck
		research.verifiedCompletion = c.verifiedCompletion
		return captureCTFResearch(ctx, s, research, message, agentKey, emit)
	}
	text, reason, err := captureRunSession(ctx, s, message, func(r db.Activity) {
		if emit != nil {
			r.Worker = agentKey
			emit(r)
		}
	})
	if observer, ok := ctx.Value(modelObserverKey{}).(*ModelObserver); ok && observer.Terminal != nil {
		observer.Terminal(reason)
	}
	if err == nil && reason != "completed" {
		err = fmt.Errorf("执行结束：%s", reason)
	}
	return text, err
}
