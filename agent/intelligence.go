package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

// Missing host/setting means off. Rechecked on every write for running sessions.
var IntelligenceEnabled func() bool
var IntelligenceRecord func(context.Context, int64, int64, string, db.IntelligenceInput) (int64, error)

func intelligenceEnabled() bool { return IntelligenceEnabled != nil && IntelligenceEnabled() }
func intelligenceSchema() map[string]any {
	return obj(map[string]any{
		"kind":       map[string]any{"type": "string", "enum": []string{"credential", "lead", "sensitive_data"}},
		"title":      str("짧은 제목. 비밀번호, Token, 민감 데이터 원문 포함 금지"),
		"content":    str("실제로 얻은 정보. 자격 증명은 JSON 문자열로 계정/비밀번호/Token 등을 저장한다. 민감 데이터는 소량의 샘플, 개수, 파일 위치만 쓰고 전체 DB 를 붙여 넣지 않는다"),
		"evidence":   str("실제 출처: 명령/요청/응답/파일 위치와 핵심 관찰. 지어내기 금지"),
		"confidence": map[string]any{"type": "string", "enum": []string{"observed", "inferred"}},
		"status":     str("자격 증명 unverified/verified/invalid, 단서 pending/in_progress/dismissed/converted, 민감 데이터 suspected/confirmed/dismissed. 생략하면 보수적인 기본값을 쓰며, verified 는 실제 인증 성공 증거가 필요하다"),
		"asset_ids":  map[string]any{"type": "array", "items": map[string]any{"type": "integer"}},
	}, "kind", "title", "content", "evidence", "confidence")
}
func (t *ToolSet) saveIntelligence(ctx context.Context, nodeID int64, in db.IntelligenceInput) (int64, error) {
	if !intelligenceEnabled() || IntelligenceRecord == nil {
		return 0, fmt.Errorf("작업 정보 자동 기록이 꺼져 있습니다")
	}
	taskID := t.taskID
	if taskID <= 0 {
		taskID = RunInfoFrom(ctx).TaskID
	}
	if taskID <= 0 {
		return 0, fmt.Errorf("작업 정보에는 현재 작업 컨텍스트가 필요합니다")
	}
	if nodeID <= 0 {
		nodeID = t.ownerNode
	}
	if nodeID <= 0 {
		nodeID = RunInfoFrom(ctx).IntentID
	}
	id, err := IntelligenceRecord(ctx, taskID, nodeID, t.worker, in)
	if err == nil {
		t.writes.Intelligence++
	}
	return id, err
}
func (t *ToolSet) recordIntelligence() actool.CoreTool {
	return writeTool("record_intelligence", "현재 작업에서 실제로 얻은 자격 증명, 중요한 단서, 민감 데이터를 기록한다. 작업과 실행 출처는 시스템이 연결한다. 자격 증명을 발견했다고 검증에 성공한 것은 아니다. 중복 기록은 자동으로 제거된다. insert_assets.auth 가 이미 자격 증명을 자동 등록하므로 중복 호출할 필요가 없다.", intelligenceSchema(), func(ctx context.Context, raw json.RawMessage) (actool.Result, error) {
		var in db.IntelligenceInput
		if err := json.Unmarshal(raw, &in); err != nil {
			return actool.Errorf("유효하지 않은 정보 파라미터"), nil
		}
		id, err := t.saveIntelligence(ctx, 0, in)
		if err != nil {
			return actool.Errorf(err.Error()), nil
		}
		return actool.Text(fmt.Sprintf("intelligence recorded: %d", id)), nil
	})
}
func intelligenceWorkflow(ctx context.Context, tools []actool.CoreTool) ([]actool.CoreTool, string) {
	enabled := intelligenceEnabled() && RunInfoFrom(ctx).TaskID > 0
	out := make([]actool.CoreTool, 0, len(tools))
	hasRecord := false
	hasFact := false
	for _, tool := range tools {
		if tool.Name() == "record_intelligence" {
			if !enabled {
				continue
			}
			hasRecord = true
		}
		if tool.Name() == "record_fact" {
			raw, _ := json.Marshal(tool.InputSchema())
			var schema map[string]any
			if json.Unmarshal(raw, &schema) == nil {
				props, _ := schema["properties"].(map[string]any)
				if props != nil {
					delete(props, "intelligence")
					if enabled {
						props["intelligence"] = map[string]any{"type": "array", "maxItems": 20, "description": "사실과 함께 정보를 한 번에 기록한다. 필드는 record_intelligence 와 같으며, 이 도구를 따로 호출할 필요가 없다", "items": intelligenceSchema()}
						hasFact = true
					}
				}
				tool = DecorateTool(tool, tool.Description(), schema)
			}
		}
		out = append(out, tool)
	}
	guide := ""
	if enabled && (hasRecord || hasFact) {
		guide = "\n\n**작업 정보 기록이 켜져 있음**: 자격 증명, 추적할 가치가 있는 단서, 실제 민감 데이터를 발견하면 사용자가 묻기를 기다리지 말고 즉시 구조화해 기록한다. 끝내기 전에 빠진 것이 없는지 확인한다. record_fact 에 intelligence 배열을 붙여 함께 기록할 수 있으며, 개별 사실과 일괄 facts 요소 모두 지원한다. 따로 기록하려면 record_intelligence(사용 가능하다면)를 쓴다. insert_assets.auth 는 백엔드가 자동 등록하므로 중복 호출하지 않는다. 새롭고 가치 있는 정보만 기록하며, 일반적인 지문/포트 상태는 여전히 fact 로만 남긴다. title 에는 비밀 원문을 넣지 않고, 본문에는 실제 내용과 출처를 넣는다. 자격 증명은 기본적으로 미검증이며 실제 인증에 성공했을 때만 verified 로 표시한다. 추론은 반드시 inferred 로 한다. 민감 데이터는 유형, 개수, 소량의 샘플, 파일 위치를 쓰고 전체 DB 를 붙여 넣지 않는다."
	}
	return out, guide
}
