package notify

import (
	"strings"
	"unicode/utf8"
)

const ellipsis = "…"

// TruncateBytes 는 s 를 max 바이트 이하로 자르며, 결과가 올바른 UTF-8 이고 문자를 끊지 않도록 보장한다.
//
// 반드시 문자 경계에서 잘라야 하는 이유: 위챗 워크 그룹 봇의 markdown 에는 4096 **바이트**의 절대 상한이 있으며(문자
// 수가 아님), 한글·한자 한 글자는 3 바이트다. 바이트로 바로 자르면 한 글자가 반으로 잘려 잘못된
// UTF-8 이 나온다——플랫폼은 통째로 거부하거나 깨진 네모로 표시한다. 여기서는 먼저 예산 위치에서
// 가장 가까운 rune 시작 바이트까지 앞으로 물러난다(utf8.RuneStart 로 연속 바이트 0b10xxxxxx 판정).
//
// max<=0 이면 제한하지 않는다. 자른 뒤에는 말줄임표를 붙이며, max 가 너무 작아 말줄임표를 담을 수 없으면 붙이지 않는다.
func TruncateBytes(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	budget := max - len(ellipsis)
	suffix := ellipsis
	if budget < 0 {
		// max 가 말줄임표보다 짧음: 말줄임표를 포기하고 순수하게 잘라, 결과가 오히려 max 를 넘지 않게 한다.
		budget = max
		suffix = ""
	}
	cut := budget
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + suffix
}

// OneLine 은 여러 줄 텍스트를 한 줄로 압축한다: 모든 공백을 접은 뒤 문자 수로 자른다.
// IM 메시지의 제목 줄에 쓴다——요약에는 줄바꿈이 흔해서 표/제목에 그대로 넣으면 레이아웃이 깨진다.
// max<=0 이면 길이를 제한하지 않는다.
func OneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	return TruncateRunes(s, max)
}

// TruncateRunes 는 s 를 max 개 문자(바이트가 아님) 이하로 자르며, 넘치면 말줄임표를 붙인다.
// max<=0 이면 제한하지 않는다.
//
// TruncateBytes 와의 차이는 플랫폼 기준이다: 위챗 워크는 바이트로, Telegram 은 문자 수로 길이를 제한한다.
// 기준을 잘못 쓰면 오류는 나지 않지만 메시지가 예상보다 훨씬 짧게 잘린다(한글 1자 = 3 바이트,
// 바이트로 4096 을 자르면 약 1365 자만 남음). 그래서 두 함수를 모두 유지하고 채널에 따라 골라 쓴다.
func TruncateRunes(s string, max int) string {
	if max <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max <= 1 {
		return string(runes[:max])
	}
	return string(runes[:max-1]) + ellipsis
}

// TruncateHTML 은 HTML 조각을 문자 수로 자르며, 반쪽짜리 태그가 생기지 않도록 보장한다.
//
// HTML 을 바로 문자 단위로 자르면 `<a href="htt` 같은 불완전한 태그가 생겨, 플랫폼 파서가
// 오류를 내며 통째로 거부하거나 뒤의 본문을 속성 값으로 삼켜 버린다. 여기서는 먼저 문자 수로 자른 뒤,
// 끝에 닫히지 않은 `<` 가 있는지 확인하고, 있으면 그 앞으로 물러난다.
//
// 태그 균형 맞추기(</b> 보충 등)는 하지 않는다: Telegram 의 HTML 파서는 닫히지 않은 태그를 자동으로 닫으며,
// 직접 균형을 맞추려면 속성 안의 따옴표, 주석, 자기 닫힘 태그를 처리해야 해 복잡도와 이득이 맞지 않는다.
func TruncateHTML(s string, max int) string {
	if max <= 0 || len([]rune(s)) <= max {
		return s
	}
	cut := TruncateRunes(s, max)
	// 끝이 `<` 로 시작하는 조각이면(마지막 `<` 뒤에 `>` 가 없음) `<` 앞으로 물러난다.
	if lt := strings.LastIndex(cut, "<"); lt >= 0 && !strings.Contains(cut[lt:], ">") {
		cut = cut[:lt]
	}
	// 끝이 잘린 HTML 엔티티(예: `&amp;` 가 `&amp` 로 잘림)라면 마찬가지로 물러나야 한다.
	// 엔티티 조각은 엔티티만 인식하는 파서에서 **메시지 전체**가 거부되게 할 수 있다——
	// 길이 상한을 넘는 요약 메시지는 원래 흔하므로, 이 때문에 알림 전체를 잃을 가치는 없다.
	if amp := strings.LastIndex(cut, "&"); amp >= 0 && !strings.Contains(cut[amp:], ";") {
		cut = cut[:amp]
	}
	return cut
}

// packItemCount 는 예산 안에 **온전히** 담을 수 있는 항목 수를 계산하며, 요약 메시지를 항목 단위로 담을 때 쓴다.
//
// 전체를 렌더링한 뒤 자르지 않고 항목 단위로 담는 이유: 자르면 뒷부분 항목이 감쪽같이 사라지는데,
// 그 전달 기록은 여전히 전달됨으로 표시된다——메시지에서도 전달 기록에서도 알 수 없어,
// 취약점이 그렇게 사라진다. 항목 단위로 담으면 담지 못한 항목은 DB 에 남아 다음 배치가 되고,
// 호출자가 받는 kept 가 바로 이 메시지가 실제로 전달한 건수가 된다.
//
// 파라미터: maxSize<=0 이면 제한 없음. reserve 는 메시지 머리/꼬리를 위해 남겨 둘 양이다.
// size 는 계량을 담당한다(플랫폼마다 기준이 다르다: 위챗 워크/딩톡은 바이트, Telegram 은 문자 수——
// 기준을 잘못 쓰면 오류는 나지 않지만 한글 메시지가 상한보다 훨씬 작게 눌린다).
// render 는 idx 번째 항목을 실제 텍스트로 렌더링한다——길이는 내용에 따라 다르므로 추정에 의존할 수 없다.
//
// 최소 1 을 반환한다(항목이 남아 있다면). 항목 하나가 극단적으로 길어도 이 하나는 보내고 호출자의
// 최종 잘림에 맡긴다. 그렇지 않으면 아주 긴 취약점 하나가 배치 전체를 영원히 그 자리에 묶어 둔다.
func packItemCount(items []Item, maxSize, reserve int, footer string, size func(string) int, render func(Item, int) string) int {
	if maxSize <= 0 {
		return len(items)
	}
	budget := maxSize - reserve - size(footer)
	if budget < 0 {
		budget = 0
	}
	used := 0
	for i, it := range items {
		used += size(render(it, i))
		if used > budget && i > 0 {
			return i
		}
	}
	return len(items)
}

// byteSize / runeSize 는 packItemCount 의 두 가지 계량 기준이다. 이름을 붙여 두면 호출하는 곳에
// 맨 func(s string) int 클로저가 나타나지 않아, 어느 기준을 쓰는지 한눈에 알 수 있다.
func byteSize(s string) int { return len(s) }
func runeSize(s string) int { return utf8.RuneCountInString(s) }

// assetLine 은 자산 목록을 한 줄 표시 텍스트로 렌더링하며, limit 개를 넘으면 나머지는 생략하고 총수를 표시한다.
// 취약점 하나는 수십 개의 자산에 고정될 수 있어, 전부 나열하면 메시지가 넘친다.
func assetLine(assets []string, limit int) string {
	if len(assets) == 0 {
		return ""
	}
	if limit <= 0 || len(assets) <= limit {
		return strings.Join(assets, "、")
	}
	return strings.Join(assets[:limit], ", ") + " 등 " + itoa(len(assets)) + " 개"
}

// itoa 는 strconv.Itoa 의 짧은 별칭이며, 표시 텍스트를 이어 붙이는 데만 써서 곳곳에서 strconv 를 import 하지 않게 한다.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
