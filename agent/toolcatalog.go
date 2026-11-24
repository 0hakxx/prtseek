package agent

import (
	"context"
	"encoding/json"

	actool "github.com/Autumn-27/norma/tool"
)

// 이 파일은 「내장 도구」를 순수 코드에서 열거 가능하고 DB 로 덮어쓸 수 있는 목록으로 만든다:
//   - BuiltinToolSeeds(): 세 실행 agent 의 내장 도구 세트를 seed 레코드(key +
//     설명 + 파라미터 schema + 기본 바인딩 agent)로 펼쳐, 서버가 시작할 때 tools 테이블에 멱등적으로 시드한다.
//   - ToolResolve 훅: 실행 시 DB 의 tools 행에 따라 조립된 도구에 「agent 별 필터 +
//     설명/schema 덮어쓰기 + 파라미터 기본값 주입」을 한다. key/handler 는 여전히 코드 계층에 있고, DB 는 「설명 문구와 기본값」만 바꾼다.
// handler(Call 동작)는 언제나 코드에서 온다——DB 로는 바꿀 수 없고, 모델이 보는 설명과 기본 입력 파라미터만 바꿀 수 있다.

// ToolSeed 는 내장 도구 하나의 시드 가능한 스냅샷이다: key 는 곧 CoreTool.Name()(handler 와 고정 연결,
// UI 읽기 전용), Desc/Schema 는 코드의 도구 정의에서 가져오며, Agents 는 코드 기본값으로 이 도구를 받는 agent 들이다.
type ToolSeed struct {
	Key    string         // = CoreTool.Name(), 기본 키, 변경 불가
	Desc   string         // 최상위 설명(UI 에서 덮어쓰기 가능)
	Schema map[string]any // 파라미터 JSON-Schema(구조는 읽기 전용, description/default 는 UI 에서 변경 가능)
	Agents []string       // 기본 바인딩 agent key(worker/planner/mainagent)
}

// builtinToolsByAgent 는 「읽기 전용 빈 껍데기」 ToolSet(nil stores)으로 각 실행 agent 의
// 도메인 도구 세트를 만든다. 도구 생성 함수는 클로저를 Spec 에 넣기만 하고 생성 시점에 store 를 역참조하지 않으므로 nil 에 안전하다——
// 이 도구들은 여기서 Name()/Description()/InputSchema() 를 읽는 데만 쓰며 절대 Call 하지 않는다.
//
// SDK 공통 도구 actool.DefaultTools()(Read/Write/Edit/MultiEdit/LS/Glob/
// Grep/Bash)는 일부러 【포함하지 않는다】: 이 도구들은 모든 agent 가 고정으로 가지며 「누구에게 바인딩할지」의 선택이 없고, 설명도 대부분 Prompt()
// 안에 있다(이 표는 Description() 만 덮어쓰므로 반쪽짜리 덮어쓰기로 오해를 낳는다). seed 안 함 → DB 행 없음 → ToolResolve 가
// 그대로 통과시키고 덮어쓰지 않으므로 동작은 이전과 같다. prtseek 자체의 도메인 도구만 표에 넣어 관리한다.
func builtinToolsByAgent() map[string][]actool.CoreTool {
	ts := NewToolSet(nil, "")
	return map[string][]actool.CoreTool{
		"mainagent": ts.MainAgentTools(),
		"planner":   ts.PlannerTools(),
		"worker":    ts.WorkerTools(),
		// goals(목표 분해기)는 기본적으로 set_goals + set_constraints 를 바인딩한다: 이 도구들로 분해한 목표와
		// 추출한 작업 제약을 DB 에 쓴다. mainagent 와 같은 관리 대상 도구를 공유하며, web 에서 설명/schema 를 바꾸고 agent 별로 체크할 수 있다.
		"goals": {ts.setGoals(), ts.setConstraints()},
		// auto 는 기본적으로 취약점 보고 + 자산 관리 도구를 바인딩하며, 다른 도메인 도구는 UI 에서 필요에 따라 체크할 수 있다.
		// 새 DB 는 이 seed 로 기록되고, 기존 DB 는 seedAutoDefaultBindings 로 마이그레이션된다.
		"auto": {ts.addFinding(), ts.insertAssets(), ts.addCompanyScope(), ts.listAssets(), ts.listCompanies()},
		// pentest(독립 침투 agent)는 기본적으로 다음을 바인딩한다: 자산 조회 / 자산 추가 / 취약점 보고 / 취약점 조회 / 기업 조회.
		// 새 DB 는 이 seed 로 기록되고, 기존 DB 는 seedPentestDefaultBindings 로 마이그레이션된다.
		"pentest": {ts.listAssets(), ts.insertAssets(), ts.addFinding(), ts.listFindings(), ts.listCompanies()},
	}
}

// defaultUnbound: 이 system 도구들은 평소처럼 목록에 들어가지만(web 에서 보이고, agent 별로 수동 체크 가능)
// 기본적으로 【어떤 agent 에도 바인딩하지 않는다】——ToolResolve 는 바인딩이 빈 도구를 모든 agent 에서 버리므로, 명시적으로 opt-in 해야 한다.
// 그런데도 어떤 agent 의 base 도구 세트에 남겨 두는 이유(예: PlannerTools 의 goal_met): 첫째, seed 가 이를
// 만들어 desc/schema 를 얻게 하기 위해서, 둘째, 사용자가 수동으로 다시 바인딩했을 때 실행 시 base 에 있어야 ToolResolve 가 유지할 수 있기 때문이다.
//
// goal_met: 개별 prove_goal 을 건너뛰고 전역에서 곧바로 【작업 전체 완료】를 선언한다. 영향이 크고 오판 위험이 있으며,
// 「prove_goal 로 마지막 목표 표시 → 자동 마무리」와 중복되므로 기본적으로 어떤 agent 에도 주지 않고, 필요할 때 수동으로 바인딩한다.
var defaultUnbound = map[string]bool{"goal_met": true}

// BuiltinToolSeeds 는 각 agent 의 내장 도구 세트를 중복 제거해 seed 목록으로 합친다: 같은 이름의 도구(예: list_assets
// 는 여러 agent 에 있음)는 하나로 합치고 Agents 는 합집합을 취한다. defaultUnbound 의 도구는 바인딩을 강제로 비운다.
func BuiltinToolSeeds() []ToolSeed {
	byAgent := builtinToolsByAgent()
	order := []string{"mainagent", "goals", "planner", "worker", "auto", "pentest"}

	type acc struct {
		tool   actool.CoreTool
		agents []string
	}
	m := map[string]*acc{}
	var keys []string
	for _, ak := range order {
		for _, t := range byAgent[ak] {
			a, ok := m[t.Name()]
			if !ok {
				a = &acc{tool: t}
				m[t.Name()] = a
				keys = append(keys, t.Name())
			}
			a.agents = append(a.agents, ak)
		}
	}

	out := make([]ToolSeed, 0, len(keys))
	for _, k := range keys {
		a := m[k]
		agents := a.agents
		if defaultUnbound[k] {
			agents = []string{} // 목록에 넣고 수동 바인딩 가능하지만 기본적으로 어떤 agent 에도 주지 않음(다른 도구와 맞추려고 null 이 아니라 [] 저장)
		}
		out = append(out, ToolSeed{
			Key:    k,
			Desc:   a.tool.Description(),
			Schema: a.tool.InputSchema(),
			Agents: agents,
		})
	}
	return out
}

// ToolResolve, if set, post-processes an agent's fully-assembled tool list against
// the DB tools table: it drops tools not bound to this agent (or globally disabled)
// and wraps the rest so the model sees the DB-overridden description/schema and
// 기본 입력 파라미터 get injected. Tools with no matching DB row (MCP/skill/host tools like
// traffic) pass through untouched. nil = tools unchanged. Wired in server/assembly.go.
var ToolResolve func(ctx context.Context, agentKey string, tools []actool.CoreTool) []actool.CoreTool

// DecorateTool wraps t so Description()/InputSchema() report the DB overrides and
// Call() injects scalar parameter defaults (from schema's "default" props) whenever
// the model omitted them. Name/Prompt/permission/scheduler flags delegate to t, so
// the tool's identity and handler are unchanged. Empty desc/schema fall back to t's.
func DecorateTool(t actool.CoreTool, desc string, schema map[string]any) actool.CoreTool {
	if desc == "" {
		desc = t.Description()
	}
	if len(schema) == 0 {
		schema = t.InputSchema()
	}
	return &overriddenTool{CoreTool: t, desc: desc, schema: schema}
}

// overriddenTool is a CoreTool decorator: it embeds the original (so all behavioral
// methods — Prompt/IsReadOnly/IsConcurrencySafe/CheckPermissions/Name — delegate)
// and overrides only the model-facing description/schema plus default injection.
type overriddenTool struct {
	actool.CoreTool
	desc   string
	schema map[string]any
}

func (o *overriddenTool) Description() string         { return o.desc }
func (o *overriddenTool) InputSchema() map[string]any { return o.schema }

func (o *overriddenTool) Call(ctx context.Context, in json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	return o.CoreTool.Call(ctx, injectDefaults(in, o.schema), tc)
}

// injectDefaults fills scalar parameter defaults declared in the (possibly edited)
// schema into the input JSON whenever the model omitted the field or left it empty/
// null. Structure (names/types/required) is untouched — only 기본값 are merged in.
func injectDefaults(in json.RawMessage, schema map[string]any) json.RawMessage {
	defs := scalarDefaults(schema)
	if len(defs) == 0 {
		return in
	}
	m := map[string]json.RawMessage{}
	if len(in) > 0 {
		if err := json.Unmarshal(in, &m); err != nil {
			return in // non-object input: don't touch it
		}
	}
	changed := false
	for k, dv := range defs {
		if cur, ok := m[k]; !ok || isEmptyJSON(cur) {
			m[k] = dv
			changed = true
		}
	}
	if !changed {
		return in
	}
	b, err := json.Marshal(m)
	if err != nil {
		return in
	}
	return b
}

// scalarDefaults extracts properties[k]["default"] for scalar params (string/
// integer/number/boolean). Array/object defaults are skipped: merging them is
// ambiguous and not worth the surprise.
func scalarDefaults(schema map[string]any) map[string]json.RawMessage {
	props, _ := schema["properties"].(map[string]any)
	if len(props) == 0 {
		return nil
	}
	out := map[string]json.RawMessage{}
	for name, raw := range props {
		p, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		dv, ok := p["default"]
		if !ok || dv == nil {
			continue
		}
		switch p["type"] {
		case "string", "integer", "number", "boolean":
			if b, err := json.Marshal(dv); err == nil {
				out[name] = b
			}
		}
	}
	return out
}

func isEmptyJSON(raw json.RawMessage) bool {
	s := string(raw)
	return s == "null" || s == `""`
}
