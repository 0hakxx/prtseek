package agent

import (
	"strings"

	"github.com/Autumn-27/norma/harness"
)

// 마무리 프롬프트(wrap-up / settlement prompt): agent 가 【단계 수 소진(MaxTurns)】이나
// 【시간 초과(run_seconds/MaxDuration)】로 종료될 때 SDK 의 settlement 단계가 이 프롬프트를 주입해,
// agent 가 식별했지만 기록하지 않은 내용을 먼저 저장한 뒤 한 문장 요약을 출력하게 하여 흐지부지 끝나지 않게 한다.
//
// 각 agent 의 마무리 프롬프트는 관리 화면에서 필요에 따라 덮어쓸 수 있으며(agents.wrapup_prompt 에 저장), 비우면 여기의
// 내장 기본값을 쓴다. 【프롬프트 본문】만 편집할 수 있다. 어떤 도구를 막을지, 마무리 자체에 몇 턴의 예산을 줄지는 코드에 고정된 정책이다.

// WrapupOverride, if set, returns the stored wrap-up prompt for an agent key and
// whether a non-empty one exists. Wired by the server to the agents table (like
// PromptOverride for system prompts). nil / empty → the built-in default is used.
var WrapupOverride func(agentKey string) (string, bool)

// WrapupMaxTurnsOverride, if set, returns the admin-configured turn budget for the
// wrap-up phase of an agent and whether a positive one exists. Wired to the agents
// table. nil / ≤0 → the built-in per-agent default (wrapupTurnDefaults) is used.
var WrapupMaxTurnsOverride func(agentKey string) (int, bool)

// 내장 기본 마무리 프롬프트이며 agent key 로 색인한다. worker 는 예전부터 하드코딩된 settleWrapUpPrompt 를 재사용하고
// (worker.go 에 정의), planner/mainagent 는 각각 자기 버전이 있다. 해당되지 않는 것(사용자 정의 agent)은 공통 대비책을 쓴다.
var wrapupDefaults = map[string]string{
	"worker":    settleWrapUpPrompt,
	"planner":   plannerWrapUpDefault,
	"mainagent": mainAgentWrapUpDefault,
}

// wrapupTurnDefaults: 각 agent 마무리 단계 【자체】의 턴 수 예산 내장 기본값(관리 화면에서 >0 으로 덮어쓸 수 있음).
// 모두 10턴을 주어 마무리 단계에 저장할 단계가 충분하도록 한다. 해당되지 않으면 genericWrapupTurns 를 쓴다.
var wrapupTurnDefaults = map[string]int{
	"worker":    10,
	"planner":   10,
	"mainagent": 10,
}

const genericWrapupTurns = 10

const plannerWrapUpDefault = "이번 턴 계획의 단계 수가 곧 소진된다——【이번 턴】만 끝나는 것이라는 점에 주의한다. 시스템은 이후에도 상황 변화에 따라 너를 다시 깨워 계획을 이어 가게 하므로, 작업이 종료되는 것이 아니며 여기서 계획 전체를 정리할 필요가 없다. 이번 턴에 정리된 결론은 반영해 이 턴이 헛되지 않게 하되, 【마무리를 위해 억지로 의도를 만들지는 않는다】(이번 턴 의도 0개도 완전히 정상인 결과다): (1) 【지금 바로 분배해야 할】 탐색 방향을 판단했다면 add_intent 한 번으로 일괄 제출한다(생각해 둔 것을 묵혀 두지 않는다); (2) 어떤 발견/사실로 달성이 증명된 목표는 prove_goal 로 met 을 표시한다(판정을 빠뜨리지 않는다); (3) 단계별로 진행해야 할 직렬 익스플로잇 체인을 식별했다면 TodoWrite 로 기록해 다음에 깨어났을 때 이어서 분배하기 쉽게 한다. 다 하면 이번 턴을 바로 끝내며, 요약 텍스트를 출력할 필요는 없다."

const mainAgentWrapUpDefault = "단계 수가 곧 소진되어 이번 상호작용이 끝난다. 새 탐색/작업을 더 시작하지 않는다. **순수 텍스트 한 문장만 따로** 써서 사용자에게 현재 진행 상황, 핵심 결론, 권장하는 다음 단계를 요약한다."

const genericWrapUpDefault = "너는 예산 소진으로 곧 종료된다. 먼저 완료했지만 저장하지 않은 결과를 기록한 뒤, **순수 텍스트 한 문장만 따로** 써서 무엇을 했고 어떤 핵심 결론을 얻었는지 요약한다(이 문장은 이번 실행의 결과로 표시된다)."

// WrapupDefault returns the built-in default wrap-up prompt for an agent key —
// used by the admin UI as the "restore default" value and empty-field placeholder.
func WrapupDefault(agentKey string) string {
	if d, ok := wrapupDefaults[agentKey]; ok {
		return d
	}
	return genericWrapUpDefault
}

// WrapupTurnsDefault returns the built-in wrap-up turn budget for an agent key —
// used by the admin UI as the "0 = default N" hint.
func WrapupTurnsDefault(agentKey string) int {
	if n, ok := wrapupTurnDefaults[agentKey]; ok {
		return n
	}
	return genericWrapupTurns
}

// resolveWrapup returns the effective wrap-up prompt: the DB override (if set and
// non-empty) over the built-in default.
func resolveWrapup(agentKey string) string {
	if WrapupOverride != nil {
		if t, ok := WrapupOverride(agentKey); ok && strings.TrimSpace(t) != "" {
			return t
		}
	}
	return WrapupDefault(agentKey)
}

// resolveWrapupTurns returns the effective wrap-up turn budget: a positive DB
// override over the built-in per-agent default.
func resolveWrapupTurns(agentKey string) int {
	if WrapupMaxTurnsOverride != nil {
		if v, ok := WrapupMaxTurnsOverride(agentKey); ok && v > 0 {
			return v
		}
	}
	return WrapupTurnsDefault(agentKey)
}

// wrapupSettlement builds the settlement config for an agent's run. Prompt and the
// turn budget are admin-editable per agent; disabled tools are code-owned policy so
// a user can't edit away the "stop probing" guardrail. Resolved fresh each run
// (reads DB live), so edits apply on the next run without a restart.
func wrapupSettlement(agentKey string, disabledTools []string) *harness.Settlement {
	return &harness.Settlement{
		Prompt:        resolveWrapup(agentKey),
		DisabledTools: disabledTools,
		MaxTurns:      resolveWrapupTurns(agentKey),
	}
}

// ---------- 작업 단위 시간 초과 마무리 문구(docs/任务级超时与收尾设计.md 참고)----------
//
// per-run 마무리 문구와는 【별개의 두 세트】다: per-run 은 "이번 run 의 예산을 다 썼다"는 뜻이고, 작업 시간 초과는
// "작업 전체의 시각이 되어 곧 끝난다"는 뜻이다. 의미가 반대인 경우가 많다(특히 planner: per-run 은 "멈추지 말고 계속 계획"이라 하고,
// 작업 시간 초과는 "시각이 되었으니 계획을 멈추고 마지막 판정을 하라"고 한다). worker/planner 에만 설정한다.

// WrapupTaskTimeoutOverride / …TurnsOverride: 작업 시간 초과 마무리 문구와 턴 수의 DB 덮어쓰기
// (agents.task_timeout_wrapup_prompt / _max_turns 에 연결, worker/planner 만).
var (
	WrapupTaskTimeoutOverride      func(agentKey string) (string, bool)
	WrapupTaskTimeoutTurnsOverride func(agentKey string) (int, bool)
)

var taskTimeoutWrapupDefaults = map[string]string{
	"worker":  workerTaskTimeoutDefault,
	"planner": plannerTaskTimeoutDefault,
}

const workerTaskTimeoutDefault = "**작업 전체가 시간 초과 상한에 도달해 곧 끝난다**(이번 run 의 예산이 아니라 탐색 전체의 시각이 된 것이다). 마지막 기회다: (1) 식별했지만 아직 기록하지 않은 내용을 【모두】 저장한다——새 자산은 insert_assets, 탐색 결론/사실은 record_fact, 확인된 취약점은 report_finding; (2) 새 명령/탐지를 더 시작하지 않는다; (3) **마지막에 순수 텍스트 한 문장만 따로** 써서 이 의도에 대한 핵심 결론을 요약한다."

const plannerTaskTimeoutDefault = "**작업 전체가 시간 초과 상한에 도달해 곧 끝난다**(이번 턴이 아니라 작업 전체가 종료된다). 현재의 【모든】 사실과 발견을 바탕으로 마지막 목표 판정을 한다: 증거로 달성이 증명된 목표는 prove_goal 로 met 을 표시한다(판정을 빠뜨리지 않는다). **새 의도는 더 이상 만들지 않는다**(이때 분배한 의도는 더 이상 실행되지 않는다). 판정을 마치면 정리하며, 요약 텍스트를 출력할 필요는 없다."

// TaskTimeoutWrapupDefault 는 특정 agent 의 작업 시간 초과 내장 기본 마무리 문구를 반환한다(관리 화면 자리표시자/기본값 복원용).
func TaskTimeoutWrapupDefault(agentKey string) string {
	return taskTimeoutWrapupDefaults[agentKey] // 설정되지 않은 경우(mainagent/chat) 빈 문자열 반환
}

// resolveTaskTimeoutWrapup: DB 덮어쓰기(비어 있지 않음) > 내장 기본값. 빈 문자열은 그 agent 에 작업 시간 초과 문구가 없다는 뜻이며
// (worker/planner 가 아님), 이때 호출자는 per-run 문구로 돌아가야 한다.
func resolveTaskTimeoutWrapup(agentKey string) string {
	if WrapupTaskTimeoutOverride != nil {
		if t, ok := WrapupTaskTimeoutOverride(agentKey); ok && strings.TrimSpace(t) != "" {
			return t
		}
	}
	return TaskTimeoutWrapupDefault(agentKey)
}

func resolveTaskTimeoutTurns(agentKey string) int {
	if WrapupTaskTimeoutTurnsOverride != nil {
		if v, ok := WrapupTaskTimeoutTurnsOverride(agentKey); ok && v > 0 {
			return v
		}
	}
	return resolveWrapupTurns(agentKey) // 기본적으로 per-run 턴 수를 그대로 사용
}

// wrapupSettlementForTask builds settlement for a worker/planner run that is aware
// of the task deadline. See §5 of the design doc:
//   - clamped=true  → 이번 run 이 작업 deadline 으로 조여짐: Timeout 으로 마무리=작업 시각 도달→작업 시간 초과 문구;
//     MaxTurns 로 마무리=조여진 구간 안에서 단계가 먼저 소진되고 작업은 몇 분 남음→per-run 문구로 돌아감.
//   - clamped=false → 작업 시간이 아직 넉넉함: 두 reason 모두 per-run 문구 사용(즉 wrapupSettlement 로 퇴화).
//
// harness 의 PromptByReason 에 맡겨 마무리할 때 【실제】 reason 에 따라 그 자리에서 고르므로, build 시점의 잘못된 매칭이 없다.
func wrapupSettlementForTask(agentKey string, disabledTools []string, clamped bool) *harness.Settlement {
	perRun := resolveWrapup(agentKey)
	st := &harness.Settlement{
		Prompt:        perRun, // 대비책(clamped 가 아닐 때 두 reason 의 값이기도 함)
		DisabledTools: disabledTools,
		MaxTurns:      resolveWrapupTurns(agentKey),
	}
	if clamped {
		if tt := resolveTaskTimeoutWrapup(agentKey); tt != "" {
			st.PromptByReason = map[harness.TerminalReason]string{
				harness.ReasonTimeout:  tt,     // 작업 시각 도달
				harness.ReasonMaxTurns: perRun, // 단계가 먼저 소진되고 작업 시간은 남음
			}
			st.MaxTurns = resolveTaskTimeoutTurns(agentKey)
		}
	}
	return st
}
