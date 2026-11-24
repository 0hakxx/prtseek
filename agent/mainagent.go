package agent

import (
	"context"
	"fmt"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// MainAgent is the thin human-interface orchestrator (docs §4.2 / §7). The human
// chats with it; it observes (read tools), and steers by injecting hints
// (→planner) or direct high-priority intents (→frontier). It does NOT run the
// autonomous intent-generation loop (that is the planner's job).
type MainAgent struct {
	findingRecorder FindingRecorder
	prov            llm.Provider
	model           string
	tx              *transcript.Store                      // raw LLM conversation persistence (nil = off)
	window          int                                    // context window in tokens (for compaction)
	windowFn        func() int                             // optional dynamic task-chain minimum
	maxTurns        int                                    // max agent turns per run (0 = unlimited)
	proxyAddr       string                                 // recording proxy for WebFetch (empty = direct)
	proxyCACert     string                                 // recording proxy's CA cert path (HTTPS verify)
	webSearch       WebSearchOpts                          // web_search tool backend selection (off by default)
	workDir         string                                 // shared work dir (surfaced in prompt as artifact-output target)
	steerWork       func(intentID int64, msg string) error // engine callback: steer a running work (nil = off)
	nonStreamingFn  func() bool                            // resolver: use non-streaming (Complete) path? (nil = streaming)
	noaEnabledFn    func() bool                            // resolver: use experimental noa compaction? (nil = off)
	maxTokensFn     func() int                             // resolver: per-reply output cap (nil/0 = send no cap)
}

// SetNoaEnabled wires a resolver deciding whether runs use the experimental noa
// context-compression mechanism. nil/unset = off (built-in compaction). Read per
// run so the settings toggle takes effect without rebuilding the agent.
func (m *MainAgent) SetNoaEnabled(fn func() bool) { m.noaEnabledFn = fn }

// SetNonStreaming wires a resolver deciding whether runs use the non-streaming
// model path (true = non-streaming). nil/unset = streaming (default).
func (m *MainAgent) SetNonStreaming(fn func() bool) { m.nonStreamingFn = fn }

func (m *MainAgent) nonStreaming() bool { return m.nonStreamingFn != nil && m.nonStreamingFn() }

// SetMaxTokens wires a resolver for the per-reply output cap. nil/unset or 0 =
// send no cap and let the endpoint decide. Read per run, like nonStreaming.
func (m *MainAgent) SetMaxTokens(fn func() int) { m.maxTokensFn = fn }

func (m *MainAgent) maxTokens() int {
	if m.maxTokensFn == nil {
		return 0
	}
	return m.maxTokensFn()
}

func NewMainAgent(prov llm.Provider, model, workDir string, tx *transcript.Store, window, maxTurns int) *MainAgent {
	return &MainAgent{prov: prov, model: model, workDir: workDir, tx: tx, window: window, maxTurns: maxTurns}
}

func (m *MainAgent) SetCompactionWindowResolver(fn func() int) { m.windowFn = fn }

func (m *MainAgent) compactionWindow() int {
	if m.windowFn != nil {
		return m.windowFn()
	}
	return m.window
}

// SetProxy points the main agent's WebFetch at the recording proxy plus the CA
// cert it trusts to verify HTTPS through it (empty addr = direct).
func (m *MainAgent) SetProxy(addr, caCert string) { m.proxyAddr, m.proxyCACert = addr, caCert }

// SetWebSearch selects the web_search backend for the main agent (off by default).
func (m *MainAgent) SetWebSearch(o WebSearchOpts) { m.webSearch = o }

// SetSteerWork wires the engine callback that lets the main agent's steer_work
// tool inject a mid-run course-correction into a running work (nil = tool off).
func (m *MainAgent) SetSteerWork(fn func(intentID int64, msg string) error) { m.steerWork = fn }

// mainAgentDefaultTmpl is the built-in EDITABLE body (구간 [A]) of the main agent
// prompt, seeded into agent_prompts. Goal is a {{.Goal}} template var; the 중간
// 산출물 출력 규칙 tail is code-owned (artifactSpec), appended after rendering.
const mainAgentDefaultTmpl = `너는 권한이 부여된 침투 테스트 시스템의 "메인 agent"이며, 사람 운영자와의 인터페이스다. 너는 직접 탐색하지 않고, 자율적으로 연달아 의도를 만들지도 않는다(그것은 플래너의 일이다). 너의 역할:

1. 관찰: graph_overview / list_findings / list_facts / list_assets / get_worker_output 으로 현재 진행 상황에 대한 사람의 질문에 답한다.
2. 조종(사람의 의도를 시스템에 반영):
   - 사람이 "방향 변경/특정 유형의 취약점 강조/특정 영역 집중"을 원하면 → add_hint 로 힌트를 쓴다(플래너가 다음에 읽는다).
   - 사람이 "특정 대상을 바로 테스트"하길 원하면 → add_intent 로 높은 우선순위 의도(priority 8-10)를 직접 주입한다. 시스템이 완료된 작업을 자동으로 실행 상태로 되돌리고, worker 가 이 의도를 받아 실행하며, 끝나면 완료 상태로 돌아간다.
     **작업 목표가 모두 달성되었을 때**(graph_overview 의 goals 가 모두 met): 내려보내기 전에 이 의도 뒤에 "새로 달성해야 할 결과"가 숨어 있는지 먼저 판단한다. 숨어 있다면 네가 추측한 목표를 한 문장으로 사람에게 되풀이해 주고 **정식 목표로 등록할지 되묻는다**——사람이 원하면 → set_goals 로 등록한다(작업은 이후 일반 계획으로 들어가고 플래너가 자율적으로 추진한다). 원하지 않거나 / 잠깐 확인만 하려는 경우 → add_intent 로 이 하나만 내려보내며, worker 가 실행을 마치면 작업은 완료 상태로 돌아간다(자율적으로 계속하지 않음). 이 의도가 분명히 일회성 확인일 뿐 새 목표를 담고 있지 않다면 바로 add_intent 하면 되고, 매번 물을 필요는 없다.
   - 사람이 "실행 중인 특정 의도(work)를 실시간으로 바로잡기(X 는 그만, Y 에 집중)"를 원하면 → steer_work 를 쓴다(중단하지 않고 기존 진행을 잃지 않으며, worker 의 다음 동작 전에 적용된다). 먼저 get_worker_output 으로 무엇을 하는지 본다. 방향이 완전히 틀렸다면 add_intent 로 새 의도를 따로 내린다.
   - 사람이 "달성할 최종 목표를 새로 추가"하길 원하면 → set_goals 로 목표를 보충한다. 시스템이 그 목표를 작업 그래프에 쓰고 **완료/일시 중지된 작업을 자동으로 실행 상태로 되돌려 계속 진행**하므로(플래너가 이후 달성 여부를 다시 판단한다), 사람이 다시 재개를 누를 필요가 없다.
   - 사람이 "테스트 제약 추가/변경(특정 유형의 작업 허용/금지, 예: 『현재 포트만 테스트』『무차별 대입 금지』『수동적 정찰만』)"을 원하면 → set_constraints 로 등록한다(type=allow 허용 / type=deny 금지). 제약은 다음 계획 턴에 planner/worker 프롬프트에 주입되어 탐색 경계를 정한다. 개요의 「제약 관리」에서 추가/삭제/수정할 수도 있다.
3. 사람이 이해하기 쉬운 말로 간결하게 답하고, 무엇을 했는지 설명한다.

현재 작업 목표: {{.Goal}}

발견을 지어내지 않는다. 도구가 반환한 실제 데이터에만 근거해 답한다.`

func mainAgentSystem(goal, dataDir, workDir string) string {
	body := renderSystem("mainagent", mainAgentDefaultTmpl, MainVars{Goal: goal, DataDir: dataDir, Now: nowStr(), NowTurn: nowStr()})
	return body + artifactSpec(workDir)
}

// Chat handles one human message and returns the assistant reply. emit, if
// non-nil, receives each execution step (thinking / tool_use / tool_result /
// text / result) so the main-agent session shows its work — exactly like the
// worker/planner sessions — not just the final answer.
func (m *MainAgent) Chat(ctx context.Context, taskID int64, mainSeg int, as *db.AssetStore, ts *db.ExplorationStore, goal, message string, emit func(db.Activity), notify, resume func(), notifyGoal, notifyHint func([]string)) (string, error) {
	tsx := NewToolSet(ts, "human")
	tsx.SetFindingRecorder(m.findingRecorder)
	if as != nil {
		tsx.SetAssetStore(as, as.Companies())
	}
	tsx.SetTaskID(taskID)
	tsx.SetCoverageEnabled(as == nil || as.CoverageEnabled(taskID))
	tsx.SetNotify(notify)         // 일반 깨우기(전용 콜백이 없는 쓰기 작업이 사용, debounced)
	tsx.SetResumeTask(resume)     // set_goals 로 목표 추가 → 완료/일시 중지된 작업을 running 으로 되돌림
	tsx.SetNotifyGoal(notifyGoal) // set_goals 로 목표 추가 → planner 에 「사람이 목표를 추가함: …」 트리거 기록
	tsx.SetNotifyHint(notifyHint) // add_hint 로 힌트 추가 → planner 에 「사람이 전략 힌트 N 개를 추가함: …」 트리거 기록
	tsx.steerWork = m.steerWork   // enable steer_work tool (nil = unavailable)
	// 도메인 도구 + 기본 도구 세트(Read/Write/Edit/MultiEdit/LS/Glob/Grep/Bash)
	// 자산 커버리지 기능이 꺼져 있으면 add_task_scope/list_untested_assets 를 제외한다(prompt 에 넣지 않음).
	base := append(tsx.DropCoverageTools(tsx.MainAgentTools()), actool.DefaultTools()...)
	ctx = WithRunInfo(ctx, RunInfo{TaskID: taskID, ExplorationID: explorationID(ts)})
	tools, def, cleanup := AugmentTools(ctx, "mainagent", base)
	defer cleanup()
	// 이 작업의 작업 디렉터리 <workDir>/tasks/<taskID> 를 먼저 만든다.
	mainDir := ensureRunDir(m.workDir, taskID, 0)
	ctx = intercept.WithReviewWorkingDirectory(ctx, mainDir)
	system, boundary := deferredSystem(mainAgentSystem(goal, m.workDir, mainDir)+responseLanguageBlock(ctx), def)
	opts := agentcore.Options{
		Provider:        m.prov,
		SystemPrompt:    system,
		DynamicBoundary: boundary,
		Tools:           tools,
		DeferredTools:   def.Deferred,
		UnlockSet:       def.Unlock,
		PermissionMode:  permission.ModeBypass,
		EnableWebFetch:  true, // 기록 프록시를 거쳐 흔적을 남김. MITM 으로 재서명된 HTTPS 인증서를 검증하도록 프록시 CA 를 불러옴
		WebFetchProxy:   m.proxyAddr,
		WebFetchCACert:  m.proxyCACert,
		// 웹 검색(선택). ddgs 는 key 불필요, brave-free 는 BraveKey 필요, tavily 는 TavilyKey 필요.
		// WebSearchProxy 는 독립적인 출구 프록시(http/https/socks5)로, 트래픽을 기록하는 MITM 프록시와는 무관하다. 비어 있으면 직접 연결.
		EnableWebSearch:       m.webSearch.Enabled,
		WebSearchBackend:      m.webSearch.Backend,
		BraveSearchAPIKey:     m.webSearch.BraveKey,
		TavilySearchAPIKey:    m.webSearch.TavilyKey,
		DeepSeekSearchBaseURL: m.webSearch.DeepSeekBaseURL,
		DeepSeekSearchAPIKey:  m.webSearch.DeepSeekAPIKey,
		DeepSeekSearchModel:   m.webSearch.DeepSeekModel,
		WebSearchProxy:        m.webSearch.Proxy,
		BashEnv:               proxyEnv(m.proxyAddr, m.proxyCACert), // Bash 하위 명령은 기본적으로 프록시 사용 + CA 신뢰
		WorkingDir:            mainDir,                              // 이 작업의 작업 디렉터리 <workDir>/tasks/<taskID>
		ToolOutputDir:         cmdOutDir(mainDir),
		MaxTurns:              m.maxTurns,                             // 0 = unlimited (configurable in agent management)
		Compaction:            compactionConfig(m.compactionWindow()), // long chats stay within the window
		Todos:                 actool.NewTodoStore(),                  // 대화 단위 임시 할 일(TodoWrite), 계획 전용이며 종료하면 버려짐
		// 예산(단계 수)에 도달 → SDK 가 마무리 실행: 사용자에게 진행 요약 한 문장 출력. Prompt 와 마무리 턴 수는 관리 화면에서 편집 가능(기본 10턴).
		Settlement:   wrapupSettlement("mainagent", nil),
		NonStreaming: m.nonStreaming(), // 이 profile 이 비스트리밍을 선택하면 Provider.Complete 사용
		MaxTokens:    m.maxTokens(),    // 0 = 상한을 보내지 않고 서버 기본값에 맡김
	}
	if m.tx != nil { // persist raw human↔AI conversation; one accumulating file per segment
		opts.Transcript = m.tx
		// Segment 0 keeps the legacy "exp%d-main" name so existing transcripts still
		// load; each new session (seg>=1) gets its own file for a clean context.
		opts.SessionID = fmt.Sprintf("exp%d-main", ts.ID())
		if mainSeg > 0 {
			opts.SessionID = fmt.Sprintf("exp%d-main-s%d", ts.ID(), mainSeg)
		}
	}
	// 실험 기능: 켜면 noa 가 컨텍스트 압축을 맡는다(아카이브는 <workDir>/noa/<SessionID> 아래에 모이며 영구 보존).
	// session id 는 transcript 와 같은 규칙(세그먼트 인식)을 따르므로 아카이브와 복원이 맞춰진다.
	noaSession := fmt.Sprintf("exp%d-main", ts.ID())
	if mainSeg > 0 {
		noaSession = fmt.Sprintf("exp%d-main-s%d", ts.ID(), mainSeg)
	}
	enableNoa(&opts, m.noaEnabledFn, m.workDir, noaSession, noaWarn(noaSession))
	ctx = attachSideCapture(ctx, &opts)
	if err := AttachResearchRuntime(ctx, &opts, m.model, mainDir, goal+"\n"+message); err != nil {
		return "", err
	}
	personalSessionOptions(ctx, &opts)
	s := agentcore.NewSession(opts)
	defer s.Close()
	// reload the prior conversation from the transcript so the agent has context
	// across turns (each Chat is a fresh session; without this it can't see earlier
	// messages). First turn: no file yet → Resume loads nothing and proceeds.
	if m.tx != nil {
		if err := resumeSession(s, opts.SessionID); err != nil {
			return "", err
		}
	}
	// C2: this session is fresh each turn; re-unlock skill-gated MCPs from prior
	// Skill() calls in the reloaded history so revealed tools stay callable.
	seedUnlockFromHistory(s.Messages(), def.UnlockSkill)
	text, _, err := captureRunSession(ctx, s, message, func(r db.Activity) {
		if emit != nil {
			r.Worker = "mainagent"
			emit(r)
		}
	})
	return text, err
}
