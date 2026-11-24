package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// Worker is an LLM work agent (docs §4.4): it claims ONE intent, completes it
// with real tools (Bash: kali tooling through the recording proxy), writes the
// FACTS it found back into the graph, and stops. It does NOT generate new
// directions (that is the planner's job) and does NOT keep exploring toward the
// goal on its own. Multiple workers run concurrently as goroutines.
// WebSearchOpts is the web-search backend selection the server pushes into each
// agent (planner/worker/main). Enabled=false leaves the web_search tool off.
// Backend is "ddgs" (no key), "brave-free" (BraveKey required), "tavily"
// (TavilyKey required), or "deepseek" (DeepSeek* required, filled from the
// active LLM profile). It maps directly onto agentcore.Options.
// Proxy is a dedicated egress proxy for the search request (http/https/socks5),
// independent of the traffic-recording MITM proxy — set it when the search endpoint
// is only reachable via a VPN/SOCKS proxy. Empty = direct.
//
// deepseek 백엔드는 나머지 셋과 성격이 다르다는 점에 주의: DeepSeek 에는 직접 호출할 수 있는 검색 인터페이스가 없고,
// 검색은 그 Anthropic 호환 messages 인터페이스 내부(web_search_20250305 server
// tool)에만 있으므로, 검색할 때마다 모델 호출이 한 번 소모되며 검색 요청은 DeepSeek 서버에서 나간다——
// 로컬 Proxy 를 거치지 않으며 트래픽 기록에도 남지 않는다.
type WebSearchOpts struct {
	Enabled   bool
	Backend   string
	BraveKey  string
	TavilyKey string
	Proxy     string
	// DeepSeek* 는 현재 활성화된 LLM 설정(anthropic 형식의 DeepSeek 공식 엔드포인트만)에서 오며,
	// 따로 설정하지 않고 LLM 설정 전환에 따라 바뀐다.
	DeepSeekBaseURL string
	DeepSeekAPIKey  string
	DeepSeekModel   string
}

type Worker struct {
	findingRecorder FindingRecorder
	prov            llm.Provider
	model           string
	workDir         string
	proxyAddr       string
	proxyCACert     string            // recording proxy's CA cert path (for WebFetch HTTPS verify)
	webSearch       WebSearchOpts     // web_search tool backend selection (off by default)
	tx              *transcript.Store // raw LLM conversation persistence (nil = off)
	window          int               // context window in tokens (for compaction)
	windowFn        func() int        // optional dynamic task-chain minimum
	maxTurns        int               // max agent turns per run (0 = unlimited)
	// runTimeout is the wall-clock budget for the main exploration of one intent
	// (0 = unlimited). When it fires, the run is cut and a settlement round is
	// forced so already-identified facts get written back instead of being lost.
	runTimeout time.Duration
	// extraTools are host-provided tools (e.g. traffic query, oast) appended to
	// the worker's graph write-back tools.
	extraTools []actool.CoreTool
	// injectConstraints resolves whether this task's operation constraints get
	// injected into the worker system prompt. Read per run so the settings toggle
	// takes effect without rebuilding the agent. nil = inject (default).
	injectConstraints func() bool
	// nonStreamingFn resolves whether this run uses the non-streaming (Complete)
	// path. Read per run so a profile/task toggle takes effect without rebuilding
	// the agent. nil = streaming (default).
	nonStreamingFn func() bool
	// noaEnabledFn resolves whether this run uses the experimental noa context-
	// compression mechanism. Read per run, like nonStreaming. nil = off (built-in
	// compaction).
	noaEnabledFn func() bool
	// maxTokensFn resolves the per-reply output cap in tokens, on the same
	// per-run basis. nil or 0 = send no cap and let the endpoint decide.
	maxTokensFn func() int
}

// WorkerSessionID returns the stable transcript key used by a worker intent.
// Worker slots are reusable, so the intent id (rather than work#N) is the
// session identity. Keep this helper public so the Worker message API and UI
// can refer to exactly the conversation that will be resumed.
func WorkerSessionID(explorationID, intentID int64) string {
	return fmt.Sprintf("exp%d-worker-i%d", explorationID, intentID)
}

const workerChatMarkerPrefix = "<!-- prtseek_WORKER_CHAT:"
const legacyWorkerChatMarkerPrefix = "<!-- ARTEX_WORKER_CHAT:"

func workerChatMarker(requestID string) string {
	return workerChatMarkerPrefix + requestID + " -->"
}

func hasWorkerChatMessage(messages []llm.Message, requestID string) bool {
	marker := workerChatMarker(requestID)
	legacyMarker := legacyWorkerChatMarkerPrefix + requestID + " -->"
	for _, message := range messages {
		if message.Role == llm.RoleUser && (strings.Contains(message.Text(), marker) || strings.Contains(message.Text(), legacyMarker)) {
			return true
		}
	}
	return false
}

// SetNonStreaming wires a resolver deciding whether runs use the non-streaming
// model path (true = non-streaming). nil/unset = streaming (default). Read per
// run so a profile or task-chain toggle takes effect without rebuilding.
func (w *Worker) SetNonStreaming(fn func() bool) { w.nonStreamingFn = fn }

func (w *Worker) nonStreaming() bool { return w.nonStreamingFn != nil && w.nonStreamingFn() }

// SetNoaEnabled wires a resolver deciding whether runs use the experimental noa
// context-compression mechanism. nil/unset = off (built-in compaction). Read per
// run so the settings toggle takes effect without rebuilding the agent.
func (w *Worker) SetNoaEnabled(fn func() bool) { w.noaEnabledFn = fn }

// SetMaxTokens wires a resolver for the per-reply output cap. nil/unset or 0 =
// send no cap and let the endpoint decide. Read per run, like nonStreaming.
func (w *Worker) SetMaxTokens(fn func() int) { w.maxTokensFn = fn }

func (w *Worker) maxTokens() int {
	if w.maxTokensFn == nil {
		return 0
	}
	return w.maxTokensFn()
}

// SetConstraintInject wires a resolver deciding whether this task's operation
// constraints get injected into the worker system prompt. nil = inject (default).
func (w *Worker) SetConstraintInject(fn func() bool) { w.injectConstraints = fn }

// wantConstraints reports whether constraint injection is enabled (default yes).
func (w *Worker) wantConstraints() bool { return w.injectConstraints == nil || w.injectConstraints() }

// SetRunTimeout configures the per-intent wall-clock budget for the main
// exploration (0 = unlimited). When it fires, the SDK settlement phase still runs
// so facts are never lost to a timeout. Safe to call before Execute.
func (w *Worker) SetRunTimeout(run time.Duration) {
	w.runTimeout = run
}

// settleWrapUpPrompt is injected by the SDK settlement phase when a worker hits its
// turn/time budget: stop probing, write back what was found, then end with a
// plain-text one-liner (which becomes this run's displayed result).
const settleWrapUpPrompt = "너는 예산 소진으로 곧 종료된다. 더 이상 명령/탐지를 실행하지 않는다. 다음을 차례로 한다: (1) 위에서 식별했지만 아직 기록하지 않은 내용을 하나씩 기록한다——새 자산은 insert_assets, 탐색 결론/사실은 record_fact, 확인된 취약점은 report_finding; (2) **마지막에 순수 텍스트 한 문장만 따로** 써서 무엇을 했고 어떤 핵심 결론을 얻었는지 요약한다(이 문장은 이번 실행의 결과로 표시되므로 반드시 출력한다)."

func NewWorker(prov llm.Provider, model, workDir string, tx *transcript.Store, window, maxTurns int, extra ...actool.CoreTool) *Worker {
	return &Worker{prov: prov, model: model, workDir: workDir, tx: tx, window: window, maxTurns: maxTurns, extraTools: extra}
}

// defaultToolsExcept returns actool.DefaultTools() minus the named tools (by
// CoreTool.Name()). Used to trim SDK default tools an agent shouldn't have.
func defaultToolsExcept(exclude ...string) []actool.CoreTool {
	drop := make(map[string]bool, len(exclude))
	for _, n := range exclude {
		drop[n] = true
	}
	all := actool.DefaultTools()
	out := make([]actool.CoreTool, 0, len(all))
	for _, t := range all {
		if !drop[t.Name()] {
			out = append(out, t)
		}
	}
	return out
}

func (w *Worker) SetCompactionWindowResolver(fn func() int) { w.windowFn = fn }

func (w *Worker) compactionWindow() int {
	if w.windowFn != nil {
		return w.windowFn()
	}
	return w.window
}

// SetProxy configures the recording proxy address that workers route target
// traffic through, plus the CA cert path WebFetch trusts to verify HTTPS through
// that MITM proxy. Empty addr disables the hint.
func (w *Worker) SetProxy(addr, caCert string) { w.proxyAddr, w.proxyCACert = addr, caCert }

// SetWebSearch selects the web_search backend for this worker (off by default).
func (w *Worker) SetWebSearch(o WebSearchOpts) { w.webSearch = o }

// proxyEnv builds the Bash-subprocess env that routes child-command HTTP through
// the egress proxy (the recording MITM when capture is on, or the global proxy
// directly when it is off) and, only when a MITM CA is present, makes the common
// toolchain trust it — so tools need no manual -x/--proxy/-k. Each ecosystem reads
// a different CA var (verified empirically): SSL_CERT_FILE→curl/urllib/Go/openssl,
// REQUESTS_CA_BUNDLE→python requests (it ignores SSL_CERT_FILE), CURL_CA_BUNDLE→curl,
// GIT_SSL_CAINFO→git, NODE_EXTRA_CA_CERTS→node; NODE_USE_ENV_PROXY makes Node 24+
// honor the proxy vars. ALL_PROXY is set too so a socks5 egress proxy (which curl
// only reads from ALL_PROXY, not HTTP(S)_PROXY) works in the capture-off path.
// Empty proxyAddr → nil (direct, unchanged env).
func proxyEnv(proxyAddr, caCert string) []string {
	if proxyAddr == "" {
		return nil
	}
	env := []string{
		"HTTP_PROXY=" + proxyAddr, "HTTPS_PROXY=" + proxyAddr,
		"http_proxy=" + proxyAddr, "https_proxy=" + proxyAddr,
		"ALL_PROXY=" + proxyAddr, "all_proxy=" + proxyAddr, // socks5 egress: curl reads only this
		"NODE_USE_ENV_PROXY=1", // Node 24+: honor HTTP(S)_PROXY in built-in fetch/http
	}
	if caCert != "" {
		env = append(env,
			"SSL_CERT_FILE="+caCert,
			"CURL_CA_BUNDLE="+caCert,
			"REQUESTS_CA_BUNDLE="+caCert,
			"GIT_SSL_CAINFO="+caCert,
			"NODE_EXTRA_CA_CERTS="+caCert,
		)
	}
	return env
}

// workerDefaultTmpl is the built-in EDITABLE body (구간 [A]) of the worker system
// prompt, seeded into agent_prompts. The trafficTool block and the 중간 산출물 출력 규칙
// are NOT here — they are code-owned and appended by workerSystem after rendering
// (구간 [B]/[C]), so editing the DB body can never drop them.
const workerDefaultTmpl = `너는 사이버 보안 플랫폼의 권한이 부여된 침투 테스트 시스템의 "실행자"(work agent)다. 너는 【의도 하나】(한 문장 탐색 방향)를 받았으며, 유일한 역할은 다음과 같다: **이 의도 하나를 완료하고, 발견을 지식 그래프에 기록한 뒤, 멈추고 반환한다.**

**경계(레드 라인)**:
1. **네가 받은 이 의도 하나만 한다**. **이 의도를 탐색하다가 의도 밖에서 깊이 파 볼 만한 단서를 언뜻 봤다면**(오류로 노출된 경로, 다른 자산과 연동될 수 있는 지점, 또 다른 익스플로잇 체인의 진입점으로 의심되는 것), **fact 의 summary 에 한 마디 적어 플래너에게 넘긴다**.
2. 처음 막혔다고(payload 필터링 / 404 / 주입에 응답 없음) 다 탐색한 것은 아니다——이 의도의 모든 우회 수단을 다 써 본 뒤 결론을 낸다;
3. 권한 범위 안에서만 작업한다. 시스템 프롬프트 맨 위에 【작업 제약】이 붙어 있다면 그것이 최우선 레드 라인이다: 모든 명령/탐지를 실행하기 전에 스스로 점검하고, 위반이면 하지 않는다(네가 받은 의도 안에 있더라도).

**발견하는 대로 기록한다**(그래프에 써야 인정되며, 머릿속/텍스트에 있는 것은 인정되지 않는다. 결과가 하나 나올 때마다 즉시 쓰고, 마지막까지 모아 두다가 단계 수가 소진되어 잃지 않는다). 세 가지 기록이 있으며, 그래프를 섞지 않는다:
- **새 자산/리소스 → insert_assets(자산 그래프)**: 서브도메인 / service / endpoint / 지문 / 자격 증명 등 모든 자산 【자체】. **여기에는 자산만 등록한다. 탐색 결론/판단은 여기 쓰지 않고 record_fact 를 쓴다.**
- **탐색 결론/사실 → record_fact(탐색 그래프, intent_id 전달)**: 모두 이것을 쓴다. **여러 관찰은 【하나의】 사실로 요약한다**(summary 는 한 문장 요약 + detail 에는 요약을 확장한 내용을 쓰며, 실제 실행 과정에 근거한다). 속성 하나마다 하나씩 쓰지 않는다. 의도 하나는 보통 사실 하나이며, 잘게 쪼개면 그래프가 끝없이 부풀어 오른다——**기본은 하나만 쓰고, detail 에 합칠 수 있는 것은 모두 합친다**. 【서로 완전히 독립적이어서 합칠 수 없는】 결론이 정말 있을 때만 facts 배열로 나누며, 이는 아주 드문 예외이지 일반적인 경우가 아니다. **증분만 쓴다**: 이번에 【새로 얻은】 것만 기록하며, 기존 사실을 표현만 바꿔 다시 기록하지 않는다(기존 것을 확인했을 뿐 새로 추가된 것이 없으면 기록할 필요 없음). **실제로 본 것만 쓴다**: evidence(한 줄: 명령+가장 잘 증명하는 출력 한두 줄, 간결하게, 세부 사항은 detail 에)를 주고, confidence(observed=직접 봄 / inferred=현상에서 추론)를 표시한다.
- **확인된 취약점 → report_finding(탐색 그래프, PoC 포함, intent_id 전달)**: **이번에 실제로 트리거했고 재현 가능한 증거(요청/응답 또는 명령 출력)를 얻었을 때만 쓴다**. "버전/지문이 CVE 와 일치함", "파라미터가 주입 가능해 보임", "외부 취약점 DB/변경 로그/코드 diff 로 추론함"을 확인된 것으로 여기는 것은 엄격히 금지하며, CVE DB 조회나 패치 버전 비교로 실제 트리거를 대신하지도 않는다. 트리거할 수 없지만 의심스럽다면 → record_fact 로 inferred 사실을 하나 기록해(의심 지점+트리거하지 못한 이유) 플래너에게 넘기고, 억지로 finding 으로 기록하지 않는다.


이 의도를 완료하면 무엇을 했고 어떤 사실을 기록했는지 한 문장으로 요약한다.`

// workerTrafficBlock is 구간 [B]: the traffic-tool note, code-injected only when
// traffic capture (recording) is on — i.e. the traffic_* tools actually exist.
// Gated on recording, NOT on the egress proxy: a global proxy with capture off
// routes traffic but records nothing, so the tools would not be there. Not stored,
// not editable.
func workerTrafficBlock(recording bool) string {
	if !recording {
		return ""
	}
	return "\n\n**트래픽 도구**:\n- traffic_search / traffic_get / traffic_blob: 응답을 되돌아보고 이미 접근한 리소스를 찾는다. **먼저 트래픽을 조회하고, 같은 URL 을 반복해서 curl 하지 않는다**. traffic_search 는 **반드시 host 를 지정**해야 하며, 기본적으로 아주 가벼운 색인 3건(id/method/url/status/resp_len, 응답 내용 없음)만 반환한다. 더 필요하면 limit 를 명시적으로 늘린다. body_contains 로 요청/응답 본문에서 전문 검색을 할 수 있다(최소 3자, 부분 문자열과 한글/중국어 지원. 예: 비밀번호/키/오류/내부망 주소 찾기). 특정 항목의 원문은 traffic_get(id) 로 보며, 그중 아주 큰 본문은 @blob sha256:<hash> 로 표시되므로 traffic_blob(hash) 로 나눠서 전문을 가져온다."
}

// artifactSpec is 구간 [C]: the code-owned, non-editable tail appended to every
// pentest agent's prompt — intermediate artifacts must land in the shared work
// dir, never /tmp. Guaranteed present regardless of how the DB body is edited.
func artifactSpec(dir string) string {
	return "\n\n**중간 산출물 출력 규칙**: 스크립트, payload, 캡처한 응답 본문, 임시 데이터 등 모든 중간 산출물은 **반드시 이 작업의 작업 디렉터리 " + dir + " 에 쓴다**(상대 경로는 여기에 쓰이며, 이 절대 경로를 써도 된다)——**/tmp 에 쓰지 말고, 다른 절대 경로도 쓰지 않는다**."
}

// workerArtifactSpec is the worker's 구간 [C]: its per-intent run dir is pre-created
// by the engine (ensureRunDir), so it just writes relative paths there — no manual
// mkdir, no cross-worker name collisions.
func workerArtifactSpec(runDir string) string {
	return "\n\n**중간 산출물 출력 규칙**: 스크립트, payload, 캡처한 응답 본문, 임시 데이터 등 모든 중간 산출물은 **반드시 이번 의도 전용 작업 디렉터리 " + runDir + " 에 쓴다**(이미 자동으로 만들어져 있으므로 상대 경로로 바로 여기에 쓰면 되며, 디렉터리를 따로 만들 필요가 없다)——**/tmp 에 쓰지 말고, 다른 절대 경로도 쓰지 않는다**."
}

// ensureRunDir builds and creates an agent's working directory under base:
// <base>/tasks/<taskID> for planner/main; <base>/tasks/<taskID>/i<intentID> for a
// worker (intentID<=0 → task dir only). The "tasks/" segment groups per-task dirs
// symmetrically with the chat agent's "sessions/<sessionID>". Best-effort mkdir — on
// failure, writes fail the same way an unwritable CWD would.
func ensureRunDir(base string, taskID, intentID int64) string {
	dir := filepath.Join(base, "tasks", strconv.FormatInt(taskID, 10))
	if intentID > 0 {
		dir = filepath.Join(dir, "i"+strconv.FormatInt(intentID, 10))
	}
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

// cmdOutDir is the SDK large-tool-output spill dir under an agent's run dir.
func cmdOutDir(dir string) string { return filepath.Join(dir, "cmd-output") }

func workerSystem(proxyAddr, caCert, dataDir, runDir string) string {
	body := renderSystem("worker", workerDefaultTmpl, WorkerVars{ProxyAddr: proxyAddr, DataDir: dataDir, Now: nowStr(), NowTurn: nowStr()})
	// caCert is present only when the recording MITM is on, which is exactly when
	// the traffic_* tools are registered — so it gates the traffic-tool note.
	// Optional finding guidance is added for every role after tool resolution.
	return body + workerTrafficBlock(caCert != "") + workerArtifactSpec(runDir)
}

// renderIntentTask formats the claimed intent for the worker's launch USER message:
// the intent is the worker's whole job. It used to live in the system prompt; it now
// rides in the first user turn (together with the situational overview) so the system
// prompt stays static/role-only — same move as the planner's situational block.
// intentAssetIDs pulls the intent's target asset ids out of its payload
// (planner's add_intent stores them as a numeric asset_ids array). nil on absence
// or malformed payload.
func intentAssetIDs(intent *db.Node) []int64 {
	if intent == nil {
		return nil
	}
	var p struct {
		AssetIDs []int64 `json:"asset_ids"`
	}
	if err := json.Unmarshal(intent.Payload, &p); err != nil {
		return nil
	}
	return p.AssetIDs
}

func renderIntentTask(intent *db.Node) string {
	return fmt.Sprintf("\n\n【네가 받은 의도(이번의 유일한 작업: 이것 하나만 하고, 사실만 만들고, 끝나면 멈춤)】:\n%s\n의도 id: %d(record_fact / report_finding 으로 기록할 때 이 값을 넘김)", string(intent.Payload), intent.ID)
}

// renderWorkerGraphOverview folds the global situational snapshot into the worker's
// launch USER message for AWARENESS ONLY. The framing is deliberately strong: the overview
// must NOT widen the worker's job — it still does only its assigned intent. Its sole
// purpose is letting the worker read context (existing facts/assets/hints)
// so it avoids redundant work and doesn't re-derive what others already found.
func renderWorkerGraphOverview(data map[string]any) string {
	// coverage 는 플래너가 「어떤 유형이 적게 테스트되었는지 / 범위를 넓혀야 하는지」를 판단하는 신호로, worker 의 「받은
	// 의도 하나만 하고 다뤄지지 않은 지점을 쫓지 않는다」는 역할 경계와 맞지 않는다 → worker 뷰에서 제외한다. data 는 이번 worker
	// 전용의 새 map 이므로 키를 지워도 planner 에 영향이 없다.
	delete(data, "coverage")
	b, err := json.Marshal(data)
	if err != nil {
		return "" // fall back silently: the worker just won't have the global context
	}
	return "\n\n【전역 탐색 상황(읽기 전용, 너의 의도를 전체 그림 속에서 보도록 돕는다)】:\n" +
		"아래는 작업 전체의 현재 탐색 개요다. 용도는 두 가지다: 하나는 다른 이들이 이미 무엇을 발견했는지 알아 중복하지 않는 것, 다른 하나는 네 의도를 탐색할 때 전체와의 관계를 떠올릴 수 있게 하는 것이다.\n" +
		"**발산은 좋은 일이다**: 이 의도를 탐색할 때 마음껏 깊이 생각하고 많이 연상한다. 유일한 경계는——다른 의도를 실제로 손대서 실행하지 않는 것이다(그것은 다른 worker 의 일이며 플래너가 조율한다). 가치 있는 단서를 떠올렸다면(자산 간 연동, 또 다른 익스플로잇 체인의 진입점으로 의심되는 것, 전역 차원의 의심스러운 지점), **반드시 fact 에 써서 플래너에게 넘긴다**——이는 중요한 산출물이지 있어도 그만 없어도 그만인 것이 아니다. 혼자 삼키느니 하나 더 보고해 플래너가 판단하게 하는 편이 낫다.\n" +
		string(b)
}

// Execute runs one intent. hooks (the per-task Guard) gates every tool call; may
// be nil. emit, if non-nil, receives one ActivityRecord per execution step.
// notifyFinding, if non-nil, is called (intentID, summary) when this worker writes
// a finding (report_finding) so the task's planner wakes mid-flight — with context
// on which intent found what — instead of waiting for the worker to finish.
// Returns the terminal reason (so the engine can distinguish completed vs
// max_turns) and a per-kind breakdown of what was written back (so an intent that
// explored but persisted nothing isn't mistaken for done, and the engine can log
// facts/assets/findings separately instead of lumping them under "facts").
func (w *Worker) Execute(ctx context.Context, name string, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, intent *db.Node, hooks harness.HookRunner, emit func(db.Activity), enr EnrichTrigger, notifyFinding func(int64, string)) (harness.TerminalReason, WriteCounts, error) {
	return w.execute(ctx, name, taskID, as, ts, intent, hooks, emit, enr, notifyFinding, "", "")
}

// ExecuteWithMessage runs the next turn in the same intent conversation with a
// human-authored message. The HTTP handler does not edit the transcript;
// agentcore records the message as a normal user turn when this Worker starts.
// This keeps Worker continuation identical to the regular agent chat flow.
func (w *Worker) ExecuteWithMessage(ctx context.Context, name string, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, intent *db.Node, hooks harness.HookRunner, emit func(db.Activity), enr EnrichTrigger, notifyFinding func(int64, string), requestID, message string) (harness.TerminalReason, WriteCounts, error) {
	return w.execute(ctx, name, taskID, as, ts, intent, hooks, emit, enr, notifyFinding, strings.TrimSpace(requestID), strings.TrimSpace(message))
}

func (w *Worker) execute(ctx context.Context, name string, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, intent *db.Node, hooks harness.HookRunner, emit func(db.Activity), enr EnrichTrigger, notifyFinding func(int64, string), requestID, message string) (harness.TerminalReason, WriteCounts, error) {
	tsx := NewToolSet(ts, name)
	tsx.SetFindingRecorder(w.findingRecorder)
	tsx.SetTaskID(taskID)
	coverageEnabled := as == nil || as.CoverageEnabled(taskID)
	tsx.SetCoverageEnabled(coverageEnabled)
	if as != nil {
		tsx.SetAssetStore(as, as.Companies())
	}
	tsx.SetOwnerNode(intent.ID)         // assets this worker discovers anchor to its intent → visible to the task
	tsx.SetEnrich(enr)                  // async DNS/HTTP auto-completion for assets this worker writes
	tsx.SetNotifyFinding(notifyFinding) // report_finding 이 저장될 때 즉시 planner 를 깨우며 「어떤 의도+finding」을 함께 전달
	// base = built-in worker tools ∪ host tools (traffic) ∪ default tools (incl. Bash);
	// then augment with the agent's visible skills/MCP. During the SDK settlement
	// phase, Bash is hidden via Settlement.DisabledTools (no local gating needed).
	base := append(tsx.WorkerTools(), w.extraTools...)
	// worker 에게는 일부러 MultiEdit/Glob/Grep 을 주지 않는다: 파일 세부 수정은 Edit, 검색은 Bash(grep/find)를 쓴다.
	// 도구 표면을 좁혀 가치 낮은 호출을 줄인다. 나머지 SDK 기본 도구(Read/Write/Edit/LS/Bash/Sleep)는 평소대로 제공한다.
	base = append(base, defaultToolsExcept("MultiEdit", "Glob", "Grep")...)
	ctx = WithRunInfo(ctx, RunInfo{TaskID: taskID, ExplorationID: explorationID(ts), IntentID: intent.ID})
	tools, def, cleanup := AugmentTools(ctx, "worker", base)
	defer cleanup()

	// 의도는 worker 의 【유일한 역할이자 run 전체에 걸친 불변량】이다 → 시작 지시, 의도가 고정한 대상 자산의
	// 원본 데이터와 함께 system prompt 에 넣는다: system 은 run 마다 새로 조립되며 compaction 으로 압축되지 않으므로,
	// 긴 run 에서도 의도가 항상 존재하고, 이어서 실행할 때도 transcript 기록이 첫 메시지를 유지했는지에 의존하지 않는다. 대가는 system
	// 에 의도별로 바뀌는 데이터가 섞여 의도 간 캐시 재사용을 잃는 것이다. 이는 의도된 절충이다(의도를 잃는 것이 token 절약보다 훨씬 심각하다).
	// planner 의 「상황 블록을 user turn 에 둔다」와 갈라지는 것은 의도적이다: planner 는 의도를 만드는 쪽이며 단일 mandate 가 없지만,
	// worker 에게는 있다. 【전역 상황 overview】만 시작 user 메시지에 남긴다——이는 품질을 낮춰도 되고 stale 을 허용하므로 압축되어도 문제없다.
	// 이번 의도의 전용 작업 디렉터리 <workDir>/tasks/<taskID>/i<intentID> 이며, 엔진 측에서 먼저 만든다.
	runDir := ensureRunDir(w.workDir, taskID, intent.ID)
	// The run-wide intent is not the current tool action. Do not forward it or
	// inherit a parent run's background into the action reviewer.
	ctx = intercept.WithReviewContext(ctx, runDir, intercept.ReviewBackground{})
	overview := renderWorkerGraphOverview(tsx.graphOverviewData())
	sysBody := workerSystem(w.proxyAddr, w.proxyCACert, w.workDir, runDir)
	if w.wantConstraints() {
		sysBody += constraintBlock(ts) // 작업 제약(있다면)을 시스템 프롬프트에 주입하며, worker 는 실행할 때 엄격히 지킨다
	}
	// 의도 블록 → 의도 고정 자산 블록 → 시작 지시를 차례로 system 끝에 덧붙인다(constraintBlock 과 같은 덧붙이기 방식).
	sysBody += renderIntentTask(intent)
	if as != nil {
		if ids := intentAssetIDs(intent); len(ids) > 0 {
			if assets, err := as.GetByIDs(ids); err == nil && len(assets) > 0 {
				if b, err := json.Marshal(assets); err == nil {
					sysBody += "\n\n이 의도의 asset_ids 에 해당하는 대상 자산:\n" + string(b)
				}
				// 의도가 명확히 겨냥한 이 자산들 → 작업 테스트 범위에 자동으로 포함한다(insertAssets 와 같은
				// 보수적 단위). upsertTaskScope 의 ON CONFLICT DO NOTHING + uq_task_scope
				// 고유 인덱스가 중복 추가를 막으며, 재실행/재시도도 마찬가지로 멱등 no-op 이다.
				// 자산 커버리지 기능이 꺼져 있으면 테스트 범위(분모)를 더 이상 누적하지 않는다.
				if coverageEnabled {
					for _, a := range assets {
						_ = as.AddAutoScope(taskID, a.Type, a.Domain, a.URL, a.IP)
					}
				}
			}
		}
	}
	sysBody += "\n\n위의 의도를 실행하기 시작한다: 그것만 하고, 사실, assets, finding 만 만들고, 끝나면 멈춘다."
	system, boundary := deferredSystem(sysBody+responseLanguageBlock(ctx), def)
	// 작업 단위 deadline(ctx 로 주입)이 이번 run 의 벽시계 예산을 조이고 마무리 문구를 결정한다(taskclock.go 참고).
	tc := taskClockFrom(ctx)
	maxDur, clamped := clampMaxDuration(tc.DeadlineUnix, w.runTimeout)
	settle := wrapupSettlement("worker", []string{"Bash"})
	if tc.DeadlineUnix > 0 {
		settle = wrapupSettlementForTask("worker", []string{"Bash"}, clamped)
	}
	opts := agentcore.Options{
		Provider:        w.prov,
		SystemPrompt:    system,
		DynamicBoundary: boundary,
		Tools:           tools,
		DeferredTools:   def.Deferred,
		UnlockSet:       def.Unlock,
		PermissionMode:  permission.ModeBypass,
		// WebFetch 는 기록 프록시를 거치므로 그 HTTP 는 curl 과 마찬가지로 기록된다. 프록시 CA 를 불러와 MITM 으로
		// 재서명된 HTTPS 인증서가 【정상적으로 검증 통과】되게 한다(검증을 끄는 것이 아님). proxy 가 비어 있으면 직접 연결.
		EnableWebFetch: true,
		WebFetchProxy:  w.proxyAddr,
		WebFetchCACert: w.proxyCACert,
		// 웹 검색(선택). ddgs 는 key 불필요, brave-free 는 BraveKey 필요, tavily 는 TavilyKey 필요.
		// WebSearchProxy 는 독립적인 출구 프록시(http/https/socks5)로, 트래픽을 기록하는 MITM 프록시와는 무관하다. 비어 있으면 직접 연결.
		EnableWebSearch:       w.webSearch.Enabled,
		WebSearchBackend:      w.webSearch.Backend,
		BraveSearchAPIKey:     w.webSearch.BraveKey,
		TavilySearchAPIKey:    w.webSearch.TavilyKey,
		DeepSeekSearchBaseURL: w.webSearch.DeepSeekBaseURL,
		DeepSeekSearchAPIKey:  w.webSearch.DeepSeekAPIKey,
		DeepSeekSearchModel:   w.webSearch.DeepSeekModel,
		WebSearchProxy:        w.webSearch.Proxy,
		// Bash 하위 명령의 HTTP 는 기본적으로 기록 프록시를 거치고 그 CA 를 신뢰한다(도구에 -x/-k 불필요).
		BashEnv:    proxyEnv(w.proxyAddr, w.proxyCACert),
		WorkingDir: runDir,
		MaxTurns:   w.maxTurns, // 0 = unlimited (configurable in agent management)
		// 벽시계 예산은 턴 경계에서 판정하며 도중에 끊지 않는다. 0 = 무제한. 작업 단위 deadline 이 있으면 min(자체 예산,
		// deadline 까지 남은 시간)으로 조여, 작업 시각이 되면 이번 run 이 자연스럽게 마무리에 들어가게 한다(taskclock.go 참고).
		MaxDuration: maxDur,
		// 예산(턴 수 OR 시간)에 도달 → SDK 가 마무리 턴을 한 번 실행해(Bash 숨김) 식별된 것을 기록하고 흐지부지 끝나지 않게 한다.
		// clamped(작업 deadline 으로 조여짐)일 때는 PromptByReason 을 쓴다: 시간 초과=작업 시각 도달→작업 시간 초과 문구,
		// 단계 수=조여진 구간 안에서 단계가 먼저 소진→per-run 문구로 돌아감. clamped 가 아니면 순수 per-run 을 유지한다.
		Settlement: settle,
		// large tool output spills to cmd-output/ with a head + pointer (SDK tool.Capture);
		// full output preserved on disk. 잘라내기 상한은 SDK 기본값(30000자)을 사용한다.
		ToolOutputDir: cmdOutDir(runDir),
		Compaction:    compactionConfig(w.compactionWindow()), // long tool-heavy runs stay within the window
		Todos:         actool.NewTodoStore(),                  // 대화 단위 임시 할 일(TodoWrite), 계획 전용이며 종료하면 버려짐
		NonStreaming:  w.nonStreaming(),                       // 이 profile 이 비스트리밍을 선택하면 Provider.Complete 사용
		MaxTokens:     w.maxTokens(),                          // 0 = 상한을 보내지 않고 서버 기본값에 맡김
	}
	if hooks != nil { // typed-nil guard: only set when concrete (avoids harness panic)
		opts.Hooks = hooks
	}
	if w.tx != nil { // persist raw LLM conversation; one file per worked intent
		opts.Transcript = w.tx
		opts.SessionID = WorkerSessionID(ts.ID(), intent.ID)
	}
	intentID := intent.ID
	emitWrap := func(r db.Activity) {
		if emit != nil {
			r.NodeID, r.Worker = &intentID, name
			emit(r)
		}
	}
	// 의도 / 시작 지시 / 의도 고정 자산은 이미 system prompt 로 전달되었다(위의 sysBody 조립 참고).
	// 이 시작 user 메시지는 【전역 상황 overview】만 담는다——품질을 낮춰도 되는 전체 그림 정보이므로 압축되어도 문제없다.
	// overview 가 드물게 marshal 에 실패해 비어 있으면, 첫 턴에 빈 user 메시지가 생기지 않도록 시작 문구 한 줄로 대체한다.
	input := overview
	if strings.TrimSpace(input) == "" {
		input = "system 에서 받은 의도를 실행하기 시작한다: 그것만 하고, 사실, assets, finding 만 만들고, 끝나면 멈춘다."
	}

	// 실험 기능: 켜면 noa 가 컨텍스트 압축을 맡는다(아카이브는 <workDir>/noa/<SessionID> 아래에 모이며 영구 보존).
	noaSession := WorkerSessionID(ts.ID(), intent.ID)
	enableNoa(&opts, w.noaEnabledFn, w.workDir, noaSession, noaWarn(noaSession))
	ctx = attachSideCapture(ctx, &opts)
	if err := AttachResearchRuntime(ctx, &opts, w.model, taskResearchFallback(w.workDir, taskID), renderIntentTask(intent)+"\n"+message); err != nil {
		return harness.ReasonModelError, tsx.Writes(), err
	}
	s := agentcore.NewSession(opts)
	defer s.Close() // release the session's background-task manager (temp dir + processes)

	// Resume prior conversation if this intent was paused/blocked/exhausted and is
	// being re-run. The transcript ID is deterministic per intent, so if a prior
	// session exists the worker continues from where it left off instead of
	// restarting from scratch.
	alreadyRecorded := false
	if w.tx != nil {
		if err := resumeSession(s, opts.SessionID); err != nil {
			return harness.ReasonModelError, tsx.Writes(), err
		}
		alreadyRecorded = requestID != "" && hasWorkerChatMessage(s.Messages(), requestID)
		if len(s.Messages()) > 0 && message == "" {
			seedUnlockFromHistory(s.Messages(), def.UnlockSkill)
			input = "계속 실행한다."
		} else if len(s.Messages()) > 0 {
			seedUnlockFromHistory(s.Messages(), def.UnlockSkill)
		}
	}
	if message != "" {
		if alreadyRecorded {
			input = "지난 수동 대화 입력으로 받은 새 의도를 계속 실행한다. 이미 완료한 동작은 반복하지 않는다."
		} else if len(s.Messages()) > 0 {
			input = workerChatMarker(requestID) + "\n【수동 대화로 입력된 새 의도】\n" + message +
				"\n\n이 수동 입력을 즉시 실행하고, 완료한 뒤 컨텍스트에 따라 원래 작업을 계속할지 결정한다."
		} else {
			input += "\n\n" + workerChatMarker(requestID) + "\n【수동 대화로 입력된 새 의도】\n" + message +
				"\n\n이 수동 입력을 우선 실행한다."
		}
	}

	// Budgets + settlement are owned by the SDK (MaxTurns/MaxDuration + Settlement):
	// on hit it runs a wrap-up turn and finishes with ReasonMaxTurns/ReasonTimeout.
	// MaxDuration now interrupts an in-flight tool at the wall-clock deadline and
	// enters the wrap-up phase on the live ctx, so a run whose tool overran the budget
	// still settles (no external hard-timeout backstop needed). ctx itself carries only
	// pause / planner kill / shutdown, which the engine distinguishes and re-queues/stops.
	_, reason, err := captureRunSession(ctx, s, input, emitWrap)
	return reason, tsx.Writes(), err
}
