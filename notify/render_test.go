package notify

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateBytesKeepsValidUTF8(t *testing.T) {
	// 이것은 본 패키지에서 가장 중요한 불변량이다. 위챗 워크는 **바이트**로 길이를 제한하고 한국어는 한 글자에 3 바이트라,
	// 바이트 단위로 무작정 자르는 구현은 한글을 반 토막 내 잘못된 UTF-8 을 만들어 플랫폼이 거부한다.
	// 길이가 서로소인 여러 한영 혼합 입력으로 가능한 모든 자름점을 친다.
	inputs := []string{
		"한국어 테스트 내용",
		"혼합 mixed 내용 content",
		"a한b국c어d테e",
		"🔴🟠🟡🔵", // 4 바이트 emoji, 잘못 자르면 더 두드러짐
		strings.Repeat("취약점", 100),
	}
	for _, in := range inputs {
		for max := 1; max <= len(in)+2; max++ {
			got := TruncateBytes(in, max)
			if !utf8.ValidString(got) {
				t.Fatalf("입력 %q max=%d: 잘못된 UTF-8 %q 생성", in, max, got)
			}
			if len(got) > max {
				t.Fatalf("입력 %q max=%d: 결과 %d 바이트가 상한 초과", in, max, len(got))
			}
			// 자르지 않았을 때는 내용을 바꾸면 안 된다.
			if len(in) <= max && got != in {
				t.Fatalf("입력 %q max=%d: 초과하지 않았는데 내용을 바꿈 -> %q", in, max, got)
			}
		}
	}
}

func TestTruncateBytesZeroMeansUnlimited(t *testing.T) {
	long := strings.Repeat("x", 10000)
	if got := TruncateBytes(long, 0); got != long {
		t.Fatal("max=0 은 제한 없음을 뜻해야 함")
	}
	if got := TruncateBytes(long, -5); got != long {
		t.Fatal("max<0 은 제한 없음을 뜻해야 함")
	}
}

func TestTruncateBytesEllipsisBudget(t *testing.T) {
	// max 가 생략 부호 자체보다 작을 때, 생략 부호를 붙이느라 거꾸로 초과하면 안 된다.
	got := TruncateBytes("abcdefgh", 1)
	if len(got) > 1 {
		t.Fatalf("max=1 일 때 결과 %q 길이 %d 가 초과", got, len(got))
	}
	// 정상이면 생략 부호를 달아야 한다.
	if got := TruncateBytes("abcdefgh", 5); !strings.HasSuffix(got, ellipsis) {
		t.Fatalf("생략 부호를 기대했는데 %q 얻음", got)
	}
}

func TestTruncateRunesCountsCharactersNotBytes(t *testing.T) {
	// TruncateBytes 와의 기준 차이는 반드시 유지해야 함: Telegram 은 문자로 길이를 제한하며,
	// 바이트 기준을 쓰면 한국어 메시지가 삼분의 일로 잘린다.
	s := "일이삼사오육칠팔구십"
	got := TruncateRunes(s, 5)
	if n := utf8.RuneCountInString(got); n != 5 {
		t.Fatalf("문자 5 개를 기대했는데 %d 개 (%q)", n, got)
	}
	// 같은 문자열은 바이트 기준으로 보면 분명히 더 짧아야 한다.
	if utf8.RuneCountInString(TruncateBytes(s, 5)) >= 5 {
		t.Fatal("바이트 기준이 문자 기준과 같은 문자 수를 내면 안 됨")
	}
}

func TestOneLineCollapsesWhitespace(t *testing.T) {
	got := OneLine("첫째 줄\n\n둘째 줄\t탭 포함   여러 공백", 0)
	if strings.ContainsAny(got, "\n\t") {
		t.Fatalf("모든 공백을 접어야 하는데 %q 얻음", got)
	}
	if strings.Contains(got, "  ") {
		t.Fatalf("연속 공백을 유지하면 안 되는데 %q 얻음", got)
	}
	// 자른 뒤에도 읽을 수 있고 올바라야 한다.
	got = OneLine("일이삼사오육칠팔구십", 4)
	if n := utf8.RuneCountInString(got); n != 4 {
		t.Fatalf("문자 4 개를 기대했는데 %d (%q)", n, got)
	}
}

func TestTruncateHTMLNeverCutsTagInHalf(t *testing.T) {
	// HTML 을 그대로 자르면 `<a href="htt` 같은 잔편이 나와 플랫폼이 메시지 전체를 거부한다.
	s := `<b>제목</b>본문본문본문<a href="https://example.com/very/long/path">상세 보기</a>`
	for max := 1; max <= utf8.RuneCountInString(s)+2; max++ {
		got := TruncateHTML(s, max)
		if n := utf8.RuneCountInString(got); max > 0 && n > max {
			t.Fatalf("max=%d: 결과 %d 문자가 초과", max, n)
		}
		// 꼬리에 닫히지 않은 `<` 가 있으면 안 된다(즉 마지막 조각에 `<` 가 있는데 `>` 가 없음).
		if lt := strings.LastIndex(got, "<"); lt >= 0 && !strings.Contains(got[lt:], ">") {
			t.Fatalf("max=%d: 꼬리 태그가 잘림 -> %q", max, got)
		}
	}
}

func TestAssetLineOmitsExcess(t *testing.T) {
	if got := assetLine(nil, 3); got != "" {
		t.Fatalf("자산이 없으면 빈 문자열을 반환해야 하는데 %q 얻음", got)
	}
	if got := assetLine([]string{"a", "b"}, 3); got != "a、b" {
		t.Fatalf("초과하지 않으면 전부 나열해야 하는데 %q 얻음", got)
	}
	// 상한을 넘으면 반드시 총수를 표시해야 함. 그러지 않으면 읽는 사람이 자산이 몇 개 더 있는지 모른다.
	got := assetLine([]string{"a", "b", "c", "d", "e"}, 2)
	if !strings.Contains(got, "등 5 개") {
		t.Fatalf("총수 5 를 표시해야 하는데 %q 얻음", got)
	}
}

func TestSeverityAndStatusLabels(t *testing.T) {
	if AtLeast("", "low") {
		t.Fatal("빈 등급의 서수는 0 이라 어떤 임계값에도 걸러져야 함")
	}
	if !AtLeast("critical", "") {
		t.Fatal("빈 임계값은 통과시켜야 함")
	}
	if got := StatusLabel("fixed"); got != "수정됨" {
		t.Fatalf("알 수 없는 상태 매핑, %q 얻음", got)
	}
	// 알 수 없는 상태는 그대로 에코하며 레이블을 지어내지 않는다.
	if got := StatusLabel("weird_status"); got != "weird_status" {
		t.Fatalf("알 수 없는 상태는 그대로 에코해야 하는데 %q 얻음", got)
	}
}

// TestTruncateHTMLNeverCutsEntity 는 감사가 지적한 누락을 다룬다: 자르기는 반 토막 태그뿐 아니라
// 잘린 HTML 엔티티도 피해야 한다.
//
// `&amp;` 가 `&amp` 로 잘리면, 엔티티만 인정하는 파서가 메시지 **전체**를 거부할 수 있다——
// 초장문 요약 메시지는 원래 흔하므로 대가가 너무 크다.
func TestTruncateHTMLNeverCutsEntity(t *testing.T) {
	s := "aaaa&amp;bbbb&lt;cccc&quot;dddd"
	for max := 1; max <= utf8.RuneCountInString(s)+2; max++ {
		got := TruncateHTML(s, max)
		// 꼬리에 「& 는 있지만 대응하는 ; 가 없는」 엔티티 잔편이 나오면 안 된다.
		if amp := strings.LastIndex(got, "&"); amp >= 0 && !strings.Contains(got[amp:], ";") {
			t.Fatalf("max=%d: 꼬리에 엔티티 잔편 %q 가 남음", max, got[amp:])
		}
		if strings.Contains(got, "&amp\x00") {
			t.Fatalf("max=%d: 잘못된 엔티티가 나타남", max)
		}
	}
}
