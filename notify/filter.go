package notify

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Filter 는 notification_channels.filter 라는 JSONB 열의 계약이며, 채널 인스턴스의 필터 조건이다.
// 모든 필드는 선택이며, 생략하면 「필터하지 않음」이다——이것이 바로 잘못된 설정의 대비 의미다. ParseFilter 참고.
type Filter struct {
	// MinSeverity 는 최소 등급 기준(low/medium/high/critical)이며, 비어 있으면 기준을 두지 않는다.
	MinSeverity string `json:"min_severity"`
	// TaskIDs / AssetIDs 가 빈 배열이면 제한 없음이고, 비어 있지 않으면 이벤트와 교집합이 있어야 한다.
	TaskIDs  []int64 `json:"task_ids"`
	AssetIDs []int64 `json:"asset_ids"`
	// VulnClassInclude 가 비어 있으면 모두 받고, 비어 있지 않으면 vulnclass 가 그중 어느 키워드에 적중해야 한다.
	// VulnClassExclude 는 어느 키워드에 적중하면 제외한다(제외가 포함보다 우선).
	// 매칭은 대소문자를 구분하지 않는 부분 문자열 방식이다——정규식보다 안전하다: 사용자가 정규식을 잘못 설정해도 채널이 조용히 무효가 되지 않는다.
	VulnClassInclude []string `json:"vulnclass_include"`
	VulnClassExclude []string `json:"vulnclass_exclude"`
	// OnStatusChange 는 이 채널이 취약점 상태 변경 이벤트를 받을지 결정한다(realtime 모드에서만 의미 있음).
	OnStatusChange bool `json:"on_status_change"`
}

// ParseFilter 는 채널 필터 설정을 파싱한다.
//
// **절대 error 를 반환하지 않는다.** 이는 의도된 설계 선택이다: 필터 조건 설정이 잘못되면 모두 0 값
// Filter(= 필터 없음 = 모두 적중)로 퇴화한다. 취약점 알림 시스템에게는 **한 건 더 보내는 것이
// 고위험 하나를 조용히 놓치는 것보다 훨씬 낫기** 때문이다. 파싱 실패를 「푸시 안 함」으로 만들면, 사용자에게 설정이 잘 된 것처럼 보이지만
// 실제로는 아무것도 보내지 않는 채널을 주는 셈이다——이것이 최악의 실패 모드다.
func ParseFilter(raw []byte) Filter {
	var f Filter
	if len(raw) == 0 {
		return f
	}
	// 파싱에 실패하면 f 는 0 값을 유지한다. 즉 필터하지 않는다.
	_ = json.Unmarshal(raw, &f)
	return f
}

// ValidMinSeverity 는 s 가 올바른 등급 기준인지 보고한다(빈 문자열은 기준을 두지 않는다는 뜻).
func ValidMinSeverity(s string) bool {
	if s == "" {
		return true
	}
	_, ok := severityRank[s]
	return ok
}

// Validate 는 필터 설정에서 **값이 제한된** 필드를 검증하며, 채널을 저장할 때 호출한다.
//
// 쓰기 시점에 반드시 막아야 하는 이유: Match 는 알 수 없는 기준을 `rank >= 0` 으로 판정하므로 항상 참이다——
// 즉 min_severity 에 오타 하나("hgih")가 있으면 필터가 **조용히 무효가 되어**
// 「전부 푸시」가 된다. 이는 이 패키지의 「덜 보내느니 더 보낸다」는 절충 방향과는 맞지만(놓치지는 않음),
// 그 결과 사용자는 등급별 푸시를 하고 있다고 생각하지만 실제로는 모든 취약점을 그룹에 쏟아붓게 되며,
// 설정을 잘못했다는 아무런 징후도 없다. 이런 「조용한 품질 저하」는 바로 입구에서 막아야 한다.
//
// Validate 는 **쓰기** 경로에만 쓴다는 점에 주의한다. 읽기 경로는 여전히 ParseFilter 의 관대한 의미를 따르므로,
// 과거 데이터에 이미 있는 잘못된 값 때문에 채널 전체를 읽지 못하는 일은 없다.
func (f Filter) Validate() error {
	if !ValidMinSeverity(f.MinSeverity) {
		return fmt.Errorf("최소 등급 %q 가 유효하지 않습니다. 선택 가능: low / medium / high / critical, 또는 비워 두면 제한 없음", f.MinSeverity)
	}
	return nil
}

// Match 는 이벤트가 이 필터 조건을 가진 채널로 전달되어야 하는지 판정한다.
//
// **절대 error 를 반환하지 않는다**. 이유는 ParseFilter 와 같다: 내부 예외는 모두 「적중」으로 처리한다.
// 판정 순서: 이벤트 유형 → 등급 기준 → 작업/자산 범위 → 취약점 유형 키워드.
func Match(f Filter, s Snapshot) bool {
	// 상태 변경 이벤트는 명시적으로 켠 채널만 받는다. 기본값은 꺼짐이다. 대부분의 사용자는
	// 「푸시」를 모든 상태 전이를 장부처럼 따라가는 것이 아니라 「새 취약점 발견」으로 기대하기 때문이다.
	if s.Kind == EventFindingStatusChanged && !f.OnStatusChange {
		return false
	}
	if !AtLeast(s.Severity, f.MinSeverity) {
		return false
	}
	if len(f.TaskIDs) > 0 && !slices.Contains(f.TaskIDs, s.TaskID) {
		return false
	}
	if len(f.AssetIDs) > 0 && !intersectsInt(f.AssetIDs, s.AssetIDs) {
		return false
	}
	// 제외 우선: 제외 키워드 중 하나에 적중하면 포함 목록에 함께 적중하더라도 탈락한다.
	if len(f.VulnClassExclude) > 0 && containsAnyFold(s.VulnClass, f.VulnClassExclude) {
		return false
	}
	if len(f.VulnClassInclude) > 0 && !containsAnyFold(s.VulnClass, f.VulnClassInclude) {
		return false
	}
	return true
}

func intersectsInt(a, b []int64) bool {
	// 작은 집합은 선형 탐색이면 충분하다. 양쪽 모두 「사람이 손으로 고른 수십 개」 규모이므로,
	// map 을 만드는 비용이 이득보다 크다.
	for _, v := range b {
		if slices.Contains(a, v) {
			return true
		}
	}
	return false
}

// containsAnyFold 는 s 가 keywords 중 어느 키워드든 포함하는지 보고한다(대소문자 구분 안 함).
func containsAnyFold(s string, keywords []string) bool {
	lower := strings.ToLower(s)
	for _, kw := range keywords {
		kw = strings.ToLower(strings.TrimSpace(kw))
		if kw != "" && strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}
