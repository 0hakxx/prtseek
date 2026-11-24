package agent

import (
	"encoding/json"
	"fmt"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

const findingIDGuidance = "\n\n**취약점 번호 규칙**: finding_id 는 독립 취약점 기록 ID 이고, finding_node_id 는 탐색 노드 ID 다. list_findings / list_task_findings / node_detail / get_task_node_detail 의 id 는 탐색 노드 ID 로 유지되므로, 같은 반환값의 finding_id 에서 독립 번호를 읽어야 한다. get_finding_traffic / bind_finding_traffic 은 독립 finding_id 를 쓴다. 기존 update_finding_report 의 finding_id 파라미터에는 여전히 finding_node_id 를 넘긴다. report_finding 첫 줄의 숫자를 증거 도구에 쓰지 말고, 번호 오류가 나도 다른 숫자를 추측하지 않는다."

// The server supplies the persisted setting. A missing setting/host is off.
// Consulted at assembly and again on writes so an already-running session
// cannot keep binding after the user switches the feature off.
var FindingTrafficBindingEnabled func() bool

func findingTrafficBindingEnabled() bool {
	return FindingTrafficBindingEnabled != nil && FindingTrafficBindingEnabled()
}

// Applied after ToolResolve: user descriptions and prompts remain intact, while
// all actual reporters (including Planner and custom chat agents) see the same
// API contract. Disabled/unbound tools are never reintroduced here.
func findingWorkflowTools(agentKey string, tools []actool.CoreTool) ([]actool.CoreTool, string) {
	if !findingTrafficBindingEnabled() {
		out := make([]actool.CoreTool, 0, len(tools))
		for _, tool := range tools {
			if tool.Name() == "bind_finding_traffic" {
				continue
			}
			if agentKey == "reporter" && (tool.Name() == "traffic_search" || tool.Name() == "traffic_get" || tool.Name() == "traffic_blob") {
				continue
			}
			switch tool.Name() {
			case "report_finding", "add_hint", "add_task_hint":
				// Work on a copy: toggling back on must restore the original schema.
				raw, _ := json.Marshal(tool.InputSchema())
				var schema map[string]any
				if json.Unmarshal(raw, &schema) == nil {
					stripTrafficParameters(schema)
					tool = DecorateTool(tool, tool.Description(), schema)
				}
			}
			out = append(out, tool)
		}
		return out, ""
	}
	out := append([]actool.CoreTool(nil), tools...)
	has := map[string]bool{}
	for i, tool := range out {
		has[tool.Name()] = true
		note := ""
		switch tool.Name() {
		case "report_finding":
			note = "\n기본적으로 보고 Agent 가 보고서를 쓰기 전에 트래픽을 확인하고 바인딩한다. 보고자는 evidence 에 검증 명령, 핵심 출력, 이미 있는 실제 트래픽 ID 와 그 용도를 남겨 보고 Agent 가 실행 기록과 대조해 확인할 수 있게 한다. 바인딩을 위해 패킷을 따로 찾을 필요는 없다. 명시적 즉시 바인딩도 호환된다: traffic_refs 나 evidence_hint_id 로 확인된 참조를 제출할 수 있으며, 후자는 이 작업의 지정된 hint 의 구조화된 참조를 읽는다. 하나라도 유효하지 않으면 이번 보고 전체가 실패한다. TCP/패킷 없음인 경우 이 선택 파라미터는 필요 없다. 반환되는 finding_id 와 finding_node_id 는 각각 독립 기록과 탐색 노드를 뜻한다."
		case "add_hint", "add_task_hint":
			note = "\n확인된 취약점을 넘길 때는 해당 힌트의 traffic_refs 에 확인된 트래픽의 ID, 용도, 설명, 순서를 남기고(단건은 최상위, 일괄은 해당 hints 요소에), text 에 그것이 증명하는 구체적인 취약점을 설명한다. 호출 측은 텍스트만 넘기고 기존 트래픽 참조를 버리면 안 된다. 확인되지 않은 후보는 증거로 전달할 수 없다."
		case "get_finding_traffic", "bind_finding_traffic", "list_findings", "list_task_findings", "node_detail", "get_task_node_detail", "update_finding_report":
			note = findingIDGuidance
		}
		if note != "" {
			out[i] = DecorateTool(tool, tool.Description()+note, tool.InputSchema())
		}
	}
	guidance := ""
	if has["report_finding"] || has["add_task_hint"] || has["add_hint"] {
		guidance = "\n\n**트래픽 증거 전달(선택)**: 자동 바인딩은 기본적으로 보고 Agent 가 취약점 등록 후 보고서 작성 전에 수행한다. 보고자는 evidence 에 검증 명령, 핵심 출력, 이미 있는 실제 트래픽 ID 와 그 용도를 남기고, 작업에는 intent_id 를 포함해 보고 Agent 가 추적할 수 있게 해야 한다. 바인딩을 위해 패킷을 따로 찾을 필요는 없다. Auto / Planner 가 대신 보고할 때 실행자가 가진 참조를 버리지 않는다. add_hint / add_task_hint 는 traffic_refs 로 전달할 수 있으며, 명시적 즉시 바인딩은 여전히 report_finding 의 traffic_refs / evidence_hint_id 와 호환된다. TCP 이거나 패킷이 없으면 정상적으로 등록하며, ID 를 추측하거나 패킷을 채우려고 반복 탐지하지 않는다."
		if has["add_task_hint"] && !has["add_hint"] {
			guidance += "\n플랫폼 대화에 작업 컨텍스트가 없으면 report_finding 을 직접 호출하지 않는다. add_task_hint 로 해당하는 기존 작업에 넘겨 작업 Agent 가 등록하게 하고, list_task_findings 로 결과를 확인한다."
		}
		if has["prove_goal"] || has["goal_met"] {
			guidance += "\n목표 완료를 판정하기 전에 이번에 확보한 증거의 보고/전달을 먼저 끝낸다. 증거 전달이 끝나지 않았는데 텍스트로 취약점이 등록되었다는 이유만으로 작업을 끝내거나 Worker 를 취소하지 않는다. 패킷이 없으면 기다리거나 억지로 캡처할 필요는 없다."
		}
	}
	if has["update_finding_report"] && has["bind_finding_traffic"] && has["get_finding_traffic"] {
		guidance += "\n\n**보고 전 트래픽 자동 연결(켜짐)**: 너는 이번에 트리거된 취약점의 트래픽을 확인하고 바인딩한 뒤 보고서를 쓴다. 먼저 report_finding 이 반환한 JSON 이나 get_task_node_detail / list_task_findings 에서 명확한 finding_id 와 finding_node_id 를 얻는다. 취약점 상세, 해당 의도의 실행 기록, 기존 증거 목록을 읽고, 보고자가 넘긴 실제 ID 를 우선 사용한다. 이번 검증이 HTTP 이고 트래픽 도구를 쓸 수 있으면 traffic_search 로 후보를 거른 뒤 traffic_get 으로 요청/응답이 실제로 그 취약점을 뒷받침하는지 하나씩 확인한다. 도메인과 시간은 거르는 데만 쓰며 귀속을 증명하지 않는다. 확인된 증거를 재현 순서대로 bind_finding_traffic(finding_id, traffic_refs) 로 연결하고, baseline / proof / verification / supporting 중에서 골라 용도를 설명한다. 이번 취약점만 다룰 수 있으며, 취약점을 중복 생성하거나 대상을 재탐지하지 않는다. 바인딩에 성공하면 get_finding_traffic 을 다시 호출해 최신 version 을 얻고 필요한 본문을 읽은 뒤, 실제로 읽은 version 을 evidence_version 으로 update_finding_report 에 넘긴다(그 finding_id 파라미터에는 여전히 finding_node_id 를 쓴다). 이미 있는 바인딩은 중복 추가할 필요가 없다. TCP, 미수집, 도구 사용 불가, 정확히 일치하는 항목이 없는 경우 자동 바인딩을 건너뛰고 텍스트/명령 증거로 보고서를 정상 작성하며 그 이유를 설명한다. 트래픽을 채우려고 추측하면 안 된다. 바인딩에 실패하면 성공했다고 주장하지 않고, 기존 증거를 유지하며 보고서에 바인딩하지 못한 이유를 설명한다."
	}
	if guidance != "" || has["get_finding_traffic"] || has["update_finding_report"] {
		guidance += findingIDGuidance
	}
	return out, guidance
}

func stripTrafficParameters(schema map[string]any) {
	props, _ := schema["properties"].(map[string]any)
	delete(props, "traffic_refs")
	delete(props, "evidence_hint_id")
	if required, ok := schema["required"].([]any); ok {
		kept := required[:0]
		for _, key := range required {
			if key != "traffic_refs" && key != "evidence_hint_id" {
				kept = append(kept, key)
			}
		}
		schema["required"] = kept
	}
	if hints, ok := props["hints"].(map[string]any); ok {
		if items, ok := hints["items"].(map[string]any); ok {
			stripTrafficParameters(items)
		}
	}
}

// HintTrafficSchema is shared by the task-local and cross-task hint tools.
func HintTrafficSchema() map[string]any {
	return map[string]any{"type": "array", "description": "선택: 확인을 거쳤고 이 힌트의 구체적인 취약점에 해당하는 트래픽 참조이며 순서를 유지한다. 전달 후 report_finding 은 evidence_hint_id 로 이 참조들을 가져갈 수 있다.", "items": obj(map[string]any{"traffic_id": str("실제 트래픽 ID"), "role": str("baseline / proof / verification / supporting"), "note": str("이 트래픽이 뒷받침하는 결론")}, "traffic_id")}
}

func (t *ToolSet) findingRefsFromHint(hintID int64, explicit []db.TrafficRef) ([]db.TrafficRef, error) {
	if hintID <= 0 {
		return db.NormalizeTrafficRefs(explicit)
	}
	n, err := t.ts.GetNode(hintID) // local store only: inherited hints cannot supply evidence
	if err != nil {
		return nil, err
	}
	if n == nil || n.Kind != db.KindHint {
		return nil, fmt.Errorf("evidence_hint_id=%d 는 이 작업의 힌트 노드여야 합니다(상속된 힌트는 바인딩에 바로 쓸 수 없음)", hintID)
	}
	var payload struct {
		Refs []db.TrafficRef `json:"traffic_refs"`
	}
	if err := json.Unmarshal(n.Payload, &payload); err != nil {
		return nil, err
	}
	return db.NormalizeTrafficRefs(append(append([]db.TrafficRef{}, explicit...), payload.Refs...))
}
