package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// Planner is the event-driven LLM planner (docs §4.3): each time the asset or
// exploration graph changes (debounced), it reads the exploration route, queries
// assets, judges whether the task goal is met, and emits 0..N exploration intents
// into the frontier. It is the sole intent generator.
type Planner struct {
	findingRecorder   FindingRecorder
	prov              llm.Provider
	model             string
	tx                *transcript.Store                      // raw LLM conversation persistence (nil = off)
	window            int                                    // context window in tokens (for compaction)
	windowFn          func() int                             // optional dynamic task-chain minimum
	maxTurns          int                                    // max agent turns per run (0 = unlimited)
	killWork          func(intentID int64) error             // engine callback to terminate a running work (nil = off)
	steerWork         func(intentID int64, msg string) error // engine callback to steer a running work mid-run (nil = off)
	proxyAddr         string                                 // recording proxy for WebFetch (empty = direct)
	proxyCACert       string                                 // recording proxy's CA cert path (HTTPS verify)
	webSearch         WebSearchOpts                          // web_search tool backend selection (off by default)
	workDir           string                                 // shared work dir (surfaced in prompt as artifact-output target)
	injectConstraints func() bool                            // resolver: inject task operation constraints into system prompt? (nil = yes)
	nonStreamingFn    func() bool                            // resolver: use non-streaming (Complete) path? (nil = streaming)
	noaEnabledFn      func() bool                            // resolver: use experimental noa compaction? (nil = off)
	maxTokensFn       func() int                             // resolver: per-reply output cap (nil/0 = send no cap)
	compactor         *Compactor                             // cold-node compaction (§7); nil = disabled

	// todos keeps ONE plan-scratchpad per task (keyed by exploration id) so the
	// planner's multi-step plan survives across wake-ups — each Plan() is a fresh
	// session, but the shared store lets it record a serial exploit chain once and
	// dispatch it step-by-step over rounds instead of front-loading it in parallel.
	todoMu sync.Mutex
	todos  map[int64]*actool.TodoStore
}

func NewPlanner(prov llm.Provider, model, workDir string, tx *transcript.Store, window, maxTurns int) *Planner {
	return &Planner{prov: prov, model: model, workDir: workDir, tx: tx, window: window, maxTurns: maxTurns, todos: map[int64]*actool.TodoStore{}}
}

func (p *Planner) SetCompactionWindowResolver(fn func() int) { p.windowFn = fn }

// SetCompactor wires the cold-node compactor (cold-digest §7). Called each
// planner wake-up to advance the round counter, maintain cold stamps, and
// (off the hot path) fold cold nodes into digests. nil = feature disabled.
func (p *Planner) SetCompactor(c *Compactor) { p.compactor = c }

// SetNonStreaming wires a resolver deciding whether runs use the non-streaming
// model path (true = non-streaming). nil/unset = streaming (default).
func (p *Planner) SetNonStreaming(fn func() bool) { p.nonStreamingFn = fn }

func (p *Planner) nonStreaming() bool { return p.nonStreamingFn != nil && p.nonStreamingFn() }

// SetNoaEnabled wires a resolver deciding whether runs use the experimental noa
// context-compression mechanism. nil/unset = off (built-in compaction). Read per
// run so the settings toggle takes effect without rebuilding the agent.
func (p *Planner) SetNoaEnabled(fn func() bool) { p.noaEnabledFn = fn }

// SetMaxTokens wires a resolver for the per-reply output cap. nil/unset or 0 =
// send no cap and let the endpoint decide. Read per run, like nonStreaming.
func (p *Planner) SetMaxTokens(fn func() int) { p.maxTokensFn = fn }

func (p *Planner) maxTokens() int {
	if p.maxTokensFn == nil {
		return 0
	}
	return p.maxTokensFn()
}

func (p *Planner) compactionWindow() int {
	if p.windowFn != nil {
		return p.windowFn()
	}
	return p.window
}

// SetProxy points the planner's WebFetch at the recording proxy plus the CA cert
// it trusts to verify HTTPS through it (empty addr = direct).
func (p *Planner) SetProxy(addr, caCert string) { p.proxyAddr, p.proxyCACert = addr, caCert }

// SetWebSearch selects the web_search backend for the planner (off by default).
func (p *Planner) SetWebSearch(o WebSearchOpts) { p.webSearch = o }

// SetConstraintInject wires a resolver deciding whether this task's operation
// constraints get injected into the planner system prompt. Read per round so the
// settings toggle takes effect without rebuilding the agent. nil = inject (default).
func (p *Planner) SetConstraintInject(fn func() bool) { p.injectConstraints = fn }

// wantConstraints reports whether constraint injection is enabled (default yes).
func (p *Planner) wantConstraints() bool { return p.injectConstraints == nil || p.injectConstraints() }

// todoFor returns the task's persistent planning todo store, creating it on first
// use. Shared across all of this task's planner wake-ups.
func (p *Planner) todoFor(expID int64) *actool.TodoStore {
	p.todoMu.Lock()
	defer p.todoMu.Unlock()
	if p.todos == nil {
		p.todos = make(map[int64]*actool.TodoStore)
	}
	s := p.todos[expID]
	if s == nil {
		s = actool.NewTodoStore()
		p.todos[expID] = s
	}
	return s
}

// SetKillWork wires the engine's per-work terminate callback so the planner's
// kill_work tool can stop a single running worker.
func (p *Planner) SetKillWork(fn func(intentID int64) error) { p.killWork = fn }

// SetSteerWork wires the engine's per-work steering callback so the planner's
// steer_work tool can inject a mid-run course-correction into a running worker.
func (p *Planner) SetSteerWork(fn func(intentID int64, msg string) error) { p.steerWork = fn }

// renderPlannerTodos formats the persistent planning todo for injection into the
// wake-up prompt (empty when there are no todos yet — first wake-up).
func renderPlannerTodos(items []actool.Todo) string {
	if len(items) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n【너의 계획 할 일(깨어날 때마다 유지됨, 이전 턴에 네가 쓴 것)】:\n")
	for _, it := range items {
		mark := map[actool.TodoStatus]string{actool.TodoPending: "☐", actool.TodoInProgress: "▶", actool.TodoCompleted: "✔"}[it.Status]
		if mark == "" {
			mark = "☐"
		}
		b.WriteString(fmt.Sprintf("  %s %s\n", mark, it.Content))
	}
	b.WriteString("이를 바탕으로 진행한다: 【선행 단계가 완료되었거나 / 의존하는 fact 가 이미 존재하는】 다음 단계에만 의도를 분배한다. TodoWrite 로 목록을 갱신한다(이미 fact 로 충족된 단계는 completed 로 표시). 목록에서 이미 pending/in_progress 인 단계를 중복 분배하지 않는다.")
	return b.String()
}

// TriggerEvent describes what concretely caused this planning round to fire, so
// the planner looks first at the actual change instead of re-scanning the whole
// overview. Kind:
//
//	"done"    — a worker finished intent IntentID (its output conclusion is fetched).
//	"finding" — a worker reported a finding on intent IntentID (Detail = 요약).
//	"goal"    — the human (via 메인 agent 의 set_goals) added one OR MORE goals in a
//	            single call (Goals = 이번에 추가된 목표 텍스트, 1건 이상. set_goals 는 일괄 지원).
//	"goal_deleted" — the human deleted a goal from 개요의 목표 관리 (Detail = 삭제된 목표 텍스트).
//	"goal_edited"  — the human edited a goal from 개요의 목표 관리 (OldGoal→NewGoal 텍스트).
//	"cancelled" — the human deleted intent IntentID (Detail = 삭제 사유). The intent is
//	            stopped (not deleted) and the reason is attached to it as a fact.
type TriggerEvent struct {
	Kind     string
	IntentID int64
	Detail   string
	Summary  string   // Kind=="cancelled" 전용: 삭제 전에 캡처한 의도 요약(실제 삭제 후에는 노드가 없어 다시 조회할 수 없음)
	Goals    []string // Kind=="goal" 전용: 이번 set_goals 로 추가된 목표 텍스트(1건 또는 여러 건)
	OldGoal  string   // Kind=="goal_edited" 전용: 수정 전 목표 텍스트
	NewGoal  string   // Kind=="goal_edited" 전용: 수정 후 목표 텍스트
	Hints    []string // Kind=="hint" 전용: 이번 add_hint 로 추가된 힌트 텍스트(1건 또는 여러 건)
}

// renderTriggers spells out the change(s) that fired this round: for a finished
// worker — which intent + its output conclusion; for a finding — which intent +
// what was found. Empty for time/heartbeat wakes. Reads the store (best-effort;
// a blank field never blocks the round).
func renderTriggers(ts *db.ExplorationStore, evs []TriggerEvent) string {
	if len(evs) == 0 || ts == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n【이번 턴을 트리거한 실제 변경(먼저 여기를 보고 방향 보충 여부를 결정)】:")
	for _, ev := range evs {
		switch ev.Kind {
		case "goal":
			if len(ev.Goals) == 1 {
				b.WriteString(fmt.Sprintf("\n- 사람(메인 agent)이 목표를 하나 추가함: %s —— 새로 달성해야 할 목표이므로, 이에 맞춰 탐색 방향을 보충할 것(아직 해당 의도가 없다면).", ev.Goals[0]))
			} else {
				b.WriteString(fmt.Sprintf("\n- 사람(메인 agent)이 목표 %d 개를 추가함: %s —— 모두 새로 달성해야 할 목표이므로, 아직 해당 의도가 없는 목표마다 탐색 방향을 보충할 것.", len(ev.Goals), strings.Join(ev.Goals, "; ")))
			}
		case "hint":
			if len(ev.Hints) == 1 {
				b.WriteString(fmt.Sprintf("\n- 사람(메인 agent)이 전략 힌트를 하나 추가함: %s —— 탐색 그래프에 이미 연결되었으므로, 이에 맞춰 탐색 방향을 조정/보충할 것(아직 해당 의도가 없다면).", ev.Hints[0]))
			} else {
				b.WriteString(fmt.Sprintf("\n- 사람(메인 agent)이 전략 힌트 %d 개를 추가함: %s —— 모두 탐색 그래프에 연결되었으므로, 하나씩 이에 맞춰 탐색 방향을 조정/보충할 것.", len(ev.Hints), strings.Join(ev.Hints, "; ")))
			}
		case "goal_deleted":
			b.WriteString(fmt.Sprintf("\n- 사람이 이 목표를 삭제함: %s —— 이 목표는 제거되었으므로, 이에 맞춰 남은 목표/방향을 다시 판단할 것(이 목표를 위해 의도를 분배할 필요 없음).", ev.Detail))
		case "goal_edited":
			b.WriteString(fmt.Sprintf("\n- 사람이 목표를 「%s」에서 「%s」(으)로 수정함 —— 새 목표에 맞춰 탐색 방향을 조정할 것(기존 방향이 더는 맞지 않으면 분배를 중지).", ev.OldGoal, ev.NewGoal))
		case "finding":
			b.WriteString(fmt.Sprintf("\n- 의도 #%d(%s)의 worker 가 finding 을 하나 보고함: %s", ev.IntentID, intentSummary(ts, ev.IntentID), ev.Detail))
		case "cancelled":
			// 의도 내용은 삭제 시 캡처한 Summary 를 우선 사용한다(실제 삭제 후에는 노드가 없어 intentSummary 로 찾을 수 없음).
			sm := ev.Summary
			if sm == "" {
				sm = intentSummary(ts, ev.IntentID)
			}
			b.WriteString(fmt.Sprintf("\n- 의도 #%d 는 사용자가 삭제함. 의도 내용: %s, 삭제 사유: %s. 이 의도는 삭제되었으므로(더 이상 실행하지 않음) 이에 맞춰 다시 계획할 것.", ev.IntentID, sm, ev.Detail))
		default: // "done"
			b.WriteString(fmt.Sprintf("\n- 의도 #%d(%s)의 worker 가 종료하며 결론을 출력함: %s", ev.IntentID, intentSummary(ts, ev.IntentID), workerOutput(ts, ev.IntentID)))
			if fids := factIDsYielded(ts, ev.IntentID); fids != "" {
				b.WriteString(fmt.Sprintf("; 이 의도가 새로 만든 사실 id: %s ", fids))
			}
		}
	}
	b.WriteString("\n(전체 세부 사항은 node_detail / get_worker_output / list_findings 로 다시 조회할 수 있음.)")
	return b.String()
}

// factIDsYielded lists the fact ids an intent produced this run as "#12、#15", so the
// planner can jump straight to the round's incremental facts. Empty (best-effort) when
// the intent yielded no facts or the lookup fails.
func factIDsYielded(ts *db.ExplorationStore, id int64) string {
	ids, err := ts.FactsYielded(id)
	if err != nil || len(ids) == 0 {
		return ""
	}
	parts := make([]string, len(ids))
	for i, fid := range ids {
		parts[i] = fmt.Sprintf("#%d", fid)
	}
	return strings.Join(parts, "、")
}

// intentSummary reads an intent node's one-line summary (best-effort, "?" on miss).
func intentSummary(ts *db.ExplorationStore, id int64) string {
	n, err := ts.GetNode(id)
	if err != nil || n == nil {
		return "?"
	}
	var p map[string]any
	if json.Unmarshal(n.Payload, &p) == nil {
		if s, ok := p["summary"].(string); ok && s != "" {
			return s
		}
	}
	return "?"
}

// workerOutput returns the finished worker's conclusion for an intent — the last
// 'result' (else 'text') activity's full detail, truncated. Same source get_worker_output uses.
func workerOutput(ts *db.ExplorationStore, id int64) string {
	acts, _, err := ts.ActivityList(&id, 0, 1000)
	if err != nil {
		return "(출력 가져오기 실패)"
	}
	var pick *db.Activity
	for i := range acts {
		if acts[i].Kind == "result" {
			pick = &acts[i]
		} else if acts[i].Kind == "text" && pick == nil {
			pick = &acts[i]
		}
	}
	if pick == nil {
		return "(이 work 에는 아직 출력 기록이 없음)"
	}
	out, _ := ts.ActivityDetail(pick.ID)
	if out == "" {
		out = pick.Summary
	}
	return truncOutput(out, 800)
}

// truncOutput caps a worker-output blob so the trigger context doesn't bloat the
// system prompt every round; full text is one get_worker_output call away.
func truncOutput(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + " …(잘림, 전체는 get_worker_output 참고)"
}

// renderGraphOverview folds the pre-computed graph_overview snapshot into the
// wake-up prompt so the planner starts each round with the full situation in
// hand — saving the round-trip it would otherwise spend calling the tool. It is
// the exact same JSON graph_overview would return; deeper detail is still one
// tool call away (node_detail / list_facts / …).
func renderGraphOverview(data map[string]any) string {
	b, err := json.Marshal(data)
	if err != nil {
		return "" // fall back to the model calling graph_overview itself
	}
	return "\n\n【이번 턴 상황(graph_overview 를 미리 가져온 것으로, 네가 그 도구를 호출한 반환값과 같다. 세부 사항이 필요하면 node_detail/list_facts 등을 필요에 따라 호출)】:\n" + string(b)
}

// plannerDefaultTmpl is the built-in EDITABLE body (구간 [A]) of the planner prompt,
// seeded into agent_prompts. Goal is a {{.Goal}} template var; the 중간 산출물 출력 규칙
// tail is code-owned (artifactSpec) and appended by plannerSystem after rendering.
const plannerDefaultTmpl = `너는 사이버 보안 플랫폼의 권한이 부여된 침투 테스트 시스템의 "플래너"이며, 자주 깨워진다(그래프가 바뀌면 바로 깨워짐). 역할: 상황 읽기 → 목표 판정 → **아직 다뤄지지 않은 새 방향이 정말 있을 때만** 탐색 의도를 보충한다. 너는 플래너이지 실행자가 아니다: 이번 턴의 모든 산출물은 【의도 생성/설명】 또는 【목표 판정】뿐이며, plan 안에서 일을 직접 해치우면 절대 안 된다.

작업 목표: {{.Goal}}

**이번 턴에 의도를 몇 개 내야 하는가(먼저 이것부터 분명히 할 것)**:
- **절대 하한(최우선)**: 【목표가 달성되지 않았고】 【현재 open 이나 running 의도가 하나도 없다면】(frontier_open=0 이고 running_intents 가 비어 있음), 이번 턴에 목표를 향해 나아가는 의도를 【반드시】 하나 이상 내야 한다——기다릴 실행 중 work 도 없고 대기 중인 방향도 없을 때 의도 0개 = 작업 정지다. 알려진 방향이 모두 recent_done 에만 있더라도 아래 done/exhausted/blocked 판단에 따라 새로 하나 열거나 이어서 하나를 분배해야 한다.
- 절대 하한 외에는 **의도 0개도 정상적인 결과지만 정당한 이유가 있어야 한다**("적게 내는 게 안전하다"는 기본값이 아니다): ①**이미 다뤄짐**——네가 떠올린 방향이 모두 아직 open/running 인 의도에서 처리되고 있다(표현만 바꿔 이미 있는 의도를 다시 만드는 것은 심각한 오류다). ②**의존성 대기**——다음 단계가 현재 실행 중인 work 의 산출물에 의존하는데 아직 나오지 않았다(이때 억지로 분배하면 하위 단계가 선행 결과를 못 받아 헛돌므로, 다음에 깨어나 그래프가 갱신된 뒤 분배해야 한다).
- 반대로: 【다뤄지지 않았고 실행 중 work 에 의존하지 않는】 새 방향이 정말 있거나, 목표가 미달성이고 범위 안에 아직 테스트하지 않은 면이 있으면 분배해야 한다——의도 0개를 게으른 기본값으로 삼지 않는다.

**깨어날 때마다의 결정 절차**:

1. **전체 상황이 이 프롬프트 아래에 첨부되어 있다**(graph_overview 의 반환값이므로 다시 호출할 필요 없음): task(원래 제목+목표/루트 노드), 자산 수, goals+상태, open/running/recent_done 의도, sites_without_endpoints(엔드포인트가 없는 사이트로, 탐색할 방향일 수 있음), facts(탐색 사실 수로 취약점과는 별개), recent_facts({id,summary,confidence?}).
   - **범위**: 탐색 노드(goals/의도/facts/findings)는 이 작업 것만 포함한다. **자산 그래프는 전역 공유**다(여러 작업이 같은 것을 쓰며, 자산 수는 전역 범위 기준이지 이 작업만의 것이 아니다)——이 작업과 무관한 자산이 보이면 무시한다.
   - **계보**: 각 의도에는 parents(상위: 어떤 사실/의도에서 파생되었는지)와 yields(하위: 어떤 사실/발견을 만들었는지)가 있고, recent_facts 에는 각각 from_intent 가 있다. 이를 바탕으로 "어떤 사실이 어느 방향에서 왔는지, 이를 종합해 새 방향을 낼 수 있는지"를 이해한다.
   - **부정/의심 관찰**(recent_facts 의 "포트 닫힘/주입 불가" 등)은 worker 의 관찰이지 확정이 아니다: 받아들이기 전에 node_detail(id) 로 evidence 를 본다——evidence 가 탄탄하고 confidence=observed 이며 수단을 다 써 본 경우에만 그 방향이 일시적으로 막힌 것으로 본다. evidence 가 없거나, "그래 보인다/한 번만 탐색"이거나, confidence=inferred 인 것은 【아직 밝혀지지 않음】으로 다루며, 범위 안이고 다른 의도가 다루지 않는다면 기본적으로 재검증 의도를 하나 분배해 입증하거나 뒤집는다(**같은 부정 방향은 최대 한 번만 재검증**. 재검증 후에도 부정이고 증거가 타당하면 그 결론을 존중하고 다시 분배하지 않는다).
   - **더 깊은 세부 사항이 필요할 때만 필요에 따라 호출**: list_facts(페이지 단위, 최신순, 기본 20, q 필터, before 로 페이지 이동, total/has_more 포함), list_findings(모든 취약점), node_detail(id)(전체 증거/상세. 목록/recent_facts 는 요약만 줌), list_assets(pull: q 검색, type/company_id/task_id 필터, 페이지 단위, 또는 id/ids 로 직접 조회), asset_neighbors. 자산은 전역 공유이므로 기본적으로 전체를 가져오지 않는다.

2. **목표 판정(핵심 역할)**: goals 필드에 목표와 상태가 있다. 어떤 발견/사실로 이미 증명된 미달성 목표는 prove_goal(goal_id, evidence_id, reason) 을 호출해 met 으로 표시한다. **네가 표시한 것이 마지막 미완료 목표라면 시스템이 작업 전체를 완료로 자동 판정한다**——마무리는 prove_goal 을 하나씩 호출하는 것으로만 이뤄지며 다른 "원클릭 완료" 수단은 없다.
   - ⚠️ **정량 검수 확인(섣불리 도장 찍기 엄금)**: 목표에 정량 조건(커버리지 X% 달성, 지정 수량 도달, 특정 권한 획득)이 있으면 prove_goal 전에 위 graph_overview 의 실측값(coverage.pct, findings_total 수 등)을 【반드시】 확인한다. 기준에 못 미치면 prove_goal 은 【금지】이며 부족분을 메울 의도를 분배한다. "대체로 달성/핵심은 확보" 같은 이유로 미리 met 을 표시하면 안 된다. 예: 커버리지 100% 요구인데 실측 coverage.pct=40% → 미달성, 보충 테스트 의도를 계속 분배.

3. **(선택, 시작 시에만, 아주 가볍게) 이해를 위한 탐지**: 그래프에 fact 가 거의 없고(recent_facts 가 사실상 비어 있고 작업이 막 시작됨), 상황만으로는 초기 의도를 구체적으로 말할 수 없을 때만 Bash 등으로 대상에 아주 적은 읽기 전용 탐지를 한다(예: curl 1–2회로 첫 페이지/지문 확인). **유일하게 허용되는 산출물은 더 정확한 의도 설명 한 문장**이다——취약점의 발견/검증/익스플로잇도 아니고, 엔드포인트/디렉터리/파라미터 열거 결과도 아니다(그것은 worker 의 일이므로 의도로 써서 내려보낸다). 세 가지 절대 경계:
   - 그래프에 worker 가 만든 fact 가 이미 있으면(facts>0 / recent_facts 가 비어 있지 않음) → 직접 탐지하는 것을 【금지】하며, 모든 판단은 기존 fact 에 근거한다. 이번 턴의 산출물은 "새 의도 분배" 또는 "종료"뿐이다. 어떤 단서를 깊이 파고 싶다면 → 직접 curl 하지 말고 의도를 분배해 worker 가 조사하게 한다.
   - 시작 시라도 탐지는 최대 3회 이하로 끝내며, 초기 의도를 분명히 하기 위한 것뿐이다. "빠르게 방향 정하기"가 아니라 "깊이 검증하기"를 하고 있다는 것을 깨달으면(엔드포인트/디렉터리를 하나씩 열거, id 를 하나씩 시도, 디코딩 체인, 같은 인터페이스를 반복 탐지, 모든 주입/권한 우회/취약점 테스트 검증——전부 worker 의 중노동이다) 즉시 멈추고 의도로 쓴다.
   - 기존 사실/상황으로 판단할 수 있다면 탐지할 필요가 전혀 없다.

4. **어떤 새 방향을 보충할지 결정**: **여기서 말하는 "절제"는 【이미 있는 의도를 반복하지 않는 것】만을 뜻하며, "가능하면 적게 분배"가 아니다**——목표가 미달성일 때 기본 질문은 "목표에 다가가기 위해 아직 다뤄지지 않은 더 깊고 더 강한 공략이 무엇인가"이지 "마무리해도 되는가"가 아니다. 의도는 【열린 탐색 방향】이며(고정된 유형/메뉴가 아님), 알려진 사실, 자산, 목표를 종합해 스스로 방향을 정하고, open + running + recent_done 과 하나씩 비교한다:
   - 이미 open/running 이 다루고 있으면 → 다시 만들지 않는다(처리 중).
   - recent_done 에 나온 적이 있으면 → **먼저 그 의도의 state(각각 있음)를 보고 어떻게 멈췄는지 구분한 뒤 결정한다**:
     · **done(정상적으로 끝남)**: 이미 다뤄짐 → 그대로 다시 분배하지 않는다. 막다른 길인지는 state 가 아니라 그것이 yields 로 낸 fact 의 결론을 보고 판단한다. 【실질적인 새 메커니즘】(새 사실/자산/파라미터/확연히 다른 공략)이 나타났을 때만 다시 분배하며, summary 에 지난번과 무엇이 다른지 분명히 쓴다. 표현만 바꾸거나 "한 번 더 해 보면 될지도"는 해당하지 않으며 재시도는 금지다.
     · **exhausted(예산 소진, 탐색 도중 끊겨 일부만 기록됨) / blocked(모델이나 네트워크 실패로 거의 탐색하지 못함)**: 둘 다 중간에 제대로 끝나지 못해 정보가 불완전하다——먼저 get_worker_trace / get_worker_output 으로 실제로 어디까지 했고 어디서 막혔는지 본 뒤 아래에서 고른다: 돌파 직전에 예산에 끊김 → "지난 진행에 이어서 계속"을 분배. 순수한 외부 장애로 실행되지 못함(blocked 는 대개 이 경우) → 같은 방향을 그대로 다시 분배. 매번 같은 곳에서 막힘 → 공략/방향을 바꾼다. 근거는 언제나 trace 의 실제 진행이지 state 자체가 아니다.
   - 어떤 의도도 전혀 다루지 않는 완전히 새로운 방향 → 생성한다.
   - 알려진 방향이 모두 아직 open/running 인 의도로 다뤄지고 있으면 → 생성하지 않고 바로 종료한다(실행 중/대기 중인 work 가 있으니 진행을 기다린다). 다만 recent_done 만 다루고 있고 open/running 이 없으며 목표가 미달성이라면 → 맨 위의 절대 하한에 따라 반드시 새로 열거나 이어서 분배한다.
   - **깊이가 커버리지보다 우선**: coverage 는 하한/검수 항목이지 탐색 목표 자체가 아니다. 가치가 높은 진입점(RCE/권한 상승/데이터 유출로 이어질 수 있음)을 발견하면, 커버리지를 고르게 맞추려고 넓게 펼치거나 자산마다 얕게 테스트하기보다 그 경로를 【깊이 끝까지 파고드는】 의도를 우선 분배한다.
   - **경로를 다양하게 유지하고 너무 일찍 수렴하지 않는다**: 목표가 미달성인데 기존 의도가 모두 같은 경로/진입점에 몰려 있고 【본질적으로 다른】 다뤄지지 않은 방향(다른 진입면/다른 유형의 자산/다른 익스플로잇 체인)이 있다면, 같은 선에 같은 뜻의 의도를 더하기보다 그 갈라진 방향을 우선 보충한다(표현이 아니라 실질적 차이를 본다). 그 갈라진 방향이 이미 기존 의도로 다뤄지고 있다면 여전히 생성하지 않는다. 이상적으로는 메커니즘이 다른 경로 2–3개가 공존하며(예: "업로드 체인으로 공략"과 "인증 우회로 공략"), 어느 하나가 【목표에 근접한】 증거를 내놓은 뒤에야 자원을 그쪽으로 집중한다. **다만 다양성은 언제나 맨 위의 【작업 제약】을 따른다**: 제약으로 제외된 진입면/포트/호스트/작업은 본질적으로 다르더라도 절대 의도를 만들지 않는다.

   **직렬 익스플로잇 체인: 단계별로 분배하고 병렬로 쪼개지 않는다.** 강하게 의존하는 직렬 체인(①→②→③, 뒤 단계가 앞 단계의 실제 산출물에 의존)은 한 번에 병렬로 내려보내지 않는다(하위 단계가 아직 없는 선행 결과를 받지 못해 반복/헛돌기만 한다). TodoWrite 로 체인 전체를 할 일로 기록하고(단계마다 하나), 이번 턴에는 "선행 조건이 충족된" 단계(보통 첫 단계)만 분배한다. 그 단계가 fact 를 낸 뒤 다음에 깨어나면(프롬프트에 할 일 목록이 함께 온다) 다음 단계를 분배하고 충족된 것은 completed 로 표시한다. "같은 일"을 두 개로 나누지 않는다("트리거 지점 확인"과 "트리거 지점 발동"은 같은 단계다). 【평행하고 서로 의존하지 않는】 차원(예: 서로 무관한 여러 엔드포인트 열거)일 때만 여러 의도를 병렬로 쓴다.

5. **제출**: 【한 번의】 add_intent 로 골라낸 새 방향을 일괄 제출한다(intents 배열, 가치가 가장 높은 최대 4개, 하나씩 여러 번 호출하지 않음):
   - **summary**: 그 방향을 자연어 한 문장으로 설명한다(테스트 대상의 전체 주소 + 무엇을 할지 + 왜). 고정 분류를 씌우지 않는다. 중복 제거는 주로 이것을 기존 의도와 비교해 이뤄진다.
   - **asset_ids**: 이 방향에서 테스트/공격할 대상 자산 id(가능한 한 넘긴다. 0/1/여러 개, list_assets 에서 가져옴)——방향이 구체적인 자산(사이트/인터페이스/파라미터/호스트)을 중심으로 한다면 반드시 넘겨 커버리지 중복 제거와 자산 체인 연결에 쓰게 하고, 여러 자산에 걸치면 모두 넘긴다. 구체적인 자산이 없는 순수 전역 정찰일 때만 비워 둔다.
   - **parent_ids**: 이 방향이 어떤 상위 노드들을 종합해 나온 것인지(선택, 0/1/여러 개)——여러 사실을 결합해 의도 하나가 나왔다면 모두 넘기고, 어떤 상위 의도/발견에서 파생되었다면 그 id 도 넘긴다. 최상위의 완전히 새로운 방향은 비워 둔다.

중복하지 않고 억지로 채우지 않는다. 다만 목표가 미달성이고 다뤄지지 않은 더 깊은 공략이 있다면 분배해야 할 때 분배한다. 간결하게, 집중해서, 효율적으로.`

func plannerSystem(goal, dataDir, workDir string) string {
	body := renderSystem("planner", plannerDefaultTmpl, PlannerVars{Goal: goal, DataDir: dataDir, Now: nowStr(), NowTurn: nowStr()})
	return body + artifactSpec(workDir)
}

// Plan runs one planning round. emit, if non-nil, receives the planner's execution
// steps (so users can see how it reads the situation and judges goals — the
// planner is the intent generator and was previously a black box). Returns whether
// the planner judged the goal met.
// triggers carries the concrete change(s) that fired this round — worker(s) done
// and/or finding(s) reported (may be several — the engine debounces a burst; empty
// for time/heartbeat wakes). They are spelled out at the top of the prompt so the
// planner looks first at the actual change (which intent, its output/finding).
func (p *Planner) Plan(ctx context.Context, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, goal string, triggers []TriggerEvent, emit func(db.Activity)) (met bool, reason string, err error) {
	// cold-digest §2.3/§7: advance this task's planner-round counter, maintain the
	// cold_since_round stamps, and (if a threshold is hit) kick off background
	// compaction. Synchronous part is cheap (a few queries); the LLM compaction
	// runs in a detached goroutine so it never adds latency to this round.
	p.compactor.OnPlannerRound(ctx, ts)
	tsx := NewToolSet(ts, "planner")
	tsx.SetFindingRecorder(p.findingRecorder)
	if as != nil {
		tsx.SetAssetStore(as, as.Companies())
	}
	tsx.SetTaskID(taskID)
	tsx.SetCoverageEnabled(as == nil || as.CoverageEnabled(taskID))
	tsx.killWork = p.killWork   // enable kill_work tool (nil = unavailable)
	tsx.steerWork = p.steerWork // enable steer_work tool (nil = unavailable)
	if origin, _ := ts.OriginFactID(); origin > 0 {
		tsx.SetOwnerNode(origin) // planner-side anchors default to the task root (origin fact)
	}
	// 도메인 도구 + 기본 도구 세트(Read/Write/Edit/MultiEdit/LS/Glob/Grep/Bash)
	// 자산 커버리지 기능이 꺼져 있으면 add_task_scope/list_untested_assets 를 제외한다(prompt 에 넣지 않음).
	base := append(tsx.DropCoverageTools(tsx.PlannerTools()), actool.DefaultTools()...)
	ctx = WithRunInfo(ctx, RunInfo{TaskID: taskID, ExplorationID: explorationID(ts)})
	tools, def, cleanup := AugmentTools(ctx, "planner", base)
	defer cleanup()
	// 핵심 상황(방금 끝난 의도 + 미리 가져온 전체 그래프)은 【이번 턴 user 입력】으로 옮겼고(아래 input 참고), system
	// 에는 정적인 계획 본문만 남긴다. move-out 으로 system 이 매 턴 안정되어 캐시에 더 유리하다. 대가는 한 턴이 길어지면 상황이
	// compaction 으로 압축될 수 있다는 것이다(planner 는 한 턴이 보통 짧아 위험이 낮음). situational 은 아래 input 에 이어 붙는다.
	situational := renderTriggers(ts, triggers) + renderGraphOverview(tsx.graphOverviewData())
	// 작업 단위 deadline / 최종 단계 모드(ctx 로 주입, taskclock.go 참고). 최종 단계의 그 턴에는 작업 시간 초과
	// planner 마무리 문구를 【이번 턴 작업 지시】로 이번 턴 user 입력에 이어 붙여(situational 과 함께), 마지막
	// 목표 판정만 하고 새 의도는 만들지 않게 한다.
	tc := taskClockFrom(ctx)
	if tc.Final {
		situational += "\n\n【작업 최종 마무리(이번 턴 특별 지시, 위의 일반 계획 절차를 대체함)】: " + resolveTaskTimeoutWrapup("planner")
	}
	// 이 작업의 작업 디렉터리 <workDir>/tasks/<taskID> 를 먼저 만든다.
	taskDir := ensureRunDir(p.workDir, taskID, 0)
	ctx = intercept.WithReviewContext(ctx, taskDir, intercept.ReviewBackground{})
	sysBody := plannerSystem(goal, p.workDir, taskDir)
	if p.wantConstraints() {
		sysBody += constraintBlock(ts) // 작업 제약(있다면)을 시스템 프롬프트에 주입해 탐색 경계를 정함
	}
	system, boundary := deferredSystem(sysBody+responseLanguageBlock(ctx), def)
	// planner 는 자체 벽시계 예산이 없다. deadline 이 있으면 MaxDuration 을 남은 시간으로 조여, 실행 중인 계획 턴이
	// 작업 시각에 도달하면 마무리에 들어가게 한다(시간 초과 → 작업 시간 초과 문구, 단계 수 → per-run 문구).
	maxDur, clamped := clampMaxDuration(tc.DeadlineUnix, 0)
	settle := wrapupSettlement("planner", nil)
	if tc.DeadlineUnix > 0 {
		settle = wrapupSettlementForTask("planner", nil, clamped)
	}
	opts := agentcore.Options{
		Provider:        p.prov,
		SystemPrompt:    system,
		DynamicBoundary: boundary,
		Tools:           tools,
		DeferredTools:   def.Deferred,
		UnlockSet:       def.Unlock,
		PermissionMode:  permission.ModeBypass,
		EnableWebFetch:  true, // 기록 프록시를 거쳐 흔적을 남김. MITM 으로 재서명된 HTTPS 인증서를 검증하도록 프록시 CA 를 불러옴
		WebFetchProxy:   p.proxyAddr,
		WebFetchCACert:  p.proxyCACert,
		// 웹 검색(선택). ddgs 는 key 불필요, brave-free 는 BraveKey 필요, tavily 는 TavilyKey 필요.
		// WebSearchProxy 는 독립적인 출구 프록시(http/https/socks5)로, 트래픽을 기록하는 MITM 프록시와는 무관하다. 비어 있으면 직접 연결.
		EnableWebSearch:       p.webSearch.Enabled,
		WebSearchBackend:      p.webSearch.Backend,
		BraveSearchAPIKey:     p.webSearch.BraveKey,
		TavilySearchAPIKey:    p.webSearch.TavilyKey,
		DeepSeekSearchBaseURL: p.webSearch.DeepSeekBaseURL,
		DeepSeekSearchAPIKey:  p.webSearch.DeepSeekAPIKey,
		DeepSeekSearchModel:   p.webSearch.DeepSeekModel,
		WebSearchProxy:        p.webSearch.Proxy,
		BashEnv:               proxyEnv(p.proxyAddr, p.proxyCACert), // Bash 하위 명령은 기본적으로 프록시 사용 + CA 신뢰
		WorkingDir:            taskDir,                              // 이 작업의 작업 디렉터리 <workDir>/tasks/<taskID>
		ToolOutputDir:         cmdOutDir(taskDir),
		MaxTurns:              p.maxTurns, // 0 = unlimited (configurable in agent management)
		MaxDuration:           maxDur,     // 0=무제한. deadline 이 있으면 = deadline 까지 남은 시간
		Compaction:            compactionConfig(p.compactionWindow()),
		// 깨어날 때마다 공유되는 계획 할 일: 직렬 체인이 여러 턴에 걸쳐 유지되게 한다(session 은 새것이지만 store 는 아님).
		Todos: p.todoFor(ts.ID()),
		// 【이번 턴】 단계 수 예산에 도달 → SDK 가 마무리 실행: 계획을 멈추는 것이 아니라 이번 턴에 정리된 결론을 반영한다(분배할 add_intent,
		// 증명할 수 있는 prove_goal, 직렬 체인은 TodoWrite 에 기록)——planner 는 이후에도 계속 깨워진다.
		// clamped(작업 deadline 으로 조여짐)일 때는 PromptByReason 을 쓴다(wrapupSettlementForTask 참고).
		Settlement:   settle,
		NonStreaming: p.nonStreaming(), // 이 profile 이 비스트리밍을 선택하면 Provider.Complete 사용
		MaxTokens:    p.maxTokens(),    // 0 = 상한을 보내지 않고 서버 기본값에 맡김
	}
	if p.tx != nil { // persist raw LLM conversation; one accumulating file per task's planner
		opts.Transcript = p.tx
		opts.SessionID = fmt.Sprintf("exp%d-planner", ts.ID())
	}
	// 실험 기능: 켜면 noa 가 컨텍스트 압축을 맡는다(아카이브는 <workDir>/noa/<SessionID> 아래에 모이며 영구 보존).
	noaSession := fmt.Sprintf("exp%d-planner", ts.ID())
	enableNoa(&opts, p.noaEnabledFn, p.workDir, noaSession, noaWarn(noaSession))
	if attachErr := AttachResearchRuntime(ctx, &opts, p.model, taskDir, goal); attachErr != nil {
		return false, "", attachErr
	}
	// 상황(방금 끝난 의도 + 전체 그래프)은 이제 이번 턴 user 입력에 이어 붙인다(아래 input 참고). user 에는
	// 지시 + 깨어날 때마다 유지되는 할 일도 있다(todo 는 모델 자신의 계획 메모로 다시 만들 수 있으므로 user 에 두면 된다).
	// 시작 문구는 「이번 턴에 구체적인 변경이 있는지」에 따라 두 가지다: 변경 있음 → 아래 【실제 변경】 블록을 가리킴. 변경 없음
	// (하트비트 정기 점검 / hint / 재개 등) → "그래프가 바뀌었다"고 거짓말하지 말고, 대신 실행 중인 의도도 함께 재점검하라고 안내한다.
	lead := "방금 구체적인 변경이 있었다(아래 【이번 턴을 트리거한 실제 변경】 참고). 이를 바탕으로 다음 단계를 계획한다: "
	if len(triggers) == 0 {
		lead = "이번 턴은 **정기 점검(하트비트 시각 도달)/구체적인 변경 신호 없음** 으로 깨어난 것이다——그래프에 새 변경이 없을 수도 있다. 실행 중인 의도도 함께 재점검한다: 오랫동안 진전이 없거나 빗나간 것은 steer_work 로 바로잡고, 방향이 완전히 틀린 것은 kill_work 로 손실을 막는다. 그다음 목표를 판정하고 방향 보충 여부를 결정한다: "
		// 하트비트/변경 없음으로 깨어났을 때 그래프 전체에 open 이나 running 의도가 하나도 없으면 → 탐색이 멈춘 것이다(실행 중인 worker 도,
		// 대기 중인 방향도 없음). planner 에 이를 명확히 알리고 이번 턴에 새 방향을 반드시 보충하게 하며, 실행 중 의도만 재점검하고 한 턴을 헛돌지 않게 한다.
		if active, err := ts.HasActiveIntent(); err == nil && !active {
			lead = "이번 턴은 **정기 점검(하트비트 시각 도달)** 으로 깨어난 것이며, 현재 **open 이나 running 의도가 하나도 없다**——실행 중인 worker 도, 대기 중인 방향도 없어 탐색이 멈췄다. 이번 턴에 목표를 향해 나아가고 그래프의 기존 의도와 **서로 중복되지 않는** 새 의도를 하나 이상 **반드시** 내야 한다(의도 0개는 안 됨). 먼저 아래 상황으로 목표 달성 여부를 판정하고, 미달성이면 즉시 방향을 보충한다: "
		}
	}
	input := lead + situational + "\n\n위 상황을 바탕으로 목표를 판정한다. 목표가 【실제로 달성】되었을 때(목표 성과를 얻었거나/목표 취약점을 확인함) prove_goal 로 하나씩 표시한다. **절대 하한: 목표가 아직 미달성이고 현재 open 이나 running 의도가 하나도 없다면(frontier_open=0 이고 running_intents 가 비어 있음), 이번 턴에 목표를 향해 나아가는 의도를 반드시 하나 이상 내야 한다——이때는 기다릴 실행 중 work 도 대기 중인 방향도 없으므로 의도 0개 = 작업 정지다. 기존 open/running 의도가 진행 중이거나 목표가 이미 달성되었을 때만 이번 턴에 새 의도를 내지 않아도 된다.**" +
		renderPlannerTodos(opts.Todos.List())
	// MaxDuration 은 이제 벽시계 시각에 도달하면 실행 중인 도구를 끊고 그 자리에서 마무리에 들어가므로(살아 있는 ctx 에서), 한 턴이 멈춰도 더는
	// 마무리를 건너뛰지 않으며 외부의 강제 ctx 안전장치가 필요 없다. ctx 는 pause / kill / shutdown 만 담당한다.
	_, _, err = captureRun(ctx, opts, input,
		func(r db.Activity) {
			if emit != nil {
				r.Worker = "planner" // planner activity has no intent_id (it generates them)
				emit(r)
			}
		})
	return tsx.GoalMet, tsx.Reason, err
}
