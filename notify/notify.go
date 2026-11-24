// Package notify 는 취약점 발견의 IM / 메일 푸시 채널 어댑터 계층을 구현한다.
//
// 계층: 이 패키지는 **말단 패키지**이며 표준 라이브러리에만 의존한다. 데이터베이스도 server 도 모른다. 채널 설정은
// map[string]any 로 전달되며(notification_channels.config 라는 JSONB 열에 대응),
// 푸시할 내용은 Message 로 전달된다. 이렇게 나누면 서명 계산, UTF-8 잘림, 필터 매칭처럼
// 정말 틀리기 쉬운 부분을 SQLite 없이 단위 테스트할 수 있고, 호스트는 server 측에서 조율만 하면 된다.
//
// 동시성 규약: Channel 구현은 반드시 **상태가 없어야** 한다. 같은 Channel 인스턴스가 여러 채널 설정
// (심지어 같은 채널의 여러 봇 인스턴스)에서 동시에 재사용되므로, 모든 자격 증명은 cfg 파라미터로 전달하며,
// webhook URL 같은 것을 구현 자체의 필드에 캐시해서는 안 된다.
package notify

// 채널 유형 식별자. 값은 동시에 notification_channels.kind 의 허용 집합이며, server 측에서
// 화이트리스트로 검증한다(findings.status 와 같은 이유로 DB CHECK 를 쓰지 않아 채널 추가가 쉽다).
const (
	KindDingTalk = "dingtalk" // 딩톡 사용자 정의 봇
	KindFeishu   = "feishu"   // 페이슈(Lark 포함) 사용자 정의 봇
	KindWeCom    = "wecom"    // 위챗 워크 그룹 봇
	KindWebhook  = "webhook"  // 일반 Webhook: 사용자 정의 메서드/헤더/JSON 템플릿
	KindTelegram = "telegram" // Telegram Bot API
	KindEmail    = "email"    // SMTP 메일
)

// 이벤트 유형이며 notification_events.kind 에 대응한다.
const (
	EventFindingCreated       = "finding_created"
	EventFindingStatusChanged = "finding_status_changed"
)

// InitKind 는 config 에 kind 가 비어 있을 때의 대비값이다.
const InitKind = KindDingTalk

// severityRank 는 취약점 등급을 비교 가능한 서수로 매핑한다. 알 수 없는 등급은 0 을 반환하므로,
// 어떤 min_severity 설정이든 알 수 없는 등급을 막는다——의심스러우면 보내지 않아 오탐 도배를 피한다.
var severityRank = map[string]int{
	"low":      1,
	"medium":   2,
	"high":     3,
	"critical": 4,
}

// SeverityRank 는 등급의 서수를 반환한다. 알 수 없는 등급은 0 을 반환한다.
func SeverityRank(severity string) int { return severityRank[severity] }

// SeverityLabel 은 emoji 가 붙은 표시용 등급 이름을 반환하며, 메시지 제목과 카드 색상에 쓴다.
// 알 수 없는 등급은 그대로 되돌려 보이며 지어내지 않는다.
func SeverityLabel(severity string) string {
	switch severity {
	case "critical":
		return "🔴 심각"
	case "high":
		return "🟠 높음"
	case "medium":
		return "🟡 중간"
	case "low":
		return "🔵 낮음"
	default:
		return severity
	}
}

// StatusLabel 은 처리 상태를 표시용 이름으로 바꾸며, 상태 변경 메시지에 쓴다.
func StatusLabel(status string) string {
	switch status {
	case "pending":
		return "처리 대기"
	case "in_progress":
		return "처리 중"
	case "confirmed":
		return "확인됨"
	case "resolved":
		return "처리됨"
	case "fixed":
		return "수정됨"
	case "false_positive":
		return "오탐"
	case "ignored":
		return "무시"
	case "duplicate":
		return "중복"
	case "risk_accepted":
		return "위험 수용"
	default:
		return status
	}
}

// AtLeast 는 severity 가 min 기준에 도달했는지 판단한다. min 이 비어 있으면 기준이 없으므로 모두 통과한다.
// 알 수 없는 severity 의 서수는 0 이므로 비어 있지 않은 어떤 min 에도 거부된다는 점에 주의한다(severityRank 주석 참고).
func AtLeast(severity, min string) bool {
	if min == "" {
		return true
	}
	return SeverityRank(severity) >= SeverityRank(min)
}
