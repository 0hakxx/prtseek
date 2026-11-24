package agent

import (
	"context"
	"strings"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
)

const PersonalDefaultPrompt = `너는 prtseek 이며, 사용자의 개인 업무 도우미다. 사용 가능한 도구를 직접 써서 현재 대화의 작업을 완료하고, 다시 확인할 수 있는 파일과 증거를 남긴다.
먼저 사용자의 목표와 제약을 이해한다. 비교적 복잡한 작업은 TodoWrite 로 단계를 기록한다. 일반 요청에는 백그라운드 작업을 만들지 않는다. 오랫동안 계속 실행해야 한다면 사용자에게 “백그라운드 지속 실행으로 전환”을 누르라고 제안할 수 있지만, 그 조작을 건너뛰고 직접 백그라운드 작업을 만들 수는 없다.
사용자가 검증 가능한 목표를 계속 완료하라고 명확히 요구하면 현재 포그라운드 실행에서 계속 추진한다. 한 방향이 실패하면 반증을 기록하고 근거 있는 다른 방향을 시도한다. 목표를 달성했거나, 사용자가 중지했거나, 환경 또는 권한 경계에 막혔거나, 증거가 뒷받침하는 실행 가능한 방향을 모두 시도한 경우에만 끝낸다. 단계별 요약이나 체크포인트를 목표 완료로 여기지 않는다.
실질적인 단계를 마쳤거나, 중요한 결과를 얻었거나, 방향을 바꿨거나, 차단에 부딪혔을 때는 사용자가 질문한 언어로 짧은 진행 상황을 출력한다: 무엇을 끝냈는지, 증거가 무엇인지, 다음 단계가 무엇인지. 말없이 도구만 호출하지 말고, 공허한 상태 보고를 반복하지도 않는다.
실제 도구 반환에만 근거해 답한다. 관찰, 추론, 검증된 결론을 구분하며, 발견, 진행 상황, 내부 사고 과정을 지어내지 않는다. 보안 연구는 사용자가 명확히 정한 권한 범위 안에서만 수행한다.`

type personalLanguageKey struct{}
type personalReadOnlyKey struct{}

// TaskResponseLanguage is resolved by the host from a linked task's snapshot.
var TaskResponseLanguage func(taskID int64) string

// Explicit persistence affects only the current foreground run. It never
// creates a background task or changes the saved agent configuration.
func PersonalPersistentGoal(message string) bool {
	q := strings.ToLower(strings.TrimSpace(message))
	if q == "继续" || q == "继续解题" || q == "继续探索" || q == "계속" || q == "계속 풀이" || q == "계속 탐색" {
		return true
	}
	for _, marker := range []string{"获取flag", "拿到flag", "持续解题", "不要停止直到", "持续执行直到", "flag 획득", "flag 를 얻", "계속 풀이", "멈추지 말고", "될 때까지 계속", "get the flag", "keep going until"} {
		if strings.Contains(q, marker) {
			return true
		}
	}
	return false
}

func WithResponseLanguage(ctx context.Context, language string) context.Context {
	return context.WithValue(ctx, personalLanguageKey{}, language)
}

func WithPersonalReadOnly(ctx context.Context) context.Context {
	return context.WithValue(ctx, personalReadOnlyKey{}, true)
}

func personalReadOnly(ctx context.Context) bool {
	readOnly, _ := ctx.Value(personalReadOnlyKey{}).(bool)
	return readOnly
}

func personalReadOnlyTool(name string) bool {
	switch strings.ToLower(name) {
	case "research_recall", "graph_overview", "list_findings", "list_facts", "list_assets", "get_worker_output", "node_detail", "get_worker_trace", "search_all_worker_traces", "list_worker_traces", "list_goals", "traffic_search", "traffic_get", "traffic_blob", "get_finding_traffic", "expand_digest", "expand_index", "read", "ls", "glob", "grep":
		return true
	}
	return false
}

func responseLanguageBlock(ctx context.Context) string {
	language, _ := ctx.Value(personalLanguageKey{}).(string)
	if language == "" && TaskResponseLanguage != nil {
		language = TaskResponseLanguage(RunInfoFrom(ctx).TaskID)
	}
	if language == "" {
		return ""
	}
	name := "中文"
	if language == "en" {
		name = "English"
	}
	return "\n\n사용자에게 보이는 진행 상황, 중요한 이벤트, 할 일, 최종 답변은 " + name + " 로 작성한다. 도구 출력, 경로, 증거 원문은 그대로 둔다. 실질적인 진전이 있을 때만 짧게 알리고, 완료 비율이나 내부 추론을 지어내지 않는다."
}

func personalTools(ctx context.Context, key string, in []actool.CoreTool) []actool.CoreTool {
	readOnly := personalReadOnly(ctx)
	if key != "personal" && !readOnly {
		return in
	}
	out := make([]actool.CoreTool, 0, len(in))
	for _, t := range in {
		if key == "personal" && t.Name() == "spawn_task" {
			continue
		}
		if readOnly && !personalReadOnlyTool(t.Name()) {
			continue
		}
		out = append(out, t)
	}
	return out
}

// NewSession adds its own tools AFTER AugmentTools, including WebFetch, Monitor,
// and deferred-tool dispatch. Disable those additions and install an execution
// guard as well, so a hallucinated call cannot bypass the visible tool schemas.
func personalSessionOptions(ctx context.Context, opts *agentcore.Options) {
	if !personalReadOnly(ctx) {
		return
	}
	opts.Tools = personalTools(ctx, "", opts.Tools) // non-nil even when empty: no SDK defaults
	opts.EnableWebFetch = false
	opts.EnableWebSearch = false
	opts.DisableBackgroundTasks = true
	opts.DeferredTools = nil
	opts.UnlockSet = nil
	opts.Skills = nil
	opts.Plan = nil
	opts.Memory = nil
	opts.Todos = nil
	opts.AskUser = nil
	opts.Hooks = &personalReadOnlyHooks{base: opts.Hooks}
}

type personalReadOnlyHooks struct{ base harness.HookRunner }

func (h *personalReadOnlyHooks) PreToolUse(ctx context.Context, name string, input []byte) (bool, string, []byte) {
	if !personalReadOnlyTool(name) {
		return true, "The task is paused. Only existing local records may be read; use the explicit resume control before performing new operations.", nil
	}
	if h.base != nil {
		return h.base.PreToolUse(ctx, name, input)
	}
	return false, "", nil
}

func (h *personalReadOnlyHooks) PostToolUse(ctx context.Context, name string, input, result []byte, isError bool) {
	if h.base != nil {
		h.base.PostToolUse(ctx, name, input, result, isError)
	}
}
func (h *personalReadOnlyHooks) Stop(ctx context.Context, messages []llm.Message) (bool, []string, string) {
	if h.base != nil {
		return h.base.Stop(ctx, messages)
	}
	return false, nil, ""
}
func (h *personalReadOnlyHooks) FireUserPromptSubmit(ctx context.Context, prompt string) string {
	if base, ok := h.base.(interface {
		FireUserPromptSubmit(context.Context, string) string
	}); ok {
		return base.FireUserPromptSubmit(ctx, prompt)
	}
	return ""
}
func (h *personalReadOnlyHooks) FireSessionStart(ctx context.Context) {
	if base, ok := h.base.(interface{ FireSessionStart(context.Context) }); ok {
		base.FireSessionStart(ctx)
	}
}
func (h *personalReadOnlyHooks) FireSessionEnd(ctx context.Context) {
	if base, ok := h.base.(interface{ FireSessionEnd(context.Context) }); ok {
		base.FireSessionEnd(ctx)
	}
}
