package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/notify"
)

// 이 파일은 푸시 기능의 종단 간 동작을 다룬다: 취약점 저장 → 이벤트 → 분배 → 실제 HTTP 전송.
//
// 보안상 주의점: 이 테스트들은 **전역 Notifier.step() 을 호출하지 않고**, 자기가 만든
// 채널에만 stepRealtime/stepDigest 를 호출한다. 이유는 step() 이 DB 의 모든 활성화된 채널을 순회하기 때문——
// 실제 딩톡/위챗 워크 봇이 설정된 개발 DB 에서 테스트를 돌리면, 전역 step 이 테스트 중
// 생성된 취약점을 그 대화방에 실제로 푸시한다. 채널별 호출은 영향 범위를 테스트가 만든 가짜 수신자에만 엄격히 제한한다.
//
// 정리: 테스트 끝날 때 이 테스트가 생성한 이벤트(전달을 연쇄 삭제)와 채널을 삭제해, 실제 채널에 적체를 남기지 않는다.
//
// 단언 기준: stepRealtime/stepDigest 는 반환값이 없고 내부적으로 로그를 남기므로, 여기서 단언하는 것은
// **관찰 가능한 외부 동작**(가짜 수신자가 무엇을 받았는지, 전달 행이 어떤 상태로 떨어졌는지)이며 함수의
// 반환값이 아니다——이것이 반환값에 stub 을 다는 것보다 실제 호출 경로에 더 가깝다.

// notifyFixture 는 이 파일 테스트의 공용 장치다.
type notifyFixture struct {
	s       *Server
	pg      *db.DB
	request func(method, path, body string) *httptest.ResponseRecorder
	n       *Notifier
	// 자체 구축한 task/exploration: 테스트는 여기에 취약점을 기록하며, 다른 테스트의 데이터와 격리한다.
	taskID int64
	expID  int64
	// cleanupMark 이후 생성된 이벤트는 정리 시 함께 삭제된다.
	cleanupMark int64
}

func newNotifyFixture(t *testing.T) *notifyFixture {
	t.Helper()
	// 이 파일의 모든 가짜 수신자는 127.0.0.1 에서 돌며, 전달은 기본적으로 루프백 주소를 거부한다
	// (동일 머신 서비스와 클라우드 메타데이터로의 SSRF 방지). 테스트는 이 스위치를 명시적으로 켠다.
	// 가드 「기본 거부」 동작은 notify 패키지의 ssrf_test.go 가 다룬다.
	t.Setenv(notify.AllowLocalTargetsEnv, "1")
	s, _, request := trafficEvidenceServer(t)
	pg := s.m.pg

	// 자체 task 구축: 공유 장치 trafficEvidenceServer 가 만든 task 는 exploration id 를 얻을 수 없고,
	// 취약점을 기록하려면 그것을 반드시 제공해야 한다.
	task, err := s.m.CreateTask("알림 푸시 테스트", "푸시 동작 검증", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := strconv.ParseInt(task.ID, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pg.Exec(`DELETE FROM tasks WHERE id=$1`, taskID) })

	var mark int64
	if err := pg.QueryRow(`SELECT COALESCE(max(id),0) FROM notification_events`).Scan(&mark); err != nil {
		t.Fatal(err)
	}
	// 장치를 자체 폐루프로 만든다: fixture 생성 전부터 있던 이벤트를 한꺼번에 분배됨으로 표시한다.
	//
	// 왜 반드시 해야 하는가: FanOutPendingEvents 는 **전역**이라, DB 의 미분배 이벤트 전체를
	// 모든 매칭 채널로 펼친다. 공유 장치 trafficEvidenceServer 자체도 취약점을 하나 기록하고
	// (바로 그것이 반환하는 초기 finding), 다른 테스트에도 잔여가 있을 수 있다. 격리하지 않으면,
	// 이 잡다한 이벤트들이 이 테스트의 채널로 분배되어 「N 건의 전달이 있어야 함」 같은 단언이
	// 때로 되고 때로 안 되며——실패 방식이 테스트 실행 순서에 따라 달라 바로 실패하는 것보다 조사하기 더 어렵다.
	if _, err := pg.Exec(`UPDATE notification_events SET fanned_out = true WHERE id <= $1 AND NOT fanned_out`, mark); err != nil {
		t.Fatal(err)
	}

	f := &notifyFixture{s: s, pg: pg, request: request, n: newNotifier(s), taskID: taskID, expID: task.ExpID, cleanupMark: mark}
	t.Cleanup(func() {
		if _, err := pg.Exec(`DELETE FROM notification_events WHERE id > $1`, f.cleanupMark); err != nil {
			t.Logf("알림 이벤트 정리 실패: %v", err)
		}
	})
	// 총 스위치는 켜져 있어야 한다(다른 테스트가 끈 적이 있을 수 있음).
	if err := pg.SetBool(settingNotifyEnabled, true); err != nil {
		t.Fatal(err)
	}
	return f
}

// record 는 실제 증거 쓰기 경로를 거쳐 취약점 하나를 떨어뜨리고 finding id 를 반환한다.
// 이 경로는 **같은 트랜잭션**에서 푸시 이벤트를 등록한다——바로 이 기능의 훅 지점이다.
func (f *notifyFixture) record(t *testing.T, vulnclass, severity string) int64 {
	t.Helper()
	out, err := f.s.evidenceStore().Record(context.Background(), db.RecordFindingInput{
		TaskID:        f.taskID,
		ExplorationID: f.expID,
		Worker:        "test",
		VulnClass:     vulnclass,
		Name:          vulnclass,
		Severity:      severity,
		Summary:       vulnclass + " 의 요약",
		Evidence:      "poc",
	}, nil)
	if err != nil {
		t.Fatalf("취약점 기록 실패: %v", err)
	}
	return out.FindingID
}

// channel 은 채널 설정을 다시 읽는다(채널별 stepX 호출에 쓴다).
func (f *notifyFixture) channel(t *testing.T, id int64) *db.NotificationChannel {
	t.Helper()
	ch, err := f.pg.NotificationChannelByID(context.Background(), id)
	if err != nil {
		t.Fatalf("채널 읽기 실패: %v", err)
	}
	return ch
}

// deliver 는 이벤트를 분배하고 지정한 채널에 대해서만 한 라운드 전달한다.
func (f *notifyFixture) deliver(t *testing.T, chID int64, baseURL string) {
	t.Helper()
	ctx := context.Background()
	if _, _, err := f.pg.FanOutPendingEvents(ctx, 500); err != nil {
		t.Fatalf("분배 실패: %v", err)
	}
	f.n.stepRealtime(ctx, f.channel(t, chID), 50, baseURL)
}

// createChannel 은 HTTP 인터페이스를 통해 채널을 만들어, 인터페이스 자체의 검증 경로도 덤으로 다룬다.
func (f *notifyFixture) createChannel(t *testing.T, payload map[string]any) int64 {
	t.Helper()
	raw, _ := json.Marshal(payload)
	r := f.request("POST", "/api/notify/channels", string(raw))
	if r.Code != 200 {
		t.Fatalf("채널 생성 실패 %d: %s", r.Code, r.Body)
	}
	var res struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &res); err != nil || res.ID == 0 {
		t.Fatalf("채널 생성 응답 비정상: %s (%v)", r.Body, err)
	}
	t.Cleanup(func() { f.pg.Exec(`DELETE FROM notification_channels WHERE id=$1`, res.ID) })
	return res.ID
}

// fakeWebhook 은 받은 요청 본문을 기록하는 가짜 수신자다.
type fakeWebhook struct {
	*httptest.Server
	mu     sync.Mutex
	bodies []map[string]any
}

func newFakeWebhook(t *testing.T) *fakeWebhook {
	t.Helper()
	f := &fakeWebhook{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		f.mu.Lock()
		f.bodies = append(f.bodies, body)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeWebhook) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

func (f *fakeWebhook) body(t *testing.T, i int) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.bodies) {
		t.Fatalf("가짜 수신자가 %d 건의 요청만 받아, %d 번째를 가져올 수 없음", len(f.bodies), i)
	}
	return f.bodies[i]
}

func (f *fakeWebhook) last(t *testing.T) map[string]any {
	t.Helper()
	if f.count() == 0 {
		t.Fatal("가짜 수신자가 어떤 요청도 받지 못함")
	}
	return f.body(t, f.count()-1)
}

// markdownText 는 요청 본문에서 본문을 꺼내며, 각 플랫폼의 필드명 차이를 맞춘다:
// 딩톡 markdown 은 `text`, ActionCard 은 `text`, 위챗 워크 markdown 은 `content`.
func markdownText(t *testing.T, body map[string]any) string {
	t.Helper()
	for _, key := range []string{"markdown", "actionCard"} {
		section, ok := body[key].(map[string]any)
		if !ok {
			continue
		}
		for _, field := range []string{"text", "content"} {
			if s, ok := section[field].(string); ok && s != "" {
				return s
			}
		}
	}
	t.Fatalf("요청 본문에 식별 가능한 본문이 없음: %v", body)
	return ""
}

// agePendingBatch 는 이 채널의 발송 대기 전달을 노화시켜 요약 배치 기한을 테스트한다.
func (f *notifyFixture) agePendingBatch(t *testing.T, chID int64) {
	t.Helper()
	if _, err := f.pg.Exec(`UPDATE notification_deliveries SET created_at = datetime('now','-2 hours')
WHERE channel_id=$1 AND state=$2`, chID, db.NotifyStatePending); err != nil {
		t.Fatal(err)
	}
}

func TestNotifyEndToEndRealtimeDelivery(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "실시간 푸시",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	f.record(t, "SQL 주입", "high")
	f.deliver(t, chID, "")

	if hook.count() != 1 {
		t.Fatalf("1 건의 메시지가 나가야 하는데 실제 %d", hook.count())
	}
	text := markdownText(t, hook.last(t))
	for _, want := range []string{"SQL 주입", "높음", "요약"} {
		if !strings.Contains(text, want) {
			t.Fatalf("메시지 본문에 %q 가 없음:\n%s", want, text)
		}
	}
	// 전달은 sent 로 흘러가야 한다.
	var pending int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1 AND state <> $2`,
		chID, db.NotifyStateSent).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Fatalf("전달 후에도 %d 건이 sent 로 표시되지 않음", pending)
	}
}

func TestNotifyChannelAPIMasksSecretsAndPreservesOnUpdate(t *testing.T) {
	f := newNotifyFixture(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "마스킹 테스트",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": "https://oapi.dingtalk.com/robot/send?access_token=abc123456", "secret": "SECabcdef123456"},
	})

	r := f.request("GET", "/api/notify/channels", "")
	if r.Code != 200 {
		t.Fatalf("채널 목록 조회 실패 %d: %s", r.Code, r.Body)
	}
	if strings.Contains(r.Body.String(), "abc123456") || strings.Contains(r.Body.String(), "SECabcdef123456") {
		t.Fatalf("인터페이스 에코가 자격 증명을 유출함: %s", r.Body)
	}
	var listed struct {
		Channels []struct {
			ID         int64          `json:"id"`
			Config     map[string]any `json:"config"`
			SecretKeys []string       `json:"secret_keys"`
		} `json:"channels"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	var mine *struct {
		ID         int64          `json:"id"`
		Config     map[string]any `json:"config"`
		SecretKeys []string       `json:"secret_keys"`
	}
	for i := range listed.Channels {
		if listed.Channels[i].ID == chID {
			mine = &listed.Channels[i]
		}
	}
	if mine == nil {
		t.Fatal("새로 만든 채널이 목록에 나타나지 않음")
	}
	if !notify.IsMasked(fmt.Sprint(mine.Config["webhook"])) || !notify.IsMasked(fmt.Sprint(mine.Config["secret"])) {
		t.Fatalf("자격 증명 필드는 마스킹 값이어야 함: %v", mine.Config)
	}
	if len(mine.SecretKeys) == 0 {
		t.Fatal("인터페이스는 프런트엔드에 어떤 필드가 자격 증명인지 알려야 함")
	}

	// PATCH 로 이름만 바꾸고 마스킹 자격 증명을 되돌려 보냄: 실제 자격 증명은 반드시 원래대로 유지되어야 함.
	body, _ := json.Marshal(map[string]any{
		"name":   "이름 변경 후",
		"config": map[string]any{"webhook": fmt.Sprint(mine.Config["webhook"]), "secret": fmt.Sprint(mine.Config["secret"])},
	})
	if r := f.request("PATCH", fmt.Sprintf("/api/notify/channels/%d", chID), string(body)); r.Code != 200 {
		t.Fatalf("갱신 실패 %d: %s", r.Code, r.Body)
	}
	cfg := f.channelConfig(t, chID)
	if cfg["webhook"] != "https://oapi.dingtalk.com/robot/send?access_token=abc123456" {
		t.Fatalf("마스킹 되돌림이 실제 자격 증명을 덮어씀: %v", cfg["webhook"])
	}
	if cfg["secret"] != "SECabcdef123456" {
		t.Fatalf("마스킹 되돌림이 secret 을 덮어씀: %v", cfg["secret"])
	}
	if f.channel(t, chID).Name != "이름 변경 후" {
		t.Fatal("이름이 갱신되지 않음")
	}

	// 명시적으로 secret 을 비우면 적용되어야 함(「마스킹 되돌림=유지」와 구분).
	body, _ = json.Marshal(map[string]any{"config": map[string]any{"secret": ""}})
	if r := f.request("PATCH", fmt.Sprintf("/api/notify/channels/%d", chID), string(body)); r.Code != 200 {
		t.Fatalf("secret 비우기 실패 %d: %s", r.Code, r.Body)
	}
	if _, still := f.channelConfig(t, chID)["secret"]; still {
		t.Fatal("빈 문자열은 secret 을 비워야 함")
	}
}

func (f *notifyFixture) channelConfig(t *testing.T, id int64) map[string]any {
	t.Helper()
	var cfg map[string]any
	if err := json.Unmarshal(f.channel(t, id).Config, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestNotifyChannelAPICreateValidation(t *testing.T) {
	f := newNotifyFixture(t)
	cases := []struct {
		name    string
		payload map[string]any
		wantSub string
	}{
		{"유형 무효", map[string]any{"name": "x", "kind": "nope", "config": map[string]any{}}, "채널 유형이 유효하지 않습니다"},
		{"이름 없음", map[string]any{"kind": notify.KindDingTalk, "config": map[string]any{"webhook": "https://e.com/h"}}, "채널 이름이 없습니다"},
		{"webhook 없음", map[string]any{"name": "x", "kind": notify.KindDingTalk, "config": map[string]any{}}, "Webhook"},
		{"webhook 프로토콜 무효", map[string]any{"name": "x", "kind": notify.KindDingTalk, "config": map[string]any{"webhook": "file:///etc/passwd"}}, "Webhook 주소가 유효하지 않습니다"},
		{"모드 무효", map[string]any{"name": "x", "kind": notify.KindDingTalk, "mode": "sometimes", "config": map[string]any{"webhook": "https://e.com/h"}}, "푸시 모드가 유효하지 않습니다"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(tc.payload)
			r := f.request("POST", "/api/notify/channels", string(raw))
			if r.Code != 400 {
				t.Fatalf("400 을 반환해야 하는데 %d: %s", r.Code, r.Body)
			}
			if !strings.Contains(r.Body.String(), tc.wantSub) {
				t.Fatalf("오류 메시지에 %q 가 있어야 하는데 %s", tc.wantSub, r.Body)
			}
		})
	}
	if r := f.request("DELETE", "/api/notify/channels/99999999", ""); r.Code != 404 {
		t.Fatalf("없는 채널 삭제는 404 여야 하는데 %d", r.Code)
	}
}

func TestNotifyFilterBlocksBelowThreshold(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "심각만",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
		"filter": map[string]any{"min_severity": "critical"},
	})
	f.record(t, "낮은 위험 문제", "low")
	if _, _, err := f.pg.FanOutPendingEvents(context.Background(), 500); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1`, chID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("임계값 미만 취약점은 전달이 생기면 안 되는데 %d 건", n)
	}
	f.n.stepRealtime(context.Background(), f.channel(t, chID), 50, "")
	if hook.count() != 0 {
		t.Fatal("필터링된 취약점은 메시지를 내보내면 안 됨")
	}
}

func TestNotifyDigestBatchesMultipleFindingsIntoOneMessage(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "요약 푸시",
		"kind":   notify.KindDingTalk,
		"mode":   db.NotifyModeDigest,
		"config": map[string]any{"webhook": hook.URL},
	})
	for i := 0; i < 3; i++ {
		f.record(t, fmt.Sprintf("요약 취약점%d", i+1), "high")
	}
	ctx := context.Background()
	if _, _, err := f.pg.FanOutPendingEvents(ctx, 500); err != nil {
		t.Fatal(err)
	}
	ch := f.channel(t, chID)

	// 기한 도달 전: 보내지 않음.
	f.n.stepDigest(ctx, ch, 50, "")
	if hook.count() != 0 {
		t.Fatal("요약 배치가 기한 전인데 보냄")
	}

	// 배치를 노화시킨 뒤: 세 건이 한 메시지로 합쳐짐.
	f.agePendingBatch(t, chID)
	f.n.stepDigest(ctx, ch, 50, "")
	if got := hook.count(); got != 1 {
		t.Fatalf("세 건이 한 메시지로 요약되어야 하는데 실제 %d 건 보냄", got)
	}
	text := markdownText(t, hook.last(t))
	if !strings.Contains(text, "최근") || !strings.Contains(text, "3 건") {
		t.Fatalf("요약 메시지에 건수/시간 구간 문구가 없음:\n%s", text)
	}
	for i := 1; i <= 3; i++ {
		if !strings.Contains(text, fmt.Sprintf("요약 취약점%d", i)) {
			t.Fatalf("요약 메시지에 %d 번째 건이 없음:\n%s", i, text)
		}
	}
	// 같은 배치는 batch_id 를 공유해야 함.
	var distinct, total int
	if err := f.pg.QueryRow(`SELECT count(DISTINCT batch_id), count(*) FROM notification_deliveries WHERE channel_id=$1`, chID).Scan(&distinct, &total); err != nil {
		t.Fatal(err)
	}
	if total != 3 || distinct != 1 {
		t.Fatalf("세 전달이 하나의 batch_id 를 공유해야 하는데 distinct=%d total=%d", distinct, total)
	}
}

func TestNotifyDisabledChannelDoesNotSend(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":    "채널 비활성화",
		"kind":    notify.KindDingTalk,
		"enabled": false,
		"config":  map[string]any{"webhook": hook.URL},
	})
	f.record(t, "비활성화 기간의 취약점", "critical")
	if _, _, err := f.pg.FanOutPendingEvents(context.Background(), 500); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1`, chID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("비활성화된 채널은 전달이 생기면 안 되는데 %d 건", n)
	}
}

func TestNotifyStatusChangeDelivery(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "상태 변경 구독",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
		"filter": map[string]any{"on_status_change": true},
	})
	finding := f.record(t, "상태 변경 사례", "high")
	r := f.request("PATCH", fmt.Sprintf("/api/exploration/findings/%d", finding), `{"status":"fixed"}`)
	if r.Code != 200 {
		t.Fatalf("상태 변경 실패 %d: %s", r.Code, r.Body)
	}
	f.deliver(t, chID, "")

	// 두 건이 있어야 함: fixed 그 건이 상태 변경이고, finding_created 그 건도 같은 라운드에 보낼 수 있다.
	// 상태 변경이 실제로 더 늦게 생성되지만 순서에 의존하지 않고 전부 찾는다.
	found := false
	for i := 0; i < hook.count(); i++ {
		text := markdownText(t, hook.body(t, i))
		if strings.Contains(text, "상태 변경") && strings.Contains(text, "수정됨") {
			found = true
		}
	}
	if !found {
		t.Fatalf("「상태 변경 → 수정됨」이 든 메시지를 받지 못함(총 %d 건)", hook.count())
	}
}

func TestNotifyStatusChangeSuppressedByDefault(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "상태 변경 구독 안 함",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	finding := f.record(t, "변경 구독 안 함", "high")
	if r := f.request("PATCH", fmt.Sprintf("/api/exploration/findings/%d", finding), `{"status":"false_positive"}`); r.Code != 200 {
		t.Fatalf("상태 변경 실패 %d: %s", r.Code, r.Body)
	}
	if _, _, err := f.pg.FanOutPendingEvents(context.Background(), 500); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries d
JOIN notification_events e ON e.id = d.event_id
WHERE d.channel_id=$1 AND e.kind=$2`, chID, notify.EventFindingStatusChanged).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("상태 변경을 구독하지 않은 채널은 상태 변경 전달을 받지 않아야 하는데 %d 건", n)
	}
}

func TestNotifyTestMessageEndpoint(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "테스트 전송",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	if r := f.request("POST", fmt.Sprintf("/api/notify/channels/%d/test", chID), ""); r.Code != 200 {
		t.Fatalf("테스트 전송 실패 %d: %s", r.Code, r.Body)
	}
	if hook.count() != 1 {
		t.Fatalf("가짜 수신자는 1 건의 테스트 메시지를 받아야 하는데 %d 건", hook.count())
	}
	// 테스트 메시지는 한눈에 테스트임을 알 수 있어야 하며, 실제 취약점으로 오인되면 안 된다.
	if text := markdownText(t, hook.last(t)); !strings.Contains(text, "테스트") {
		t.Fatalf("테스트 메시지는 테스트임을 표시해야 함: %s", text)
	}
	// 설정이 망가지면 채널의 원본 오류를 사용자에게 사실대로 되돌려 주어야 함.
	badID := f.createChannel(t, map[string]any{
		"name":   "잘못된 주소",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": "http://127.0.0.1:1/hook"},
	})
	if r := f.request("POST", fmt.Sprintf("/api/notify/channels/%d/test", badID), ""); r.Code != 502 {
		t.Fatalf("전달 실패 시 502 를 반환해야 하는데 %d: %s", r.Code, r.Body)
	}
}

func TestNotifyDeliveriesHistoryAndRetry(t *testing.T) {
	f := newNotifyFixture(t)
	// 반드시 실패하는 주소를 가리켜 failed 전달을 만든다.
	chID := f.createChannel(t, map[string]any{
		"name":   "실패 재시도",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": "http://127.0.0.1:1/hook"},
	})
	f.record(t, "실패할 푸시", "high")
	ctx := context.Background()
	if _, _, err := f.pg.FanOutPendingEvents(ctx, 500); err != nil {
		t.Fatal(err)
	}
	ch := f.channel(t, chID)
	// 재시도 예산을 다 쓸 때까지 연속 전달.
	for i := 0; i < db.MaxNotifyAttempts; i++ {
		f.n.stepRealtime(ctx, ch, 50, "")
		if _, err := f.pg.Exec(`UPDATE notification_deliveries SET next_attempt_at = datetime('now','-1 minute') WHERE channel_id=$1`, chID); err != nil {
			t.Fatal(err)
		}
	}
	var state string
	if err := f.pg.QueryRow(`SELECT state FROM notification_deliveries WHERE channel_id=$1`, chID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != db.NotifyStateFailed {
		t.Fatalf("재시도 소진 뒤에는 failed 여야 하는데 %s", state)
	}

	r := f.request("GET", fmt.Sprintf("/api/notify/deliveries?channel_id=%d&state=failed", chID), "")
	if r.Code != 200 {
		t.Fatalf("이력 조회 실패 %d: %s", r.Code, r.Body)
	}
	var hist struct {
		Deliveries []struct {
			ID        int64  `json:"id"`
			State     string `json:"state"`
			LastError string `json:"last_error"`
			Attempts  int    `json:"attempts"`
			Title     string `json:"title"`
		} `json:"deliveries"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &hist); err != nil {
		t.Fatal(err)
	}
	if hist.Total != 1 || len(hist.Deliveries) != 1 {
		t.Fatalf("1 건의 실패 전달이 조회되어야 하는데 total=%d len=%d", hist.Total, len(hist.Deliveries))
	}
	if hist.Deliveries[0].LastError == "" {
		t.Fatal("이력에 실패 원인을 실어야 함. 그러지 않으면 사용자가 조사할 수 없다")
	}
	if hist.Deliveries[0].Attempts < db.MaxNotifyAttempts {
		t.Fatalf("시도 횟수가 기록되어야 하는데 %d", hist.Deliveries[0].Attempts)
	}
	if hist.Deliveries[0].Title != "실패할 푸시" {
		t.Fatalf("이력에 취약점 제목이 나와야 하는데 %q", hist.Deliveries[0].Title)
	}

	// 수동 재전송: pending 으로 돌아가고 카운트가 0 이 되어야 함.
	if r := f.request("POST", fmt.Sprintf("/api/notify/deliveries/%d/retry", hist.Deliveries[0].ID), ""); r.Code != 200 {
		t.Fatalf("재전송 실패 %d: %s", r.Code, r.Body)
	}
	var attempts int
	if err := f.pg.QueryRow(`SELECT state, attempts FROM notification_deliveries WHERE id=$1`, hist.Deliveries[0].ID).Scan(&state, &attempts); err != nil {
		t.Fatal(err)
	}
	if state != db.NotifyStatePending || attempts != 0 {
		t.Fatalf("재전송 뒤에는 pending 이고 attempts=0 이어야 하는데 %s/%d", state, attempts)
	}
}

func TestNotifyMetaAndSettingsRoundTrip(t *testing.T) {
	f := newNotifyFixture(t)
	r := f.request("GET", "/api/notify/meta", "")
	if r.Code != 200 {
		t.Fatalf("meta 실패: %s", r.Body)
	}
	var meta struct {
		Kinds []struct {
			Kind       string   `json:"kind"`
			SecretKeys []string `json:"secret_keys"`
		} `json:"kinds"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &meta); err != nil {
		t.Fatal(err)
	}
	if len(meta.Kinds) != len(notify.Kinds()) {
		t.Fatalf("meta 는 모든 %d 개 채널을 나열해야 하는데 %d", len(notify.Kinds()), len(meta.Kinds))
	}
	for _, k := range meta.Kinds {
		if len(k.SecretKeys) == 0 {
			t.Errorf("채널 %s 가 자격 증명 필드를 보고하지 않음", k.Kind)
		}
	}

	// 세 전역 설정의 왕복. 꼬리 슬래시는 정규화되어야 함. 그러지 않으면 링크가 "//function/..." 로 붙는다.
	if r := f.request("PUT", "/api/settings", `{"notify_public_base_url":"https://artex.example.com/","notify_digest_interval_min":15,"notify_enabled":true}`); r.Code != 200 {
		t.Fatalf("설정 쓰기 실패 %d: %s", r.Code, r.Body)
	}
	t.Cleanup(func() {
		f.pg.Exec(`DELETE FROM settings WHERE key IN ($1,$2)`, settingNotifyPublicBaseURL, settingNotifyDigestMinutes)
	})
	payload := f.s.settingsPayload()
	if payload["notify_public_base_url"] != "https://artex.example.com" {
		t.Fatalf("링크 주소가 정규화되지 않음: %v", payload["notify_public_base_url"])
	}
	if payload["notify_digest_interval_min"] != 15 {
		t.Fatalf("요약 주기가 적용되지 않음: %v", payload["notify_digest_interval_min"])
	}

	// 잘못된 값은 거부되어야 함.
	for _, body := range []string{
		`{"notify_public_base_url":"ftp://x"}`,
		`{"notify_digest_interval_min":0}`,
		`{"notify_digest_interval_min":99999}`,
	} {
		if r := f.request("PUT", "/api/settings", body); r.Code != 400 {
			t.Errorf("%s 는 400 을 반환해야 하는데 %d", body, r.Code)
		}
	}
}

// TestNotifyDeepLinkUsesPublicBaseURL 은 링크 조립을 다룬다: public_base_url 이 설정되면
// 단건 메시지는 반드시 버튼이 있는 ActionCard 를 쓰고, 링크는 취약점 상세 페이지를 가리켜야 한다.
func TestNotifyDeepLinkUsesPublicBaseURL(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "링크",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	finding := f.record(t, "링크가 있는 취약점", "high")
	f.deliver(t, chID, "https://artex.example.com")

	body := hook.last(t)
	card, _ := body["actionCard"].(map[string]any)
	if card == nil {
		t.Fatalf("링크가 있으면 ActionCard 를 써야 하는데 msgtype=%v", body["msgtype"])
	}
	want := fmt.Sprintf("https://artex.example.com/function/findings/detail?id=%d", finding)
	if card["singleURL"] != want {
		t.Fatalf("링크 불일치\n기대 %s\n얻음 %v", want, card["singleURL"])
	}
}

// TestNotifyNoDeepLinkWithoutBaseURL 는 역방향 테스트다: 외부 주소를 설정하지 않으면
// 잘못된 링크(예: localhost 나 상대 경로)를 만들면 안 되고, 순수 markdown 으로 돌아가야 한다.
func TestNotifyNoDeepLinkWithoutBaseURL(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "링크 없음",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	f.record(t, "링크 없는 취약점", "high")
	f.deliver(t, chID, "")

	body := hook.last(t)
	if body["msgtype"] != "markdown" {
		t.Fatalf("외부 주소 미설정 시 markdown 으로 보내야 하는데 %v", body["msgtype"])
	}
	if text := markdownText(t, body); strings.Contains(text, "상세 보기") {
		t.Fatalf("외부 주소 미설정 시 상세 링크가 나타나면 안 됨:\n%s", text)
	}
}

// TestNotifyDigestSegmentsAndDefersRemainder 는 「조용한 유실」 수정의 종단 간 증거다.
//
// 요약 메시지는 채널 길이 상한(위챗 워크 4096 바이트) 제약을 받으며, 한 배치가 안 맞으면 반드시 **통째로** 쪼개야 한다:
// 이 건에 담긴 것은 전달됨으로 표시하고, 나머지는 큐로 돌아가 다음 메시지를 기다린다. 예전 구현은 배치 전체를
// 성공으로 표시했다——잘려 나간 것들은 메시지에도 실패 목록에도 없고 전달 이력도 성공으로 표시되어,
// 취약점이 그렇게 사라졌다.
//
// 네 가지를 단언한다: ① 실제로 담긴 건수만 표시됨 ② 나머지는 여전히 발송 대기 ③ 미뤄진 항목은
// **재시도 횟수를 소비하지 않음** ④ 한 라운드를 더 돌리면 나머지도 보낼 수 있음(막히지 않음).
func TestNotifyDigestSegmentsAndDefersRemainder(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	// 위챗 워크를 쓴다: markdown 상한이 4096 바이트로, 여섯 채널 중 가장 빡빡하다.
	chID := f.createChannel(t, map[string]any{
		"name":   "분할 요약",
		"kind":   notify.KindWeCom,
		"mode":   db.NotifyModeDigest,
		"config": map[string]any{"webhook": hook.URL},
	})
	const total = 60
	// 제목을 좀 길게 해, 60 건이 4096 바이트를 훨씬 넘어 반드시 분할되게 한다.
	longName := strings.Repeat("초장문 취약점 이름", 6)
	for i := 0; i < total; i++ {
		f.record(t, longName+strconv.Itoa(i+1), "high")
	}
	ctx := context.Background()
	if _, _, err := f.pg.FanOutPendingEvents(ctx, 500); err != nil {
		t.Fatal(err)
	}
	f.agePendingBatch(t, chID)
	ch := f.channel(t, chID)

	f.n.stepDigest(ctx, ch, 50, "")
	if hook.count() != 1 {
		t.Fatalf("1 건의 메시지만 나가야 하는데 %d 얻음", hook.count())
	}

	var sent, pending int
	if err := f.pg.QueryRow(`SELECT
    count(*) FILTER (WHERE state=$2),
    count(*) FILTER (WHERE state=$3)
  FROM notification_deliveries WHERE channel_id=$1`, chID, db.NotifyStateSent, db.NotifyStatePending).
		Scan(&sent, &pending); err != nil {
		t.Fatal(err)
	}
	if sent == 0 {
		t.Fatal("일부 항목이 전달됨으로 표시되어야 함")
	}
	if pending == 0 {
		t.Fatalf("한 배치 %d 건은 4096 바이트에 전부 담길 수 없으므로 발송 대기가 남아야 함. sent=%d", total, sent)
	}
	if sent+pending != total {
		t.Fatalf("항목 수가 맞지 않음: sent=%d pending=%d total=%d (전달도 대기도 아님 = 유실)", sent, pending, total)
	}
	// 메시지 본문은 이 건에 포함되지 않은 것이 몇 건인지 사실대로 알려야 한다.
	if text := markdownText(t, hook.last(t)); !strings.Contains(text, "나머지") {
		t.Fatalf("메시지는 이 건에 포함되지 않은 항목이 있음을 알려야 함:\n%.400s", text)
	}

	// 미뤄진 항목은 재시도 예산을 소비하면 안 됨: 가져갈 때 attempts 가 낙관적으로 +1 되었으니 미룰 때는 되돌려야 한다.
	var maxAttempts int
	if err := f.pg.QueryRow(`SELECT COALESCE(max(attempts),0) FROM notification_deliveries
WHERE channel_id=$1 AND state=$2`, chID, db.NotifyStatePending).Scan(&maxAttempts); err != nil {
		t.Fatal(err)
	}
	if maxAttempts > 0 {
		t.Fatalf("미뤄진 항목은 재시도 횟수를 소비하면 안 됨(그러지 않으면 몇 건 뒤에 실패로 판정됨), attempts=%d 얻음", maxAttempts)
	}

	// 수렴할 때까지 반복해 돌린다. 단언은 **최종적으로 전부 전달됨**과 중간에 실제로 여러 라운드 돌았음이다——
	// 이것이 「두 번째 라운드에 다 보냄」보다 더 강하다: 분할이 막히지도 않고 나머지도 유실하지 않음을 증명한다.
	rounds := 0
	for {
		var undelivered int
		if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries
WHERE channel_id=$1 AND state <> $2 AND state <> $3`, chID, db.NotifyStateSent, db.NotifyStateFailed).
			Scan(&undelivered); err != nil {
			t.Fatal(err)
		}
		if undelivered == 0 {
			break
		}
		rounds++
		if rounds > total+5 {
			t.Fatalf("분할 전달이 수렴하지 않음: %d 라운드를 돌렸는데 %d 건이 아직 미결", rounds, undelivered)
		}
		before := hook.count()
		f.n.stepDigest(ctx, ch, 50, "")
		if hook.count() == before {
			t.Fatalf("%d 번째 라운드에 아무 진전이 없어 %d 건이 영원히 막힘", rounds, undelivered)
		}
	}
	if rounds < 2 {
		t.Fatalf("4096 바이트 메시지 한 건에 %d 건의 긴 제목 취약점이 안 담기므로 여러 라운드로 보내야 하는데 실제 %d 라운드만 씀", total, rounds)
	}
	// 첫 라운드 이후의 모든 라운드는 **순수 이어 보내기**여야 하며 채널이 거부한 항목이 있으면 안 된다.
	var failed int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1 AND state=$2`,
		chID, db.NotifyStateFailed).Scan(&failed); err != nil {
		t.Fatal(err)
	}
	if failed != 0 {
		t.Fatalf("가짜 수신자는 항상 성공을 반환하므로 실패 항목이 있으면 안 됨, %d 얻음", failed)
	}
}

// TestNotifyBackoffTableMatchesAttemptBudget 은 드리프트 방지 단언이다.
//
// 재시도 예산(db.MaxNotifyAttempts)과 백오프 시퀀스 표(notifyBackoff)는 패키지 두 곳에 나뉘어 있다:
// 전자는 상태 기계의 정책, 후자는 엔진의 실행 박자다. 둘 중 하나만 바꾸면——예컨대 예산을 5
// 로 올리고 백오프 단을 추가하는 것을 잊으면——코드는 오류를 내지 않고, 4, 5 번째 재시도가 마지막 단 간격을 이어 써,
// 「재시도 박자가 알 수 없게 느려진다」로 나타나며 조사할 때 여기까지 연관 짓기 어렵다.
// 둘의 길이가 같다고 단언해 이 드리프트가 CI 에서 드러나게 한다.
func TestNotifyBackoffTableMatchesAttemptBudget(t *testing.T) {
	if len(notifyBackoff) != db.MaxNotifyAttempts {
		t.Fatalf("백오프 단 수(%d)와 최대 시도 횟수(%d) 불일치——하나를 바꾸면 다른 하나도 반드시 같이 바꿔야 함",
			len(notifyBackoff), db.MaxNotifyAttempts)
	}
	// 백오프 간격은 단조 비감소여야 한다. 그러지 않으면 재시도가 갈수록 급해져 속도 제한을 오히려 악화시킨다.
	for i := 1; i < len(notifyBackoff); i++ {
		if notifyBackoff[i] < notifyBackoff[i-1] {
			t.Fatalf("백오프 간격은 단조 비감소여야 함: %d 번째 단 %v < %d 번째 단 %v",
				i, notifyBackoff[i], i-1, notifyBackoff[i-1])
		}
	}
}

// TestNotifyRateLimitDoesNotConsumeRetryBudget 은 「토큰 먼저, 가져가기 나중」 순서를 못 박는다.
// 반대로 하면(먼저 가져가고 포기), 속도 제한에 막힌 전달이 이미 attempts 를 한 번 셌으므로,
// 예산이 순수 대기로 소진되어 결국 failed 로 떨어진다.
func TestNotifyRateLimitDoesNotConsumeRetryBudget(t *testing.T) {
	// 토큰 버킷 자체만 테스트하며 Server 가 필요 없다(그것을 위해 하나 만들어서도 안 됨).
	n := &Notifier{buckets: map[int64]*notifyBucket{}}
	now := time.Now()
	// 분당 1 건: 버킷이 차면 최대 1 건.
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now); got != 1 {
		t.Fatalf("버킷이 찼을 때 분당 1 건은 1 개 토큰을 가져가야 하는데 %d", got)
	}
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now.Add(time.Millisecond)); got != 0 {
		t.Fatalf("토큰 소진 후에는 바로 0 을 반환해야 하는데 %d", got)
	}
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now.Add(30*time.Second)); got != 0 {
		t.Fatalf("반환될 동안에는 토큰 하나를 채우면 안 되는데 %d", got)
	}
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now.Add(time.Minute)); got != 1 {
		t.Fatalf("한 주기가 차면 1 개 토큰을 채워야 하는데 %d", got)
	}
	// 속도 제한 없는 채널은 유한한 상한을 쓰며, 단일 라운드가 무한 적체에 발목 잡히지 않게 한다.
	if got := n.takeTokens(2, 0, notifyUnlimitedBurstPerTick+10, now); got != notifyUnlimitedBurstPerTick {
		t.Fatalf("속도 제한 없음은 라운드당 상한 %d 를 반환해야 하는데 %d", notifyUnlimitedBurstPerTick, got)
	}
	// 채널 사이의 토큰 버킷은 서로 독립이다.
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now.Add(time.Millisecond)); got != 0 {
		t.Fatalf("채널 1 의 버킷은 여전히 비어 있어야 하는데 %d", got)
	}
}

// TestNotifyTakeTokensKeepsUnusedTokens 는 「want 개만 가져감」의 의미를 못 박는다.
//
// 예전 구현은 버킷을 통째로 비운 뒤에야 호출자가 잘랐으므로, rate=100/min 인 채널이 버킷을 가득 채우고
// 한 라운드에 5 건만 쓰면 남은 95 토큰이 그대로 버려졌다. 그 라운드에 채널에 발송 대기 전달이 없을 때도 마찬가지로 차감했다.
// 그 결과 주석이 주장한 「적체 시 한 번에 rate_per_min 건을 밀어낼 수 있다」가 어떤 경우에도 되지 않았다.
func TestNotifyTakeTokensKeepsUnusedTokens(t *testing.T) {
	n := &Notifier{buckets: map[int64]*notifyBucket{}}
	now := time.Now()
	// 버킷은 처음에 꽉 찬 상태(100), 이번 라운드에 5 개만 필요.
	if got := n.takeTokens(1, 100, 5, now); got != 5 {
		t.Fatalf("want=5 일 때 정확히 5 토큰을 가져가야 하는데 %d", got)
	}
	// 핵심 단언: 남은 95 는 반드시 버킷에 있어야 하며, 비워져 버려지면 안 된다.
	// 시간을 진행시키지 않아, 가져간 것이 보충이 아니라 재고에서만 올 수 있도록 보장.
	if got := n.takeTokens(1, 100, 95, now); got != 95 {
		t.Fatalf("남은 토큰은 여전히 가져갈 수 있어야 함(기대 95), 실제 %d 얻음——버킷이 라운드 전체에 걸쳐 비워짐", got)
	}
	if got := n.takeTokens(1, 100, 1, now); got != 0 {
		t.Fatalf("버킷이 다 떨어졌으면 0 을 반환해야 하는데 %d", got)
	}
	// want<=0 은 어떤 토큰도 차감하면 안 됨(빈 라운드는 과금 없음).
	n2 := &Notifier{buckets: map[int64]*notifyBucket{}}
	if got := n2.takeTokens(1, 20, 0, now); got != 0 {
		t.Fatalf("want=0 은 0 을 반환해야 하는데 %d", got)
	}
	if got := n2.takeTokens(1, 20, 20, now); got != 20 {
		t.Fatalf("want=0 번째는 토큰을 소비하면 안 되며 여전히 20 을 꽉 가져갈 수 있어야 하는데 %d", got)
	}
}

// TestDigestTickPlanDecouplesBatchSizeFromSendBudget 은 요약 모드의 두 차원을 못 박는다.
//
// 요약 배치 크기가 라운드당 요청 예산과 엮이면, rate_per_min=20 인 채널은 각 3 초 tick 마다 토큰 1 개만 보충되어,
// 각 요약 메시지에 취약점 1 건만 담긴다——기능상 요약이 없는 것과 같은데 메시지 머리에는 여전히
// 「최근 30 분 새 취약점 1 건」이라고 쓴다. 이 퇴화는 오류를 내지 않으며, 기존 종단 간 테스트도 알아채지 못한다
// (수동으로 stepDigest 에 충분히 큰 limit 를 넘겨, step 안의 할당량 계산을 우회한다). 그래서 여기서 결정 자체를 바로 단언한다.
// 한 배치 = 한 메시지 = 한 요청 = 한 토큰. 토큰의 단위는 메시지이지 취약점이 아니다.
func TestDigestTickPlanDecouplesBatchSizeFromSendBudget(t *testing.T) {
	tokens, claimLimit := digestTickPlan()
	// 一批 = 一条消息 = 一次请求 = 一个令牌。令牌的单位是消息，不是漏洞。
	if tokens != 1 {
		t.Fatalf("요약 한 배치가 한 메시지만 보내므로 정확히 1 토큰을 소비해야 하는데 %d", tokens)
	}
	if claimLimit != db.MaxDigestBatchSize {
		t.Fatalf("요약 배치 크기는 메모리 상한 db.MaxDigestBatchSize=%d 여야 하는데 %d",
			db.MaxDigestBatchSize, claimLimit)
	}
	// 핵심 관계: 배치 크기는 반드시 라운드당 요청 예산보다 훨씬 커야 한다. 둘이 같은 차원이면,
	// 「메시지 몇 건 보냄」과 「한 배치에 취약점 몇 건 담김」을 한 숫자로 섞은 것이다.
	if claimLimit <= notifyMaxSendsPerChannelPerTick {
		t.Fatalf("요약 배치 크기 %d 는 라운드당 요청 예산 %d 에 제약되면 안 됨——"+
			"요청 예산은 임대에서 역산한 「요청 몇 번 보냄」이며, 「한 배치에 취약점 몇 건 담김」과는 두 차원이다",
			claimLimit, notifyMaxSendsPerChannelPerTick)
	}
}

// TestNotifyTickBudgetFitsWithinLease 는 또 하나의 드리프트 방지 단언이다.
//
// 단일 채널 라운드당 전달 건수 상한(notifyMaxSendsPerChannelPerTick)은 임대 시간에서 역산한 것이다:
// 한 라운드에서 직렬 전달의 최악 소요가 반드시 임대 < 여야 하며, 그러지 않으면 뒤쪽 몇 건을 다 보내기 전에 임대가 만료되어
// 다중 인스턴스 배포에서 상대가 그것들을 다시 가져가 중복 전송한다. 이 세 상수는 서로 다른 곳에 있고,
// 어느 하나를 바꿔도 아무 오류 없이 관계가 깨질 수 있다——그래서 여기서 못 박는다.
func TestNotifyTickBudgetFitsWithinLease(t *testing.T) {
	worst := time.Duration(notifyMaxSendsPerChannelPerTick) * notifySendTimeout
	if worst >= notifyLease {
		t.Fatalf("단일 채널 라운드의 최악 소요 %v 가 임대 %v 에 도달하거나 초과하면 안 됨"+
			"（notifyMaxSendsPerChannelPerTick=%d × notifySendTimeout=%v）——"+
			"이 세 상수 중 하나를 바꾸면 다른 둘도 같이 확인해야 함",
			worst, notifyLease, notifyMaxSendsPerChannelPerTick, notifySendTimeout)
	}
}
