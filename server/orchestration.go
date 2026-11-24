package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

// jsonResult marshals v to a JSON tool result.
func jsonResult(v any) (actool.Result, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return actool.Errorf(err.Error()), nil
	}
	return actool.Text(string(b)), nil
}

// 이 파일은 P2 「작업 간 오케스트레이션 도구 세트」를 구현한다(docs/跑分编排 §2 P2). 이것들은 host 도구다——
// Manager(임의 작업의 Store), Engine(일시 중지), 작업 생성 흐름에 접근해야 하므로 server 계층에 둔다.
// 읽기 도구는 「기존 per-task 도구」를 대상 작업의 store 에서 실행하도록 리디렉션하고(임시 ToolSet 을 만들어
// 그 대응 도구를 Call), 완전히 같은 로직을 재사용한다. 제어 도구(spawn/pause)는 Manager/Engine 을 직접 호출한다.
// 트래픽 도구처럼 tools 테이블에 seed 되고 agent 별로 바인딩된다(오케스트레이션 agent 에만 바인딩해 보이게 함).

// hostTools is the runtime host-tool provider fed to ToolAugment: traffic tools
// (gated by capture) + cross-task orchestration tools + user-defined custom tools.
// The second return is the names of custom tools flagged `deferred` (schema
// withheld, routed via SearchExtraTools/ExecuteExtraTool). Per-agent binding still
// decides who actually sees any of them.
//
//nolint:unused // used as the hostTools provider in wireAgentAugment
func (s *Server) hostTools() ([]actool.CoreTool, map[string][]string) {
	tools := append(s.m.HostTools(), s.orchestrationTools()...)
	tools = append(tools, s.toolpackTools()...)
	tools = append(tools, s.findingRetestTools()...)
	tools = append(tools, s.platformTools()...) // 플랫폼 조작 도구(skill/도구/MCP 생성·수정, Auto 용)
	custom, err := s.customTools()
	if err != nil {
		log.Printf("[custom-tool] 로드 실패: %v", err)
		return tools, nil
	}
	tools = append(tools, custom...)
	// deferred custom tools → name -> its bound agent keys. ToolAugment turns a
	// name into a deferred entry only for agents it's actually bound to (so we don't
	// advertise a tool the per-agent binding will drop from the callable set).
	deferred := map[string][]string{}
	rows, _ := s.m.pg.ListCustomTools()
	for _, t := range rows {
		if t.Deferred && t.Enabled {
			deferred[t.Key] = t.Agents
		}
	}
	return tools, deferred
}

// orchestrationTools returns the cross-task tool set. Bound per-agent via the
// tools table (default: no binding — opt-in for orchestration agents).
func (s *Server) orchestrationTools() []actool.CoreTool {
	return append([]actool.CoreTool{
		s.toolListTasks(),
		s.toolListLLMProfiles(),
		s.toolSpawnTask(),
		s.toolPauseTask(),
		s.toolGetTaskGraph(),
		s.toolListTaskFindings(),
		s.toolAddHint(),
		s.toolGetWorkerTrace(),
		s.toolListWorkerTraces(),
		s.toolSearchWorkerTraces(),
		s.toolGetTaskNodeDetail(),
		s.toolUpdateFindingReport(),
		s.toolGetFindingTraffic(),
		s.toolBindFindingTraffic(),
	}, s.findingCaseTools()...)
}

// --- schema helpers ---

func strParam(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

// parseProfileID reads an LLM profile id from a tool arg that may arrive as a JSON
// number (5) or a numeric string ("5"); returns 0 when absent/unparseable.
func parseProfileID(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var n int64
	if json.Unmarshal(raw, &n) == nil {
		return n
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		v, _ := strconv.ParseInt(strings.TrimSpace(str), 10, 64)
		return v
	}
	return 0
}

func objSchema(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		req := make([]any, len(required))
		for i, r := range required {
			req[i] = r
		}
		m["required"] = req
	}
	return m
}

func roTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: name, Description: desc, Schema: schema,
		ReadOnly:   func(json.RawMessage) bool { return true },
		Concurrent: func(json.RawMessage) bool { return true },
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			return run(ctx, in)
		},
	})
}

func wrTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: name, Description: desc, Schema: schema,
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			return run(ctx, in)
		},
	})
}

// delegateToTask resolves the `task_id` in the input, builds a ToolSet bound to
// that task's store, strips task_id, and calls the chosen per-task tool — so the
// cross-task read reuses the exact in-task logic against another task.
func (s *Server) delegateToTask(ctx context.Context, in json.RawMessage, pick func(*agent.ToolSet) actool.CoreTool) (actool.Result, error) {
	var head struct {
		TaskID string `json:"task_id"`
	}
	_ = json.Unmarshal(in, &head)
	if strings.TrimSpace(head.TaskID) == "" {
		return actool.Errorf("task_id 는 필수입니다"), nil
	}
	t, ok := s.m.Task(head.TaskID)
	if !ok {
		return actool.Errorf("task 가 존재하지 않습니다: " + head.TaskID), nil
	}
	var m map[string]json.RawMessage
	_ = json.Unmarshal(in, &m)
	delete(m, "task_id")
	inner, _ := json.Marshal(m)
	tsx := agent.NewToolSet(t.Store, "orchestrator")
	if s.m.Assets() != nil {
		tsx.SetAssetStore(s.m.Assets(), s.m.Assets().Companies())
	}
	tsx.SetNotify(t.Notify)         // 일반 깨우기(전용 콜백이 없는 쓰기 작업이 사용. 읽기 도구는 no-op)
	tsx.SetNotifyHint(t.NotifyHint) // add_hint → 「사람이 전략 힌트 N 개를 추가함:…」 트리거를 기록하고 planner 를 깨움
	return pick(tsx).Call(ctx, inner, nil)
}

// --- tools ---

func (s *Server) toolListTasks() actool.CoreTool {
	return roTool("list_tasks",
		"모든 작업을 나열한다(id/설명/목표/상태/실행 시간/상위 작업/LLM 설정). 오케스트레이션 agent 가 이것으로 전체를 파악하고, 어떤 작업이 너무 오래 멈췄는지, 각자 어떤 LLM 을 쓰는지 본다. 실행 시간: 실행 중=생성→현재, 최종 상태=생성→마지막 활동(초). llm_profile: 작업의 planner/worker 가 쓰는 설정 이름. (활성 설정)=전역 활성을 따름.",
		objSchema(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			lastAct, _ := s.m.Database().LastActivityAll()
			// id -> name to resolve each task's pinned LLM profile.
			profName := map[int64]string{}
			if profs, err := s.m.pg.ListProfiles(); err == nil {
				for _, p := range profs {
					profName[p.ID] = p.Name
				}
			}
			out := make([]map[string]any, 0)
			for _, t := range s.m.List() {
				status := s.deriveTaskStatus(t)
				end := lastAct[t.ExpID]
				if live := s.engine.LastActivity(t.ID); live > end {
					end = live
				}
				dur := int64(0)
				if status == "running" {
					dur = time.Now().Unix() - t.CreatedAt
				} else if end > t.CreatedAt {
					dur = end - t.CreatedAt
				}
				row := map[string]any{"id": t.ID, "description": t.Description, "goal": t.Goal, "status": status, "run_seconds": dur}
				if t.ParentRef != "" {
					row["parent_ref"] = t.ParentRef
				}
				llmState := t.llmStateSnapshot()
				if llmState.ProfileID == nil {
					row["llm_profile"] = "(활성 설정)"
				} else if n, ok := profName[*llmState.ProfileID]; ok {
					row["llm_profile"] = n
				} else {
					row["llm_profile"] = fmt.Sprintf("#%d(삭제됨)", *llmState.ProfileID)
				}
				out = append(out, row)
			}
			return jsonResult(out)
		})
}

// toolListLLMProfiles lists the available LLM profiles (name/model/active) so an
// orchestration agent can pick one for spawn_task's llm_profile. Never leaks keys.
func (s *Server) toolListLLMProfiles() actool.CoreTool {
	return roTool("list_llm_profiles",
		"사용 가능한 LLM 설정(profile)을 나열한다: id, 이름, 모델, 형식, 현재 활성 설정인지. id 로 spawn_task 의 llm_profile_id 파라미터에 하위 작업 전용 LLM 을 지정한다(예: 정찰은 저렴한 모델, 익스플로잇은 강한 모델). API Key 는 포함하지 않는다.",
		objSchema(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			profs, err := s.m.pg.ListProfiles()
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			out := make([]map[string]any, 0, len(profs))
			for _, p := range profs {
				out = append(out, map[string]any{
					"id": p.ID, "name": p.Name, "model": p.Model, "format": p.Format, "is_active": p.IsDefault,
				})
			}
			return jsonResult(map[string]any{"profiles": out})
		})
}

func (s *Server) toolSpawnTask() actool.CoreTool {
	return wrTool("spawn_task",
		"하위 작업을 새로 만들고 탐색 엔진을 시작하며 task_id 를 반환한다. 어떤 일(예: 문제 하나/대상 하나)을 독립된 작업으로 분배하는 데 쓴다. parent_ref 는 선택: 현재 오케스트레이션과 연결된 상위 작업 id 를 넣어 부모-자식으로 연결한다.",
		objSchema(map[string]any{
			"description":            strParam("작업 설명(짧은 제목)"),
			"goal":                   strParam("작업 목표(무엇을 달성할지)"),
			"parent_ref":             strParam("선택: 상위 작업 id(부모-자식 연결)"),
			"source_task_ids":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": fmt.Sprintf("선택: 읽기 전용으로 상속할 출처 작업 id 목록(최대 %d 개). 하위 작업은 이 작업들이 이미 밝혀낸 자산/결론을 시작점으로 읽기 전용 참조할 수 있다. parent_ref 의 순수 부모-자식 포인터와 달리 이것은 내용 상속이다.", db.MaxTaskSourceCount)},
			"llm_profile_id":         map[string]any{"type": "integer", "description": "선택: 이 하위 작업의 planner/worker 가 쓸 LLM 설정 id 지정(list_llm_profiles 참고). 비우면 상위 작업을 상속하고, 그다음 전역 활성 설정으로 돌아감"},
			"timeout_seconds":        map[string]any{"type": "integer", "description": "선택: 작업 단위 시간 초과(초). 시각이 되면 정상 마무리를 트리거하고 timeout 최종 상태에 들어감. 비우거나 0 = 시간 무제한"},
			"plan_heartbeat_seconds": map[string]any{"type": "integer", "description": "선택: planner 하트비트 트리거 간격(초). 지난 계획 종료/작업 시작 이후 이 값만큼 지났고 그동안 트리거가 없으면 → 계획 턴을 트리거함(교착 상태 대비 + 비행 중인 worker 감독을 위한 깨우기). 비우거나 0 = 기본 600(10min);"},
			"seed_first_intent":      map[string]any{"type": "boolean", "description": "선택: 간단한 작업은 켤 수 있으며, 생성할 때 시드 의도(내용=설명+목표)를 바로 내려보내 worker 가 첫 planner 를 기다리지 않고 바로 테스트를 시작하게 함. 기본 false(표준인 먼저 계획 후 실행)."},
		}, "description", "goal"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Description          string          `json:"description"`
				Goal                 string          `json:"goal"`
				ParentRef            string          `json:"parent_ref"`
				SourceTaskIDs        []string        `json:"source_task_ids"`
				LLMProfileID         json.RawMessage `json:"llm_profile_id"`
				TimeoutSeconds       int             `json:"timeout_seconds"`
				PlanHeartbeatSeconds int             `json:"plan_heartbeat_seconds"`
				SeedFirstIntent      bool            `json:"seed_first_intent"`
			}
			_ = json.Unmarshal(in, &a)
			if strings.TrimSpace(a.Description) == "" {
				a.Description = "이름 없는 작업"
			}
			if strings.TrimSpace(a.Goal) == "" {
				return actool.Errorf("goal 은 필수입니다"), nil
			}
			if a.TimeoutSeconds < 0 {
				a.TimeoutSeconds = 0
			}
			// 읽기 전용 상속 출처 작업: 개수 상한 + 각 id 유효/중복 제거/존재 검증이며, 검증 규칙은 HTTP 작업 생성과 같다.
			if len(a.SourceTaskIDs) > db.MaxTaskSourceCount {
				return actool.Errorf(fmt.Sprintf("연결 작업은 최대 %d 개까지 선택할 수 있습니다", db.MaxTaskSourceCount)), nil
			}
			sourceIDs := make([]int64, 0, len(a.SourceTaskIDs))
			seenSources := map[int64]bool{}
			for _, raw := range a.SourceTaskIDs {
				id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
				if err != nil || id <= 0 || seenSources[id] {
					return actool.Errorf("연결 작업 id 가 유효하지 않거나 중복되었습니다"), nil
				}
				if _, ok := s.m.Task(strconv.FormatInt(id, 10)); !ok {
					return actool.Errorf(fmt.Sprintf("연결 작업 #%d 가 존재하지 않습니다", id)), nil
				}
				seenSources[id] = true
				sourceIDs = append(sourceIDs, id)
			}
			// LLM profile resolution: explicit id > inherit parent's pin > active(nil).
			var pin *int64
			if id := parseProfileID(a.LLMProfileID); id > 0 {
				if _, ok := s.loadProfileConfig(id); !ok {
					return actool.Errorf(fmt.Sprintf("LLM 설정 #%d 가 없거나 API Key 가 설정되지 않았습니다", id)), nil
				}
				pin = &id
			} else if a.ParentRef != "" {
				if pt, ok := s.m.Task(a.ParentRef); ok {
					pin = pt.LLMProfileID
				}
			}
			var llmIDs []int64
			if pin != nil {
				llmIDs = []int64{*pin}
			}
			t, err := s.m.CreateTaskWithOptions(a.Description, a.Goal, db.TaskCreateOptions{
				SourceTaskIDs:        sourceIDs,
				LLMProfileIDs:        llmIDs,
				TimeoutSeconds:       a.TimeoutSeconds,
				PlanHeartbeatSeconds: a.PlanHeartbeatSeconds,
			})
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if a.ParentRef != "" {
				t.ParentRef = a.ParentRef
				if id, e := strconv.ParseInt(t.ID, 10, 64); e == nil {
					_ = s.m.Database().SetParentRef(id, a.ParentRef)
				}
			}
			// 공유되는 생성 후 흐름이며, HTTP 작업 생성(server.go createTask)과 같은 launchTask 를 재사용한다:
			// seed + 백그라운드에서 보이게 목표 분해(0 번째 턴/LLM 단계/목표 하나씩) + engine.Run.
			// seed_first_intent 는 기본 false(표준인 먼저 계획 후 실행). 간단한 작업은 켜서 work 하나를 바로 내려보내 테스트할 수 있다.
			s.launchTask(t, a.Description+" "+a.Goal, a.SeedFirstIntent)
			return actool.Text(fmt.Sprintf("task created: %s", t.ID)), nil
		})
}

func (s *Server) toolPauseTask() actool.CoreTool {
	return wrTool("pause_task", "지정한 작업을 일시 중지한다(그 planner/worker 루프를 멈춤).",
		objSchema(map[string]any{"task_id": strParam("일시 중지할 작업 id")}, "task_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				TaskID string `json:"task_id"`
			}
			_ = json.Unmarshal(in, &a)
			t, ok := s.m.Task(a.TaskID)
			if !ok {
				return actool.Errorf("task 가 존재하지 않습니다: " + a.TaskID), nil
			}
			if _, err := s.applyTaskControlWithCause(t, "pause", agent.AbortPausedByOrchestrator); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text("task paused: " + a.TaskID), nil
		})
}

func (s *Server) toolGetTaskGraph() actool.CoreTool {
	return roTool("get_task_graph", "지정한 작업의 탐색 그래프 개요를 읽는다(graph_overview 와 같음: 자산 수/frontier/발견/커버리지 등). task_id 로 작업을 지정한다.",
		objSchema(map[string]any{"task_id": strParam("작업 id")}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).GraphOverviewTool)
		})
}

func (s *Server) toolListTaskFindings() actool.CoreTool {
	return roTool("list_task_findings", "지정한 작업의 확인된 취약점을 읽는다(flag/PoC 포함. 항목마다 id/task_id/intent_id/vulnclass/severity/요약/상태). task_id 로 작업을 지정한다.",
		objSchema(map[string]any{"task_id": strParam("작업 id")}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).ListFindingsTool)
		})
}

func (s *Server) toolAddHint() actool.CoreTool {
	return wrTool("add_task_hint", "지정한 작업에 전략 힌트를 주입한다(그 작업의 planner 가 다음 턴에 의도를 만들 때 읽는다).\n"+
		"★일괄 우선: 여러 힌트는 hints 배열에 넣어 한 번에 제출한다(ids 배열을 반환하며 hints 와 길이와 순서가 같고, 실패 항목은 id=0). 단건이면 hints 를 생략하고 최상위 text 를 바로 준다.",
		objSchema(map[string]any{
			"task_id":      strParam("작업 id"),
			"hints":        map[string]any{"type": "array", "description": "【이것을 우선 사용】 힌트 배열이며, 각 요소의 필드는 최상위와 같다(text/asset_ids/traffic_refs).", "items": objSchema(map[string]any{"text": strParam("힌트 내용"), "asset_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}}, "traffic_refs": agent.HintTrafficSchema()})},
			"text":         strParam("[단건] 힌트 내용"),
			"traffic_refs": agent.HintTrafficSchema(),
			"asset_ids":    map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "고정할 자산 id(선택, 0/1/여러 개. 그 작업 안의 자산 id)"},
		}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).AddHintTool)
		})
}

func (s *Server) toolGetWorkerTrace() actool.CoreTool {
	return roTool("get_task_worker_trace",
		"지정한 작업에서 특정 work(의도)의 실행 과정을 본다: get_task_worker_trace(task_id, intent_id) 로 단계 요약을 보고, step_ids=[...] 를 함께 주면 그 단계들의 전체 내용을 가져온다(한 번에 최대 5 개, 더 넘기면 앞 5 개만 반환).",
		objSchema(map[string]any{
			"task_id":   strParam("작업 id"),
			"intent_id": map[string]any{"type": "integer", "description": "의도 id(그 작업 안의 work)"},
			"step_ids":  map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "선택: 전체 내용을 가져올 단계 id(한 번에 최대 5 개, 더 넘기면 앞 5 개만 반환하고 나머지는 omitted_step_ids 에 나열)"},
		}, "task_id", "intent_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).GetWorkerTraceTool)
		})
}

func (s *Server) toolListWorkerTraces() actool.CoreTool {
	return roTool("list_task_worker_traces", "지정한 작업에서 어떤 work(의도)를 실행했는지와 각자의 단계 수를 나열한다. 어떤 work 를 살펴볼 가치가 있는지 찾는 데 쓴다(그다음 get_task_worker_trace 사용).",
		objSchema(map[string]any{"task_id": strParam("작업 id")}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).ListWorkerTracesTool)
		})
}

func (s *Server) toolSearchWorkerTraces() actool.CoreTool {
	return roTool("search_task_worker_traces", "지정한 작업에서 키워드로 모든 work 의 실행 과정을 검색한다(적중한 단계 요약 + intent_id 반환).",
		objSchema(map[string]any{"task_id": strParam("작업 id"), "q": strParam("검색 키워드")}, "task_id", "q"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).SearchWorkerTracesTool)
		})
}

func (s *Server) toolGetTaskNodeDetail() actool.CoreTool {
	return roTool("get_task_node_detail",
		"지정한 작업에서 특정 탐색 그래프 노드의 전체 내용을 읽는다(발견/사실/의도/목표: 요약 + 상세/증거/PoC). id 는 탐색 노드 id 다(예: report_finding 반환값, 또는 list_task_findings 의 id). 취약점 보고서를 쓰기 전에 이것으로 그 취약점의 전체 증거를 가져온다.",
		objSchema(map[string]any{
			"task_id": strParam("작업 id"),
			"id":      map[string]any{"type": "integer", "description": "탐색 그래프 노드 id(자산 id 아님)"},
		}, "task_id", "id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).NodeDetailTool)
		})
}

// toolUpdateFindingReport writes/overwrites a finding's detailed Markdown report.
// finding_id is the id report_finding returned ("finding recorded: <id>", the
// finding node id). The write (SetFindingReportByNodeID) is keyed by node_id and
// task-agnostic, so this host tool needs no task_id / exploration store.
func (s *Server) toolUpdateFindingReport() actool.CoreTool {
	return wrTool("update_finding_report",
		"이미 등록된 취약점에 【상세 보고서】를 쓰거나 갱신한다(Markdown 전문, 기존 내용을 통째로 덮어씀). finding_id 에는 report_finding 이 반환한 그 id 를 넘긴다(\"finding recorded: <id>\" 의 숫자). 보고서에는 취약점 개요, 영향과 위험, 재현 절차, 증거/PoC, 수정 권고를 담는 것이 좋다.",
		objSchema(map[string]any{
			"finding_id":       map[string]any{"type": "integer", "description": "대상 취약점 id(report_finding 이 반환한 id)"},
			"report":           strParam("상세 보고서 전문, Markdown 형식"),
			"evidence_version": map[string]any{"type": "integer", "description": "get_finding_traffic 가 반환한 증거 version. 보고서가 새 증거 변경을 덮어쓰는 것을 막는 데 쓴다"},
		}, "finding_id", "report"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				EvidenceVersion *int64          `json:"evidence_version"`
				FindingID       json.RawMessage `json:"finding_id"`
				Report          string          `json:"report"`
			}
			_ = json.Unmarshal(in, &a)
			nodeID := parseProfileID(a.FindingID) // 「숫자 또는 숫자 문자열」 해석 재사용
			if nodeID <= 0 {
				return actool.Errorf("finding_id 가 유효하지 않습니다"), nil
			}
			fid, err := s.m.pg.FindingIDByNodeID(nodeID)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if fid > 0 {
				if err := s.checkCaseReviewScope(ctx, "update_finding_report", findingCaseRequest{FindingID: json.RawMessage(i64s(fid))}); err != nil {
					return actool.Errorf(err.Error()), nil
				}
			}
			n, err := s.m.pg.SetFindingReportVersionByNodeID(ctx, nodeID, a.Report, a.EvidenceVersion)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if n == 0 {
				return actool.Errorf(fmt.Sprintf("finding_id=%d 에 해당하는 취약점 기록을 찾지 못했습니다(먼저 report_finding 으로 등록하세요)", nodeID)), nil
			}
			return actool.Text(fmt.Sprintf("finding %d report updated (%d chars)", nodeID, len(a.Report))), nil
		})
}

// deriveTaskStatus mirrors listTasks' status derivation for the list_tasks tool.
func (s *Server) deriveTaskStatus(t *Task) string {
	lifecycle := t.lifecycleSnapshot()
	switch {
	case isTerminalStatus(lifecycle.Status):
		return lifecycle.Status
	case lifecycle.Paused || s.engine.IsPaused(t.ID):
		return "paused"
	case s.engine.ReadyFor(t) && s.engine.Started(t.ID):
		return "running"
	}
	return "created"
}

// orchestrationToolSeeds seeds the cross-task tools into the tools table so they
// are bindable per-agent (default: bound to nobody — opt-in for orchestration
// agents). First-insert only, like the traffic seeds.
func (s *Server) seedOrchestrationTools() {
	// task-op + platform tools default-bind to the built-in Auto agent (태생적으로 플랫폼을
	// 조작하는 데 쓰임). SeedTool 은 첫 삽입에서만 적용됨. 기존 DB 에 이미 seed 된 행은 seedAutoDefaultBindings 가 보충 바인딩함.
	autoAgents, _ := json.Marshal([]string{"auto"})
	for _, t := range s.orchestrationTools() {
		schema, _ := json.Marshal(t.InputSchema())
		bindings := autoAgents
		if t.Name() == "bind_finding_traffic" || strings.Contains(t.Name(), "finding_case") || t.Name() == "search_finding_duplicates" || t.Name() == "get_finding_record" || t.Name() == "merge_finding_records" || t.Name() == "suggest_finding_merge" {
			bindings = json.RawMessage(`["reporter"]`)
		}
		_ = s.m.Database().SeedTool(t.Name(), t.Description(), schema, bindings)
	}
	for _, t := range s.platformTools() {
		schema, _ := json.Marshal(t.InputSchema())
		_ = s.m.Database().SeedTool(t.Name(), t.Description(), schema, autoAgents)
	}
	s.refreshBuiltinToolSchemas()
	s.seedAutoDefaultBindings()
	s.seedPlannerDefaultBindings()
	s.seedPlannerListAssetsBinding()
	s.seedCompanyScopeRebind()
	s.seedWorkerReadToolsUnbind() // list_facts/list_companies/list_worker_traces 를 worker 기본값에서 해제(일회성)
	s.seedWorkerReadbackRebind()  // 예전 마이그레이션의 잘못된 삭제 수정: search_all_worker_traces/get_worker_trace/node_detail 을 worker 에 다시 바인딩(일회성)
	s.seedAutoReportFindingBinding()
	s.unbindGoalMetDefault()
	s.reseedGoalsPrompt()             // goals 프롬프트에 「작업 제약 추출」 단계 추가 → 기존 DB 에 새 기본값 한 벌 추가(일회성)
	s.reseedMainAgentPrompt()         // mainagent 프롬프트에 「목표 달성 후 add_intent 로 목표 등록 여부 되묻기」 추가(일회성)
	s.reseedPlannerPrompt()           // planner 프롬프트: 「의도 0 개」 정당 이유 재작성 + 정량 검수 확인 추가(일회성)
	s.reseedWorkerPrompt()            // worker 프롬프트: 부정 결론 증거 기준 추가(일회성)
	s.seedReporterAgent()             // 「보고서 작성」 agent + 도구 바인딩 + finding 트리거 사전 설정(일회성)
	s.upgradeReporterTriggerMessage() // 기존 DB 마이그레이션 보충: reporter 가 evidence_version 을 되돌려 보내게 함(일회성)
	s.seedFindingTrafficTools()       // 선택적 증거 파라미터와 읽기 전용 증거 도구를 추가하며, 사용자 설정을 유지함
	s.seedFindingWorkflowTools()
	s.seedFindingCaseTools()
	// 참고: pentest 의 기본 도구 바인딩은 마이그레이션이 필요 없다——BuiltinToolSeeds 가 완전히 새로 초기화할 때
	// list_assets/insert_assets/report_finding/list_findings/list_companies 를 pentest 와 함께
	// seed 해 둔다(프로젝트에 아직 기존 DB 가 없어 마이그레이션하지 않음).
}

// refreshBuiltinToolSchemas propagates code schema/description changes on the
// orchestration + platform tools into already-seeded rows ONCE per version flag —
// SeedTool is first-insert-only, so a new param (e.g. spawn_task 의 llm_profile) never
// reaches an old DB otherwise. Preserves each tool's agent binding + enabled flag.
// Bump the flag whenever these tools' schemas/descriptions change in code.
func (s *Server) refreshBuiltinToolSchemas() {
	const flag = "tool_schema_refresh_v8_finding_case_tools"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	tools := append(s.orchestrationTools(), s.platformTools()...)
	for _, t := range tools {
		schema, _ := json.Marshal(t.InputSchema())
		if err := s.m.pg.RefreshToolDefaults(t.Name(), t.Description(), schema); err != nil {
			log.Printf("[tools] refresh %s schema failed: %v", t.Name(), err)
		}
	}
	// 동시에 일부 내장 agent 도구를 코드 기본값으로 갱신한다:
	//   - goal_met: 기존 DB 에 seed 된 설명에 "이번 계획 턴을 끝냄"이라는 오해 소지가 있어, planner 가 이를
	//     "빈 턴을 끝내는" 수단으로 여겨 시작하자마자 작업 전체를 완료로 오판한다.
	//   - insert_assets: related 입력 파라미터 추가(자산이 현재 작업과 관련되는지 표시하고 커버리지에 넣을지 결정),
	//     SeedTool 은 첫 삽입만이라 기존 DB 에 이미 seed 된 schema 는 이 새 파라미터를 받지 못한다.
	//   - list_facts: 페이지 방식으로 변경하고 limit/before/q 입력 파라미터 추가. 기존 DB 에 이미 seed 된 빈 schema 는
	//     도구 관리 페이지에 「파라미터 없음」으로 표시되고, 모델도 이 파라미터 설명을 받지 못한다.
	refreshBuiltin := map[string]bool{"goal_met": true, "insert_assets": true, "list_facts": true}
	for _, sd := range agent.BuiltinToolSeeds() {
		if !refreshBuiltin[sd.Key] {
			continue
		}
		schema, _ := json.Marshal(sd.Schema)
		if err := s.m.pg.RefreshToolDefaults(sd.Key, sd.Desc, schema); err != nil {
			log.Printf("[tools] refresh %s desc failed: %v", sd.Key, err)
		}
	}
	_ = s.m.pg.SetSetting(flag, "true")
	log.Printf("[tools] orchestration/platform 도구 schema 를 코드 기본값으로 갱신함(일회성)")
}

// unbindGoalMetDefault removes goal_met's default "planner" binding ONCE (guarded by
// a settings flag), so existing DBs match the new default of NO agent. goal_met bypasses
// per-goal prove_goal to declare the whole task done — powerful/risky and redundant with
// the prove_goal→auto-complete path — so it ships unbound; users can re-bind it per agent
// in the UI. A user's own binding to another agent is untouched (we only strip planner).
func (s *Server) unbindGoalMetDefault() {
	const flag = "goal_met_unbind_default_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.RemoveAgentFromTool("planner", "goal_met"); err != nil {
		log.Printf("[tools] goal_met 의 planner 해제 실패: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// reseedGoalsPrompt 는 goals 목표 분해기의 프롬프트를 【현재 코드 기본값】으로 갱신한다——기본 본문에 새로 추가된
// 「먼저 작업 제약 추출(set_constraints) 후 목표 분해」 단계인데, SeedPromptIfEmpty 는 첫 삽입만이라 기존 DB 의
// 기존 version 1 은 이 단계를 받지 못한다. 여기서는 버전 관리로 【새 버전을 추가】하고 거기로 전환한다(ResetPromptToDefault).
// 기존 버전은 이력에 남아, 사용자가 커스터마이즈했다면 버전 기록에서 되찾을 수 있다. settings flag 가드 → 한 번만 수행한다.
// 이후 기본값이 또 바뀌면 이 flag 를 올린다. 완전히 새 DB 는 처리할 필요가 없다(SeedPromptIfEmpty 가 이미 최신 기본값을 seed 함).
func (s *Server) reseedGoalsPrompt() {
	const flag = "goals_prompt_constraint_step_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 성공 여부와 상관없이 한 번만 시도
	a, err := s.m.pg.GetAgentByKey("goals")
	if err != nil || a == nil {
		return // 완전히 새 DB 라 아직 agent 행을 만들지 않은 경우, seedPrompts 가 바로 최신 기본값을 seed 하므로 이 마이그레이션이 필요 없음
	}
	tmpl := agent.BuiltinPromptSeeds()["goals"]
	if tmpl == "" {
		return
	}
	// 완전히 새 DB 는 seedPrompts 가 이미 최신 기본값을 seed 함 → 현재 버전이 이미 코드 기본값과 같으므로 중복 버전을 또 추가하지 않는다.
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] goals 프롬프트를 새 기본값으로 다시 갱신 실패: %v", err)
		return
	}
	log.Printf("[prompts] goals 프롬프트에 새 기본 버전 추가함(작업 제약 추출 단계 포함, 일회성)")
}

// reseedMainAgentPrompt 는 mainagent 프롬프트를 【현재 코드 기본값】으로 갱신한다——기본 본문에 「목표가 모두
// 달성된 뒤 add_intent 로 의도를 직접 투입할 때 사람에게 정식 목표로 등록할지 되묻기」라는 안내가 추가되었는데, SeedPromptIfEmpty 는 첫 삽입만이라
// 기존 DB 의 기존 버전은 받지 못한다. 버전 관리로 【새 버전을 추가】하고 거기로 전환한다(ResetPromptToDefault). 기존 버전은
// 이력에 남아 사용자가 커스터마이즈했다면 버전 기록에서 되찾을 수 있다. settings flag 가드 → 한 번만 수행. 완전히 새 DB 는 처리 불필요
// (SeedPromptIfEmpty 가 이미 최신 기본값을 seed 함). reseedGoalsPrompt 와 완전히 같은 구조다.
func (s *Server) reseedMainAgentPrompt() {
	const flag = "mainagent_prompt_goalless_intent_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 성공 여부와 상관없이 한 번만 시도
	a, err := s.m.pg.GetAgentByKey("mainagent")
	if err != nil || a == nil {
		return // 완전히 새 DB 라 아직 agent 행을 만들지 않은 경우, seedPrompts 가 바로 최신 기본값을 seed 하므로 이 마이그레이션이 필요 없음
	}
	tmpl := agent.BuiltinPromptSeeds()["mainagent"]
	if tmpl == "" {
		return
	}
	// 완전히 새 DB 는 seedPrompts 가 이미 최신 기본값을 seed 함 → 현재 버전이 이미 코드 기본값과 같으므로 중복 버전을 또 추가하지 않는다.
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] mainagent 프롬프트를 새 기본값으로 다시 갱신 실패: %v", err)
		return
	}
	log.Printf("[prompts] mainagent 프롬프트에 새 기본 버전 추가함(목표 달성 후 목표 등록 되묻기 포함, 일회성)")
}

// reseedPlannerPrompt 는 planner 프롬프트를 【현재 코드 기본값】으로 갱신한다——기본 본문을 간결하게 재구성하고, 「절제」를
// 중복 제거만으로 낮췄으며, 「깊이가 커버리지보다 우선」「절대 하한: 목표 미달성이고 실행 중 의도가 없으면 반드시 산출해야 함」을 추가하고, 부정 결론 재검토에 상한을 두었다.
// 기본값이 실질적으로 바뀔 때마다 아래 flag 를 올려(현재 v2) 기존 DB 를 다시 한 번 갱신한다. SeedPromptIfEmpty 는 첫 삽입만이라 기존 DB 의 기존 버전은 받지 못하므로, 버전 관리로
// 【새 버전을 추가】하고 거기로 전환한다(ResetPromptToDefault). 기존 버전은 이력에 남아 사용자가 커스터마이즈했다면 버전 기록에서
// 되찾을 수 있다. settings flag 가드 → 한 번만 수행. 완전히 새 DB 는 처리 불필요(SeedPromptIfEmpty 가 이미 최신 기본값을 seed 함).
// reseedGoalsPrompt 와 완전히 같은 구조다.
func (s *Server) reseedPlannerPrompt() {
	const flag = "planner_prompt_compact_realistic_v2"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 성공 여부와 상관없이 한 번만 시도
	a, err := s.m.pg.GetAgentByKey("planner")
	if err != nil || a == nil {
		return // 완전히 새 DB 라 아직 agent 행을 만들지 않은 경우, seedPrompts 가 바로 최신 기본값을 seed 하므로 이 마이그레이션이 필요 없음
	}
	tmpl := agent.BuiltinPromptSeeds()["planner"]
	if tmpl == "" {
		return
	}
	// 완전히 새 DB 는 seedPrompts 가 이미 최신 기본값을 seed 함 → 현재 버전이 이미 코드 기본값과 같으므로 중복 버전을 또 추가하지 않는다.
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] planner 프롬프트를 새 기본값으로 다시 갱신 실패: %v", err)
		return
	}
	log.Printf("[prompts] planner 프롬프트에 새 기본 버전 추가함(간결 재구성+절제를 중복 제거로 낮춤+깊이 우선+부정 재검토 상한, 일회성)")
}

// reseedWorkerPrompt 는 worker 프롬프트를 【현재 코드 기본값】으로 갱신한다——기본 본문의 record_fact 구간에서 「부정 결론은
// 관찰+잠정적 해석으로 쓰라」는 문장 전체를 삭제하고, confidence(observed/inferred)와 「이 의도의 수단을 다 썼는지」를 분리했으며(이것들이 플래너를 오도하기 쉬움),
// 동시에 facts 배열 분리를 「서로 완전히 독립적이어서 합칠 수 없는」 아주 드문 예외로 좁혔다. flag 를 v3 로 올려 기존 DB 를 다시 한 번 갱신한다.
// SeedPromptIfEmpty 는 첫 삽입만이라 기존 DB 의 기존 버전은 받지 못하므로, 버전 관리로 【새 버전을 추가】하고 거기로 전환하며, 기존 버전은 이력에 남아 되찾을 수 있다.
// settings flag 가드 → 한 번만 수행. 완전히 새 DB 는 처리 불필요. reseedGoalsPrompt 와 완전히 같은 구조다.
func (s *Server) reseedWorkerPrompt() {
	const flag = "worker_prompt_compact_v4"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 성공 여부와 상관없이 한 번만 시도
	a, err := s.m.pg.GetAgentByKey("worker")
	if err != nil || a == nil {
		return // 완전히 새 DB 라 아직 agent 행을 만들지 않은 경우, seedPrompts 가 바로 최신 기본값을 seed 하므로 이 마이그레이션이 필요 없음
	}
	tmpl := agent.BuiltinPromptSeeds()["worker"]
	if tmpl == "" {
		return
	}
	// 완전히 새 DB 는 seedPrompts 가 이미 최신 기본값을 seed 함 → 현재 버전이 이미 코드 기본값과 같으므로 중복 버전을 또 추가하지 않는다.
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] worker 프롬프트를 새 기본값으로 다시 갱신 실패: %v", err)
		return
	}
	log.Printf("[prompts] worker 프롬프트에 새 기본 버전 추가함(컨텍스트 조회 구간을 list_assets/list_findings 로 수렴하고 list_facts/node_detail/asset_neighbors 제거, 일회성)")
}

// reporterToolCallMessage 는 보고서를 쓰기 전에 반드시 get_finding_traffic 을 한 번 읽도록 무조건 요구해야 한다.
// 이 도구는 읽기 전용이고 「캡처 스위치에 의존하지 않으며」, 자동 바인딩을 켜든 끄든 수동으로 바인딩한 증거를 읽을 수 있다. 여기에
// 「자동 바인딩을 켜야만 읽음」이라고 쓰면, 기본 꺼짐 설정에서 reporter 가 evidence_version 을 넘기지 않아
// SetFindingReportVersionByNodeID 가 legacy 의미로 -1 을 쓰고, 취약점 상세와 Markdown 내보내기가
// 그때부터 「증거가 변경됨, 보고서 갱신 대기」에 상주하며 UI 에는 그것을 지울 수 있는 진입점이 전혀 없다.
const reporterToolCallMessage = "위에서 방금 취약점이 report_finding 으로 등록되었다. 반환 JSON 의 finding_id(독립 취약점 기록 ID)와 finding_node_id(탐색 노드 ID)를 읽고, " +
	"먼저 get_finding_traffic(finding_id) 로 현재 증거 목록과 그 version 을 읽어라(빈 목록은 정상이며 평소대로 보고서를 쓴다). " +
	"실행 지침이 자동 바인딩을 켰다면, 읽기 전에 이번 취약점의 트래픽을 확인하고 연결하라. 노드 상세에는 finding_node_id 를 쓴다. " +
	"마지막에 update_finding_report(finding_id=finding_node_id, report, evidence_version=실제로 읽은 버전) 를 호출해 저장하라. " +
	"evidence_version 은 반드시 넘겨야 하며, 그렇지 않으면 보고서가 영원히 갱신 대기로 표시된다. 두 종류의 번호를 섞어 쓰지 마라."

// 이전 버전 트리거 메시지(0.3.8 및 그 이전). 여전히 이것과 글자 그대로 같은 기록만 마이그레이션으로 덮어쓰며, 사용자가 바꾼 것은 그대로 둔다.
const reporterToolCallMessageV1 = "위에서 방금 취약점이 report_finding 으로 등록되었다. 트리거 컨텍스트에서 finding_id" +
	"(도구가 반환한 \"finding recorded: <id>\" 의 숫자)와 작업 id 를 꺼내, 네 역할에 따라 그 취약점의 상세 보고서를 작성하고, " +
	"마지막에 update_finding_report(finding_id, report) 를 호출해 저장하라."

// upgradeReporterTriggerMessage 는 기존 DB 에서 여전히 기본 문구인 reporter 트리거 메시지를 새 버전으로 갱신한다.
// seedReporterAgent 는 reporter_agent_seed_v1 가드의 제한을 받고 agent 를 새로 만들 때만 트리거를 쓰므로,
// 업그레이드된 DB 는 새 문구를 받지 못한다 —— 도구 schema 는 seedFindingTrafficTools 가
// evidence_version 을 보충했지만, reporter 에게 그것을 쓰라고 알려 주는 것이 아무것도 없다. 일회성이며, 바뀌지 않은 문구만 덮어쓴다.
func (s *Server) upgradeReporterTriggerMessage() {
	const flag = "reporter_trigger_evidence_version_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 한 번만 시도
	triggers, err := s.m.pg.ListTriggersFor("reporter")
	if err != nil {
		log.Printf("[reporter] 트리거 읽기 실패: %v", err)
		return
	}
	for _, t := range triggers {
		if !t.OnToolCall || t.ToolCallMessage != reporterToolCallMessageV1 {
			continue // 사용자가 바꿨거나 finding 트리거가 아니면 건드리지 않음.
		}
		t.ToolCallMessage = reporterToolCallMessage
		if err := s.m.pg.UpdateTrigger(t); err != nil {
			log.Printf("[reporter] 트리거 메시지 업그레이드 실패: %v", err)
			return
		}
		log.Printf("[reporter] 트리거 메시지를 evidence_version 읽기·회신으로 업그레이드함")
	}
}

// seedReporterAgent 는 「보고서 작성」 사용자 정의 agent 를 사전 설정한다(builtin=false, UI 에서 편집/삭제 가능):
// update_finding_report + 작업 조회 도구를 바인딩하고, 「report_finding 이 호출되면 트리거」 트리거를 걸어 ——
// 취약점이 하나 등록될 때마다 깨워 상세 보고서를 쓰게 한다. 일회성(settings flag 가드): 사용자가 삭제하면 다시 만들지 않는다.
// 의존성: orchestration 도구가 이 함수 위쪽 SeedTool 로 DB 에 들어가므로 바인딩할 수 있다.
func (s *Server) seedReporterAgent() {
	const flag = "reporter_agent_seed_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 성공 여부와 상관없이 한 번만 시도

	if exist, _ := s.m.pg.GetAgentByKey("reporter"); exist != nil {
		return // key 가 이미 점유됨(사용자가 직접 만듦)——덮어쓰지 않음
	}
	a, err := s.m.pg.CreateAgent("reporter", "보고서 작성",
		"취약점 상세 보고서 작성: 취약점을 발견하면 자동으로 트리거되어 증거와 실행 과정을 조회한 뒤 Markdown 보고서를 쓰고 되돌려 쓴다.")
	if err != nil {
		log.Printf("[reporter] agent 생성 실패: %v", err)
		return
	}
	if err := s.m.pg.SeedPromptIfEmpty(a.ID, agent.ReporterDefaultPrompt); err != nil {
		log.Printf("[reporter] seed prompt 실패: %v", err)
	}
	// 트리거 실행 정책: parallel + none —— 취약점 하나에 보고서 하나, 여러 finding 이 각자 병렬로 작성한다.
	// merge 는 반드시 none 이어야 한다: 그렇지 않으면(기본 all) 일련의 finding 이 한 번의 실행으로 병합되어 병렬이 의미가 없다.
	// maxParallel=5: 동시에 최대 5 개 보고서 대화로, 순간적으로 LLM 호출이 너무 많아지는 것을 피한다.
	if err := s.m.pg.SetAgentTriggerBehavior("reporter", "parallel", "none", 5); err != nil {
		log.Printf("[reporter] 트리거 실행 정책 설정 실패: %v", err)
	}
	// 필요한 도구를 바인딩한다: 보고서 작성 + 증거/실행 과정/상황 읽기.
	if err := s.m.pg.AddAgentToToolBinding("reporter", []string{
		"update_finding_report", "get_task_node_detail", "list_task_findings",
		"get_task_worker_trace", "list_task_worker_traces", "search_task_worker_traces",
		"get_task_graph",
	}); err != nil {
		log.Printf("[reporter] 도구 바인딩 실패: %v", err)
	}
	// 트리거: report_finding 이 호출되면 트리거(도구가 반환한 "finding recorded: <id>" 에 finding_id 가 붙고,
	// 작업 id 도 트리거 메시지에 있다).
	if _, err := s.m.pg.CreateTrigger(&db.AgentTrigger{
		AgentKey:        "reporter",
		Enabled:         true,
		OnToolCall:      true,
		ToolNames:       []string{"report_finding"},
		ToolCallMessage: reporterToolCallMessage,
	}); err != nil {
		log.Printf("[reporter] 트리거 생성 실패: %v", err)
	}
	log.Printf("[reporter] 「보고서 작성」 agent + finding 트리거를 사전 설정함")
}

// seedAutoReportFindingBinding adds "auto" to report_finding's binding ONCE so
// conversation-context agents can call it without requiring an intent_id.
func (s *Server) seedAutoReportFindingBinding() {
	const flag = "auto_report_finding_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("auto", []string{"report_finding"}); err != nil {
		log.Printf("[auto] report_finding 기본 바인딩 실패: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedPlannerDefaultBindings adds "planner" to report_finding's binding ONCE
// (guarded by a settings flag), so existing DBs — whose report_finding row was
// seeded as worker-only — also let the planner record findings. Fresh DBs already
// get it via PlannerTools(); this only backfills without overriding a user unbind.
func (s *Server) seedPlannerDefaultBindings() {
	const flag = "planner_report_finding_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("planner", []string{"report_finding"}); err != nil {
		log.Printf("[planner] report_finding 기본 바인딩 실패: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedPlannerListAssetsBinding adds "planner" to list_assets's binding ONCE
// (guarded by a settings flag), so existing DBs — whose list_assets row was seeded
// as auto/pentest-only — also let the planner query the asset store by DSL. Fresh
// DBs already get it via PlannerTools(); this only backfills without overriding a
// user unbind.
func (s *Server) seedPlannerListAssetsBinding() {
	const flag = "planner_list_assets_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("planner", []string{"list_assets"}); err != nil {
		log.Printf("[planner] list_assets 기본 바인딩 실패: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedCompanyScopeRebind changes add_company_scope's default binding ONCE on
// existing DBs (guarded by a settings flag): the tool moves off worker and onto
// planner — defining a company's asset scope is a planning/main/auto concern, not
// something a worker does mid-exploration. Fresh DBs already get planner via
// PlannerTools() and lack worker via WorkerTools(); this only backfills old rows.
// One-shot + flag-guarded so a user who later re-binds worker isn't overridden.
func (s *Server) seedCompanyScopeRebind() {
	const flag = "company_scope_rebind_v1" // worker→planner 기본 바인딩 전환
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("planner", []string{"add_company_scope"}); err != nil {
		log.Printf("[planner] add_company_scope 기본 바인딩 실패: %v", err)
		return
	}
	if err := s.m.pg.RemoveAgentFromTool("worker", "add_company_scope"); err != nil {
		log.Printf("[worker] add_company_scope 바인딩 해제 실패: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedWorkerReadToolsUnbind strips the read-context tools off worker's default
// binding ONCE on existing DBs (guarded by a settings flag): a worker executes one
// intent and writes back — reading facts/companies and listing all workers' traces is
// a planning/main concern, not the executor's. Fresh DBs already lack these via
// WorkerTools(); this only backfills old rows without overriding a user who
// deliberately re-binds worker. Each RemoveAgentFromTool is per-tool +
// membership-guarded, so planner/mainagent bindings of the same tool are untouched.
//
// NOTE: search_all_worker_traces / get_worker_trace / node_detail are intentionally NOT
// unbound — worker owns them for cross-work look-back + node drill-down (see WorkerTools).
// They used to be in this list back when worker lacked them; seedWorkerReadbackRebind
// repairs DBs whose old run stripped them.
func (s *Server) seedWorkerReadToolsUnbind() {
	const flag = "worker_readtools_unbind_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	for _, k := range []string{
		"list_facts", "list_companies", "list_worker_traces",
	} {
		if err := s.m.pg.RemoveAgentFromTool("worker", k); err != nil {
			log.Printf("[worker] %s 를 worker 에서 해제 실패: %v", k, err)
			return // 오류가 나면 flag 를 두지 않고, 다음 시작 때 재시도
		}
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedWorkerReadbackRebind re-binds the cross-work look-back / drill-down tools onto
// worker ONCE (guarded by a settings flag): an earlier seedWorkerReadToolsUnbind wrongly
// stripped search_all_worker_traces / get_worker_trace / node_detail from worker after
// they had been added to WorkerTools(), so any DB that ran that migration lost them.
// Fresh DBs already have them via WorkerTools() and this is a harmless no-op there.
// One-shot + flag-guarded so a user who later deliberately unbinds them isn't overridden.
func (s *Server) seedWorkerReadbackRebind() {
	const flag = "worker_readback_rebind_v2" // v2: node_detail 추가
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("worker", []string{
		"search_all_worker_traces", "get_worker_trace", "node_detail",
	}); err != nil {
		log.Printf("[worker] 되돌아보기/상세 도구 보충 바인딩 실패: %v", err)
		return // 오류가 나면 flag 를 두지 않고, 다음 시작 때 재시도
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedAutoDefaultBindings adds "auto" to the task-op + platform tools' bindings
// ONCE (guarded by a settings flag), so existing DBs whose tool rows were seeded
// before Auto existed still give Auto its default toolset — without re-adding it
// after a user deliberately unbinds.
func (s *Server) seedAutoDefaultBindings() {
	const flag = "auto_default_bindings_v3" // v3: 예전 자산 도구 이름 교체, insert_assets/add_company_scope 추가
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	keys := make([]string, 0, len(platformToolKeys)+12)
	for _, t := range s.orchestrationTools() {
		keys = append(keys, t.Name())
	}
	keys = append(keys, platformToolKeys...)
	// 자산 도구: Auto 가 플랫폼을 조작하려면 자산을 보거나 등록하고 회사 범위를 관리하는 일이 잦다.
	keys = append(keys, "insert_assets", "add_company_scope", "list_assets")
	if err := s.m.pg.AddAgentToToolBinding("auto", keys); err != nil {
		log.Printf("[auto] 기본 바인딩 실패: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}
