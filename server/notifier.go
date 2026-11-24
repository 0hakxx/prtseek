package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/notify"
)

// 전역 설정 키(settings 키-값 표에 있으며 테이블을 따로 만들 필요 없음).
const (
	// settingNotifyEnabled 는 푸시 전체 스위치다. 기본 켜짐: 유지보수 기간에 원클릭으로 지혈하는 용도이지
	// 기능의 활성화 조건이 아니다——진짜 활성화 조건은 「채널을 설정했는지」다.
	settingNotifyEnabled = "notify_enabled"
	// settingNotifyPublicBaseURL 은 취약점 상세 링크를 만드는 외부 접근 주소다
	// (예: https://prtseek.example.com). 비워 두면 메시지에 링크 버튼이 붙지 않는다.
	// 프로젝트에 재사용할 수 있는 외부 주소 설정이 없어 여기에 하나 추가한다.
	settingNotifyPublicBaseURL = "notify_public_base_url"
	// settingNotifyDigestMinutes 는 요약 모드의 주기(분)다.
	settingNotifyDigestMinutes = "notify_digest_interval_min"
)

const (
	// notifyTick 은 전달 엔진의 폴링 간격이다. 3 초는 이 엔진 실시간성의 상한이며,
	// 「취약점 저장」에서 「메시지가 IM 에 도착」까지의 주된 지연 원인이기도 하다.
	notifyTick = 3 * time.Second
	// notifyLease 는 전달을 가져갈 때의 임대 시간이다. 한 번의 전달 최악 소요 시간
	// (notify 패키지의 HTTP 클라이언트 시간 초과 15 초)보다 확연히 커야 한다. 그렇지 않으면 같은 행을 두
	// dispatcher 가 동시에 전달한다.
	notifyLease = 3 * time.Minute
	// notifyFanOutPerTick 은 턴마다 분배하는 이벤트 수를 제한해, 채널을 처음 켤 때
	// 과거 적체를 한 번에 전달 작업으로 펼치지 않게 한다.
	notifyFanOutPerTick = 200
	// notifyDefaultDigestMinutes 는 요약 주기의 기본값이다.
	notifyDefaultDigestMinutes = 30
	// notifyUnlimitedBurstPerTick 은 채널이 속도 제한을 설정하지 않았을 때의 턴당 전달 상한이다.
	// 존재 이유는 「채널 하나를 속도 제한 없음으로 설정 + 한 번에 취약점 수천 건 스캔」이
	// 한 턴의 루프를 오랫동안 블로킹하는 것을 막기 위함이다.
	notifyUnlimitedBurstPerTick = 50
	// notifyMaxSendsPerChannelPerTick 은 채널 하나가 턴마다 최대 몇 건을 전달하는지다.
	//
	// 이 상한은 **임대 시간**에서 역산한다: 가져갈 때 행에 찍는 것은 임대인데(notifyLease = 3 분),
	// 한 턴에 직렬로 전달하는 건수가 많아 최악 소요 시간이 임대를 넘으면, 뒤의 몇 건은 다 보내기 전에 임대가 만료된다.
	// 단일 프로세스 안에서는 상관없지만(Run 은 단일 goroutine 으로 직렬 실행하며 tick 은 재진입하지 않음), **두
	// 프로세스가 같은 DB 에 연결**되면 상대가 임대 만료된 행을 다시 가져가 중복 전송하고,
	// attempts 를 두 배로 증가시키며, 원래 프로세스가 아직 전달 중인데도 실패로 판정한다.
	//
	// 값: 3 분 임대 / 30 초 단일 시간 초과 = 6 은 **임대를 딱 채우는** 여유 0 이라
	// 쓸 수 없다. 5 로 하면 최악 소요 시간 150 초로 30 초 여유를 남긴다. 이 관계는
	// TestNotifyTickBudgetFitsWithinLease 가 못 박는다——notifyLease,
	// notifySendTimeout, 이 값 중 어느 하나만 바꿔도 그 단언이 실패한다.
	notifyMaxSendsPerChannelPerTick = 5
	// notifySendTimeout 은 한 번의 전달 시간 초과다. 이것은 동시에 위 상수의 값을 결정하며,
	// 둘을 곱한 값이 notifyLease 를 넘을 수 없다. TestNotifyTickBudgetFitsWithinLease 참고.
	notifySendTimeout = 30 * time.Second
)

// notifyBackoff 는 실패 재시도의 백오프 수열이며, 인덱스는 이미 시도한 횟수다.
// 3 번의 기회(첫 시도 포함)는 db.MaxNotifyAttempts 에 대응하며, 둘을 함께 바꿔야 한다.
var notifyBackoff = []time.Duration{
	time.Second,
	5 * time.Second,
	30 * time.Second,
}

// Notifier 는 취약점 푸시의 전달 엔진이다.
//
// Scheduler 와 나란히 독립 goroutine 으로 실행된다(server.New 참고). 일부러
// Scheduler 의 tick 을 재사용하지 않는다: 푸시의 실시간성 요구(3 초)는 트리거의 업무 리듬과 다르고,
// 둘의 실패는 서로 영향을 주지 않는다——푸시가 막혀도 agent 트리거에 영향을 주면 안 된다.
type Notifier struct {
	s  *Server
	pg *db.DB

	// mu 는 buckets 를 보호한다. 채널 수가 적고 경쟁이 낮아 뮤텍스 하나로 충분하며,
	// 이를 위해 더 세밀한 구조를 도입할 가치가 없다.
	mu      sync.Mutex
	buckets map[int64]*notifyBucket
}

// notifyBucket 은 채널 하나의 토큰 버킷이다.
//
// 「분당 세고 0 으로 초기화」하는 슬라이딩 윈도 대신 토큰 버킷을 쓰는 것은 후자의 경계 효과가 나쁘기 때문이다:
// 윈도 끝에서 20 건을 꽉 채워 보내고 다음 순간 다시 20 건을 보내면, 플랫폼에게는 1 초에 40 건이라
// 속도 제한에 걸린다. 토큰 버킷은 일정한 속도로 채워져 이런 돌발을 자연스럽게 피한다.
type notifyBucket struct {
	tokens   float64
	lastFill time.Time
}

func newNotifier(s *Server) *Notifier {
	return &Notifier{s: s, pg: s.m.pg, buckets: map[int64]*notifyBucket{}}
}

// Run 은 ctx 가 끝날 때까지 반복한다. server.New 가 한 번 시작한다.
func (n *Notifier) Run(ctx context.Context) {
	if n.pg == nil {
		return
	}
	t := time.NewTicker(notifyTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n.step(ctx)
		}
	}
}

// step 은 한 턴을 실행한다: 먼저 새 이벤트를 분배한 뒤 기한이 된 작업을 전달한다.
//
// 어느 단계든 실패하면 로그만 남기고 루프를 중단하지 않는다——알림 시스템의 장애가 프로세스 단위 문제로 번지면 절대 안 된다.
// 각 tick 은 독립적이며, 다음 턴에 자연스럽게 재시도한다.
func (n *Notifier) step(ctx context.Context) {
	if !n.enabled() {
		return
	}
	if _, _, err := n.pg.FanOutPendingEvents(ctx, notifyFanOutPerTick); err != nil {
		log.Printf("[notify] 이벤트 분배 실패: %v", err)
		return
	}
	channels, err := n.pg.ListNotificationChannels(ctx)
	if err != nil {
		log.Printf("[notify] 채널 읽기 실패: %v", err)
		return
	}
	baseURL := n.publicBaseURL()
	for _, ch := range channels {
		if !ch.IsEnabled() {
			continue
		}
		// 토큰 버킷의 계량 단위는 **메시지 건수**(HTTP 요청 수와 같음)이지 취약점 건수가 아니다.
		// 실시간 모드에서는 둘이 같지만(취약점 하나에 메시지 하나), 요약 모드에서는 취약점 한 배치가 메시지
		// 하나로 합쳐지므로 토큰 하나만 소모한다.
		//
		// 두 모드 모두 먼저 토큰 버킷에 묻고 그다음 할당량만큼 가져간다——순서를 바꾸면 안 된다. 그렇지 않으면 속도 제한에 막힌
		// 전달이 이미 재시도 횟수를 소모한다.
		now := time.Now()
		if ch.Mode == db.NotifyModeDigest {
			tokens, claimLimit := digestTickPlan()
			if n.takeTokens(ch.ID, ch.RatePerMin, tokens, now) <= 0 {
				continue
			}
			n.stepDigest(ctx, ch, claimLimit, baseURL)
			continue
		}
		allow := n.takeTokens(ch.ID, ch.RatePerMin, notifyMaxSendsPerChannelPerTick, now)
		if allow <= 0 {
			continue
		}
		n.stepRealtime(ctx, ch, allow, baseURL)
	}
}

// digestTickPlan 은 요약 채널의 이번 턴 토큰 소모와 배치 크기 상한을 반환한다.
//
// 두 반환값은 **서로 다른 두 차원**이며, 이것이 바로 함수로 따로 뺀 이유다:
//
//   - tokens 는 메시지 건수다. 취약점 한 배치가 메시지 하나로 합쳐져 HTTP 요청을 한 번 보내므로 항상 1 이다.
//     그래서 rate_per_min 은 여전히 digest 에 적용된다(분당 최대 이만큼의 요약 메시지).
//   - claimLimit 은 이 배치에 최대 몇 건의 취약점을 담을지다. 메모리 상한에만 제약되며 요청 예산과는 무관하다.
//
// 예전에 rate_per_min 을 digest 에 적용하려고 턴당 요청 예산
// (notifyMaxSendsPerChannelPerTick, 임대에서 역산)을 배치 크기로 그대로 넘겼다.
// 그 결과 rate_per_min=20 인 채널은 3 초 tick 에서 토큰을 1 개만 보충해, 요약 메시지마다
// 취약점을 1 개만 담았다——digest 가 「요약 문구가 붙은 실시간 푸시」로 퇴화해, 읽는 사람은
// 「최근 30 분 동안 새 취약점 1 건」의 연속을 받고 db.MaxDigestBatchSize 에는 영원히 도달하지 못했다.
//
// 이 증상은 종단 간 테스트에서 발견하기 어려워(기존 테스트는 모두 충분히 큰 limit 를
// stepDigest 에 수동으로 넘겨 step 의 할당량 계산을 우회함), 결정을 여기에 모아
// TestDigestTickPlanDecouplesBatchSizeFromSendBudget 가 직접 못 박는다.
func digestTickPlan() (tokens, claimLimit int) {
	return 1, db.MaxDigestBatchSize
}

// stepRealtime 은 특정 채널의 실시간 작업을 가져와 전달한다. 취약점 하나에 메시지 하나.
func (n *Notifier) stepRealtime(ctx context.Context, ch *db.NotificationChannel, allow int, baseURL string) {
	deliveries, err := n.pg.ClaimRealtimeDeliveries(ctx, ch.ID, allow, notifyLease)
	if err != nil {
		log.Printf("[notify] 실시간 전달 가져오기 실패 channel=%d: %v", ch.ID, err)
		return
	}
	if len(deliveries) == 0 {
		return
	}
	channel, cfg, ok := n.adapt(ch)
	if !ok {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), fmt.Sprintf("채널 유형 %q 가 등록되지 않았습니다", ch.Kind))
		return
	}
	for _, dl := range deliveries {
		msg, err := n.renderSingle(ctx, dl, baseURL)
		if err != nil {
			// 렌더링 실패는 로컬 데이터 문제라 재시도해도 나아지지 않는다.
			_ = n.pg.FailDeliveries(ctx, []int64{dl.ID}, err.Error())
			continue
		}
		n.send(ctx, channel, cfg, msg, []*db.NotificationDelivery{dl})
	}
}

// stepDigest 는 배치가 기한이 되면 특정 채널의 발송 대기 전달을 모아 메시지 하나로 보낸다.
func (n *Notifier) stepDigest(ctx context.Context, ch *db.NotificationChannel, allow int, baseURL string) {
	window := n.digestInterval()
	due, err := n.pg.DigestBatchDue(ctx, ch.ID, window)
	if err != nil {
		log.Printf("[notify] 요약 배치 판단 실패 channel=%d: %v", ch.ID, err)
		return
	}
	if !due {
		return
	}
	deliveries, err := n.pg.ClaimDigestBatch(ctx, ch.ID, allow, notifyLease)
	if err != nil {
		log.Printf("[notify] 요약 배치 가져오기 실패 channel=%d: %v", ch.ID, err)
		return
	}
	if len(deliveries) == 0 {
		return
	}
	channel, cfg, ok := n.adapt(ch)
	if !ok {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), fmt.Sprintf("채널 유형 %q 가 등록되지 않았습니다", ch.Kind))
		return
	}
	msg, included, err := n.renderBatch(ctx, deliveries, baseURL, int(window.Minutes()))
	if err != nil {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), err.Error())
		return
	}
	// 스냅샷이 망가져 메시지에 들어가지 못한 전달은 명시적으로 실패로 판정해야 한다. 그러지 않으면 그것들이
	// included 밖에 남아 메시지에도 실패 목록에도 들어가지 않는다——전송에 성공하면 그 상태가
	// 이후의 일괄 표시에서 누락되어, 임대가 만료되어 반복해서 가져가질 때까지 영원히 sending 에 머문다.
	if skipped := excludeDeliveries(deliveries, included); len(skipped) > 0 {
		reason := "이벤트 스냅샷을 파싱할 수 없어 이 취약점을 메시지로 렌더링할 수 없습니다"
		if fErr := n.pg.FailDeliveries(ctx, deliveryIDs(skipped), reason); fErr != nil {
			log.Printf("[notify] 잘못된 스냅샷 전달 실패 표시 channel=%s ids=%v: %v", ch.Kind, deliveryIDs(skipped), fErr)
		}
		log.Printf("[notify] 스냅샷을 파싱할 수 없는 전달 %d 건 건너뜀 channel=%d", len(skipped), ch.ID)
	}
	// 메시지에 들어간 것만 send 에 넘긴다: included[i] 와 msg.Items[i] 가 엄격히 대응하며,
	// send 는 이 대응 관계에 의존해 「채널이 앞 K 건을 담았다」를 올바른 전달 행에 반영한다.
	n.send(ctx, channel, cfg, msg, included)
}

// send 는 전달하고 결과에 따라 상태를 전이시킨다.
//
// 같은 배치의 전달(요약 모드에서는 수십 건일 수 있음)은 하나의 전송 결과를 공유한다: 전달되거나, 배치 전체를 재시도한다.
// 건별 재시도는 하지 않는다——요약 메시지는 하나이므로 그중 일부만 다시 보내면 배치 의미가 어긋난다.
//
// 유일한 예외는 **채널 길이 상한으로 인한 분할**이다: 채널이 실제로 앞 K 건만 담았다고 보고하면,
// K+1 건부터는 반드시 다음 배치로 남겨야 하며 함께 성공으로 표시하면 안 된다. 그렇지 않으면 잘린
// 취약점들이 메시지에도 실패 목록에도 없이 완전히 사라진다.
func (n *Notifier) send(ctx context.Context, channel notify.Channel, cfg map[string]any, msg notify.Message, deliveries []*db.NotificationDelivery) {
	// 한 번의 전달에 상한을 두어, 어떤 채널이 막혀 이번 턴의 남은 채널을 모두 붙잡는 것을 막는다.
	sendCtx, cancel := context.WithTimeout(ctx, notifySendTimeout)
	defer cancel()
	delivered, err := channel.Send(sendCtx, cfg, msg)
	if err == nil && delivered > 0 {
		if delivered > len(deliveries) {
			// 채널이 보고한 건수는 전달 수를 넘을 수 없다. 정말 그런 일이 생기면 렌더링 계층이 잘못 계산한 것이므로,
			// 전부 전달된 것으로 처리하고 문제를 기록하는 편이 기록을 엉망으로 쓰는 것보다 낫다.
			log.Printf("[notify] 채널 보고 전달 건수 %d 가 전달 수 %d 를 초과함 channel=%s, 전부 전달된 것으로 처리",
				delivered, len(deliveries), channel.Kind())
			delivered = len(deliveries)
		}
		sent, rest := deliveries[:delivered], deliveries[delivered:]
		if err := n.pg.MarkDeliveriesSent(ctx, deliveryIDs(sent)); err != nil {
			log.Printf("[notify] 전달됨 표시 실패 channel=%s ids=%v: %v", channel.Kind(), deliveryIDs(sent), err)
		}
		if len(rest) > 0 {
			// 이 메시지가 채널 길이 상한에 도달함: 나머지를 즉시 대기열로 돌려보내 다음 tick 이 이어서 보낸다.
			// RescheduleDeliveries 가 아니라 DeferDeliveries 를 쓴다 —— 이것은 실패가 아니므로
			// 재시도 예산을 소모하면 안 된다(가져갈 때 이미 낙관적으로 +1 했으며, 거기서 되돌린다).
			if err := n.pg.DeferDeliveries(ctx, deliveryIDs(rest),
				fmt.Sprintf("이 메시지가 채널 길이 상한에 도달해 앞 %d 건만 전달했고, 나머지는 다음 배치로 남깁니다", delivered)); err != nil {
				log.Printf("[notify] 분할 이어 보내기 대기열 추가 실패 channel=%s ids=%v: %v", channel.Kind(), deliveryIDs(rest), err)
			}
		}
		return
	}
	if err == nil {
		// 채널이 오류도 내지 않고 몇 건을 전달했는지도 말하지 않음. 실패로 처리한다(백오프를 탐).
		// 그러지 않으면 이 전달이 반복해서 가져가지지만 영원히 표시되지 않는다.
		err = fmt.Errorf("채널이 전달 건수를 보고하지 않았습니다(delivered=%d)", delivered)
	}

	// 실패 처리는 배치의 최대 시도 횟수가 아니라 **건별**로 결정한다.
	//
	// 예전에는 `if maxAttempts(deliveries) >= MaxNotifyAttempts` 로 배치 전체를 사망 판정했지만, 배치 안의
	// 각 건의 시도 횟수는 서로 다르다: 이미 두 번 재시도한 오래된 전달(attempts=2)이 같은 배치의
	// 새 전달(attempts=1)까지 함께 failed 로 끌고 간다——새 취약점이 재시도 한 번도 쓰지 못하고 영구히 사라져,
	// 「오래된 행이 새 행을 끌어들이지 못하게」라는 본래 의도와 정반대가 된다.
	permanent := notify.IsPermanent(err)
	var failIDs, exhaustedIDs []int64
	byDelay := map[time.Duration][]int64{}
	for _, dl := range deliveries {
		switch {
		case permanent:
			failIDs = append(failIDs, dl.ID)
		case dl.Attempts >= db.MaxNotifyAttempts:
			exhaustedIDs = append(exhaustedIDs, dl.ID)
		default:
			delay := notifyBackoff[min(dl.Attempts, len(notifyBackoff)-1)]
			byDelay[delay] = append(byDelay[delay], dl.ID)
		}
	}

	if len(failIDs) > 0 {
		if fErr := n.pg.FailDeliveries(ctx, failIDs, err.Error()); fErr != nil {
			log.Printf("[notify] 실패 상태 표시 오류 channel=%s ids=%v: %v", channel.Kind(), failIDs, fErr)
		}
	}
	if len(exhaustedIDs) > 0 {
		reason := fmt.Sprintf("%d 번 재시도했지만 여전히 실패: %s", db.MaxNotifyAttempts, err)
		if fErr := n.pg.FailDeliveries(ctx, exhaustedIDs, reason); fErr != nil {
			log.Printf("[notify] 실패 상태 표시 오류 channel=%s ids=%v: %v", channel.Kind(), exhaustedIDs, fErr)
		}
	}
	// 지연별로 묶어 다시 정렬한다: 백오프 단계가 3 개뿐이라 그룹 수가 자연히 적으므로, 건마다 따로
	// UPDATE 를 보낼 필요가 없다(그러면 500 건 배치가 500 번 왕복한다).
	for delay, group := range byDelay {
		if rErr := n.pg.RescheduleDeliveries(ctx, group, delay, err.Error()); rErr != nil {
			log.Printf("[notify] 전달 재정렬 실패 channel=%s ids=%v: %v", channel.Kind(), group, rErr)
		}
	}
	if len(failIDs)+len(exhaustedIDs) > 0 {
		log.Printf("[notify] 전달 실패 channel=%d kind=%s 영구 실패=%d 재시도 소진=%d 재시도 대기=%d: %s",
			deliveries[0].ChannelID, channel.Kind(), len(failIDs), len(exhaustedIDs), len(byDelay), err)
	}
}

// excludeDeliveries 는 all 중 keep 에 없는 것들을 반환한다(포인터 신원으로 비교).
// 「메시지에 들어가지 못한」 전달을 찾는 데 쓴다——그것들은 반드시 명시적으로 처리해야 하며 회색 지대에 남겨 둘 수 없다.
func excludeDeliveries(all, keep []*db.NotificationDelivery) []*db.NotificationDelivery {
	inKeep := make(map[*db.NotificationDelivery]bool, len(keep))
	for _, dl := range keep {
		inKeep[dl] = true
	}
	var out []*db.NotificationDelivery
	for _, dl := range all {
		if !inKeep[dl] {
			out = append(out, dl)
		}
	}
	return out
}

// adapt 는 채널 구현을 가져와 그 설정을 해석한다.
// ok=false 를 반환하면 유형이 등록되지 않았다는 뜻이며, 전달은 무한 재시도하지 않고 바로 실패로 판정해야 한다.
func (n *Notifier) adapt(ch *db.NotificationChannel) (notify.Channel, map[string]any, bool) {
	channel, ok := notify.Get(ch.Kind)
	if !ok {
		return nil, nil, false
	}
	var cfg map[string]any
	if len(ch.Config) > 0 {
		// 설정 해석에 실패하면 빈 map 을 준다: 채널 자체의 Validate 가 「어떤 필드가 빠졌는지」를 알려 주며,
		// 그 오류가 JSON 파싱 오류보다 사용자의 수정을 더 잘 안내한다.
		_ = json.Unmarshal(ch.Config, &cfg)
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	return channel, cfg, true
}

// renderSingle 은 단건 취약점 메시지를 렌더링한다.
func (n *Notifier) renderSingle(ctx context.Context, dl *db.NotificationDelivery, baseURL string) (notify.Message, error) {
	snap, err := parseSnapshot(dl)
	if err != nil {
		return notify.Message{}, err
	}
	item, err := n.itemFor(ctx, snap, baseURL)
	if err != nil {
		return notify.Message{}, err
	}
	return notify.Message{Items: []notify.Item{item}, HomeURL: baseURL}, nil
}

// renderBatch 는 요약 메시지를 렌더링한다. 스냅샷을 하나씩 파싱한다——하나가 망가지면 그 하나만 건너뛰고,
// 그것이 요약 배치 전체를 날리지 않게 한다.
//
// 반환값 included 는 msg.Items 와 **엄격히 일대일 대응**한다(i 번째 전달 ↔ i 번째 항목).
// 이 대응 관계는 절대 조건이다: 호출자는 「채널이 앞 K 건을 담았다」에 따라 앞 K 개 전달을
// 전달됨으로 표시한다. 여기서 잘못된 스냅샷을 건너뛰면서 그 전달을 included 에서 빼지 않으면,
// 인덱스가 어긋난다——실패해야 할 잘못된 항목이 전달됨으로 표시되고, 멀쩡한 항목이 미전달로 오판된다.
// 망가진 것들은 호출자가 명시적으로 실패로 표시한다. stepDigest 참고.
func (n *Notifier) renderBatch(ctx context.Context, deliveries []*db.NotificationDelivery, baseURL string, windowMinutes int) (notify.Message, []*db.NotificationDelivery, error) {
	items := make([]notify.Item, 0, len(deliveries))
	included := make([]*db.NotificationDelivery, 0, len(deliveries))
	for _, dl := range deliveries {
		snap, err := parseSnapshot(dl)
		if err != nil {
			// 잘못된 스냅샷은 메시지에도 included 에도 들어가지 않는다——그 처리는 호출자가 책임진다
			// (「전달됨」에 섞어 얼버무리지 않고 명시적으로 실패로 표시).
			log.Printf("[notify] 요약 배치에서 파싱할 수 없는 스냅샷 건너뜀 delivery=%d: %v", dl.ID, err)
			continue
		}
		item, err := n.itemFor(ctx, snap, baseURL)
		if err != nil {
			return notify.Message{}, nil, err
		}
		items = append(items, item)
		included = append(included, dl)
	}
	if len(items) == 0 {
		return notify.Message{}, nil, fmt.Errorf("요약 배치의 전달 %d 건을 모두 파싱할 수 없습니다", len(deliveries))
	}
	return notify.Message{
		Items:         items,
		Batch:         true,
		WindowMinutes: windowMinutes,
		HomeURL:       baseURL,
	}, included, nil
}

// itemFor 는 이벤트 스냅샷을 푸시할 항목으로 렌더링하며, 자산 이름과 상세 링크도 함께 해석한다.
func (n *Notifier) itemFor(ctx context.Context, snap notify.Snapshot, baseURL string) (notify.Item, error) {
	assets, err := n.pg.NotificationAssetNames(ctx, snap.AssetIDs)
	if err != nil {
		// 자산 이름 해석 실패가 푸시를 막아서는 안 된다: 이름을 못 읽는 것이 알림을 못 받는 것보다 훨씬 가벼우며,
		// 메시지에 자산 한 줄이 빠질 뿐이다.
		log.Printf("[notify] 자산 이름 해석 실패 finding=%d: %v", snap.FindingID, err)
	}
	item := notify.Item{
		FindingID:  snap.FindingID,
		Name:       snap.Name,
		VulnClass:  snap.VulnClass,
		Severity:   snap.Severity,
		Summary:    snap.Summary,
		Assets:     assets,
		FromStatus: snap.FromStatus,
		ToStatus:   snap.ToStatus,
	}
	if baseURL != "" {
		// 상세 페이지 라우트는 web/src/app/(main)/function/findings/detail/page.tsx 참고,
		// query 파라미터 id 에서 취약점 id 를 읽는다.
		item.DetailURL = fmt.Sprintf("%s/function/findings/detail?id=%d", baseURL, snap.FindingID)
	}
	return item, nil
}

// takeTokens 는 채널 토큰 버킷에서 **최대 want 개**의 토큰을 가져오고, 실제로 가져온 수를 반환한다.
//
// 토큰 하나 = 메시지 하나(HTTP 요청 한 번). 실시간 모드에서는 호출자가 원하는 수만큼 넘기고,
// 요약 모드에서는 취약점 한 배치가 메시지 하나만 보내므로 1 을 넘긴다.
//
// 버킷 용량은 그 채널의 분당 상한이며 일정한 속도로 채워진다. ratePerMin<=0 은 속도 제한 없음이라,
// 유한하지만 충분히 큰 값을 반환해 한 턴의 루프가 무한 적체에 붙잡히지 않게 한다.
//
// want 라는 이 상한은 필수다: 없으면 버킷을 통째로 비우는 수밖에 없는데, 호출자는 자체적으로 턴당 상한이 있어
// 더 가져간 토큰은 쓰이지도 않고 다음 보충 전에 사라진다——쌓인 돌발 용량에 영원히 도달하지 못하고,
// 「이번 턴에 발송 대기 전달이 전혀 없음」도 그냥 한 번 차감된다.
func (n *Notifier) takeTokens(channelID int64, ratePerMin, want int, now time.Time) int {
	if want <= 0 {
		return 0
	}
	if ratePerMin <= 0 {
		return min(want, notifyUnlimitedBurstPerTick)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	b := n.buckets[channelID]
	if b == nil {
		b = &notifyBucket{tokens: float64(ratePerMin), lastFill: now}
		n.buckets[channelID] = b
	}
	// 경과한 실제 시간에 따라 보충하며, 속도는 초당 ratePerMin/60 이다.
	if elapsed := now.Sub(b.lastFill).Seconds(); elapsed > 0 {
		b.tokens = minF(float64(ratePerMin), b.tokens+elapsed*float64(ratePerMin)/60)
		b.lastFill = now
	}
	// 아주 작은 epsilon 을 더한 뒤 정수로 내린다: 토큰 수는 부동소수점 누적이라, 두 번에 나눠 채울 때
	// 0.5 + 0.5 가 0.9999999999 가 될 수 있어 바로 int() 하면 0 으로 잘린다——
	// 수학적으로는 가득 찬 버킷인데 토큰을 못 꺼낸다. 1e-9 는 토큰 하나보다 훨씬 작아 진짜 부족분을 놓치지 않는다.
	take := min(int(b.tokens+1e-9), want)
	if take <= 0 {
		return 0
	}
	b.tokens -= float64(take)
	return take
}

// enabled 는 전체 스위치를 읽는다.
func (n *Notifier) enabled() bool {
	return n.pg.GetBool(settingNotifyEnabled, true)
}

// publicBaseURL 은 링크용 외부 주소를 반환하며 끝의 슬래시를 뗀다.
func (n *Notifier) publicBaseURL() string {
	v, ok, err := n.pg.GetSetting(settingNotifyPublicBaseURL)
	if err != nil || !ok {
		return ""
	}
	return trimTrailingSlash(v)
}

// digestInterval 은 요약 주기를 반환하며, 잘못되었거나 설정되지 않으면 기본값으로 돌아간다.
func (n *Notifier) digestInterval() time.Duration {
	v, ok, err := n.pg.GetSetting(settingNotifyDigestMinutes)
	if err != nil || !ok {
		return time.Duration(notifyDefaultDigestMinutes) * time.Minute
	}
	m := 0
	if _, err := fmt.Sscanf(v, "%d", &m); err != nil || m <= 0 {
		return time.Duration(notifyDefaultDigestMinutes) * time.Minute
	}
	return time.Duration(m) * time.Minute
}

// parseSnapshot 은 전달에 대응하는 이벤트의 스냅샷을 파싱한다.
func parseSnapshot(dl *db.NotificationDelivery) (notify.Snapshot, error) {
	var snap notify.Snapshot
	if len(dl.Snapshot) == 0 {
		return snap, fmt.Errorf("전달 %d 의 이벤트 스냅샷이 비어 있습니다", dl.ID)
	}
	if err := json.Unmarshal(dl.Snapshot, &snap); err != nil {
		return snap, fmt.Errorf("전달 %d 의 이벤트 스냅샷 파싱 실패: %w", dl.ID, err)
	}
	if snap.Kind == "" {
		// 이벤트 유형은 이벤트 행을 기준으로 한다. 스냅샷에 있는 것은 이전 버전이 썼을 수 있다.
		snap.Kind = dl.EventKind
	}
	return snap, nil
}

func deliveryIDs(deliveries []*db.NotificationDelivery) []int64 {
	out := make([]int64, 0, len(deliveries))
	for _, dl := range deliveries {
		out = append(out, dl.ID)
	}
	return out
}

func trimTrailingSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
