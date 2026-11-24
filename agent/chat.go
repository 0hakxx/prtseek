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
	prov            llm.Provider
	model           string
	workDir         string
	tx              *transcript.Store
	window          int
	proxyAddr       string
	proxyCACert     string
	webSearch       WebSearchOpts
	guard           *guard.Guard // optional; nil disables intercept hooks for chat
	nonStreamingFn  func() bool  // resolver: use non-streaming (Complete) path? (nil = streaming)
	noaEnabledFn    func() bool  // resolver: use experimental noa compaction? (nil = off)
	maxTokensFn     func() int   // resolver: per-reply output cap (nil/0 = send no cap)
	extraToolsFn    func(context.Context, string) []actool.CoreTool
	completionCheck func() string
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
// wording ("payload", "응답 본문 캡처") that would be odd in a general assistant.
func chatWorkDirSpec(workDir string) string {
	return "\n\n**파일 출력 규칙**: 파일을 써야 할 때는 반드시 작업 디렉터리 " + workDir + " 에 쓴다(이곳이 기본 CWD 이므로 상대 경로는 여기에 생성되며, 이 절대 경로를 써도 된다)——/tmp 나 다른 절대 경로에 쓰지 않는다.\n" + shellEnvironment()
}

// chatSystem renders the DB-managed prompt body for agentKey. Custom agents have
// no per-key in-code default, so DefaultAssistantPrompt is the render fallback.
func chatSystem(agentKey, dataDir, workDir string) string {
	return chatSystemFor(agentKey, dataDir, workDir, "")
}

// chatSystemFor renders the body for one conversation: {{.Now}} is pinned to that
// session's start stamp so the system prefix stays byte-identical across turns
// (prefix cache), while {{.NowTurn}} still carries the live time.
func chatSystemFor(agentKey, dataDir, workDir, sessionID string) string {
	prompt := renderSystem(agentKey, DefaultAssistantPrompt, chatVars{DataDir: dataDir, Now: sessionNow(sessionID), NowTurn: nowStr()})
	if agentKey == "triage" {
		prompt += "\n\n협업 규칙: 자료가 충분하고 몇 단계로 해결할 수 있으면 delegate_agent 로 solver 에게 바로 맡긴다. 서로 보완하는 여러 방향이 필요할 때만 command 에 넘긴다. 문제나 첨부 파일이 없으면 무엇이 빠졌는지 분명히 밝히고, 말로만 분담했다고 주장하지 않는다."
	}
	if agentKey == "command" {
		prompt += "\n\n협업 규칙: 복잡도에 따라 delegate_agents 로 서로 보완하는 solver 를 1~6개 시작한다. 쉬운 문제는 하나면 충분하다. 이 도구는 즉시 번호를 반환하므로 wait_agents 로 결과 중 하나를 기다리고, 돌파구는 바로 활용하며, cancel_agent 로 가치 없는 분기를 중지하고 취소된 작업의 결과도 회수한다. 작업마다 자료, 목표, 경계, 산출물, 검수 기준을 명확히 쓴다. 동시 최대 8개, 한 턴 총 24개 하위 작업, 깊이 4단계까지이며 턴 전체가 200회의 모델 호출을 공유한다. 병렬도를 채우려고 중복 분담하지 않는다. 결과가 수렴하면 필요에 따라 writeup 에 넘긴다."
	}
	if agentKey == "triage" || agentKey == "command" || agentKey == "solver" || agentKey == "writeup" {
		prompt += "\n\n" + CTFEvidencePriority + "\n\n" + ctfRoleMethod(agentKey)
		prompt += "\n실험 전에 research_experiment 로 가설, 바꾼 변수, 예상을 기록하고, 고유한 디렉터리/원격 산출물 접두사를 사용하며 먼저 기준선을 기록한다. 답을 읽기만 했다면 verification_level=observed, 독립 재현이면 reproduced 로 하고 experiment_id, baseline_evidence, result_evidence 를 제공한다. 플랫폼 수락은 accepted 로 하고 acceptance_evidence 를 제공한다. 이전 산출물을 다시 읽는 것은 초기 상태에서 독립적으로 재현했다는 증거가 아니며, 플랫폼 수락에는 실제 수신 확인이 있어야 한다. 단계 재검토에는 결론, 증거, 반증, 다음 단계만 쓰고 내부 사고 과정을 보여 줄 필요는 없다."
		prompt += "\n\n결과를 넘기기 전에 report_outcome 을 호출한다: 검수 조건, 목표 상태(unverified/partial/blocked/verified), 결론, 증거 번호, 산출물 상대 경로를 명확히 한다. verified 는 실제 검증 방법을 설명하고 성공한 도구 증거를 인용해야 한다. 후보 flag 나 추론만 있다면 partial/unverified 만 쓸 수 있다. 하위 작업 증거를 인용할 때는 실제 conversation_id 를 함께 적는다. 실행이 정상 종료되었다고 목표가 달성된 것은 아니다."
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
type chatVars struct{ DataDir, Now, NowTurn string }

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
	base = hostTools(base, c.proxyAddr, c.proxyCACert)
	tools, def, cleanup := AugmentTools(ctx, agentKey, base)
	defer cleanup()
	if c.extraToolsFn != nil {
		tools = append(tools, c.extraToolsFn(ctx, agentKey)...)
	}

	system, boundary := deferredSystem(chatSystemFor(agentKey, c.workDir, sessionWorkDir, sessionID)+responseLanguageBlock(ctx), def)
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
		// 웹 검색(선택). ddgs 는 key 불필요, brave-free 는 BraveKey 필요, tavily 는 TavilyKey 필요.
		// WebSearchProxy 는 독립적인 출구 프록시(http/https/socks5)로, 트래픽을 기록하는 MITM 프록시와는 무관하다. 비어 있으면 직접 연결.
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
		// 잘라내기 상한은 SDK 기본값(tool.Capture 의 30000자)을 사용한다.
		ToolOutputDir: filepath.Join(sessionWorkDir, "cmd-output"),
		// 예산(단계 수)에 도달 → SDK 가 마무리 실행: 한 문장 요약을 출력. Prompt 와 마무리 턴 수는 이 agent key 별로 관리 화면에서 편집 가능
		// (사용자 정의 agent 마다 하나씩. 비우거나 0 이면 공통 기본값: 10턴).
		Settlement:   wrapupSettlement(agentKey, nil),
		NonStreaming: c.nonStreaming(), // 이 profile 이 비스트리밍을 선택하면 Provider.Complete 사용
		MaxTokens:    c.maxTokens(),    // 0 = 상한을 보내지 않고 서버 기본값에 맡김
	}
	if persistentGoal {
		opts.SystemPrompt = append(opts.SystemPrompt, "이번 턴에서 사용자는 목표를 계속 추진하라고 명확히 요구했다. 몇 가지 방향을 시도했다거나, 단계 요약을 썼다거나, 체크포인트를 저장했다는 이유로 끝내지 말고, 새 증거에 따라 계속 탐색한다. 검증 가능한 결과를 얻었거나, 사용자가 중단했거나, 환경/권한/실행 가능한 경로의 실제 차단 요인을 명확히 설명할 때만 최종 답변을 한다.")
	}
	if c.guard != nil {
		opts.Hooks = c.guard.Hooks()
	}
	if c.tx != nil { // persist raw human↔AI conversation; one accumulating file per thread
		opts.Transcript = c.tx
		opts.SessionID = sessionID
	}
	// 실험 기능: 켜면 noa 가 컨텍스트 압축을 맡는다(아카이브는 <workDir>/noa/<SessionID> 아래에 모이며 영구 보존).
	enableNoa(&opts, c.noaEnabledFn, c.workDir, "chat-"+sessionID, noaWarn("chat-"+sessionID))
	ctx = attachSideCapture(ctx, &opts)
	if agentKey == "triage" || agentKey == "command" || agentKey == "solver" || agentKey == "writeup" {
		ctx = WithResearchOptimization(ctx, true)
		ctx = withoutResearchGuide(ctx)
	}
	if err := AttachResearchRuntime(ctx, &opts, c.model, sessionWorkDir, message); err != nil {
		return "", err
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
		err = fmt.Errorf("실행 종료: %s", reason)
	}
	return text, err
}
