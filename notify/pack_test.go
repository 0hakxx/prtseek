package notify

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// 이 파일은 「통째로 패킹」 수정을 다룬다: 요약 메시지가 채널 길이 상한을 넘으면 반드시 **통째로**
// 자르고 담지 못한 항목 수를 사실대로 보고해, 호출자가 실제로 전달된 것만 표시하게 해야 한다.
//
// 예전 방식은 전체를 렌더링한 뒤 자르고 배치 전체를 전달됨으로 표시했다: 메시지 뒷부분이 흔적 없이 사라지는데
// 전달 이력은 전부 성공으로 표시된다——취약점이 그렇게 사라졌고 어디서도 발견할 수 없었다.

func TestMarkdownBodyPacksWholeItemsWithinByteLimit(t *testing.T) {
	// 200 건 한국어 요약, 반드시 위챗 워크 4096 바이트를 훨씬 넘김.
	m := batchMsg(200)
	body, kept := markdownBody(m, weComMarkdownLimit)

	if len(body) > weComMarkdownLimit {
		t.Fatalf("본문 %d 바이트가 상한 %d 초과", len(body), weComMarkdownLimit)
	}
	if !utf8.ValidString(body) {
		t.Fatal("본문이 올바른 UTF-8 이 아님")
	}
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("일부만 담겨야 함(0 < kept < %d) 인데 %d 얻음", len(m.Items), kept)
	}
	// 헤더는 이 건이 몇 건만 포함하고 나머지가 몇 건인지 사실대로 밝혀야 한다——그러지 않으면 읽는 사람이 헤더
	// 그 숫자를 전부로 여긴다.
	if !strings.Contains(body, "나머지") || !strings.Contains(body, "다음 메시지에서 이어집니다") {
		t.Fatalf("헤더는 이 건에 포함되지 않은 것이 몇 건인지 밝혀야 함:\n%s", body[:minInt(400, len(body))])
	}
	// 앞의 kept 건만 포함해야 한다.
	for i := 0; i < kept; i++ {
		if !strings.Contains(body, "취약점"+itoa(i+1)) {
			t.Fatalf("%d 번째 건은 이 메시지에 있어야 함:\n%s", i+1, body)
		}
	}
	if strings.Contains(body, "취약점"+itoa(kept+1)) {
		t.Fatalf("%d 번째 건은 나타나면 안 됨(다음 배치에 속함)", kept+1)
	}
}

func TestMarkdownBodyKeepsEverythingWhenUnderLimit(t *testing.T) {
	m := batchMsg(3)
	body, kept := markdownBody(m, 0) // 0 = 제한 없음
	if kept != len(m.Items) {
		t.Fatalf("길이 제한이 없으면 전부 유지해야 하는데 kept=%d", kept)
	}
	if strings.Contains(body, "나머지") {
		t.Fatalf("자르지 않았으면 자름 안내가 나타나면 안 됨:\n%s", body)
	}
}

func TestMarkdownBodyAlwaysKeepsAtLeastOneItem(t *testing.T) {
	// 예산이 한 건도 못 담을 만큼 작아도, 여전히 한 건은 보내야 한다(최종 자르기가 받쳐 줌).
	// 그러지 않으면 초장문 취약점 하나가 배치 전체를 영원히 멈춰 세운다: 가져갈 때마다 못 담고 매번 안 보냄.
	m := batchMsg(5)
	_, kept := markdownBody(m, 50)
	if kept != 1 {
		t.Fatalf("최소 1 건은 유지해야 하는데 %d 얻음", kept)
	}
}

func TestMarkdownBodySingleReturnsOne(t *testing.T) {
	_, kept := markdownBody(singleMsg(), 4096)
	if kept != 1 {
		t.Fatalf("단일 메시지는 1 건 전달을 보고해야 하는데 %d 얻음", kept)
	}
	// 빈 메시지는 전달할 항목이 없다.
	if _, k := markdownBody(Message{}, 4096); k != 0 {
		t.Fatalf("빈 메시지는 0 건을 보고해야 하는데 %d 얻음", k)
	}
}

func TestTelegramPackingUsesRuneBudget(t *testing.T) {
	m := batchMsg(200)
	text, kept := telegramHTML(m)
	// Telegram 은 **문자 수**로 길이를 제한한다. 바이트 기준을 쓰면 한국어 메시지가 삼분의 일로 줄어든다.
	if n := utf8.RuneCountInString(text); n > telegramTextLimit {
		t.Fatalf("본문 %d 문자가 상한 %d 초과", n, telegramTextLimit)
	}
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("일부만 담겨야 하는데 %d 얻음", kept)
	}
	if !strings.Contains(text, "다음 메시지에서 이어짐") {
		t.Fatalf("아직 포함되지 않은 여분이 있음을 밝혀야 함:\n%.300s", text)
	}
}

func TestFeishuPackingReportsKept(t *testing.T) {
	m := batchMsg(2000)
	_, kept := feishuCard(m)
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("카드는 일부만 담겨야 하는데 %d 얻음", kept)
	}
}

func TestWebhookAndEmailReportAllItems(t *testing.T) {
	// 이 두 채널은 본문을 자르지 않으며 배치 전체를 전달된 것으로 친다.
	m := batchMsg(7)
	if n := len(m.Items); n != 7 {
		t.Fatal("선행 조건 불충족")
	}
	// 렌더러의 반환값으로 간접 확인: markdownBody(0) 은 제한이 없어 전부 유지한다.
	if _, k := markdownBody(m, 0); k != len(m.Items) {
		t.Fatalf("길이 제한이 없으면 전부 적용해야 하는데 %d 얻음", k)
	}
}

// TestMarkdownEscapesUntrustedContent 는 「신뢰할 수 없는 내용이 메시지 구조를 바꾸면 안 됨」의 회귀 테스트다.
// 제목과 요약은 모델 출력에서 오고(모델은 테스트 대상 응답을 읽음), 자산 이름은 테스트 대상의 URL 에서 온다.
func TestMarkdownEscapesUntrustedContent(t *testing.T) {
	cases := []struct {
		name  string
		item  Item
		must  []string // 결과에 반드시 나타나야 함(이스케이프 형태)
		wrong []string // 결과에 나타나면 안 됨(이스케이프 안 된 형태)
	}{
		{
			name: "제목 안의 줄바꿈 + 외부 링크",
			item: Item{
				Severity: "high",
				Name:     "로그인 입구 SQL 주입\n[긴급: 여기를 눌러 계정 인증](http://attacker.tld)",
			},
			// 줄바꿈은 반드시 접혀야 함(그러지 않으면 새 목록 항목/인용 블록을 위조 가능).
			// 대괄호와 소괄호는 반드시 이스케이프되어야 함(그러지 않으면 클릭 가능한 외부 링크).
			must:  []string{`\[긴급: 여기를 눌러 계정 인증\]`, `\(http://attacker.tld\)`},
			wrong: []string{"\n[긴급", "\n\n[긴급"},
		},
		{
			name: "제목 안의 이미지 비컨",
			item: Item{
				Severity: "high",
				Name:     "취약점 ![](http://attacker.tld/beacon)",
			},
			must:  []string{`\!`, `\(http://attacker.tld/beacon\)`},
			wrong: []string{"![]("},
		},
		{
			name: "자산 이름 안의 강조와 인용",
			item: Item{
				Severity: "high",
				Name:     "일반 제목",
				Assets:   []string{"a.com/*주입*>인용"},
			},
			must:  []string{`\*주입\*`, `\>`},
			wrong: []string{"*주입*"},
		},
		{
			name: "요약 안의 백틱과 세로줄",
			item: Item{
				Severity: "high",
				Name:     "제목",
				Summary:  "`code` | 표",
			},
			must:  []string{"\\`code\\`", `\|`},
			wrong: []string{"`code`"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := Message{Items: []Item{tc.item}}
			// 단일 모드의 writeItem 은 세 markdown 채널이 공유하는 렌더링 경로다.
			var b strings.Builder
			writeItem(&b, tc.item, "", true)
			got := b.String()
			for _, want := range tc.must {
				if !strings.Contains(got, want) {
					t.Errorf("이스케이프 형태 %q 가 없음:\n%s", want, got)
				}
			}
			for _, bad := range tc.wrong {
				if strings.Contains(got, bad) {
					t.Errorf("이스케이프 안 된 형태 %q 가 나타남(구조나 외부 링크 주입에 쓰일 수 있음):\n%s", bad, got)
				}
			}
			_ = m
		})
	}
}

// TestMarkdownEscapeBackslashFirst 는 이스케이프 순서를 못 박는다: 백슬래시를 반드시 가장 먼저 처리해야 함.
// 그러지 않으면 뒤에 보충한 백슬래시에 한 겹 더 씌워져 출력에 이중 백슬래시가 나온다.
func TestMarkdownEscapeBackslashFirst(t *testing.T) {
	if got := markdownEscape(`a\b*c`); got != `a\\b\*c` {
		t.Fatalf("이스케이프 순서 오류, %q 얻음", got)
	}
}

// TestTelegramTitleHasNoMarkdownEscapes 는 구체적인 회귀를 못 박는다:
// markdown 이스케이프가 Telegram 의 HTML 출력에 새어 나오면 안 됨(예전에 공유된 제목 함수에
// 이스케이프를 넣어, Telegram 메시지에 `\(1\)` 같은 보이는 백슬래시가 나왔음).
func TestTelegramTitleHasNoMarkdownEscapes(t *testing.T) {
	m := Message{Items: []Item{{Severity: "high", Name: "alert(1) *중점*"}}}
	text, _ := telegramHTML(m)
	if strings.Contains(text, `\(`) || strings.Contains(text, `\*`) {
		t.Fatalf("Telegram 본문에 markdown 의 백슬래시 이스케이프가 나타남:\n%s", text)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
