package agent

// Curated, bounded examples for the personal research harness. These are
// workflow demonstrations, never target observations or permission grants.
// Selection is deterministic and does not consume a model call or fetch data.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

const (
	ResearchGuidanceMaxRunes = 1800
	ResearchGuideMaxRunes    = 3200
	researchGuideMinRunes    = 600
)

type researchExampleCard struct {
	id, title, body, source string
	keywords                []string
	tools                   []string
}

var researchExampleCards = []researchExampleCard{
	{
		id: "source-evidence", title: "소스 감사: 호출 지점에서 제어 가능한 입력으로 거슬러 올라가기",
		keywords: []string{"源码审计", "代码审计", "漏洞审计", "安全代码审查", "代码安全", "审计仓库", "审计项目", "污点", "调用链", "可达性", "소스 감사", "코드 감사", "취약점 감사", "보안 코드 리뷰", "코드 보안", "저장소 감사", "오염", "호출 체인", "도달 가능성", "source audit", "code audit", "taint", "source review", "security code review", "source-evidence"},
		tools:    []string{"Grep", "Read", "research_record", "research_recall"},
		body:     "절차: 먼저 심볼과 호출 지점을 찾고 필요한 컨텍스트를 읽는다. 입력 출처, 전달 경로, 권한, 보호 조건을 각각 확인한다. 위험한 호출은 단서일 뿐이다.\n예시(가상): 관찰 E1: 어떤 처리 함수가 파라미터를 파일 작업에 넘긴다. 가설 H1: 외부 입력이 경로에 영향을 줄 수 있다. 반증 E2: 현재 진입점에는 고정 매핑이 있다. 결론: H1 은 입증되지 않았다. 다음 단계: 바로 취약점으로 보고하지 말고 매핑이 모든 호출자를 다루는지 확인한다. 증거 기록에는 실제 파일과 줄 번호, 버전, 진입 조건을 남긴다. 전체 출력이 잘리면 위치를 지정해 계속 읽는다.",
		source:   "방법 참고: PentestGPT https://arxiv.org/abs/2308.06782; AVRI https://arxiv.org/abs/2610.11602 . 예시는 이 프로젝트에서 작성한 것이며 논문 결과의 재현이 아니다.",
	},
	{
		id: "web-comparison", title: "Web/API: 기준선, 단일 변수와 실제 영향",
		keywords: []string{"接口对比", "响应对比", "对比响应", "越权", "业务逻辑", "网站漏洞", "接口漏洞", "web安全", "web 安全", "web漏洞", "web 漏洞", "api安全", "api 安全", "api漏洞", "api 漏洞", "授权测试", "请求差异", "인터페이스 비교", "응답 비교", "권한 우회", "비즈니스 로직", "웹 취약점", "웹 보안", "api 보안", "api 취약점", "권한 테스트", "요청 차이", "api comparison", "authorization test", "web security", "web vulnerability", "api vulnerability", "web-comparison"},
		tools:    []string{"traffic_search", "traffic_get", "traffic_blob", "Read", "research_record", "research_recall"},
		body:     "절차: 먼저 실제 기준선을 읽고, 세션과 환경 조건을 비교 가능하게 유지한다. 매번 근거 있는 변수 하나만 바꾼 뒤 비즈니스 결과를 확인한다. 응답 코드가 같다고 권한이 같은 것은 아니며, 응답 코드가 이상하다고 취약점이 증명되는 것도 아니다.\n예시(가상): 관찰 E1: 통제된 두 계정의 요청이 같은 상태 코드를 받았다. 가설 H1: 다른 사람의 객체가 반환되었다. 반증 E2: 본문은 실제로 둘 다 공개 요약이다. 결론: 현재 증거는 권한 우회를 뒷받침하지 않는다. 다음 단계: 객체 소유권과 보호 필드를 비교하고, 요청/응답 증거 참조와 계정 별칭을 기록한다. 자격 증명 원문은 요약에 넣지 않는다. 먼저 사용자 권한 범위와 데이터 소유권을 확인한다.",
		source:   "방법 참고: EnIGMA https://arxiv.org/abs/2409.16165 . 이 카드는 검증 절차를 제공하며 대상, 공격 지시, 권한을 포함하지 않는다.",
	},
	{
		id: "environment-check", title: "환경 장애: 먼저 분류하고 재시도를 결정",
		keywords: []string{"环境故障", "命令不存在", "找不到命令", "command not found", "executable not found", "connection refused", "连接被拒绝", "路径错误", "依赖缺失", "shell错误", "shell 错误", "환경 장애", "명령 없음", "명령을 찾을 수 없음", "연결 거부", "경로 오류", "의존성 누락", "shell 오류", "environment-check"},
		tools:    []string{"Bash", "Read", "LS", "research_record", "research_recall"},
		body:     "절차: 인터프리터/작업 디렉터리/의존성/서비스 준비/네트워크 문제와 대상의 동작을 구분한다. 먼저 전체 종료 코드와 오류 위치를 읽고, 현재 운영 체제와 이미 찾은 경로를 확인한다. 도구가 지원하는 파라미터로 최소한의 읽기 전용 검사를 한다.\n예시(가상): 관찰 E1: 도구가 executable not found 를 반환했다. 가설 H1: 의존성이 설치되지 않았다. 반증 E2: 환경 기록에 이미 인터프리터 절대 경로가 있다. 다음 단계: 그 경로와 호출 방식을 확인한다. 이 시점에는 아직 대상 로직을 실행하지 않았으므로, 결과를 대상에 취약점이 없다고 쓸 수 없다. 반복 시도는 반드시 파라미터를 고치거나 새 증거를 얻어야 하며, 일반적인 환경 실패 때문에 시스템 보안 정책을 자동으로 바꾸지 않는다.",
		source:   "방법 참고: EnIGMA https://arxiv.org/abs/2409.16165 . 오류 분류와 검사 단계는 이 프로젝트에서 작성했다.",
	},
	{
		id: "resume-state", title: "장기 작업 복구: 증거와 의존성은 복구하고, 이전 동작은 재생하지 않기",
		keywords: []string{"恢复任务", "任务恢复", "长任务", "断点续", "交接", "压缩后", "继续审计", "작업 복구", "작업 재개", "장기 작업", "이어서", "인수인계", "압축 후", "감사 계속", "resume task", "resume research", "handoff", "checkpoint", "resume-state"},
		tools:    []string{"research_recall", "research_record", "Read", "graph_overview", "list_facts"},
		body:     "절차: 먼저 목표, 완료 조건, 확인된 사실, 실패한 경로, 선행 의존성을 복구한 뒤, 현재 환경에서도 여전히 성립하는지 확인한다. 과거 기록은 확인해야 할 컨텍스트이며 현재 작업 권한이나 실행 상태를 대신할 수 없다.\n예시(가상): 이전 기록 E1: 어떤 서비스가 시작된 적이 있다. 복구 시 E2: 준비 상태 검사가 실패했다. 결론: 서비스 상태가 바뀌어 원래 후속 단계의 전제가 성립하지 않는다. 다음 단계: 변화를 기록하고 환경 원인을 찾는다. 이미 완료된 쓰기 작업은 먼저 결과를 확인한 뒤 계속할지 결정해 부수 효과의 재생을 피한다. 일시 중지 상태에서는 기존 기록만 읽을 수 있으며, 재개 실행은 현재 작업 제어를 따른다.",
		source:   "방법 참고: PentestGPT https://arxiv.org/abs/2308.06782; Reflexion https://arxiv.org/abs/2303.11366 .",
	},
	{
		id: "failure-feedback", title: "실패 수정: 새 증거로 다음 단계를 고르기",
		keywords: []string{"工具失败", "重复失败", "反复失败", "无进展", "停滞", "工具报错", "验证失败", "复现失败", "도구 실패", "반복 실패", "진전 없음", "정체", "도구 오류", "검증 실패", "재현 실패", "tool failure", "tool error", "stuck", "no progress", "failed verification", "failure-feedback"},
		tools:    []string{"research_recall", "research_record", "Read", "Bash", "traffic_get"},
		body:     "절차: 예상, 실제 결과, 차이, 부정된 가설, 아직 부족한 증거를 기록한다. 경쟁하는 설명을 구분할 수 있는 검사를 우선한다. 같은 호출로 같은 결과를 얻은 것은 새로운 뒷받침 증거가 아니다.\n예시(가상): 연속된 결과 E1/E2 가 모두 선행 조건이 충족되지 않았음을 보여 준다. 원래 가설 H1 은 여전히 알 수 없다. 다음 단계: 먼저 선행 조건을 확인한 뒤 계속할지, 경로를 바꿀지, 멈출지 결정한다. 파라미터 변경에 명확한 근거가 있거나 환경이 실제로 바뀌었을 때만 재시도한다. 외부 피드백이 자기 평가보다 우선한다. “다시 생각해 보기”나 여러 모델의 동의를 재현 성공으로 여기지 않는다.",
		source:   "방법 참고: Reflexion https://arxiv.org/abs/2303.11366; 자기 수정의 한계 https://arxiv.org/abs/2310.01798 .",
	},
}

const researchExamplesNotice = "【연구 작업 예시|personal 1.2】\n아래는 방법 예시이며, 현재 작업의 사실, 증거, 작업 권한이 아니다. 현재 권한 범위 안에서만 사용한다. 실제 결론은 반드시 실제 도구 결과에서 나와야 한다. 짧은 사실, 가설, 반증, 다음 단계를 기록하며, 내부 사고 과정을 출력할 필요는 없다.\n"

// ResearchGuidance selects at most two relevant cards for a run. Unrelated
// tasks receive no prompt text. Pass the final visible tool set, after filtering.
func ResearchGuidance(task string, tools []actool.CoreTool) string {
	return renderResearchExamples(task, tools, 2, ResearchGuidanceMaxRunes)
}

// ResearchGuideTool provides bounded, local retrieval over a curated library.
// A captured snapshot of the final tool set prevents cross-session visibility
// changes and never exposes tool descriptions supplied by external MCP servers.
func ResearchGuideTool(tools []actool.CoreTool) actool.CoreTool {
	visible := append([]actool.CoreTool(nil), tools...)
	return actool.Build(actool.Spec{
		Name:        "research_guide",
		Description: "현재 문제에 맞춰 소수의 연구 방법과 작업 예시를 검색한다: 소스 감사, Web/API 증거 비교, 환경 장애, 작업 복구, 실패 수정. 내장 지식 카드만 조회하며, 네트워크에 접근하거나 작업을 실행하지 않는다. 구체적인 문제로 조회한다. 예시는 현재 사실이나 권한이 아니다.",
		Schema: map[string]any{
			"type": "object", "additionalProperties": false,
			"properties": map[string]any{
				"query":       map[string]any{"type": "string", "minLength": 1, "maxLength": 512, "description": "구체적인 문제 또는 카드 ID: source-evidence, web-comparison, environment-check, resume-state, failure-feedback."},
				"max_results": map[string]any{"type": "integer", "minimum": 1, "maximum": 3, "description": "반환할 최대 카드 수, 기본 2."},
				"max_chars":   map[string]any{"type": "integer", "minimum": researchGuideMinRunes, "maximum": ResearchGuideMaxRunes, "description": "전체 출력 글자 수 예산, 기본 2400, Unicode 문자 기준으로 셈."},
			},
			"required": []any{"query"},
		},
		ReadOnly:   func(json.RawMessage) bool { return true },
		Concurrent: func(json.RawMessage) bool { return true },
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(ctx context.Context, raw json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			if err := ctx.Err(); err != nil {
				return actool.Errorf("방법 조회가 취소되었습니다"), nil
			}
			if len(raw) > 8192 {
				return actool.Errorf("조회 파라미터가 너무 큽니다. query 는 최대 512자입니다"), nil
			}
			var in struct {
				Query      string `json:"query"`
				MaxResults *int   `json:"max_results"`
				MaxChars   *int   `json:"max_chars"`
			}
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&in); err != nil {
				return actool.Errorf("유효하지 않은 조회 파라미터: query 문자열이 필요하며, max_results, max_chars 정수는 선택입니다"), nil
			}
			if err := decoder.Decode(new(any)); err != io.EOF {
				return actool.Errorf("조회는 단일 JSON 객체여야 합니다"), nil
			}
			in.Query = strings.TrimSpace(in.Query)
			if in.Query == "" || utf8.RuneCountInString(in.Query) > 512 {
				return actool.Errorf("query 는 1~512자여야 합니다"), nil
			}
			maxResults, maxChars := 2, 2400
			if in.MaxResults != nil {
				maxResults = *in.MaxResults
			}
			if in.MaxChars != nil {
				maxChars = *in.MaxChars
			}
			if maxResults < 1 || maxResults > 3 || maxChars < researchGuideMinRunes || maxChars > ResearchGuideMaxRunes {
				return actool.Errorf("max_results 범위는 1~3, max_chars 범위는 600~3200 입니다"), nil
			}
			text := renderResearchExamples(in.Query, visible, maxResults, maxChars)
			if text == "" {
				text = "현재 문제와 보이는 도구에 맞는 방법 카드가 없다. 조회 가능: 소스 감사, 인터페이스 응답 비교, 환경 장애, 작업 복구, 도구 실패. 이를 근거로 없는 도구를 쓸 수 있다고 가정하지 않는다."
			}
			return actool.Text(text), nil
		},
	})
}

func renderResearchExamples(query string, tools []actool.CoreTool, limit, budget int) string {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return ""
	}
	// The task may contain a large transcript; routing is bounded independently
	// of prompt size and never reflects task content back into the system prompt.
	runes := []rune(query)
	if len(runes) > 8192 {
		query = string(runes[:4096]) + " " + string(runes[len(runes)-4096:])
	}
	visible := make(map[string]actool.CoreTool)
	for _, t := range tools {
		if t != nil {
			visible[t.Name()] = t
		}
	}
	type match struct {
		card  researchExampleCard
		score int
	}
	var matches []match
	for _, card := range researchExampleCards {
		score := 0
		for _, word := range card.keywords {
			if researchKeywordMatch(query, word) {
				score++
			}
		}
		if score == 0 {
			continue
		}
		hasTool := false
		for _, name := range card.tools {
			if visible[name] != nil {
				hasTool = true
				break
			}
		}
		if hasTool {
			matches = append(matches, match{card, score})
		}
	}
	if len(matches) == 0 {
		return ""
	}
	sort.SliceStable(matches, func(i, j int) bool { return matches[i].score > matches[j].score })
	var out strings.Builder
	out.WriteString(researchExamplesNotice)
	count := 0
	for _, m := range matches {
		if count >= limit {
			break
		}
		block := "\n[" + m.card.id + "] " + m.card.title + "\n" + m.card.body + "\n" + researchExampleTools(m.card, visible) + "\n" + m.card.source + "\n"
		if utf8.RuneCountInString(out.String())+utf8.RuneCountInString(block) > budget {
			continue
		}
		out.WriteString(block)
		count++
	}
	if count == 0 {
		return ""
	}
	return out.String()
}

func researchKeywordMatch(query, keyword string) bool {
	start := 0
	for start <= len(query) {
		i := strings.Index(query[start:], keyword)
		if i < 0 {
			return false
		}
		i += start
		// English route terms must be whole words: "unstuck" and "web api"
		// alone must not spuriously enable failure/security guidance.
		ascii := keyword[0] < utf8.RuneSelf
		end := i + len(keyword)
		if !ascii || ((i == 0 || !researchASCIIWord(query[i-1])) && (end == len(query) || !researchASCIIWord(query[end]))) {
			return true
		}
		start = end
	}
	return false
}

func researchASCIIWord(b byte) bool { return b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '_' }

func researchExampleTools(card researchExampleCard, visible map[string]actool.CoreTool) string {
	var descriptions []string
	for _, name := range card.tools {
		t := visible[name]
		if t == nil {
			continue
		}
		// Names are curated. Required fields come from the final runtime schema,
		// including per-agent DB overrides; do not invent argument names.
		var fields []string
		schema := t.InputSchema()
		switch required := schema["required"].(type) {
		case []any:
			for _, r := range required {
				if s, ok := r.(string); ok && researchSchemaField(s) {
					fields = append(fields, s)
				}
			}
		case []string:
			for _, s := range required {
				if researchSchemaField(s) {
					fields = append(fields, s)
				}
			}
		}
		if len(fields) > 5 {
			fields = fields[:5]
		}
		desc := name
		if len(fields) > 0 {
			desc += "(필수: " + strings.Join(fields, ", ") + ")"
		}
		descriptions = append(descriptions, desc)
		if len(descriptions) == 3 {
			break
		}
	}
	return fmt.Sprintf("현재 사용 가능한 도구: %s. 실제 도구 schema 를 기준으로 한다. 예시의 E/H 번호와 경로는 이 작업의 실제 기록으로 바꿔야 한다.", strings.Join(descriptions, "; "))
}

func researchSchemaField(s string) bool {
	if len(s) == 0 || len(s) > 48 {
		return false
	}
	for _, r := range s {
		if !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-') {
			return false
		}
	}
	return true
}
