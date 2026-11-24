package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/traffic"
	actool "github.com/Autumn-27/norma/tool"
)

func (s *Server) seedFindingWorkflowTools() {
	const hostSearchDescriptionFlag = "finding_workflow_tools_v3_host_search_description"
	if value, _, _ := s.m.pg.GetSetting(hostSearchDescriptionFlag); value != "true" {
		// Only replace the original built-in text. A user-edited description is
		// authoritative and must survive upgrades.
		legacy := "기록 프록시가 이미 캡처한 대상 트래픽을 조회한다(host 를 반드시 지정해야 하며, URL 부분 문자열이나 본문 키워드로 더 거를 수 있음). body_contains 는 캡처된 요청/응답 헤더와 본문에서 전문 검색을 하며, 임의의 부분 문자열과 한글/중국어(최소 3 자)를 지원해, 응답 속의 비밀번호, 키, 오류, 내부망 주소 등을 찾는 데 쓸 수 있다. 아주 가벼운 색인(id/method/url/status/resp_len)만 반환하고 응답 내용은 담지 않는다. 기본적으로 3 건만 반환하며 페이지당 최대 10 건이다. 결과가 많으면 page 로 넘긴다(page=0 부터). 특정 항목의 요청/응답 원문은 traffic_get(id) 로 본다. 이미 접근한 리소스를 되돌아보거나 엔드포인트를 찾을 때 먼저 이것을 써서 같은 URL 을 반복해 curl 하지 않는다."
		if _, err := s.m.pg.Exec(`UPDATE tools SET description=$1,updated_at=(strftime('%Y-%m-%d %H:%M:%f','now')) WHERE key='traffic_search' AND system AND description=$2`, traffic.TrafficSearchDescription, legacy); err != nil {
			// Log and leave the flag unset so the next startup retries; do not
			// return, or a transient error here would also skip the reporter
			// migration below — the two are independent.
			log.Printf("[evidence] upgrade traffic_search description: %v", err)
		} else {
			_ = s.m.pg.SetSetting(hostSearchDescriptionFlag, "true")
		}
	}
	const flag = "finding_workflow_tools_v2_reporter"
	if value, _, _ := s.m.pg.GetSetting(flag); value == "true" {
		return
	}
	for _, key := range []string{"report_finding", "add_hint", "add_task_hint"} {
		row, err := s.m.pg.GetTool(key)
		if err != nil {
			log.Printf("[evidence] load %s: %v", key, err)
			return
		}
		if row == nil || !row.System {
			continue
		}
		var schema map[string]any
		if err := json.Unmarshal(row.Schema, &schema); err != nil {
			log.Printf("[evidence] invalid schema for %s: %v", key, err)
			return
		}
		if schema == nil {
			log.Printf("[evidence] missing object schema for %s", key)
			return
		}
		props := objectProperty(schema, "properties")
		if key == "report_finding" {
			if _, exists := props["evidence_hint_id"]; !exists {
				props["evidence_hint_id"] = map[string]any{"type": "integer", "description": "선택: 이 작업에서 이 취약점에 해당하는 hint ID. 그 힌트에 저장된 traffic_refs 를 읽어 함께 바인딩하며, 힌트가 없으면 생략"}
			}
		} else {
			if _, exists := props["traffic_refs"]; !exists {
				props["traffic_refs"] = agent.HintTrafficSchema()
			}
			hints := objectProperty(props, "hints")
			if _, exists := hints["type"]; !exists {
				hints["type"] = "array"
			}
			items := objectProperty(hints, "items")
			if _, ok := items["type"]; !ok {
				items["type"] = "object"
			}
			itemProps := objectProperty(items, "properties")
			for name, value := range map[string]any{"text": strParam("힌트 내용"), "asset_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}}, "traffic_refs": agent.HintTrafficSchema()} {
				if _, exists := itemProps[name]; !exists {
					itemProps[name] = value
				}
			}
		}
		raw, _ := json.Marshal(schema)
		result, err := s.m.pg.Exec(`UPDATE tools SET schema=$2,updated_at=(strftime('%Y-%m-%d %H:%M:%f','now')) WHERE key=$1 AND system AND schema=$3`, key, string(raw), string(row.Schema))
		if err != nil {
			log.Printf("[evidence] upgrade %s: %v", key, err)
			return
		}
		if n, _ := result.RowsAffected(); n != 1 {
			return
		} // preserve concurrent user edits
	}
	// Upgrade only the original default binding. Customized lists and enabled
	// flags survive; the one-time flag also preserves future user unbinding.
	readers := `["worker","reporter"]`
	for _, key := range []string{"traffic_search", "traffic_get", "traffic_blob"} {
		if _, err := s.m.pg.Exec(`UPDATE tools SET agents=$2 WHERE key=$1 AND system AND (agents='["worker"]' OR (NOT EXISTS (SELECT 1 FROM json_each('["worker","planner","mainagent","auto","pentest"]') required WHERE required.value NOT IN (SELECT value FROM json_each(agents))) AND json_array_length(agents)=5))`, key, readers); err != nil {
			return
		}
	}
	if _, err := s.m.pg.Exec(`UPDATE tools SET agents=$1 WHERE key='get_finding_traffic' AND system AND NOT EXISTS (SELECT 1 FROM json_each('["auto","reporter"]') required WHERE required.value NOT IN (SELECT value FROM json_each(agents))) AND json_array_length(agents)=2`, `["auto","reporter","worker","planner","mainagent","pentest"]`); err != nil {
		return
	}
	// Replace the previous code default only; preserve customized binding lists.
	if _, err := s.m.pg.Exec(`UPDATE tools SET agents='["reporter"]' WHERE key='bind_finding_traffic' AND system AND NOT EXISTS (SELECT 1 FROM json_each('["worker","planner","mainagent","auto","pentest"]') required WHERE required.value NOT IN (SELECT value FROM json_each(agents))) AND json_array_length(agents)=5`); err != nil {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("reporter", []string{"bind_finding_traffic"}); err != nil {
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

func objectProperty(parent map[string]any, key string) map[string]any {
	value, ok := parent[key].(map[string]any)
	if !ok {
		value = map[string]any{}
		parent[key] = value
	}
	return value
}

func (s *Server) agentFindingTrafficAccess(ctx context.Context, id int64, write bool) error {
	if write {
		if err := s.checkCaseReviewScope(ctx, "bind_finding_traffic", findingCaseRequest{}); err != nil {
			return err
		}
	}
	if id <= 0 {
		return errors.New("finding_id 는 독립 취약점 기록 ID 여야 합니다. 탐색 노드 ID 가 아닙니다")
	}
	f, err := s.m.pg.GetFinding(id)
	if err != nil {
		return err
	}
	if f == nil {
		return fmt.Errorf("%w: finding_id=%d. 증거 도구는 독립 취약점 기록 ID 를 쓰므로 list_task_findings / get_task_node_detail 의 finding_id 필드에서 읽으세요. id / finding_node_id 를 넘기지 마세요", db.ErrFindingNotFound, id)
	}
	if ri := agent.RunInfoFrom(ctx); ri.TaskID > 0 {
		task := s.m.ResolveTask(strconv.FormatInt(ri.TaskID, 10))
		if task == nil {
			return errors.New("작업이 존재하지 않습니다")
		}
		_, inherited, allowed := findingProvenanceInTask(task, f.TaskID)
		if !allowed {
			return errors.New("현재 작업에서는 이 취약점을 읽을 수 없습니다")
		}
		if write && inherited {
			return errors.New("상속된 취약점의 트래픽 증거는 읽기 전용입니다. 출처 작업에서 수정하세요")
		}
	}
	return nil
}

func (s *Server) toolBindFindingTraffic() actool.CoreTool {
	return wrTool("bind_finding_traffic", "이미 등록된 취약점에 확인된 실제 HTTP 트래픽을 나중에 바인딩한다. finding_id 는 독립 취약점 기록 ID 를 쓰고 탐색 노드 ID 를 넘기지 않는다. 같은 배치의 참조는 전부 성공하거나 전부 실패하며, 중복 참조는 기존 설명을 덮어쓰지 않는다. 나중에 바인딩하면 기존 보고가 갱신 대기로 표시된다. 패킷을 채우려고 다시 탐지하거나 취약점을 중복 생성하지 않는다.",
		objSchema(map[string]any{"finding_id": strParam("독립 취약점 기록 ID. list_task_findings / get_task_node_detail 의 finding_id 필드에서 읽음"), "traffic_refs": agent.HintTrafficSchema()}, "finding_id", "traffic_refs"),
		func(ctx context.Context, raw json.RawMessage) (actool.Result, error) {
			if !s.m.pg.GetBool(settingAgentTrafficBinding, false) {
				return actool.Errorf("Agent 자동 트래픽 바인딩이 꺼져 있습니다. 시스템 설정에서 켜거나 페이지에서 수동으로 바인딩하세요."), nil
			}
			var args struct {
				FindingID json.RawMessage `json:"finding_id"`
				Refs      []db.TrafficRef `json:"traffic_refs"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			id := parseProfileID(args.FindingID)
			if err := s.agentFindingTrafficAccess(ctx, id, true); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if len(args.Refs) == 0 {
				return actool.Errorf("나중에 바인딩하려면 확인된 traffic_refs 가 최소 하나 필요합니다. 트래픽이 없으면 이 도구를 호출할 필요가 없습니다"), nil
			}
			list, err := s.evidenceStore().Bind(ctx, id, args.Refs)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return jsonResult(trafficSummary(list))
		})
}
