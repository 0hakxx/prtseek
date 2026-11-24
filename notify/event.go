package notify

// Snapshot 은 notification_events.snapshot 이라는 JSONB 열의 계약이다. 쓰는 쪽은 db 계층의
// 취약점 저장 트랜잭션이고, 읽는 쪽은 server 계층의 전달 엔진과 필터 매칭이다. 정의를 이 패키지에 두는 것은 이것이
// 「알림 도메인」의 페이로드이기 때문이다: db 는 직렬화만 하고 필드의 의미는 모른다.
//
// 렌더링할 때 다시 조회하지 않고 취약점 필드를 중복 저장하는 이유: 취약점은 나중에 이름, 등급, 상태가 바뀔 수 있는데,
// 푸시 내용은 **사건 발생 당시**의 결론을 반영해야 한다——다시 조회하면 「나중에 low 로 바뀐」
// 위험한 오해를 낳는다. 또한 덕분에 fan-out 과 렌더링에서 findings/tasks/assets 세 테이블을 JOIN 할 필요가 없다.
type Snapshot struct {
	// 이벤트 유형: finding_created / finding_status_changed
	Kind      string  `json:"kind"`
	FindingID int64   `json:"finding_id"`
	TaskID    int64   `json:"task_id"`
	VulnClass string  `json:"vulnclass"`
	Name      string  `json:"name"`
	Severity  string  `json:"severity"`
	Summary   string  `json:"summary"`
	AssetIDs  []int64 `json:"asset_ids"`
	// kind=finding_status_changed 일 때만 비어 있지 않음.
	FromStatus string `json:"from_status,omitempty"`
	ToStatus   string `json:"to_status,omitempty"`
}

// Item 은 푸시할 취약점 하나이며 채널 렌더링에 쓴다.
type Item struct {
	FindingID int64
	Name      string
	VulnClass string
	Severity  string
	Summary   string
	// Assets 는 해석된 자산 표시 이름(예: 도메인/IP)이다. server 계층이 채운다——
	// 이 패키지는 DB 를 건드리지 않으므로 이름을 얻을 수 없다.
	Assets []string
	// DetailURL 은 취약점 상세 링크다. 비어 있으면 public_base_url 이 설정되지 않았다는 뜻이며 렌더링할 때 생략한다.
	DetailURL string
	// 상태 변경 이벤트 전용. 두 값이 모두 비어 있지 않으면 「처리 대기 → 수정됨」 으로 렌더링한다.
	FromStatus string
	ToStatus   string
}

// IsStatusChange 는 이 항목이 상태 변경 이벤트인지 보고한다.
func (i Item) IsStatusChange() bool { return i.FromStatus != "" || i.ToStatus != "" }

// Title 은 항목의 표시 제목을 반환한다: 사람이 붙인 name 을 우선하고, 없으면 취약점 유형 vulnclass 를 쓰며,
// 둘 다 비어 있으면 자리표시자를 쓴다——빈 제목은 절대 내보내지 않는다.
func (i Item) Title() string {
	if i.Name != "" {
		return i.Name
	}
	if i.VulnClass != "" {
		return i.VulnClass
	}
	return "(이름 없는 취약점)"
}

// Message 는 채널 전송 한 번의 전체 내용이다.
type Message struct {
	// 단건 푸시일 때 길이는 1, 요약 푸시(digest)일 때는 배치 전체다.
	// 빈 슬라이스는 허용되지 않으므로 호출자는 최소 한 건을 보장해야 한다.
	Items []Item
	// Batch=true 이면 요약 메시지로 렌더링한다(제목을 바꾸고 시간 구간과 건수를 붙임).
	Batch bool
	// WindowMinutes 는 요약 주기(분)이며, Batch=true 일 때만 문구 「최근 N 분」에 쓴다.
	// 렌더링 시 time.Since 로 계산하지 않고 설정에서 명시적으로 넘기는 것은 의도적이다: 렌더링이 결정적이어야 테스트하기 쉽다.
	WindowMinutes int
	// HomeURL 은 플랫폼 대시보드 주소(전역 public_base_url)다. 비어 있으면 대시보드 링크를 넣지 않는다.
	HomeURL string
}
