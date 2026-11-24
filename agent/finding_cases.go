package agent

import (
	"context"
	actool "github.com/Autumn-27/norma/tool"
)

type findingReviewKey struct{}

// Historical consolidation has a narrower capability set than new reporting.
func WithFindingCaseReview(ctx context.Context) context.Context {
	return context.WithValue(ctx, findingReviewKey{}, true)
}
func isFindingCaseReview(ctx context.Context) bool {
	v, _ := ctx.Value(findingReviewKey{}).(bool)
	return v
}

func historicalFindingCaseTools(tools []actool.CoreTool) []actool.CoreTool {
	allowed := map[string]bool{}
	for _, name := range []string{"search_finding_duplicates", "get_finding_record", "merge_finding_records", "suggest_finding_merge", "get_finding_case", "update_finding_case_report", "complete_finding_case_review", "get_finding_traffic", "traffic_search", "traffic_get", "traffic_blob", "get_task_node_detail", "list_task_findings", "get_task_graph", "list_task_worker_traces", "get_task_worker_trace", "Read"} {
		allowed[name] = true
	}
	out := make([]actool.CoreTool, 0, len(tools))
	for _, tool := range tools {
		if allowed[tool.Name()] {
			out = append(out, tool)
		}
	}
	return out
}

// This guidance is appended to the runtime prompt, including editable old prompts.
// Never reintroduce unbound tools or recreate a deleted Reporter.
func findingCaseWorkflow(key string, tools []actool.CoreTool) ([]actool.CoreTool, string) {
	if key != "reporter" {
		return tools, ""
	}
	has := map[string]bool{}
	for _, t := range tools {
		has[t.Name()] = true
	}
	if !has["search_finding_duplicates"] || !has["merge_finding_records"] || !has["get_finding_case"] || !has["update_finding_case_report"] {
		return tools, ""
	}
	return tools, `

【같은 취약점의 중복 보고 정리】이번 독립 보고는 유지하며 다른 원래 보고를 덮어쓰지 않는다. "보고를 마치면 멈춘다"는 기존 기본 지침을 여기서 확장한다: 먼저 이번 보고를 저장한 뒤, search_finding_duplicates(finding_id=독립 기록 ID) 로 같은 작업의 후보 요약을 페이지 단위로 읽는다(score 순 정렬. strong=취약점 유형이 같고 자산이 겹침, 우선 확인. weak=같은 취약점 유형과 자산 겹침을 동시에 갖추지 않음, hit_reasons 와 함께 확인하며 참고용). 필요하면 get_finding_record 로 증거를 본다. 취약점 유형, 제목, 토큰, 로그인 진입점이 비슷하거나 상/중/하 등급만으로 병합할 수 없다. 대상, 인터페이스, 파라미터, 권한 경계, 근본 원인, 실제 검증 증거를 확인해야 한다. 같은 작업만 처리할 수 있으며 수동 기각과 finding_case_blocks 를 존중한다.
【판단 기준】증거가 엔드포인트/라우트, 핸들러나 백엔드 컴포넌트, 취약한 파라미터, 또는 누락된 검사가 서로 다른 코드 경로에 속한다는 것을 보여 주면 independent 로 판정할 수 있으며, 각각의 증거를 설명해야 한다. 경로, 인터페이스, 로그인 진입점, 토큰, 제목, 취약점 유형이 같거나 다르다는 것만으로는 같은 근본 원인이나 다른 근본 원인을 단독으로 증명할 수 없다. 근본 원인이 명확히 같을 때만 merge_finding_records(task_id,finding_ids,title,reason) 를 바로 호출한다. 구체적인 공통점(두 기록 모두 증거상 같은 엔드포인트, 파라미터, 컴포넌트 또는 누락된 검사를 가리킴)이 있지만 공통 근본 원인을 확정할 핵심 증거가 부족할 때만 suggest_finding_merge 를 호출한다. 한 번의 제출은 같은 작업 기록 2..20건을 포괄하는 하나의 그룹이며, 둘씩 쪼개 제안하지 않는다. 취약점 유형이나 자산만 공유하고 구체적인 공통점이 없으면 제안을 제출하지 않는다. 구체적인 공통점이 없으면 independent 를 유지하고, 불확실하다는 이유로 suggest 하지 않는다. 중복 보고, 기록 삭제, 대상 재탐지를 하지 않는다. 사람이 정리를 선택한 경우 선택된 기록만 판단할 수 있으며, 선택되지 않은 기록을 추가할 수 없다(기존 그룹 구성원은 보고 컨텍스트로 쓸 수 있음).
병합에 성공했거나 이미 case_id 가 있으면 get_finding_case 로 실제 version 을 기록하고, 모든 구성원을 페이지 단위로 읽으며, 필요하면 get_finding_record, get_finding_traffic 으로 원래 보고, PoC, 저장된 트래픽을 읽는다. 근본 원인이 명확하면 병합은 통합 보고서 때문에 막히지 않으며, 이어서 update_finding_case_report 로 통합 보고서를 저장하도록 최선을 다한다. 이번에 끝내지 못하면 폴더에 “통합 보고서 갱신 대기”가 표시되고, 이후 reporter 실행이나 사람이 보충한다. 통합 보고서는 저장할 때 개요, 영향, 전제 조건, 재현 절차, 증거, PoC, 근본 원인 분석, 수정 권고 장에 내용이 있어야 하며 중간에 잘리면 안 된다. 등급과 severity_reason 은 검증된 영향에 근거해야 하며, 구성원의 최고 등급을 그대로 가져오면 안 된다. 전제 조건, 전체 실행 순서, 입출력이 어떻게 이어지는지, 실행 가능한 완전한 스크립트와 명령을 한곳에 제시하고, 필요한 단계를 원래 보고들에 흩어 두지 않는다. 누락된 조건이나 검증되지 않은 단계는 사실대로 표시한다. 버전 충돌 시 자료를 다시 읽고 새로 생성하며, version 만 바꿔 재시도하면 안 된다. 각 구성원은 여전히 독립된 보고, 등급, 증거를 가진다. 도구가 꺼져 있으면 설정을 우회하지 않는다.`
}

const historicalFindingReviewGuidance = `

【과거 정리 모드, 기존 보고 작성 및 자동 트래픽 바인딩 지침보다 우선】이번에는 이미 존재하는 보고만 정리한다. 원래 보고가 비어 있거나 갱신 대기 중이어도 누락 상황만 기록하며, update_finding_report, report_finding, bind_finding_traffic 을 절대 호출하지 않는다. 원래 제목, 등급, 상태, 증거, 보고를 수정하지 않고, Bash 실행, 파일 쓰기, 대상 재탐지를 하지 않는다.
먼저 get_finding_record 로 선택된 기록을 하나씩 읽고, 기존 원래 보고는 판단 자료로 그대로 재사용하며 다시 작성하지 않는다. 선택된 범위 안에서만 같은 실제 결함인지 확인하며, 필요하면 이미 있는 증거를 읽을 수 있다. 구조화된 증거가 서로 다른 엔드포인트/라우트, 핸들러나 백엔드 컴포넌트, 파라미터 또는 누락된 검사가 서로 다른 코드 경로에 속함을 보여 주면 independent 로 판정할 수 있다. 유형, 자산, 제목, 토큰, 로그인 진입점이 비슷한 것만으로는 부족하다. 구체적인 공통점이 명확하고 핵심 근본 원인 증거가 없을 때만 기록 2..20건을 포괄하는 의심 그룹 하나를 저장하며, 구체적인 공통점이 없으면 independent 를 유지한다. 과거 정리는 selected scope 만 처리할 수 있으며, 같은 작업 제한과 수동 기각을 반드시 지킨다.
과거 모드에서는 update_finding_report, report_finding, bind_finding_traffic 호출이 금지되며, 원래 제목, 등급, 상태, 증거, 보고를 수정하지 않고, Bash 실행, 파일 쓰기, 대상 재탐지를 하지 않는다. 기존 폴더나 새 폴더는 모든 구성원을 읽고, update_finding_case_report 로 완전하고 최신인 통합 보고서를 쓰도록 최선을 다한다. 병합은 보고서가 잠시 없다고 막히지 않으며, 완료하지 못하면 폴더에 “통합 보고서 갱신 대기”가 표시되고 이후 실행이나 사람이 보충할 수 있다. 통합 보고서는 별도의 자료이며 원래 보고를 덮어쓰지 않는다. 마지막에는 반드시 complete_finding_case_review 를 호출해 선택된 finding_id 마다 verdict(independent/merged/suggested), reason, root_cause_evidence 를 제출하고 요약을 제공한다. 근본 원인 증거에는 구체적으로 같거나 다른 검사와 증거 출처를 설명해야 하며, 의심 제안에는 보충해야 할 증거를 명확히 한다. 서버 검수를 통과한 뒤에야 완료를 선언할 수 있다. 수동 정리는 selected scope 만 판단할 수 있으며 선택되지 않은 기록을 추가할 수 없다. 도구를 쓸 수 없거나 검수에 실패하면 사실대로 보고하며, 자연어로만 완료를 주장할 수 없다.`
