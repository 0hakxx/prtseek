package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/artex/selfupdate"
)

// 페이지 원클릭 업데이트의 HTTP 측면. 실제 다운로드/검증/교체 로직은 모두 selfupdate 패키지에 있고,
// 여기서는 인증 경계, 동시 실행 상호 배제, 진행 상황 전파, 그리고 "이제 종료할 때"를 main 에 알리는 것만 담당한다.
//
// 재시작은 이 프로세스가 하지 않는다: 새 버전을 스테이징한 뒤 프로세스가 selfupdate.ExitRestart 로 종료하고,
// 감시 스크립트(start.sh / start.bat, Docker 에서는 ENTRYPOINT)가 다시 띄운다.

// restartCh 는 업그레이드가 준비되거나 롤백이 완료되면 닫히며, main 이 받으면 ExitRestart 로 종료한다.
var (
	restartOnce sync.Once
	restartCh   = make(chan struct{})
)

// IsPersonalBuild keeps the fork outside the upstream updater across branding
// changes. Both historical and current version prefixes identify this fork.
func IsPersonalBuild(version string) bool {
	return strings.HasPrefix(version, "personal-") || strings.HasPrefix(version, "prtseek-")
}

// RestartRequested 는 "종료하고 감시 프로세스가 나를 다시 띄워 달라"일 때 닫히는 channel 을 반환한다.
func RestartRequested() <-chan struct{} { return restartCh }

func requestRestart() { restartOnce.Do(func() { close(restartCh) }) }

// bootState 는 이번 시작 시 selfupdate.Bootstrap 의 결론이다(업그레이드 성공 / 방금 롤백 /
// 스테이징 파일 버려짐). main 이 주입하며, /api/update/check 가 지난 업그레이드의 결말을 프론트엔드에 있는 그대로 알리는 데 쓴다.
var (
	bootStateMu sync.Mutex
	bootState   selfupdate.State
)

// SetBootUpdateState 는 main 이 시작할 때 한 번 호출한다.
func SetBootUpdateState(st selfupdate.State) {
	bootStateMu.Lock()
	defer bootStateMu.Unlock()
	bootState = st
}

func bootUpdateState() selfupdate.State {
	bootStateMu.Lock()
	defer bootStateMu.Unlock()
	return bootState
}

// releaseCache 는 GitHub 의 최신 버전 조회 결과를 캐시한다.
//
// 상단 바의 "새 버전 있음" 안내는 전체 페이지가 로드될 때마다 한 번 조회하는데, 인증하지 않은 GitHub API 는
// IP 당 시간당 60 회다——캐시하지 않으면 탭을 몇 개 열거나 페이지를 몇 번 새로 고치면 할당량을 다 쓰고,
// 정작 업데이트하려 할 때 조회가 안 된다. 사용자가 "업데이트 확인"을 명시적으로 누를 때는 force 로 캐시를 우회할 수 있다.
type releaseCache struct {
	mu      sync.Mutex
	rel     *selfupdate.Release
	err     error
	at      time.Time
	network string
	// fetch 는 데이터 가져오기 함수이며 테스트용 주입 지점일 뿐이다. nil 이면 진짜 GitHub 조회를 탄다.
	fetch func(context.Context, *http.Client) (*selfupdate.Release, error)
}

const (
	releaseTTL = 30 * time.Minute
	// 실패 결과도 잠깐 캐시한다. 그렇지 않으면 GitHub 에 도달할 수 없을 때 페이지 로드마다 시간 초과를 한 번씩 기다려야 한다.
	// 다만 TTL 은 짧게 둬서, 네트워크가 회복되면 금방 스스로 좋아지게 한다.
	releaseErrTTL = 2 * time.Minute
	// 조회용 시간 초과. NewClient 의 30 분 시간 초과는 패키지 전체를 다운로드하는 데 쓰는 것이며, 버전 조회는 그렇게 오래 기다릴 수 없다.
	releaseTimeout = 20 * time.Second
)

var relCache = &releaseCache{}

// get 은 최신 Release 를 반환하며, 캐시가 적중하면 네트워크에 접근하지 않는다.
//
// 데이터를 가져오는 동안 계속 잠금을 쥔다: 동시 요청은 각자 GitHub 를 치지 않고 같은 조회의 결과를 대기열에서 기다린다
// (페이지가 막 로드될 때 여러 탭이 동시에 조회하는 것이 바로 속도 제한을 가장 쉽게 유발하는 순간이다).
func (c *releaseCache) get(ctx context.Context, client *http.Client, force bool, network string) (*selfupdate.Release, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !force && c.network == network {
		ttl := releaseTTL
		if c.err != nil {
			ttl = releaseErrTTL
		}
		if !c.at.IsZero() && time.Since(c.at) < ttl {
			return c.rel, c.err
		}
	}

	fetch := c.fetch
	if fetch == nil {
		fetch = selfupdate.FetchLatest
	}
	ctx, cancel := context.WithTimeout(ctx, releaseTimeout)
	defer cancel()
	rel, err := fetch(ctx, client)
	// 요청이 취소됨(사용자가 탭을 닫음)은 GitHub 에 문제가 있다는 뜻이 아니므로 캐시에 쓰지 않는다.
	// 그렇지 않으면 다음 방문자가 영문 모를 "취소됨" 오류를 받는다.
	if err != nil && ctx.Err() != nil && errors.Is(ctx.Err(), context.Canceled) {
		return c.rel, err
	}
	c.rel, c.err, c.at, c.network = rel, err, time.Now(), network
	return rel, err
}

// updateProgress 는 프론트엔드로 보내는 진행 상황 하나다.
type updateProgress struct {
	Phase   selfupdate.Phase `json:"phase"`
	Percent int              `json:"percent"` // 다운로드 단계에서만 의미가 있음. 나머지는 -1
	Message string           `json:"message"`
	Version string           `json:"version,omitempty"`
	Error   string           `json:"error,omitempty"`
}

// updateHub 는 한 번의 업그레이드 진행 상황을 쥐고 SSE 구독자에게 전파한다.
//
// running 은 동시에 상호 배제 역할을 한다: 업그레이드 중에 다시 POST /api/update/apply 하면 바로 409 로,
// 두 goroutine 이 동시에 같은 prtseek.new 에 쓰는 것을 막는다.
type updateHub struct {
	mu      sync.Mutex
	running bool
	cur     updateProgress
	subs    map[chan updateProgress]struct{}
}

var updHub = &updateHub{
	cur:  updateProgress{Phase: selfupdate.PhaseIdle, Percent: -1},
	subs: map[chan updateProgress]struct{}{},
}

// begin 은 업그레이드 권한을 선점하며, 이미 진행 중이면 false 를 반환한다.
func (h *updateHub) begin(version string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.running {
		return false
	}
	h.running = true
	h.cur = updateProgress{Phase: selfupdate.PhaseDownload, Percent: 0, Message: "준비 중…", Version: version}
	h.fanout(h.cur)
	return true
}

// finish 는 한 번의 업그레이드를 끝낸다. err 가 nil 이면 스테이징 성공이며 재시작을 기다린다.
func (h *updateHub) finish(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.running = false
	if err != nil {
		h.cur = updateProgress{Phase: selfupdate.PhaseFailed, Percent: -1, Message: "업데이트 실패", Error: err.Error(), Version: h.cur.Version}
	} else {
		h.cur = updateProgress{Phase: selfupdate.PhaseStaged, Percent: 100, Message: "새 버전이 준비되어 재시작하는 중…", Version: h.cur.Version}
	}
	h.fanout(h.cur)
}

func (h *updateHub) publish(ph selfupdate.Phase, pct int, msg string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cur = updateProgress{Phase: ph, Percent: pct, Message: msg, Version: h.cur.Version}
	h.fanout(h.cur)
}

// fanout 은 반드시 h.mu 를 쥐고 호출해야 한다. 구독자 channel 은 버퍼가 있고, 가득 차면 버린다——
// 진행 상황은 버려도 되는 일시적 정보이며, 막힌 SSE 연결 하나가 업그레이드 자체를 막게 해서는 절대 안 된다.
func (h *updateHub) fanout(p updateProgress) {
	for ch := range h.subs {
		select {
		case ch <- p:
		default:
		}
	}
}

func (h *updateHub) snapshot() (updateProgress, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cur, h.running
}

func (h *updateHub) subscribe() (<-chan updateProgress, func()) {
	ch := make(chan updateProgress, 64)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs, ch)
			h.mu.Unlock()
			close(ch)
		})
	}
}

// updateCheck 는 GitHub 의 최신 정식 버전을 조회해 현재 버전과 비교한다.
//
// 프론트엔드도 api.github.com 에 직접 연결하지만(GitHub 의 CORS 는 *) **이 인터페이스를 기준으로 한다**:
// 다운로드는 백엔드가 하므로, 백엔드가 GitHub 에 접근할 수 있어야만 업데이트를 말할 수 있다. 브라우저는 연결되는데 서버는 연결되지 않는
// 경우가 흔하다(서버가 내부망에 있거나, 프록시가 브라우저에만 설정됨). 그때 업데이트를 누르면 반드시 실패하므로,
// 확인 단계에서 있는 그대로 오류를 내는 편이 낫다.
func (s *Server) updateCheck(w http.ResponseWriter, r *http.Request) {
	if IsPersonalBuild(BuildVersion) {
		writeJSON(w, 200, map[string]any{"current": BuildVersion, "mode": "personal", "has_update": false, "has_backup": false, "reason": "개인판은 상위 프로젝트 원클릭 업데이트를 꺼서 2차 개발 수정이 덮어써지는 것을 막습니다"})
		return
	}
	current := BuildVersion
	mode := "binary"
	if selfupdate.InDocker() {
		mode = "docker"
	}
	boot := bootUpdateState()
	out := map[string]any{
		"current":     current,
		"mode":        mode,
		"os":          runtime.GOOS,
		"arch":        runtime.GOARCH,
		"has_backup":  selfupdate.HasBackup(),
		"repo":        selfupdate.Repo,
		"boot_notice": boot.Detail,
		"rolled_back": boot.RolledBack,
	}

	// 상단 바 안내는 캐시를 탄다(기본). 사용자가 "업데이트 확인"을 누를 때는 force=1 로 강제로 원본에서 조회한다.
	force := r.URL.Query().Get("force") != ""
	client, network, err := s.updateClient()
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	defer client.CloseIdleConnections()
	rel, err := relCache.get(r.Context(), client, force, network)
	if err != nil {
		out["error"] = err.Error()
		writeJSON(w, 200, out)
		return
	}

	latest := rel.TagName
	out["latest"] = latest
	out["notes"] = rel.Body
	out["html_url"] = rel.HTMLURL
	if !rel.PublishedAt.IsZero() {
		out["published_at"] = rel.PublishedAt.Format(time.RFC3339)
	}

	asset := selfupdate.AssetName(latest, runtime.GOOS, runtime.GOARCH)
	out["asset"] = asset
	if a, ok := rel.FindAsset(asset); ok {
		out["asset_available"] = true
		out["size"] = a.Size
	} else {
		out["asset_available"] = false
	}

	cmp, comparable := selfupdate.CompareVersions(current, latest)
	out["comparable"] = comparable
	out["has_update"] = comparable && cmp < 0
	if !comparable {
		// 개발 빌드(dev / git describe 접미사 포함)는 비교할 수 있는 버전 번호가 없다. 통과시키면
		// 로컬에서 디버깅 중인 바이너리를 정식 버전으로 덮어쓸 뿐이므로, 아예 업데이트를 주지 않는다.
		out["reason"] = fmt.Sprintf("현재 버전 %q 는 정식 릴리스 버전이 아니므로 원클릭 업데이트가 비활성화되었습니다", current)
	}
	writeJSON(w, 200, out)
}

// updateApply 는 새 버전을 다운로드하고 스테이징한 뒤, 완료되면 프로세스를 종료해 감시 스크립트가 재시작하게 한다.
//
// 즉시 202 를 반환하고 실제 작업은 백그라운드 goroutine 에서 돌린다: 패키지 전체 다운로드는 몇 분이 걸릴 수 있어
// 요청에 매달아 두면 리버스 프록시 시간 초과로 끊긴다. 진행 상황은 /api/update/stream 을 탄다.
func (s *Server) updateApply(w http.ResponseWriter, r *http.Request) {
	if IsPersonalBuild(BuildVersion) {
		writeErr(w, 409, "개인판은 상위 프로젝트 설치 패키지를 받지 않습니다. 독립 개인판 릴리스 패키지를 사용하세요")
		return
	}
	current := BuildVersion

	// 캐시를 탄다: 설치되는 것이 사용자가 화면에서 보고 확인한 그 버전이 되도록 보장한다.
	client, network, err := s.updateClient()
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	defer client.CloseIdleConnections()
	rel, err := relCache.get(r.Context(), client, false, network)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	cmp, comparable := selfupdate.CompareVersions(current, rel.TagName)
	if !comparable {
		writeErr(w, 400, fmt.Sprintf("현재 버전 %q 는 정식 릴리스 버전이 아니므로 원클릭 업데이트가 비활성화되었습니다", current))
		return
	}
	if cmp >= 0 {
		writeErr(w, 400, fmt.Sprintf("현재 이미 최신 버전입니다 %s", current))
		return
	}
	if !updHub.begin(rel.TagName) {
		writeErr(w, 409, "이미 진행 중인 업데이트가 있습니다")
		return
	}

	go func() {
		// 일부러 요청의 ctx 가 아니라 s.ctx 를 쓴다: HTTP 응답이 반환되면 요청이 끝나므로,
		// 그것에 매달아 다운로드하면 즉시 취소된다.
		defer client.CloseIdleConnections()
		err := selfupdate.Stage(s.ctx, client, rel, current, func(ph selfupdate.Phase, pct int, msg string) {
			updHub.publish(ph, pct, msg)
		})
		updHub.finish(err)
		if err != nil {
			log.Printf("[update] 업데이트 실패: %v", err)
			return
		}
		log.Printf("[update] %s → %s 를 스테이징했습니다. 곧 종료해 교체를 완료합니다", current, rel.TagName)
		// 마지막 진행 상황 하나를 프론트엔드로 보낼 시간을 조금 남긴 뒤 종료를 트리거한다.
		time.Sleep(1500 * time.Millisecond)
		requestRestart()
	}()

	writeJSON(w, 202, map[string]any{"ok": true, "target": rel.TagName})
}

// updateRollback 은 이전 버전(교체 전 백업한 prtseek.old)으로 능동적으로 되돌린다.
func (s *Server) updateRollback(w http.ResponseWriter, r *http.Request) {
	if IsPersonalBuild(BuildVersion) {
		writeErr(w, 409, "개인판은 상위 프로젝트 업데이트 롤백 채널을 쓰지 않습니다")
		return
	}
	if _, running := updHub.snapshot(); running {
		writeErr(w, 409, "업데이트가 진행 중이어서 롤백할 수 없습니다")
		return
	}
	if err := selfupdate.Rollback(); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	log.Printf("[update] 이전 버전으로 수동 롤백했습니다. 곧 종료해 전환을 완료합니다")
	writeJSON(w, 202, map[string]any{"ok": true})
	go func() {
		time.Sleep(500 * time.Millisecond)
		requestRestart()
	}()
}

// updateStream 은 SSE 로 업데이트 진행 상황을 보낸다.
func (s *Server) updateStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch, unsub := updHub.subscribe()
	defer unsub()

	send := func(p updateProgress) {
		b, _ := json.Marshal(p)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}
	// 먼저 현재 상태를 하나 보충해, 페이지를 새로 고친 뒤 진행 중인 업그레이드를 바로 볼 수 있게 한다.
	cur, _ := updHub.snapshot()
	send(cur)

	ctx := r.Context()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case p, ok := <-ch:
			if !ok {
				return
			}
			send(p)
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}
