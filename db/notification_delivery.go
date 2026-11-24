package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// 이 파일은 전달 작업의 가져가기와 상태 전이다.
//
// 가져가기는 긴 트랜잭션 대신 「임대」를 쓴다: 행을 sending 으로 설정하고 next_attempt_at 을 미래로 밀어
// 임대 만료 시각으로 삼은 뒤, 트랜잭션을 커밋하고 나서 네트워크 전달을 한다. 그래서 전달 중에는 DB 잠금을 쥐지 않는다——
// 네트워크 요청은 몇 초가 걸릴 수 있으며(클라이언트 시간 초과 15초), 행 잠금을 계속 쥐고 있으면 같은 DB 의 다른 쓰기 작업을 무너뜨린다.
//
// 대가는 프로세스가 전달 도중 죽으면 행이 sending 에 멈춘다는 것이다. 이는 **스스로 복구된다**: 임대가 만료되면
// next_attempt_at 이 과거가 되어, 다음 턴의 가져가기가 같은 행을 다시 건져 올린다(가져가기 조건의
// state IN ('pending','sending') 참고). 재시도 횟수는 가져갈 때 이미 +1 되므로, 프로세스가 죽어도
// 무한 재시도가 되지 않는다——MaxNotifyAttempts 번의 기회를 다 쓰면 failed 로 떨어져 사람의 처리를 기다린다.

// MaxNotifyAttempts 는 전달 하나의 최대 시도 횟수다(첫 시도 포함).
// 전달 엔진이 아니라 여기에 정의한다: 이것은 상태 기계 자체의 정책이며, 엔진은 실행자일 뿐이다.
const MaxNotifyAttempts = 3

// MaxDigestBatchSize 는 요약 배치 하나에 한 번에 합칠 수 있는 최대 전달 수다.
//
// 존재 이유는 자원이다: 한 요약 주기 안에 취약점이 수만 건 스캔되면(충분히 가능하다——전체 스캔 한 번이면
// 된다), 상한이 없을 경우 가져가기가 모든 행을 메모리에 읽어 아주 긴 메시지 하나로 렌더링하고,
// 채널의 길이 상한에 걸려 대부분이 잘린다——메모리를 낭비할 뿐 아니라 잘린 취약점들이 **조용히 사라진다**.
// 상한을 두면 넘치는 부분은 DB 에 남아 다음 배치가 되고, 다음 주기에 자연스럽게 보내지므로 사라지지 않는다.
//
// 500 으로 정한 근거: 메시지로 렌더링했을 때 위챗 워크 4096 바이트 상한 안에서 여전히 "읽을 내용이 있는" 규모다.
// 더 크게 해 봐야 잘림이 더 뒤쪽에서 일어날 뿐이다.
const MaxDigestBatchSize = 500

// NotificationDelivery 는 전달 작업 하나이며, 렌더링에 필요한 채널 설정과 이벤트 스냅샷을 포함한다.
type NotificationDelivery struct {
	ID            int64           `json:"id"`
	EventID       int64           `json:"event_id"`
	ChannelID     int64           `json:"channel_id"`
	State         string          `json:"state"`
	Attempts      int             `json:"attempts"`
	NextAttemptAt time.Time       `json:"next_attempt_at"`
	LastError     string          `json:"last_error"`
	BatchID       *int64          `json:"batch_id,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
	SentAt        *time.Time      `json:"sent_at,omitempty"`
	Snapshot      json.RawMessage `json:"snapshot,omitempty"`
	// 함께 불러온 렌더링 컨텍스트이며 JSON 에 들어가지 않는다(server 계층이 DTO 를 조립함).
	Channel *NotificationChannel `json:"-"`
	// FindingID/EventKind 는 이벤트에서 가져오며, 기록 목록에서 취약점 상세로 바로 이동하는 데 쓴다.
	FindingID int64  `json:"finding_id,string"`
	EventKind string `json:"event_kind"`
	// ChannelName/ChannelKind 는 목록 표시용 중복 필드로, 프론트엔드의 두 번째 조회를 줄인다.
	ChannelName string `json:"channel_name"`
	ChannelKind string `json:"channel_kind"`
}

const notificationDeliveryCols = `d.id, d.event_id, d.channel_id, d.state, d.attempts, d.next_attempt_at,
       d.last_error, d.batch_id, d.created_at, d.sent_at`

// joinedDeliveryQuery 는 전달 행의 통일된 읽기 형태다: 전달 + 이벤트 스냅샷 + 채널 설정.
// 메시지 하나를 렌더링하려면 셋 다 필요하며, 따로 조회하면 왕복이 세 번 생긴다.
const joinedDeliveryQuery = `SELECT ` + notificationDeliveryCols + `,
       e.snapshot, e.kind, e.finding_id,
       c.id, c.name, c.kind, c.enabled, c.config, c.mode, c.filter, c.rate_per_min
FROM notification_deliveries d
JOIN notification_events e ON e.id = d.event_id
JOIN notification_channels c ON c.id = d.channel_id`

func scanNotificationDelivery(sc interface{ Scan(...any) error }) (*NotificationDelivery, error) {
	var (
		dl        NotificationDelivery
		lastErr   sql.NullString
		batchID   sql.NullInt64
		sentAt    sql.NullTime
		snapshot  []byte
		eventKind string
		channel   NotificationChannel
		chEnabled bool
	)
	if err := sc.Scan(&dl.ID, &dl.EventID, &dl.ChannelID, &dl.State, &dl.Attempts, &dl.NextAttemptAt,
		&lastErr, &batchID, &dl.CreatedAt, &sentAt,
		&snapshot, &eventKind, &dl.FindingID,
		&channel.ID, &channel.Name, &channel.Kind, &chEnabled, &channel.Config, &channel.Mode, &channel.Filter, &channel.RatePerMin); err != nil {
		return nil, err
	}
	dl.LastError = lastErr.String
	if batchID.Valid {
		dl.BatchID = &batchID.Int64
	}
	if sentAt.Valid {
		dl.SentAt = &sentAt.Time
	}
	dl.Snapshot = json.RawMessage(snapshot)
	dl.EventKind = eventKind
	dl.ChannelName = channel.Name
	dl.ChannelKind = channel.Kind
	channel.Enabled = &chEnabled
	dl.Channel = &channel
	return &dl, nil
}

// claimQuery 는 가져가기 한 번을 설명한다: 먼저 sel 로 후보를 골라 잠근 뒤, sending 으로 설정하고
// 임대를 연장한다. sel 안의 lease 위치는 호출자가 $n 자리표시자로 두고 직접 파라미터를 넘긴다.
type claimQuery struct {
	sql  string
	args []any
}

// ClaimRealtimeDeliveries 는 특정 채널의 기한이 된 실시간 전달을 최대 limit 건 가져간다.
//
// 일부러 「전역으로 한 묶음 가져와 골라서 보내기」가 아니라 **채널 하나씩** 가져간다: 속도 제한 게이트는 전달 엔진에서 채널별로
// 관리되므로, 이 채널이 이번 턴에 몇 건 보낼 수 있는지 먼저 알고 그만큼만 행을 가져가야 속도 제한이
// 재시도 횟수를 소모하지 않는다. 반대로 먼저 가져간 뒤 버리면, 속도 제한에 막힌 행은 이미 attempts 가 한 번 세어져,
// 3 회 예산이 순전히 기다림으로 소진되고 결국 failed 로 떨어진다.
//
// 조건에는 「임대가 만료된 sending」이 포함된다——프로세스가 죽은 뒤 스스로 복구되는 지점이다. lease 는 한 번의
// 전달 최악 소요 시간(채널 HTTP 클라이언트 시간 초과 15초)보다 충분히 커야 한다. 그렇지 않으면 같은 행을 두 dispatcher 가
// 동시에 전달한다. 비활성화된 채널도 함께 막는다: 비활성화 작업은 이미 남은 전달을 skipped 로 표시했지만,
// 여기서 한 번 더 막아 비활성화와 가져가기가 동시에 일어날 때 빠져나가는 것을 막는다.
func (d *DB) ClaimRealtimeDeliveries(ctx context.Context, channelID int64, limit int, lease time.Duration) ([]*NotificationDelivery, error) {
	if limit <= 0 {
		return nil, nil
	}
	return d.claimDeliveries(ctx, lease, claimQuery{
		sql: `SELECT dd.id FROM notification_deliveries dd
JOIN notification_channels c ON c.id = dd.channel_id
WHERE dd.channel_id = $1 AND dd.state IN ($2,$3) AND dd.next_attempt_at <= (strftime('%Y-%m-%d %H:%M:%f','now'))
  AND c.enabled AND c.mode = $4
ORDER BY dd.next_attempt_at, dd.id
LIMIT $5`,
		args: []any{channelID, NotifyStatePending, NotifyStateSending, NotifyModeRealtime, limit},
	}, nil)
}

// DigestBatchDue 는 이 채널에 기한이 된 배치가 쌓였는지 보고한다: 발송 대기 전달이 있고, **가장 오래된 것**의
// 나이가 요약 주기에 도달했는지.
//
// 판정 기준은 벽시계가 아니라 가장 오래된 전달의 나이다: 그래야 막 만든 채널이 정각에 맞춰
// 한 건뿐인 「요약」을 바로 내뱉지 않고, 오래 쌓인 배치도 한 주기를 헛되이 더 기다리지 않는다.
//
// ClaimDigestBatch 와 분리한 것은 의미가 다르기 때문이다: 이 함수는 「보내야 하는가」에만 답하고,
// 가져가기는 그 채널의 **모든** 발송 대기 행(아직 나이가 차지 않은 것 포함)을 가져가야 한다——그렇지 않으면 한 주기가
// 여러 메시지로 쪼개져 요약의 의미가 사라진다.
func (d *DB) DigestBatchDue(ctx context.Context, channelID int64, minAge time.Duration) (bool, error) {
	var due bool
	err := d.QueryRowContext(ctx, `SELECT EXISTS (
  SELECT 1 FROM notification_deliveries d
  JOIN notification_channels c ON c.id = d.channel_id
  WHERE d.channel_id = $1 AND d.state IN ($2,$3) AND c.enabled
  GROUP BY d.channel_id
  HAVING min(d.created_at) <= strftime('%Y-%m-%d %H:%M:%f','now','-' || $4 || ' seconds')
)`, channelID, NotifyStatePending, NotifyStateSending, int64(minAge.Seconds())).Scan(&due)
	return due, err
}

// ClaimDigestBatch 는 특정 채널에서 현재 기한이 된 발송 대기 전달을 요약 배치 하나로 가져가며,
// 배치 하나에 최대 MaxDigestBatchSize 건이다.
//
// 같은 배치의 모든 전달은 batch_id 를 공유하며, 집합 안의 가장 작은 id 를 배치 번호로 쓴다(안정적이고 읽기 쉬우며
// 별도 시퀀스가 필요 없다). 재시도할 때는 COALESCE 로 원래 배치 번호를 유지해, 「이 N 건은 함께 보낸 것」이
// 여러 번 재시도한 뒤에도 성립하게 한다.
//
// 무작위가 아니라 id 오름차순으로 앞 N 건을 가져간다: 가장 먼저 생긴 전달이 먼저 나가므로, 쌓였을 때
// 「새 취약점이 먼저 나가고 오래된 취약점은 영원히 뒤에 남는」 기아가 생기지 않는다.
func (d *DB) ClaimDigestBatch(ctx context.Context, channelID int64, limit int, lease time.Duration) ([]*NotificationDelivery, error) {
	if limit <= 0 {
		return nil, nil
	}
	// limit 은 **메모리 상한**이며 호출자는 MaxDigestBatchSize 를 넘긴다. 여기서 한 번 더 조여
	// 호출자가 더 큰 값을 넘기는 것을 막는다.
	//
	// 일부러 「속도 제한 할당량」을 배치 크기로 받지 않는다: 속도 제한의 단위는 메시지 수다——배치 하나는 메시지를
	// 하나만 보내고 토큰 하나를 소모하며, server 계층의 takeTokens 가 차감한다——이는 「배치 하나에 취약점을 몇 건 담는가」와는
	// 서로 다른 차원이다. 예전에 rate_per_min 을 digest 에 적용하려고 매 턴의
	// 요청 예산을 배치 크기로 넘겼더니, rate=20/min 인 채널은 배치마다 취약점을 1 건만 담아,
	// digest 가 요약 문구만 붙은 실시간 푸시로 퇴화했다. 속도 제한을 바꾸려면 takeTokens 의 want 를 바꾸고,
	// 여기를 건드리지 않는다.
	if limit > MaxDigestBatchSize {
		limit = MaxDigestBatchSize
	}
	out, err := d.claimDeliveries(ctx, lease, claimQuery{
		sql: `SELECT dd.id FROM notification_deliveries dd
JOIN notification_channels c ON c.id = dd.channel_id
WHERE dd.channel_id = $1 AND dd.state IN ($2,$3) AND dd.next_attempt_at <= (strftime('%Y-%m-%d %H:%M:%f','now')) AND c.enabled
ORDER BY dd.id
LIMIT $4`,
		args: []any{channelID, NotifyStatePending, NotifyStateSending, limit},
	}, func(tx *sql.Tx, ids []int64) error {
		batchID := ids[0]
		for _, id := range ids {
			if id < batchID {
				batchID = id
			}
		}
		ph, idArgs := placeholders(2, ids)
		_, err := tx.ExecContext(ctx, `UPDATE notification_deliveries SET batch_id = COALESCE(batch_id, $1)
WHERE id IN (`+ph+`)`, append([]any{batchID}, idArgs...)...)
		return err
	})
	return out, err
}

// claimDeliveries 는 「선택 + sending 설정 및 임대 연장 + 전체 행 읽기」를 모두 트랜잭션 하나에서 실행한다.
// postClaim 은 선택적인 추가 단계다(요약 배치는 이것으로 batch_id 를 쓴다).
func (d *DB) claimDeliveries(ctx context.Context, lease time.Duration, cq claimQuery, postClaim func(*sql.Tx, []int64) error) ([]*NotificationDelivery, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // 커밋에 성공한 뒤에는 no-op

	ids, err := selectForClaim(ctx, tx, cq.sql, cq.args...)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, tx.Commit()
	}
	// sending 으로 설정하고 next_attempt_at 을 미래로 민다: 이 미래 시각이 곧 임대 만료 시각이므로,
	// 「임대가 만료되지 않음」과 「재시도 시각이 되지 않음」이 같은 조건식을 공유해 새 열을 추가할 필요가 없다.
	ph, idArgs := placeholders(3, ids)
	if _, err := tx.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$1, attempts=attempts+1, next_attempt_at=strftime('%Y-%m-%d %H:%M:%f','now','+' || $2 || ' seconds')
WHERE id IN (`+ph+`)`,
		append([]any{NotifyStateSending, lease.Seconds()}, idArgs...)...); err != nil {
		return nil, err
	}
	if postClaim != nil {
		if err := postClaim(tx, ids); err != nil {
			return nil, err
		}
	}
	out, err := loadDeliveriesTx(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

func selectForClaim(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func loadDeliveriesTx(ctx context.Context, tx *sql.Tx, ids []int64) ([]*NotificationDelivery, error) {
	ph, args := placeholders(1, ids)
	rows, err := tx.QueryContext(ctx, joinedDeliveryQuery+` WHERE d.id IN (`+ph+`) ORDER BY d.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*NotificationDelivery{}
	for rows.Next() {
		dl, err := scanNotificationDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, dl)
	}
	return out, rows.Err()
}

// MarkDeliveriesSent 는 전달 묶음을 전달됨으로 표시한다.
func (d *DB) MarkDeliveriesSent(ctx context.Context, ids []int64) error {
	ph, args := placeholders(2, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$1, sent_at=(strftime('%Y-%m-%d %H:%M:%f','now')), last_error='' WHERE id IN (`+ph+`)`, append([]any{NotifyStateSent}, args...)...)
	return err
}

// RescheduleDeliveries 는 전달 묶음을 pending 으로 되돌리고 재시도 시각을 뒤로 미룬다.
//
// 새 중간 상태를 도입하지 않고 pending 으로 되돌리는 것은 「몇 번 기회가 남았는가」를 한 곳에서만
// 표현하게 하기 위함이다(MaxNotifyAttempts). 그래야 상태 기계의 분기가 재시도 정책에 따라 부풀지 않는다.
func (d *DB) RescheduleDeliveries(ctx context.Context, ids []int64, delay time.Duration, errMsg string) error {
	ph, args := placeholders(4, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$1, next_attempt_at=strftime('%Y-%m-%d %H:%M:%f','now','+' || $2 || ' seconds'), last_error=$3
WHERE id IN (`+ph+`)`,
		append([]any{NotifyStatePending, delay.Seconds(), truncateNotifyError(errMsg)}, args...)...)
	return err
}

// DeferDeliveries 는 전달 묶음을 pending 으로 되돌려 즉시 다시 가져갈 수 있게 하고, **가져갈 때 센 그 한 번의 시도를 취소한다**.
//
// 용도는 하나뿐이다: 요약 메시지를 채널 길이 상한에 따라 나눠 보낼 때, 이번 메시지에 담기지 못한 항목을 다음 배치로 남기는 것이다.
// 이는 실패가 아니므로 재시도 예산을 소모하면 안 된다——가져갈 때 attempts 가 이미 낙관적으로 +1 되었으니,
// 여기서 반드시 되돌려야 한다. 그렇지 않으면 500 건의 적체가 20 건씩 25 개 구간으로 잘릴 때,
// 끝부분 항목은 세 번째 구간에서 MaxNotifyAttempts 로 failed 판정을 받는다. 그것들은 한 번도 오류를 낸 적이 없는데도 말이다.
//
// GREATEST(...,0) 은 「누군가 수동으로 다시 보내 attempts 를 0 으로 만든 뒤 여기에 온」 경우를 막아,
// 수가 음수가 되지 않게 한다.
func (d *DB) DeferDeliveries(ctx context.Context, ids []int64, reason string) error {
	ph, args := placeholders(3, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$1, attempts=max(attempts-1, 0), next_attempt_at=(strftime('%Y-%m-%d %H:%M:%f','now')), last_error=$2
WHERE id IN (`+ph+`)`,
		append([]any{NotifyStatePending, truncateNotifyError(reason)}, args...)...)
	return err
}

// FailDeliveries 는 전달 묶음을 최종 실패로 표시하고, 사람이 전달 기록에서 다시 보내기를 기다린다.
func (d *DB) FailDeliveries(ctx context.Context, ids []int64, errMsg string) error {
	// 자리표시자는 $3 부터 시작한다: $1 은 state, $2 는 last_error.
	ph, args := placeholders(3, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries SET state=$1, last_error=$2 WHERE id IN (`+ph+`)`,
		append([]any{NotifyStateFailed, truncateNotifyError(errMsg)}, args...)...)
	return err
}

// RetryNotificationDelivery 는 전달 하나를 수동으로 다시 보낸다: pending 으로 초기화하고 재시도 횟수를 0 으로 만들며,
// 즉시 기한이 되게 한다. 횟수를 지우는 것은 의도적이다——사람이 「다시 보내기」를 누른 것은 이전 실패의 원인이 처리되었다는 뜻이므로,
// 예전 횟수로 제한할 이유가 없다.
func (d *DB) RetryNotificationDelivery(ctx context.Context, id int64) error {
	res, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$2, attempts=0, next_attempt_at=(strftime('%Y-%m-%d %H:%M:%f','now')), last_error=''
WHERE id=$1 AND state IN ($3,$4)`, id, NotifyStatePending, NotifyStateFailed, NotifyStateSkipped)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("전달 %d 가 없거나 현재 상태에서는 다시 보낼 수 없습니다", id)
	}
	return nil
}

// NotificationDeliveryFilter 는 전달 기록의 조회 조건이다.
type NotificationDeliveryFilter struct {
	ChannelID int64
	State     string
	EventKind string
}

func (f NotificationDeliveryFilter) where() (string, []any) {
	var conds []string
	var args []any
	if f.ChannelID > 0 {
		args = append(args, f.ChannelID)
		conds = append(conds, fmt.Sprintf("d.channel_id=$%d", len(args)))
	}
	if f.State != "" {
		args = append(args, f.State)
		conds = append(conds, fmt.Sprintf("d.state=$%d", len(args)))
	}
	if f.EventKind != "" {
		args = append(args, f.EventKind)
		conds = append(conds, fmt.Sprintf("e.kind=$%d", len(args)))
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// ListNotificationDeliveries 는 전달 기록을 페이지 단위로 최신순 반환한다.
func (d *DB) ListNotificationDeliveries(ctx context.Context, f NotificationDeliveryFilter, page, pageSize int) ([]*NotificationDelivery, int, error) {
	if page < 1 {
		page = 1
	}
	if pageSize <= 0 || pageSize > 200 {
		pageSize = 50
	}
	where, args := f.where()

	var total int
	if err := d.QueryRowContext(ctx, `SELECT count(*) FROM notification_deliveries d
JOIN notification_events e ON e.id = d.event_id`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	q := fmt.Sprintf("%s%s ORDER BY d.id DESC LIMIT $%d OFFSET $%d",
		joinedDeliveryQuery, where, len(args)+1, len(args)+2)
	rows, err := d.QueryContext(ctx, q, append(args, pageSize, (page-1)*pageSize)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []*NotificationDelivery{}
	for rows.Next() {
		dl, err := scanNotificationDelivery(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, dl)
	}
	return out, total, rows.Err()
}

// truncateNotifyError 는 오류 메시지를 열이 받을 수 있는 길이로 자른다. 채널이 반환한 응답 본문은 매우 길 수 있으며
// (일반 Webhook 이 자체 서비스로 보낼 때 특히 그렇다), 자르지 않으면 기록 목록의 페이로드가 부푼다.
func truncateNotifyError(msg string) string {
	const max = 500
	if len(msg) <= max {
		return msg
	}
	// 문자 경계에서 물러나 UTF-8 문자 반쪽이 남아 프론트엔드에서 깨져 보이는 것을 막는다.
	cut := max
	for cut > 0 && !isUTF8Start(msg[cut]) {
		cut--
	}
	return msg[:cut] + "…"
}

func isUTF8Start(b byte) bool { return b&0xC0 != 0x80 }

// placeholders 는 start 부터 시작하는 $n 자리표시자 문자열과 해당 파라미터를 만들어 IN (...) 에 쓴다.
// 예: start=3, ids=[7,8] → "$3,$4", [7,8].
func placeholders(start int, ids []int64) (string, []any) {
	ph := make([]string, 0, len(ids))
	args := make([]any, 0, len(ids))
	for i, id := range ids {
		ph = append(ph, fmt.Sprintf("$%d", start+i))
		args = append(args, id)
	}
	return strings.Join(ph, ","), args
}
