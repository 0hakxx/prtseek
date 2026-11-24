package db

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Autumn-27/artex/notify"
)

// 이 파일의 테스트는 모두 실제로 SQLite 에 연결한다(DB 가 없으면 건너뜀). 이 SQL 들은
// FOR UPDATE SKIP LOCKED, make_interval, JSONB, 여러 줄 IN(...) 자리표시자 조합을 쓰며,
// 모두 「컴파일은 통과하지만 런타임에 오류를 낼 수 있는」 방식이라 실제로 돌려야 검증된다.

func notifyTestDB(t *testing.T) *DB {
	t.Helper()
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatalf("SQLite unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// newTestChannel 은 채널을 하나 만들고 테스트가 끝나면 자동으로 삭제한다.
func newTestChannel(t *testing.T, d *DB, kind, mode string, filter string) *NotificationChannel {
	t.Helper()
	if filter == "" {
		filter = `{}`
	}
	ch := &NotificationChannel{
		Name:       "테스트 채널-" + t.Name(),
		Kind:       kind,
		Mode:       mode,
		Config:     json.RawMessage(`{"webhook":"https://example.com/hook"}`),
		Filter:     json.RawMessage(filter),
		RatePerMin: 100,
	}
	id, err := d.SaveNotificationChannel(context.Background(), ch)
	if err != nil {
		t.Fatalf("채널 생성 실패: %v", err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_channels WHERE id=$1`, id) })
	ch.ID = id
	return ch
}

// addTestEvent 는 이벤트를 하나 직접 쓰며(finding 을 거치지 않음), 분배와 전달을 테스트하는 데 쓴다.
func addTestEvent(t *testing.T, d *DB, kind string, findingID int64, snap notify.Snapshot) int64 {
	t.Helper()
	snap.Kind = kind
	snap.FindingID = findingID
	id, err := d.AddNotificationEvent(context.Background(), kind, findingID, snap)
	if err != nil {
		t.Fatalf("이벤트 쓰기 실패: %v", err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_events WHERE id=$1`, id) })
	return id
}

func TestNotificationAssetNamesResolvesAndPreservesOrder(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	// 세 종류의 자산은 각자 다른 표시 기준을 가진다: 도메인, IP, URL.
	insertAsset := func(query, value string) int64 {
		t.Helper()
		var id int64
		if err := d.QueryRow(query, value).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	domID := insertAsset(`INSERT INTO assets(type, domain) VALUES('subdomain',$1) RETURNING id`, "a.example.com")
	ipID := insertAsset(`INSERT INTO assets(type, ip) VALUES('ip',$1) RETURNING id`, "10.1.2.3")
	svcID := insertAsset(`INSERT INTO assets(type, url) VALUES('service',$1) RETURNING id`, "https://a.example.com/admin")
	t.Cleanup(func() {
		d.Exec(`DELETE FROM assets WHERE id IN ($1,$2,$3)`, domID, ipID, svcID)
	})

	// 입력 순서를 일부러 섞고, 존재하지 않는 id 를 하나 포함한다.
	got, err := d.NotificationAssetNames(ctx, []int64{svcID, 999999999, domID, ipID, svcID})
	if err != nil {
		t.Fatalf("자산 이름 해석 실패: %v", err)
	}
	want := []string{"https://a.example.com/admin", "a.example.com", "10.1.2.3"}
	if len(got) != len(want) {
		t.Fatalf("자산 이름 개수 불일치, 기대 %v 얻음 %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("순서/값 불일치, 기대 %v 얻음 %v", want, got)
		}
	}
}

// TestRecordNotificationEventTxUnwindsOnFailure 는 세이브포인트 메커니즘의 핵심 테스트다:
// 트랜잭션에서 먼저 notification_events 쓰기가 반드시 실패하게 만든 뒤(항상 false 인 제약을 임시로 추가),
// ① 이 함수가 false 를 보고하고 ② 트랜잭션이 aborted 상태에 들어가지 않아 이후 구문도 실행됨을 단언한다.
//
// 세이브포인트가 없으면 SQLite 는 트랜잭션 전체를 무효화하고, 이후 어떤 구문도
// "current transaction is aborted" 로 실패한다——그것이 바로 「알림 테이블 하나의 문제가
// 취약점을 DB 에 저장하지 못하게 하는」 장애 경로다.
//
// 여기서는 일부러 **COMMIT 이 아니라 ROLLBACK 으로 마무리**한다: ALTER TABLE 은 SQLite 에서 트랜잭션성이라,
// 커밋하면 그 임시 제약이 schema 에 영구히 남아 이후 모든 테스트를 함께 망가뜨린다.
// 롤백은 DDL 을 자동으로 되돌리므로 수동 정리가 필요 없다. 단언은 「트랜잭션이 아직 살아 있음」만
// 필요하고 실제로 커밋할 필요는 없다.
func TestRecordNotificationEventTxUnwindsOnFailure(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	// 방어적 정리: 이전 실행이 이 제약을 남겼으면 먼저 떼어 낸다.
	if _, err := d.Exec(`DROP TRIGGER IF EXISTS notify_test_never`); err != nil {
		t.Fatal(err)
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback() //nolint:errcheck // 임시 제약을 되돌림, 함수 주석 참고

	// NOT VALID: 이후에 쓰이는 행만 제약하고, DB 의 기존 과거 이벤트는 검증하지 않는다
	// (그러지 않으면 기존 행 위반으로 제약을 걸 수 없음).
	if _, err := tx.ExecContext(ctx, `CREATE TRIGGER notify_test_never BEFORE INSERT ON notification_events BEGIN SELECT RAISE(ABORT,'test failure'); END`); err != nil {
		t.Fatalf("임시 제약 추가 실패: %v", err)
	}
	if RecordNotificationEventTx(ctx, tx, notify.EventFindingCreated, 1, notify.Snapshot{Severity: "high"}) {
		t.Fatal("반드시 실패하는 제약 아래에서 쓰기 성공을 보고함")
	}
	// 핵심 단언: 트랜잭션이 아직 쓸 수 있다.
	var one int
	if err := tx.QueryRowContext(ctx, `SELECT 1`).Scan(&one); err != nil {
		t.Fatalf("트랜잭션이 오염됨(세이브포인트 미적용): %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("롤백 실패: %v", err)
	}
	// DDL 이 롤백과 함께 되돌려졌는지 확인해 이후 테스트에 지뢰를 남기지 않는다.
	var exists bool
	if err := d.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='trigger' AND name='notify_test_never')`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("임시 제약이 롤백으로 되돌려지지 않아 이후 테스트를 오염시킴")
	}
}

func TestFanOutRoutesEventsByFilter(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	all := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	onlyCritical := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{"min_severity":"critical"}`)
	sqlOnly := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{"vulnclass_include":["SQL"]}`)

	highSQL := addTestEvent(t, d, notify.EventFindingCreated, 1001, notify.Snapshot{Severity: "high", VulnClass: "SQL 주입"})
	lowXSS := addTestEvent(t, d, notify.EventFindingCreated, 1002, notify.Snapshot{Severity: "low", VulnClass: "XSS"})
	criticalXSS := addTestEvent(t, d, notify.EventFindingCreated, 1003, notify.Snapshot{Severity: "critical", VulnClass: "XSS"})

	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatalf("분배 실패: %v", err)
	}

	cases := []struct {
		name    string
		eventID int64
		channel int64
		want    bool
	}{
		{"전부 받는 채널이 high 를 받음", highSQL, all.ID, true},
		{"전부 받는 채널이 low 를 받음", lowXSS, all.ID, true},
		{"심각만 받는 채널이 high 를 건너뜀", highSQL, onlyCritical.ID, false},
		{"심각만 받는 채널이 critical 을 받음", criticalXSS, onlyCritical.ID, true},
		{"SQL 만 받는 채널이 SQL 을 받음", highSQL, sqlOnly.ID, true},
		{"SQL 만 받는 채널이 XSS 를 건너뜀", lowXSS, sqlOnly.ID, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var exists bool
			if err := d.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM notification_deliveries WHERE event_id=$1 AND channel_id=$2)`,
				tc.eventID, tc.channel).Scan(&exists); err != nil {
				t.Fatal(err)
			}
			if exists != tc.want {
				t.Fatalf("전달 존재 여부: 기대 %v 얻음 %v", tc.want, exists)
			}
		})
	}

	// 다시 분배해도 중복 전달이 생기면 안 됨(fanned_out 멱등).
	events, deliveries, err := d.FanOutPendingEvents(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if events != 0 || deliveries != 0 {
		t.Fatalf("이미 분배된 이벤트가 다시 처리되면 안 됨, events=%d deliveries=%d 얻음", events, deliveries)
	}
}

// TestFanOutMarksEventsWithNoMatchingChannel 은 「이벤트가 어떤 채널에도 적중 안 함」 경우를 다룬다.
// 이런 이벤트도 분배됨으로 표시되어야 한다. 그러지 않으면 영원히 분배 대기 집합에 남아 tick 마다 다시 스캔된다.
func TestFanOutMarksEventsWithNoMatchingChannel(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	pick := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{"vulnclass_include":["절대 매칭 안 되는 유형"]}`)
	_ = pick

	ev := addTestEvent(t, d, notify.EventFindingCreated, 2001, notify.Snapshot{Severity: "high", VulnClass: "XSS"})
	_, deliveries, err := d.FanOutPendingEvents(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if deliveries != 0 {
		t.Fatalf("전달이 생기면 안 됨, %d 얻음", deliveries)
	}
	var fanned bool
	if err := d.QueryRowContext(ctx, `SELECT fanned_out FROM notification_events WHERE id=$1`, ev).Scan(&fanned); err != nil {
		t.Fatal(err)
	}
	if !fanned {
		t.Fatal("적중 안 한 채널의 이벤트도 분배됨으로 표시되어야 함, 그러지 않으면 무한 재스캔됨")
	}
}

func TestClaimRealtimeDeliveriesHonorsLeaseAndMode(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	realtime := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	digest := newTestChannel(t, d, notify.KindDingTalk, NotifyModeDigest, `{}`)

	addTestEvent(t, d, notify.EventFindingCreated, 3001, notify.Snapshot{Severity: "high", VulnClass: "XSS"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}

	// 실시간 가져가기는 realtime 채널의 그 한 건만 가져와야 하고 digest 채널은 건드리면 안 된다.
	got, err := d.ClaimRealtimeDeliveries(ctx, realtime.ID, 10, time.Minute)
	if err != nil {
		t.Fatalf("가져가기 실패: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("1 건을 가져가야 하는데 %d 건", len(got))
	}
	if got[0].State != NotifyStateSending || got[0].Attempts != 1 {
		t.Fatalf("가져간 뒤 sending 이고 attempts=1 이어야 하는데 state=%s attempts=%d", got[0].State, got[0].Attempts)
	}
	// 함께 불러온 렌더링 컨텍스트가 완전해야 한다(채널 설정 + 이벤트 스냅샷 + finding id).
	if got[0].Channel == nil || len(got[0].Channel.Config) == 0 {
		t.Fatal("가져간 결과에 채널 설정이 없어 렌더링이 실패함")
	}
	if got[0].FindingID != 3001 {
		t.Fatalf("finding id 가 이벤트에서 나오지 않음, %d 얻음", got[0].FindingID)
	}

	// 임대가 만료되지 않았으면 두 번째 가져가기는 비어야 한다——이것이 「같은 행을 두 dispatcher 가 동시에 전달하지 않는다」는
	// 보장이다.
	again, err := d.ClaimRealtimeDeliveries(ctx, realtime.ID, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("임대 기간에는 중복 가져가기가 안 됨, %d 건 얻음", len(again))
	}

	// digest 채널의 전달은 실시간 가져가기에 걸리면 안 된다.
	left, err := d.ClaimRealtimeDeliveries(ctx, digest.ID, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("실시간 가져가기가 digest 채널의 전달을 가져오면 안 됨, %d 건 얻음", len(left))
	}
}

// TestClaimExpiredLeaseRecovers 는 충돌 자가 복구를 다룬다: 프로세스가 전달 도중 죽으면 sending
// 행이 남는데, 임대가 만료되면 다시 가져가질 수 있어야 한다. 그러지 않으면 이 전달이 영원히 막힌다.
func TestClaimExpiredLeaseRecovers(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	addTestEvent(t, d, notify.EventFindingCreated, 4001, notify.Snapshot{Severity: "high"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	first, err := d.ClaimRealtimeDeliveries(ctx, ch.ID, 10, time.Minute)
	if err != nil || len(first) != 1 {
		t.Fatalf("첫 가져가기 실패: %v (%d 건)", err, len(first))
	}
	// 임대를 수동으로 과거로 밀어 「임대 만료」를 흉내 낸다.
	if _, err := d.Exec(`UPDATE notification_deliveries SET next_attempt_at = datetime('now','-1 minute') WHERE id=$1`, first[0].ID); err != nil {
		t.Fatal(err)
	}
	second, err := d.ClaimRealtimeDeliveries(ctx, ch.ID, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 {
		t.Fatalf("임대가 만료된 sending 행은 다시 가져가질 수 있어야 함, %d 건 얻음", len(second))
	}
	if second[0].Attempts != 2 {
		t.Fatalf("다시 가져가기는 시도 횟수를 누적해야 함, %d 얻음", second[0].Attempts)
	}
}

func TestClaimSkipsDisabledChannel(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	addTestEvent(t, d, notify.EventFindingCreated, 5001, notify.Snapshot{Severity: "high"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	// 비활성화하면 남은 발송 대기 전달을 함께 skipped 로 표시한다.
	if err := d.SetNotificationChannelEnabled(ctx, ch.ID, false); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := d.QueryRow(`SELECT state FROM notification_deliveries WHERE channel_id=$1`, ch.ID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != NotifyStateSkipped {
		t.Fatalf("비활성화한 채널의 남은 발송 대기 전달은 skipped 로 표시되어야 함, %s 얻음", state)
	}
	got, err := d.ClaimRealtimeDeliveries(ctx, ch.ID, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("비활성화된 채널은 가져가질 수 없어야 함, %d 건 얻음", len(got))
	}
}

func TestDigestBatchDueAndStableBatchID(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeDigest, `{}`)
	for i := 0; i < 3; i++ {
		addTestEvent(t, d, notify.EventFindingCreated, int64(6000+i), notify.Snapshot{Severity: "high"})
	}
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}

	// 배치가 막 생겨 나이가 0 이라 30 분 주기에서는 기한이 되면 안 된다.
	due, err := d.DigestBatchDue(ctx, ch.ID, 30*time.Minute)
	if err != nil {
		t.Fatalf("배치 기한 판단 실패: %v", err)
	}
	if due {
		t.Fatal("막 생긴 배치는 즉시 기한이 되면 안 됨")
	}

	// 세 전달의 생성 시각을 함께 과거로 밀어 주기를 채운 배치를 흉내 낸다.
	if _, err := d.Exec(`UPDATE notification_deliveries SET created_at = datetime('now','-40 minutes') WHERE channel_id=$1`, ch.ID); err != nil {
		t.Fatal(err)
	}
	due, err = d.DigestBatchDue(ctx, ch.ID, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !due {
		t.Fatal("주기를 넘은 배치는 기한이 된 것으로 판정되어야 함")
	}

	batch, err := d.ClaimDigestBatch(ctx, ch.ID, MaxDigestBatchSize, time.Minute)
	if err != nil {
		t.Fatalf("요약 배치 가져가기 실패: %v", err)
	}
	if len(batch) != 3 {
		t.Fatalf("요약은 한 번에 3 건 모두 가져가야 하는데 %d 건", len(batch))
	}
	if batch[0].BatchID == nil {
		t.Fatal("요약 배치는 batch_id 를 반드시 써야 함, 그러지 않으면 이력에서 이것들이 함께 보내진 것을 알 수 없음")
	}
	firstBatchID := *batch[0].BatchID
	for _, dl := range batch {
		if dl.BatchID == nil || *dl.BatchID != firstBatchID {
			t.Fatalf("같은 배치는 batch_id 를 공유해야 함, %v vs %d 얻음", dl.BatchID, firstBatchID)
		}
	}

	// 이 배치를 **통째로** 실패시켜 다시 정렬한 뒤 가져가면, batch_id 는 원래 값을 유지해야 한다(COALESCE 의 역할):
	// 그러지 않으면 한 번의 재시도로 「이 배치가 함께 보내졌다」는 사실이 지워진다.
	//
	// 한 건만이 아니라 배치 전체를 다시 정렬해야 한다——전달 엔진은 요약 메시지를 보낼 때 바로 이렇게 처리한다
	// (한 메시지가 배치 전체를 대표하며 성패를 함께한다). 한 건만 다시 정렬하면 나머지는 여전히 임대 기간이라,
	// 다시 가져가면 당연히 그 한 건만 가져온다.
	allIDs := make([]int64, 0, len(batch))
	for _, dl := range batch {
		allIDs = append(allIDs, dl.ID)
	}
	if err := d.RescheduleDeliveries(ctx, allIDs, time.Second, "실패 흉내"); err != nil {
		t.Fatal(err)
	}
	// 임대를 과거로 밀어 백오프 시각이 지난 것을 흉내 낸다.
	if _, err := d.Exec(`UPDATE notification_deliveries SET next_attempt_at = datetime('now','-1 minute') WHERE channel_id=$1`, ch.ID); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := d.ClaimDigestBatch(ctx, ch.ID, MaxDigestBatchSize, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(reclaimed) != 3 {
		t.Fatalf("다시 가져가기는 3 건 모두 가져와야 하는데 %d 건", len(reclaimed))
	}
	if reclaimed[0].BatchID == nil || *reclaimed[0].BatchID != firstBatchID {
		t.Fatalf("재시도 후 batch_id 는 원래 값 %d 를 유지해야 하는데 %v 얻음", firstBatchID, reclaimed[0].BatchID)
	}
}

func TestDeliveryStateTransitions(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	addTestEvent(t, d, notify.EventFindingCreated, 7001, notify.Snapshot{Severity: "high"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	got, err := d.ClaimRealtimeDeliveries(ctx, ch.ID, 10, time.Minute)
	if err != nil || len(got) != 1 {
		t.Fatalf("가져가기 실패: %v (%d)", err, len(got))
	}
	id := got[0].ID

	if err := d.RescheduleDeliveries(ctx, []int64{id}, time.Second, "네트워크 흔들림"); err != nil {
		t.Fatal(err)
	}
	var state, lastErr string
	if err := d.QueryRow(`SELECT state, last_error FROM notification_deliveries WHERE id=$1`, id).Scan(&state, &lastErr); err != nil {
		t.Fatal(err)
	}
	if state != NotifyStatePending || lastErr != "네트워크 흔들림" {
		t.Fatalf("다시 정렬한 뒤 pending 이고 원인을 기록해야 하는데 state=%s err=%q", state, lastErr)
	}

	if err := d.FailDeliveries(ctx, []int64{id}, "재시도 소진"); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`SELECT state FROM notification_deliveries WHERE id=$1`, id).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != NotifyStateFailed {
		t.Fatalf("failed 여야 하는데 %s 얻음", state)
	}

	// 수동 재전송은 재시도 횟수를 0 으로 만들고 즉시 기한이 되게 해야 한다. 그러지 않으면 예전 실패 예산을 상속한다.
	if err := d.RetryNotificationDelivery(ctx, id); err != nil {
		t.Fatalf("재전송 실패: %v", err)
	}
	var attempts int
	var next time.Time
	if err := d.QueryRow(`SELECT state, attempts, next_attempt_at FROM notification_deliveries WHERE id=$1`, id).Scan(&state, &attempts, &next); err != nil {
		t.Fatal(err)
	}
	if state != NotifyStatePending || attempts != 0 {
		t.Fatalf("재전송 후 pending 이고 attempts=0 이어야 하는데 state=%s attempts=%d", state, attempts)
	}
	if next.After(time.Now().Add(time.Second)) {
		t.Fatal("재전송은 즉시 가져가질 수 있어야 함")
	}

	// 전달된 전달은 다시 보낼 수 없어야 한다.
	if err := d.MarkDeliveriesSent(ctx, []int64{id}); err != nil {
		t.Fatal(err)
	}
	if err := d.RetryNotificationDelivery(ctx, id); err == nil {
		t.Fatal("이미 전달된 전달은 재전송을 허용하면 안 됨")
	}
}

func TestListNotificationDeliveriesPagingAndFilter(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	for i := 0; i < 5; i++ {
		addTestEvent(t, d, notify.EventFindingCreated, int64(8000+i), notify.Snapshot{Severity: "high", Name: "페이지 테스트"})
	}
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ClaimRealtimeDeliveries(ctx, ch.ID, 10, time.Minute); err != nil {
		t.Fatal(err)
	}

	page1, total, err := d.ListNotificationDeliveries(ctx, NotificationDeliveryFilter{ChannelID: ch.ID, State: NotifyStateSending}, 1, 2)
	if err != nil {
		t.Fatalf("조회 실패: %v", err)
	}
	if total != 5 {
		t.Fatalf("총수는 5 여야 하는데 %d", total)
	}
	if len(page1) != 2 {
		t.Fatalf("페이지당 2 건이어야 하는데 %d", len(page1))
	}
	// 최신순: 첫 페이지의 첫 건 id 가 두 번째 페이지의 첫 건보다 커야 한다.
	page2, _, err := d.ListNotificationDeliveries(ctx, NotificationDeliveryFilter{ChannelID: ch.ID, State: NotifyStateSending}, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page2) != 2 || page2[0].ID >= page1[0].ID {
		t.Fatalf("페이지 순서는 최신순이어야 하는데 page1[0]=%d page2[0]=%d 얻음", page1[0].ID, page2[0].ID)
	}
	// 렌더링 컨텍스트가 이력과 함께 반환되어야 한다. 그러지 않으면 목록이 「무엇을 보냈는지」를 표시할 수 없다.
	if page1[0].ChannelName == "" || page1[0].FindingID == 0 {
		t.Fatalf("이력 항목에 표시 필드가 없음: %+v", page1[0])
	}

	// 상태로 필터: pending 이 없음.
	pending, totalPending, err := d.ListNotificationDeliveries(ctx, NotificationDeliveryFilter{ChannelID: ch.ID, State: NotifyStatePending}, 1, 50)
	if err != nil {
		t.Fatal(err)
	}
	if totalPending != 0 || len(pending) != 0 {
		t.Fatalf("pending 전달이 없어야 하는데 %d 건 (total=%d)", len(pending), totalPending)
	}
}

func TestSetFindingStatusWithNotifyOnlyEmitsOnRealChange(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	tk, err := d.CreateTask("알림 상태 변경 테스트", "목표", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(tk.ID)
	es := d.Exploration(tk.ExplorationID)
	f, err := es.RecordFinding(ctx, RecordFindingInput{
		TaskID: tk.ID, Worker: "test", VulnClass: "SQL 주입", Name: "상태 변경 사례",
		Severity: "high", Summary: "요약",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_events WHERE finding_id=$1`, f.FindingID) })

	// 저장할 때 finding_created 이벤트를 하나 등록했으므로, 먼저 그것을 세어 기준선으로 삼는다.
	var base int
	if err := d.QueryRow(`SELECT count(*) FROM notification_events WHERE finding_id=$1`, f.FindingID).Scan(&base); err != nil {
		t.Fatal(err)
	}
	if base < 1 {
		t.Fatal("취약점 저장은 같은 트랜잭션에서 푸시 이벤트를 하나 등록해야 함")
	}

	// 같은 상태로 바꿈: 이벤트가 생기면 안 됨(중복 제출로 푸시 잡음을 내지 않도록).
	from, found, notified, err := d.SetFindingStatusWithNotify(ctx, f.FindingID, "pending")
	if err != nil || !found {
		t.Fatalf("상태 설정 실패: found=%v err=%v", found, err)
	}
	if notified {
		t.Fatal("상태가 바뀌지 않았을 때 푸시 이벤트를 등록하면 안 됨")
	}
	if from != "pending" {
		t.Fatalf("변경 전 상태 pending 을 반환해야 하는데 %q 얻음", from)
	}

	// 실제 변경: 이벤트를 등록하고 from/to 를 기록해야 함.
	from, found, notified, err = d.SetFindingStatusWithNotify(ctx, f.FindingID, "fixed")
	if err != nil || !found {
		t.Fatalf("상태 설정 실패: found=%v err=%v", found, err)
	}
	if !notified {
		t.Fatal("상태가 실제로 변경될 때 푸시 이벤트를 등록해야 함")
	}
	if from != "pending" {
		t.Fatalf("from 은 pending 이어야 하는데 %q 얻음", from)
	}
	var snapshot []byte
	if err := d.QueryRow(`SELECT snapshot FROM notification_events WHERE finding_id=$1 AND kind=$2`,
		f.FindingID, notify.EventFindingStatusChanged).Scan(&snapshot); err != nil {
		t.Fatalf("상태 변경 이벤트를 찾지 못함: %v", err)
	}
	var snap notify.Snapshot
	if err := json.Unmarshal(snapshot, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.FromStatus != "pending" || snap.ToStatus != "fixed" {
		t.Fatalf("스냅샷의 상태 전이가 틀림: %s → %s", snap.FromStatus, snap.ToStatus)
	}
	// 스냅샷은 렌더링에 필요한 필드를 담아야 함. 그러지 않으면 상태 변경 메시지가 빈 껍데기가 됨.
	if snap.VulnClass != "SQL 주입" || snap.Severity != "high" || snap.Name != "상태 변경 사례" {
		t.Fatalf("스냅샷에 렌더링 필드가 없음: %+v", snap)
	}
	var status string
	if err := d.QueryRow(`SELECT status FROM findings WHERE id=$1`, f.FindingID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "fixed" {
		t.Fatalf("상태가 fixed 로 갱신되어야 하는데 %s 얻음", status)
	}

	// 존재하지 않는 취약점: found=false, 오류 없음.
	if _, found, _, err := d.SetFindingStatusWithNotify(ctx, 999999999, "fixed"); err != nil || found {
		t.Fatalf("존재하지 않는 취약점은 found=false 이고 오류가 없어야 하는데 found=%v err=%v", found, err)
	}
}

func TestNotificationStatsSnapshot(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	addTestEvent(t, d, notify.EventFindingCreated, 9001, notify.Snapshot{Severity: "high"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	stats, err := d.NotificationStatsSnapshot(ctx)
	if err != nil {
		t.Fatalf("통계 실패: %v", err)
	}
	if stats.Channels < 1 || stats.ChannelsOn < 1 {
		t.Fatalf("채널 수가 틀림: %+v", stats)
	}
	if stats.Pending < 1 {
		t.Fatalf("발송 대기 전달을 집계해야 함: %+v", stats)
	}
	// 막 생긴 전달의 적체 나이는 음수나 거대한 값이 아니라 0 에 가까워야 함.
	if stats.BacklogAgeMS < 0 || stats.BacklogAgeMS > int64(time.Hour/time.Millisecond) {
		t.Fatalf("적체 나이가 유효하지 않음: %d ms", stats.BacklogAgeMS)
	}
	_ = ch
}

func TestNotificationChannelCRUDRoundTrip(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	ch := &NotificationChannel{
		Name:       "CRUD 왕복",
		Kind:       notify.KindEmail,
		Mode:       NotifyModeDigest,
		Config:     json.RawMessage(`{"host":"smtp.example.com","port":587,"from":"a@b.c","to":["x@y.z"]}`),
		Filter:     json.RawMessage(`{"min_severity":"medium","on_status_change":true}`),
		RatePerMin: 42,
	}
	id, err := d.SaveNotificationChannel(ctx, ch)
	if err != nil {
		t.Fatalf("생성 실패: %v", err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_channels WHERE id=$1`, id) })

	got, err := d.NotificationChannelByID(ctx, id)
	if err != nil {
		t.Fatalf("읽기 실패: %v", err)
	}
	if got.Mode != NotifyModeDigest || got.RatePerMin != 42 || got.Name != "CRUD 왕복" {
		t.Fatalf("왕복 필드 불일치: %+v", got)
	}
	if !got.IsEnabled() {
		t.Fatal("기본값은 활성화여야 함")
	}
	var cfg map[string]any
	if err := json.Unmarshal(got.Config, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["host"] != "smtp.example.com" {
		t.Fatalf("설정이 올바르게 저장되지 않음: %v", cfg)
	}
	var filter notify.Filter
	if err := json.Unmarshal(got.Filter, &filter); err != nil {
		t.Fatal(err)
	}
	if filter.MinSeverity != "medium" || !filter.OnStatusChange {
		t.Fatalf("필터 조건이 올바르게 저장되지 않음: %+v", filter)
	}

	// 갱신한 뒤 다시 읽는다.
	got.Name = "이름 변경됨"
	off := false
	got.Enabled = &off
	if _, err := d.SaveNotificationChannel(ctx, got); err != nil {
		t.Fatal(err)
	}
	after, err := d.NotificationChannelByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if after.Name != "이름 변경됨" || after.IsEnabled() {
		t.Fatalf("갱신이 적용되지 않음: %+v", after)
	}

	// 삭제 후에는 조용히 성공하지 않고 「없음」을 보고해야 함.
	if err := d.DeleteNotificationChannel(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := d.NotificationChannelByID(ctx, id); err != ErrNotificationChannelNotFound {
		t.Fatalf("ErrNotificationChannelNotFound 를 기대했는데 %v 얻음", err)
	}
	if err := d.DeleteNotificationChannel(ctx, id); err != ErrNotificationChannelNotFound {
		t.Fatalf("중복 삭제는 없음을 보고해야 하는데 %v 얻음", err)
	}
}

// TestSaveNotificationChannelKeepsExplicitZeroRate 는 예전에 잘못 쓴 곳을 못 박는다:
// **0 은 올바른 설정이며 「속도 제한 없음」을 뜻하므로, db 계층이 「미지정」으로 취급해 기본값으로 덮어쓰면 안 된다**.
//
// 과거 버그: SaveNotificationChannel 에 `if RatePerMin <= 0 { 기본값 사용 }` 이 있어,
// 문서, UI 안내, takeTokens 는 모두 「0=속도 제한 없음」으로 해석하는데 DB 쓰기 계층만 조용히
// 20(딩톡/위챗 워크/Telegram)이나 100(페이슈)으로 바꿨다——운영자는 속도 제한을 풀었다고 생각했지만 실제로는 막혀 있고,
// 아무 안내도 없었다. 「미지정」과 「명시적 0」의 차이는 요청 본문만 표현할 수 있으므로,
// 기본값은 server 계층에서 채우고(notifyCreateChannel 참고) db 계층은 저장만 한다.
func TestSaveNotificationChannelKeepsExplicitZeroRate(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	// 명시적 0(속도 제한 없음): 반드시 그대로 저장해야 한다.
	unlimited := &NotificationChannel{
		Name: "속도 제한 없음", Kind: notify.KindDingTalk, RatePerMin: 0,
		Config: json.RawMessage(`{"webhook":"https://example.com/h"}`),
	}
	id, err := d.SaveNotificationChannel(ctx, unlimited)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_channels WHERE id=$1`, id) })
	got, err := d.NotificationChannelByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.RatePerMin != 0 {
		t.Fatalf("명시적 0 은 속도 제한 없음을 뜻하므로 그대로 저장해야 하는데 %d 얻음", got.RatePerMin)
	}
	if got.Mode != NotifyModeRealtime {
		t.Fatalf("기본 모드는 realtime 이어야 하는데 %s 얻음", got.Mode)
	}

	// 음수는 잘못된 입력이므로 조용히 다른 값으로 바꾸지 않고 거부해야 한다.
	bad := &NotificationChannel{
		Name: "음수 속도 제한", Kind: notify.KindDingTalk, RatePerMin: -1,
		Config: json.RawMessage(`{"webhook":"https://example.com/h"}`),
	}
	if _, err := d.SaveNotificationChannel(ctx, bad); err == nil {
		t.Fatal("음수 속도 제한은 거부되어야 함")
	}
}

// TestDeleteChannelCascadesDeliveries 는 외래 키 동작을 못 박는다: 채널을 삭제하면 그 전달 이력도 함께 사라지지만
// (설정이 없으면 이력을 해석할 수 없음), 이벤트 자체는 남아야 한다——다른 채널이 여전히 참조할 수 있다.
func TestDeleteChannelCascadesDeliveries(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	ev := addTestEvent(t, d, notify.EventFindingCreated, 9101, notify.Snapshot{Severity: "high"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	var before int
	if err := d.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1`, ch.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if before == 0 {
		t.Fatal("선행 조건 불충족: 전달이 생기지 않음")
	}
	if err := d.DeleteNotificationChannel(ctx, ch.ID); err != nil {
		t.Fatal(err)
	}
	var after int
	if err := d.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1`, ch.ID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != 0 {
		t.Fatalf("채널을 삭제하면 그 전달도 연쇄 삭제되어야 하는데 여전히 %d 건", after)
	}
	var evExists bool
	if err := d.QueryRow(`SELECT EXISTS(SELECT 1 FROM notification_events WHERE id=$1)`, ev).Scan(&evExists); err != nil {
		t.Fatal(err)
	}
	if !evExists {
		t.Fatal("채널을 삭제해도 이벤트 자체를 함께 삭제하면 안 됨")
	}
}

// TestClaimDigestBatchHonorsCallerLimit 는 감사가 지적한 구멍을 다룬다:
// 요약 채널은 예전에 토큰 버킷을 완전히 우회했다——allow 가 takeTokens 로 차감되는데 아무도 쓰지 않아,
// rate_per_min 이 digest 모드에 전혀 작용하지 않았다. 이제 limit 도 제약에 참여한다.
func TestClaimDigestBatchHonorsCallerLimit(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeDigest, `{}`)
	for i := 0; i < 10; i++ {
		addTestEvent(t, d, notify.EventFindingCreated, int64(7000+i), notify.Snapshot{Severity: "high"})
	}
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	// limit=3 으로 가져감: 3 건만 가져가고 나머지는 DB 에 남는다.
	got, err := d.ClaimDigestBatch(ctx, ch.ID, 3, time.Minute)
	if err != nil {
		t.Fatalf("가져가기 실패: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("호출자 속도 제한 할당량에 따라 3 건만 가져가야 하는데 %d 얻음", len(got))
	}
	// limit=0 은 이번 턴 할당량 소진을 뜻함: 한 건도 가져가면 안 되고 오류도 내면 안 됨.
	if got, err := d.ClaimDigestBatch(ctx, ch.ID, 0, time.Minute); err != nil || len(got) != 0 {
		t.Fatalf("할당량이 0 이면 0 건을 가져가고 오류가 없어야 하는데 %d 건 err=%v", len(got), err)
	}
}

// TestFinishFindingRetestEmitsStatusChange 는 감사가 지적한 완전성 빈틈을 다룬다:
// 재테스트 결론이 「수정됨」일 때 상태는 실제로 바뀌었지만, 그 UPDATE 가 DB 에 직접 쓰여
// 알림 버전을 우회했다——그래서 on_status_change 를 설정한 채널은 이런 상태 전이에
// 아무 푸시도 받지 못했고, 화면에서 상태가 조용히 바뀌어 운영자는 플랫폼을 열어야만 알았다.
//
// 이 테스트는 「상태를 바꾸는 모든 경로가 상태 변경 이벤트를 등록해야 한다」를 못 박는다.
func TestFinishFindingRetestEmitsStatusChange(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	tk, err := d.CreateTask("재테스트 푸시 테스트", "목표", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(tk.ID)
	es := d.Exploration(tk.ExplorationID)
	f, err := es.RecordFinding(ctx, RecordFindingInput{
		TaskID: tk.ID, Worker: "test", VulnClass: "SQL 주입", Name: "재테스트 대상",
		Severity: "high", Summary: "요약",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_events WHERE finding_id=$1`, f.FindingID) })

	// 재테스트 기록을 하나 만들고 바로 완료 상태로 민다.
	rt, _, _, err := d.CreateFindingRetest(ctx, f.FindingID, "재검토")
	if err != nil {
		t.Fatal(err)
	}
	if rt.ConversationID == nil {
		t.Fatal("재테스트는 대화 하나와 연결되어야 함")
	}
	// 재테스트는 먼저 running 에 들어가야 결론을 저장할 수 있다(실제 흐름과 같음).
	if ok, err := d.StartFindingRetest(ctx, rt.ID); err != nil || !ok {
		t.Fatalf("재테스트 시작 실패: ok=%v err=%v", ok, err)
	}
	if err := d.RecordFindingRetestResult(ctx, *rt.ConversationID, "fixed", "수정됨", "증거"); err != nil {
		t.Fatal(err)
	}
	if err := d.FinishFindingRetest(rt.ID, "completed", ""); err != nil {
		t.Fatalf("재테스트 종료 실패: %v", err)
	}

	var status string
	if err := d.QueryRow(`SELECT status FROM findings WHERE id=$1`, f.FindingID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != FindingFixed {
		t.Fatalf("재테스트가 수정됨으로 판정한 뒤 상태는 fixed 여야 하는데 %s 얻음", status)
	}

	// 핵심 단언: 상태 변경 이벤트가 하나 있어야 하고 from/to 가 올바라야 한다.
	var snapshot []byte
	err = d.QueryRow(`SELECT snapshot FROM notification_events WHERE finding_id=$1 AND kind=$2 ORDER BY id DESC LIMIT 1`,
		f.FindingID, notify.EventFindingStatusChanged).Scan(&snapshot)
	if err != nil {
		t.Fatalf("재테스트 수정됨 판정은 상태 변경 푸시 이벤트를 등록해야 함(그러지 않으면 on_status_change 를 설정한 채널이 못 받음): %v", err)
	}
	var snap notify.Snapshot
	if err := json.Unmarshal(snapshot, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.FromStatus != "pending" || snap.ToStatus != FindingFixed {
		t.Fatalf("스냅샷의 상태 전이가 틀림: %s → %s", snap.FromStatus, snap.ToStatus)
	}
	// 스냅샷은 렌더링에 필요한 필드를 담아야 함. 그러지 않으면 푸시가 빈 껍데기가 됨.
	if snap.Name != "재테스트 대상" || snap.Severity != "high" {
		t.Fatalf("스냅샷에 렌더링 필드가 없음: %+v", snap)
	}
}
