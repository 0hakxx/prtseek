package notify

import "testing"

func TestParseFilterMalformedFallsBackToMatchAll(t *testing.T) {
	// 잘못된 JSON, 빈 입력, 타입이 틀린 필드——모두 반드시 제로값 Filter 로,
	// 즉 「필터 안 함」으로 퇴화해야 한다. 이 불변식은 「덜 푸시하느니 더 푸시한다」의 착지점이다:
	// 여기서 오류를 내거나 반만 파싱하게 바꾸면, 사용자가 한 글자만 틀려도 모든 고위험 알림이 조용히 사라진다.
	cases := []struct {
		name string
		raw  string
	}{
		{"빈 입력", ""},
		{"잘못된 JSON", `{not json`},
		{"잘린 JSON", `{"min_severity":`},
		{"타입 불일치", `{"min_severity": 123, "task_ids": "abc"}`},
		{"최상위가 배열", `[1,2,3]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := ParseFilter([]byte(tc.raw))
			if f.MinSeverity != "" || len(f.TaskIDs) != 0 || len(f.AssetIDs) != 0 {
				t.Fatalf("잘못된 설정은 제로값 Filter 로 퇴화해야 하는데 %+v 얻음", f)
			}
			// 제로값 Filter 는 반드시 모든 이벤트에 적중해야 한다.
			ev := Snapshot{Kind: EventFindingCreated, Severity: "low", VulnClass: "XSS"}
			if !Match(f, ev) {
				t.Fatal("제로값 Filter 는 모든 이벤트에 적중해야 함")
			}
		})
	}
}

func TestMatchSeverityThreshold(t *testing.T) {
	ev := func(sev string) Snapshot {
		return Snapshot{Kind: EventFindingCreated, Severity: sev}
	}
	cases := []struct {
		min    string
		sev    string
		expect bool
	}{
		{"", "low", true},
		{"", "critical", true},
		{"high", "critical", true},
		{"high", "high", true},
		{"high", "medium", false},
		{"high", "low", false},
		{"critical", "high", false},
		{"critical", "critical", true},
		// 알 수 없는 등급은 서수가 0 이라 비어 있지 않은 어떤 임계값에도 걸러져야 한다(의심스러우면 푸시 안 함).
		{"low", "", false},
		{"low", "unknown", false},
		{"", "", true},
	}
	for _, tc := range cases {
		got := Match(Filter{MinSeverity: tc.min}, ev(tc.sev))
		if got != tc.expect {
			t.Errorf("min=%q sev=%q: 기대 %v 얻음 %v", tc.min, tc.sev, tc.expect, got)
		}
	}
}

func TestMatchScopeRestrictions(t *testing.T) {
	ev := Snapshot{
		Kind:      EventFindingCreated,
		Severity:  "high",
		TaskID:    7,
		AssetIDs:  []int64{10, 20},
		VulnClass: "SQL 주입",
	}
	cases := []struct {
		name   string
		filter Filter
		expect bool
	}{
		{"빈 범위=제한 없음", Filter{}, true},
		{"작업 적중", Filter{TaskIDs: []int64{7}}, true},
		{"작업 미적중", Filter{TaskIDs: []int64{8}}, false},
		{"작업 다중 선택에 적중 포함", Filter{TaskIDs: []int64{8, 7}}, true},
		{"자산 교집합 있음", Filter{AssetIDs: []int64{20, 99}}, true},
		{"자산 교집합 없음", Filter{AssetIDs: []int64{99}}, false},
		{"작업과 자산 동시 적중", Filter{TaskIDs: []int64{7}, AssetIDs: []int64{10}}, true},
		{"작업은 적중하지만 자산은 미적중", Filter{TaskIDs: []int64{7}, AssetIDs: []int64{99}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Match(tc.filter, ev); got != tc.expect {
				t.Errorf("기대 %v 얻음 %v", tc.expect, got)
			}
		})
	}
}

func TestMatchVulnClassKeywords(t *testing.T) {
	ev := func(class string) Snapshot {
		return Snapshot{Kind: EventFindingCreated, Severity: "high", VulnClass: class}
	}
	cases := []struct {
		name   string
		filter Filter
		class  string
		expect bool
	}{
		{"include 가 비면=전부 수신", Filter{}, "임의 유형", true},
		{"include 적중", Filter{VulnClassInclude: []string{"SQL"}}, "SQL 주입", true},
		{"include 미적중", Filter{VulnClassInclude: []string{"명령 실행"}}, "SQL 주입", false},
		{"include 여러 단어 중 하나 적중", Filter{VulnClassInclude: []string{"명령 실행", "SQL"}}, "SQL 주입", true},
		{"대소문자 구분 안 함", Filter{VulnClassInclude: []string{"sql"}}, "SQL 주입", true},
		{"exclude 적중 시 제외", Filter{VulnClassExclude: []string{"정보 유출"}}, "정보 유출", false},
		{"exclude 미적중 시 통과", Filter{VulnClassExclude: []string{"정보 유출"}}, "SQL 주입", true},
		// 제외가 포함보다 우선: 동시에 적중하면 탈락해야 한다.
		{"제외가 포함보다 우선", Filter{
			VulnClassInclude: []string{"SQL"},
			VulnClassExclude: []string{"주입"},
		}, "SQL 주입", false},
		// 순수 공백 키워드는 무시해야 한다. 그러지 않으면 「공백이 든 모든 문자열에 매칭」으로 퇴화한다.
		{"공백 키워드 무시됨", Filter{VulnClassInclude: []string{"", "  "}}, "SQL 주입", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Match(tc.filter, ev(tc.class)); got != tc.expect {
				t.Errorf("기대 %v 얻음 %v", tc.expect, got)
			}
		})
	}
}

func TestMatchStatusChangeRequiresOptIn(t *testing.T) {
	ev := Snapshot{Kind: EventFindingStatusChanged, Severity: "critical", FromStatus: "pending", ToStatus: "fixed"}
	// 기본 꺼짐: 대부분의 사람이 말하는 「취약점 푸시」는 새 취약점 발견이지 상태 변경 내역이 아니다.
	if Match(Filter{MinSeverity: "low"}, ev) {
		t.Fatal("상태 변경 이벤트는 켜지 않았을 때 건너뛰어야 함")
	}
	if !Match(Filter{OnStatusChange: true}, ev) {
		t.Fatal("on_status_change 를 켠 뒤 상태 변경 이벤트는 적중해야 함")
	}
	// 생성 이벤트는 on_status_change 의 영향을 받지 않는다.
	created := Snapshot{Kind: EventFindingCreated, Severity: "critical"}
	if !Match(Filter{MinSeverity: "low"}, created) {
		t.Fatal("생성 이벤트는 on_status_change 에 의존하면 안 됨")
	}
}
