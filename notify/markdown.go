package notify

import (
	"fmt"
	"strings"
)

// 이 파일은 「Markdown 계열」 채널(딩톡, 위챗 워크)이 공유하는 메시지 렌더링이다.
// 페이슈는 카드 JSON, Telegram 은 HTML, 메일은 HTML 을 쓰며, 각자의 어댑터에서 렌더링한다.

// maxAssetsShown 은 메시지에 나열할 최대 자산 수다. 취약점 하나는 수십 개의 자산에 고정될 수 있는데,
// 전부 나열하면 메시지가 넘치고 정보 가치도 없다——네 번째 이후의 도메인은 아무도 IM 에서 보지 않는다.
const maxAssetsShown = 3

// maxSummaryRunes 는 요약을 몇 자로 압축할지다. IM 메시지는 「상세를 보러 가라는 안내」이지
// 보고서 본체가 아니며, 전체 내용은 플랫폼에 있다.
const maxSummaryRunes = 120

// markdownReservedBytes 는 메시지 머리(요약 줄 + 등급 분포 + 가능한 잘림 안내)와
// 꼬리(플랫폼 링크)를 위해 남겨 둔다. 항목 전체를 담을 때 이 부분을 예산에서 빼서 머리와 꼬리가 잘리지 않게 한다——
// 머리와 꼬리가 잘리면 읽는 사람은 「이게 어느 배치이고 표시되지 않은 것이 몇 건인지」조차 알 수 없다.
const markdownReservedBytes = 320

// markdownEscape 는 markdown 메타 문자를 이스케이프한다.
//
// 반드시 해야 하는 이유: 취약점 제목, 요약, 유형, 자산 표시 이름은 모두 **신뢰할 수 없는 출처**에서 온다——
// 제목과 요약은 모델 출력(모델은 테스트 대상의 응답을 읽음)에서 나오고, 자산의 url 은 스캔으로
// 얻은 전체 URL(대상이 제어할 수 있는 쿼리 문자열 포함)이다. 이스케이프하지 않으면 제목이
//
//	로그인 페이지 SQL 주입\n[긴급: 여기를 눌러 계정 확인](http://attacker.tld)
//
// 인 취약점은 보안 엔지니어의 딩톡/페이슈에서 **클릭 가능한 외부 링크**로 렌더링된다. 또한
// `![](http://attacker.tld/beacon)` 은 렌더링할 때 클라이언트가 가져오므로,
// 「이 취약점을 이미 봤다」는 것을 알리고 읽는 사람의 IP 를 유출하는 셈이다. 악의가 없는 내용이더라도 주입된 굵은 글씨나
// 인용 블록이 아래의 심각한 취약점을 접힘 선 밖으로 밀어낼 수 있다.
//
// 이스케이프 집합은 제목/링크/강조/목록/인용/취소선처럼 구조를 바꾸거나 클릭 가능한
// 요소를 만드는 문자들을 다룬다. `\` 는 반드시 가장 먼저 처리해야 한다. 그렇지 않으면 나중에 붙인 백슬래시를 다시 이스케이프한다.
func markdownEscape(s string) string {
	replacer := strings.NewReplacer(
		`\`, `\\`,
		"`", "\\`",
		"*", `\*`,
		"_", `\_`,
		"[", `\[`,
		"]", `\]`,
		"(", `\(`,
		")", `\)`,
		"!", `\!`,
		"#", `\#`,
		">", `\>`,
		"|", `\|`,
		"~", `\~`,
	)
	return replacer.Replace(s)
}

// markdownText 는 신뢰할 수 없는 텍스트를 한 줄로 압축하고 이스케이프해 markdown 본문에 쓴다.
// 한 줄화는 이스케이프 외의 나머지 절반이다: 줄바꿈 자체로 새 목록 항목이나 인용 블록을 위조할 수 있으며,
// 이스케이프 문자로는 이를 막을 수 없다.
func markdownText(s string, maxRunes int) string {
	return markdownEscape(OneLine(s, maxRunes))
}

// markdownTitle 은 메시지 제목(IM 플랫폼의 제목 표시줄/카드 제목)을 반환하며, 내용은 **이스케이프하지 않은 원문**이다.
//
// 여기서는 일부러 이스케이프하지 않는다: 이 제목은 네 가지 맥락의 렌더러가 공유한다——markdown 본문, Telegram 의
// HTML, 페이슈 카드의 plain_text, 일반 Webhook 의 JSON 과 메일 제목. 맥락마다
// 이스케이프 규칙이 다르므로(markdown 이스케이프를 HTML 에 넣으면 보이는 백슬래시가 남고, JSON 에 넣으면
// 데이터가 오염됨) 이스케이프는 각 출력 쪽이 책임져야 한다. writeItem / feishuItemLines /
// telegramEscape 참고. 예전에 공유 함수에 markdown 이스케이프를 넣었더니 Telegram 메시지에
// `\(1\)` 같은 보이는 백슬래시가 나타났다.
func markdownTitle(m Message) string {
	if m.Batch {
		return fmt.Sprintf("취약점 요약 · 총 %d 건", len(m.Items))
	}
	if len(m.Items) == 0 {
		return "취약점 알림"
	}
	it := m.Items[0]
	return fmt.Sprintf("[%s] %s", SeverityLabel(it.Severity), OneLine(it.Title(), 0))
}

// markdownBody 는 메시지 본문을 렌더링하고, 본문과 **실제로 쓴 항목 수**를 반환한다.
//
// 반환값 kept 는 이번 전달에서 실제로 전달된 항목 수이며, 호출자는 이에 따라 앞의 kept 건만
// 전달됨으로 표시한다——채널 길이 상한에 막혀 밖에 남은 항목은 반드시 다음 배치로 남겨야 하며, 함께
// 성공으로 표시되면 안 된다. 이것이 바로 「조용한 유실」의 원인이다: 메시지는 잘렸는데 전달 기록은 전부 전달됨으로 표시되어,
// 뒷부분이 한 번도 보내지지 않았다는 것을 어디서도 알 수 없다.
//
// maxBytes<=0 이면 제한하지 않는다.
func markdownBody(m Message, maxBytes int) (string, int) {
	if !m.Batch {
		if len(m.Items) == 0 {
			return "", 0
		}
		var b strings.Builder
		writeItem(&b, m.Items[0], "", true)
		// 단건 메시지는 너무 길어도 그대로 보낸다(최종 잘림이 대비책): 취약점 하나의 일부 정보라도
		// 하나도 보내지 않는 것보다 낫다.
		return TruncateBytes(b.String(), maxBytes), 1
	}

	footer := ""
	if m.HomeURL != "" {
		footer = fmt.Sprintf("\n[플랫폼에서 전체 보기](%s)\n", m.HomeURL)
	}
	kept := packItemCount(m.Items, maxBytes, markdownReservedBytes, footer, byteSize, func(it Item, idx int) string {
		var b strings.Builder
		writeItem(&b, it, fmt.Sprintf("%d. ", idx+1), false)
		return b.String()
	})

	items := m.Items[:kept]
	var b strings.Builder
	b.WriteString(markdownBatchIntro(m, items, len(m.Items)))
	for i, it := range items {
		writeItem(&b, it, fmt.Sprintf("%d. ", i+1), false)
	}
	b.WriteString(footer)
	return TruncateBytes(b.String(), maxBytes), kept
}

// markdownBatchIntro 는 요약 메시지의 첫머리를 렌더링한다: 시간 구간, 건수, 등급 분포.
// 이것이 있으면 요약을 받은 사람은 플랫폼에 들어가지 않고도 이 배치를 바로 처리해야 하는지 판단할 수 있다.
//
// items 는 **실제로 담긴** 항목이고, total 은 이번 배치에 있어야 할 총수다. 둘이 다르면 반드시
// 「나머지 몇 건이 다음 메시지에 있다」고 밝혀야 한다——그렇지 않으면 읽는 사람은 머리에 적힌 숫자가 전부라고 생각하고,
// 한 번도 보내지지 않은 뒤쪽 항목은 화면에 전혀 존재하지 않게 된다.
func markdownBatchIntro(m Message, items []Item, total int) string {
	var b strings.Builder
	if m.WindowMinutes > 0 {
		fmt.Fprintf(&b, "**최근 %d 분 동안 새 취약점 %d 건**", m.WindowMinutes, total)
	} else {
		fmt.Fprintf(&b, "**새 취약점 %d 건**", total)
	}
	if extra := total - len(items); extra > 0 {
		fmt.Fprintf(&b, "(이 메시지에는 앞의 %d 건만 표시하며, 나머지 %d 건은 다음 메시지에서 이어집니다)", len(items), extra)
	}
	// 등급별 분포를 보여 줘 읽는 사람이 심각한 항목이 있는지 한눈에 보게 한다. **이 메시지에 실제로 포함된**
	// 항목만 집계해, 「심각 3」이 아래에서 셀 수 있는 항목과 일치하도록 보장한다.
	counts := map[string]int{}
	for _, it := range items {
		counts[it.Severity]++
	}
	var parts []string
	for _, sev := range []string{"critical", "high", "medium", "low"} {
		if n := counts[sev]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", SeverityLabel(sev), n))
		}
	}
	if len(parts) > 0 {
		b.WriteString("\n" + strings.Join(parts, " · "))
	}
	b.WriteString("\n\n")
	return b.String()
}

// writeItem 은 취약점 항목 하나를 렌더링한다.
//
// prefix 는 요약 목록의 번호에 쓴다. single=true 이면 전체 버전(요약과 링크 포함)을 렌더링하고,
// 요약 목록에서는 한 줄 요약만 렌더링한다——그렇지 않으면 50 건짜리 요약이 긴 문서가 된다.
//
// 외부에서 온 모든 내용(제목/유형/자산/요약)은 markdownText 를 거친다:
// 한 줄화 + 이스케이프. 링크는 관리자가 설정한 public_base_url 로 만든 것이라 신뢰할 수 없는 내용이 아니며,
// 클릭 가능한 링크여야 하므로 그대로 출력한다.
func writeItem(b *strings.Builder, it Item, prefix string, single bool) {
	line := fmt.Sprintf("%s**%s · %s**", prefix, SeverityLabel(it.Severity), markdownText(it.Title(), 0))
	if !single {
		// 요약 모드: 한 줄로 표시하며, 자산과 요약은 압축해 뒤에 붙인다.
		var extras []string
		if a := assetLine(it.Assets, maxAssetsShown); a != "" {
			extras = append(extras, markdownText(a, 0))
		}
		if it.Summary != "" {
			extras = append(extras, markdownText(it.Summary, 60))
		}
		if len(extras) > 0 {
			line += " — " + strings.Join(extras, " · ")
		}
		b.WriteString(line + "\n")
		return
	}
	b.WriteString(line + "\n")
	if it.IsStatusChange() {
		fmt.Fprintf(b, "**상태 변경**: %s → %s\n",
			markdownText(StatusLabel(it.FromStatus), 0), markdownText(StatusLabel(it.ToStatus), 0))
	}
	if it.VulnClass != "" && it.VulnClass != it.Title() {
		fmt.Fprintf(b, "**유형**: %s\n", markdownText(it.VulnClass, 0))
	}
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		fmt.Fprintf(b, "**자산**: %s\n", markdownText(a, 0))
	}
	if it.Summary != "" {
		if s := markdownText(it.Summary, maxSummaryRunes); s != "" {
			fmt.Fprintf(b, "**요약**: %s\n", s)
		}
	}
	if it.DetailURL != "" {
		fmt.Fprintf(b, "[상세 보기](%s)\n", it.DetailURL)
	}
}
