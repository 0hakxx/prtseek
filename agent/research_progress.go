package agent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

type researchProgressObservation struct {
	signature, evidence string
	failed              bool
}

// Normalize only tool-owned HTTP metadata. Arbitrary output is hashed exactly;
// fuzzy text matching would confuse different successful observations.
func progressSignature(name, result string) string {
	var response httpObservation
	if name == "WebFetch" && json.Unmarshal([]byte(result), &response) == nil && response.SHA256 != "" {
		result = fmt.Sprintf("%d/%d/%s/%t", response.Status, response.BodyBytes, response.SHA256, response.BodyTruncated)
	}
	h := sha256.Sum256([]byte(name + "\x00" + result))
	return hex.EncodeToString(h[:])
}

// Called with r.mu held. This is a soft review gate, not a semantic verdict.
// It never blocks an otherwise permitted call or starts extra model requests.
func (r *researchRuntime) observeProgress(ctx context.Context, name, result, id string, failed bool) string {
	r.stageSteps++
	sig := progressSignature(name, result)
	r.recent = append(r.recent, researchProgressObservation{sig, id, failed})
	if len(r.recent) > 8 {
		r.recent = r.recent[len(r.recent)-8:]
	}
	repeats, failures := 0, 0
	ids := []string{}
	for _, o := range r.recent {
		if o.signature == sig {
			repeats++
		}
		if o.failed {
			failures++
		}
		if o.evidence != "" {
			ids = append(ids, o.evidence)
		}
	}
	stalled := repeats >= 3 || failures >= 4
	if r.stageSteps-r.lastReview < 4 || (!stalled && r.stageSteps%8 != 0) {
		return ""
	}
	r.lastReview = r.stageSteps
	summary := fmt.Sprintf("자동 단계 체크포인트: 비폴링 도구를 %d 회 실행함. 최근 %d 회 중 실패 %d 회, 이번 응답과 같은 것 %d 회. 이 기록은 호출 결과만 설명하며 가설이나 목표 완료를 확인하지 않는다.", r.stageSteps, len(r.recent), failures, repeats)
	note := "\n단계 재검토: research_record 로 현재 가설, 증거, 반증, 다음 단계에서 구분할 변수를 간단히 기록한다."
	if stalled {
		note += "파라미터를 바꿔도 결과가 반복되거나 여러 번 실패함: 먼저 Shell/프록시/인증서/리디렉션과 초기 상태를 확인한 뒤, 가설을 구분할 수 있는 대조 실험 하나를 고른다. 새 증거가 없으면 출처를 바꾸거나 차단을 명확히 하며, 같은 응답을 진전으로 여기지 않는다."
	}
	if len(ids) > 0 {
		_, err := r.memory.Record(ctx, ResearchRecordInput{Entry: &ResearchEntryInput{Kind: "hypothesis", Summary: summary, EvidenceIDs: ids}})
		if err != nil {
			note += " 자동 체크포인트 저장 실패: " + err.Error()
		}
	}
	return note
}

func researchExperimentTool(memory *ResearchMemory) actool.CoreTool {
	return actool.Build(actool.Spec{Name: "research_experiment", Description: "가설을 구분할 수 있는 실험을 시작한다: 가설, 바꾼 변수, 예상 관측을 기록하고, 고유한 디렉터리와 산출물 접두사를 할당해 이전 실험이 남긴 파일이 검증을 오염시키지 않게 한다. 디렉터리 격리가 원격 상태가 초기화되었다는 뜻은 아니다. 반드시 먼저 기준선을 기록한 뒤 실험을 실행하고 새 증거로 검증한다.", Schema: map[string]any{
		"type": "object", "properties": map[string]any{"hypothesis": map[string]any{"type": "string"}, "changed_variable": map[string]any{"type": "string"}, "prediction": map[string]any{"type": "string"}}, "required": []string{"hypothesis", "changed_variable", "prediction"}},
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Decision{Behavior: permission.Allow}
		},
		Run: func(ctx context.Context, raw json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			var in struct {
				Hypothesis      string `json:"hypothesis"`
				ChangedVariable string `json:"changed_variable"`
				Prediction      string `json:"prediction"`
			}
			if len(raw) > 6000 || json.Unmarshal(raw, &in) != nil || strings.TrimSpace(in.Hypothesis) == "" || strings.TrimSpace(in.ChangedVariable) == "" || strings.TrimSpace(in.Prediction) == "" {
				return actool.Errorf("짧은 가설, 바꾼 변수, 예상 관측이 필요합니다(최대 6000 바이트)"), nil
			}
			id := "exp-" + rand.Text()
			path := filepath.Join(".research", "experiments", id)
			root, err := os.OpenRoot(memory.root)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			defer root.Close()
			if err = root.MkdirAll(path, 0700); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			_, err = memory.Record(ctx, ResearchRecordInput{Entry: &ResearchEntryInput{Kind: "hypothesis", Scope: id, Summary: in.Hypothesis + "; 바꾼 변수: " + in.ChangedVariable + "; 예상: " + in.Prediction}})
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			out, _ := json.Marshal(map[string]string{"experiment_id": id, "artifact_dir": filepath.ToSlash(path), "artifact_prefix": id, "next": "먼저 이 실험의 고유 산출물이 없는지 확인하고 기준선을 기록한다. 원격 실험은 이 고유 접두사나 검증된 상태 초기화를 사용한다. 기존 산출물을 다시 읽는 것은 관찰이 반복 가능하다는 것만 증명하며, 독립 재현을 주장할 수 없다."})
			return actool.Text(string(out)), nil
		},
	})
}
