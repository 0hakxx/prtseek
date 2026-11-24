package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/Autumn-27/artex/config"
)

// smokeEnv 는 스모크 테스트가 띄운 하위 프로세스가 Bootstrap 을 바로 건너뛰게 한다.
//
// 엄밀히 말하면 없어도 문제는 없다: 하위 프로세스의 os.Executable() 은 현재 프로그램의 .new 파일이므로, 도출되는
// 모든 경로에 .new 접두사가 붙어 실제 업그레이드 파일에 닿지 않는다. 하지만 이런 우연에 기대는 것은 너무 취약하다.
// 명시적으로 단락시키면 한눈에 알 수 있고, 하위 프로세스의 쓸데없는 디스크 탐색도 한 번 줄인다.
const smokeEnv = "prtseek_SELFUPDATE_SMOKE"

// Action 은 Bootstrap 이 main 에 주는 지시다.
type Action int

const (
	// Continue: 평소대로 server 를 시작한다.
	Continue Action = iota
	// Restart: 즉시 ExitRestart 로 종료해 감시 스크립트가 다시 띄우게 한다.
	Restart
)

// State 는 이번 시작 시의 업그레이드 상태를 설명하며, /api/update/check 가 프론트엔드에
// "지난 업그레이드가 성공했는지 롤백되었는지"를 있는 그대로 알려 주는 데 쓴다.
type State struct {
	Pending     bool   // 교체 후 아직 안정성이 확인되지 않음
	RolledBack  bool   // 이번 시작에서 방금 자동 롤백을 수행함
	FailedStage bool   // 스테이징 파일 검증/스모크를 통과하지 못해 버림
	Detail      string // 사용자용 한 문장 설명
}

// Bootstrap 은 main 의 맨 처음에 실행되며, 어떤 포트를 수신하거나 데이터베이스를 열기 전에 호출해야 한다.
//
// 세 가지 상황:
//
//	① .new 스테이징 파일이 있음     → 검증 + 스모크, 통과하면 교체하고 재시작을 요구. 통과하지 못하면 버리고 기존 버전으로 계속 실행
//	② 표시 파일만 남음          → 방금 교체했다는 뜻이므로 시도를 한 번 누적. 연속 실패가 충분히 많으면 롤백
//	③ 아무것도 없음            → 정상 시작
func Bootstrap() (Action, State) {
	if config.Env("SELFUPDATE_SMOKE") != "" {
		return Continue, State{}
	}
	p, err := ResolvePaths()
	if err != nil {
		log.Printf("[update] 부트스트랩 건너뜀: %v", err)
		return Continue, State{}
	}

	if _, err := os.Stat(p.New); err == nil {
		return applyStaged(p)
	}

	m, ok := readMarker(p.Marker)
	if !ok {
		return Continue, State{}
	}
	return confirmOrRollback(p, m)
}

// applyStaged 는 "스테이징 파일이 있는" 상황을 처리한다: 검증을 통과하면 교체하고, 실패하면 버린다.
//
// 업그레이드 경로 전체에서 실행 파일을 덮어쓰는 유일한 곳이자 마지막 관문이다——스모크 테스트가
// 다운로드 손상, 잘못된 아키텍처, 동적 링크 누락 같은 문제를 막는다. 실행되지 않는 바이너리를 한 번 통과시키면,
// 감시 스크립트가 지치지 않고 계속 다시 띄우는데 Go 코드는 실행될 기회조차 없어 자동 롤백도 불가능하다.
func applyStaged(p Paths) (Action, State) {
	m, _ := readMarker(p.Marker)

	if err := verifyStaged(p); err != nil {
		log.Printf("[update] 스테이징된 새 버전이 검증을 통과하지 못해 버리고 현재 버전으로 계속 실행합니다: %v", err)
		cleanStaged(p)
		_ = os.Remove(p.Marker)
		return Continue, State{FailedStage: true, Detail: "새 버전 검증에 실패해 버렸습니다: " + err.Error()}
	}

	if err := swap(p); err != nil {
		log.Printf("[update] 교체에 실패해 현재 버전으로 계속 실행합니다: %v", err)
		cleanStaged(p)
		_ = os.Remove(p.Marker)
		return Continue, State{FailedStage: true, Detail: "교체 실패: " + err.Error()}
	}

	// 교체 성공. 표시를 남겨 두고, 다음 시작(새 버전으로 실행됨)에서 안정성을 확인하게 한다.
	m.Attempts = 0
	if m.StagedAt == 0 {
		m.StagedAt = time.Now().Unix()
	}
	if err := writeMarker(p.Marker, m); err != nil {
		log.Printf("[update] 업그레이드 표시 쓰기 실패(자동 롤백 기능을 잃음): %v", err)
	}
	log.Printf("[update] %s 로 교체했습니다. 재시작을 위해 종료합니다(exit %d)", orUnknown(m.To), ExitRestart)
	return Restart, State{Pending: true}
}

// confirmOrRollback 은 "교체 후 시작"을 처리한다: 시도 횟수를 누적하고, 한도를 넘으면 기존 버전으로 되돌린다.
//
// 횟수는 Go 코드가 실행된 뒤에만 증가하므로, 이것이 다루는 것은 "실행은 되지만 초기화에서 충돌하는"
// (설정 비호환, 포트 점유, DB 마이그레이션 폭발) 종류의 장애다. "아예 exec 할 수 없는" 경우는 교체 전의
// 스모크 테스트가 막으며, 둘을 합쳐야 완전해진다.
func confirmOrRollback(p Paths, m marker) (Action, State) {
	m.Attempts++
	if m.Attempts > maxAttempts {
		if err := rollback(p); err != nil {
			// 롤백까지 실패하면 다시 재시작하지 않는다. 그렇지 않으면 무한 재시작에 빠진다. 표시를 지우고,
			// 프로세스가 현재 상태로 시작하게 한다——시작하지 못하더라도 사용자는 적어도 로그에서 원인을 볼 수 있다.
			log.Printf("[update] 새 버전이 %d 회 연속 시작에 실패했고 롤백도 실패했습니다: %v", maxAttempts, err)
			_ = os.Remove(p.Marker)
			return Continue, State{Detail: "새 버전 시작에 실패했고 롤백도 실패했습니다: " + err.Error()}
		}
		log.Printf("[update] 새 버전이 %d 회 연속 시작에 실패해 %s 로 롤백했습니다. 재시작을 위해 종료합니다(exit %d)",
			maxAttempts, orUnknown(m.From), ExitRestart)
		_ = os.Remove(p.Marker)
		return Restart, State{RolledBack: true, Detail: fmt.Sprintf("새 버전 시작에 실패해 %s 로 롤백했습니다", orUnknown(m.From))}
	}
	if err := writeMarker(p.Marker, m); err != nil {
		log.Printf("[update] 업그레이드 표시 갱신 실패: %v", err)
	}
	log.Printf("[update] 새 버전 시작 중(%d/%d 번째 시도). 안정적으로 실행되면 업그레이드를 확정합니다",
		m.Attempts, maxAttempts)
	return Continue, State{Pending: true}
}

// Settle 은 새 버전이 안정적으로 실행되고 있음을 확인하고 업그레이드 표시를 지운다.
//
// main 이 HTTP 수신을 시작한 뒤 지연 호출한다: 이 시간을 버텨야 인정되며, 그렇지 않으면 표시가 그대로 남아,
// 다음 시작에서도 시도 횟수를 계속 누적하다가 롤백이 트리거된다.
func Settle() {
	p, err := ResolvePaths()
	if err != nil {
		return
	}
	settle(p)
}

func settle(p Paths) {
	if _, ok := readMarker(p.Marker); !ok {
		return // 업그레이드 후 시작이 아니므로 할 일이 없음
	}
	if err := os.Remove(p.Marker); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("[update] 업그레이드 표시 지우기 실패: %v", err)
		return
	}
	log.Printf("[update] 새 버전이 안정적으로 실행되어 업그레이드를 완료했습니다(이전 버전은 %s 로 보관)", p.Old)
}

// SettleDelay 는 "새 버전이 살아남았다"고 판정하는 데 필요한 실행 시간이다.
const SettleDelay = 30 * time.Second

// verifyStaged 는 스테이징 파일을 검증한다: 먼저 SHA256 을 비교한 뒤 실제로 한 번 띄워 실행한다.
func verifyStaged(p Paths) error {
	want, err := os.ReadFile(p.Sum)
	if err != nil {
		return fmt.Errorf("체크섬 읽기: %w", err)
	}
	got, err := fileSHA256(p.New)
	if err != nil {
		return fmt.Errorf("체크섬 계산: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(string(want)), got) {
		return errors.New("SHA256 불일치(다운로드 손상 또는 변조)")
	}
	return smokeTest(p.New)
}

// smokeTest 는 -h 로 새 바이너리를 띄워 현재 시스템에서 정말 실행되는지 확인한다.
// 다운로드 잘림, 잘못된 아키텍처(exec format error), 의존성 누락 같은 많은 문제를 막을 수 있다.
func smokeTest(bin string) error {
	if err := os.Chmod(bin, 0o755); err != nil {
		return fmt.Errorf("실행 권한 부여: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "-h")
	// The staged executable may predate the rename and only know the old flag.
	cmd.Env = append(os.Environ(), smokeEnv+"=1", "ARTEX_SELFUPDATE_SMOKE=1")
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return errors.New("스모크 테스트 시간 초과(새 바이너리가 응답하지 않음)")
	}
	if err != nil {
		snippet := strings.TrimSpace(string(out))
		if len(snippet) > 300 {
			snippet = snippet[:300] + "…"
		}
		return fmt.Errorf("스모크 테스트 실패: %v: %s", err, snippet)
	}
	return nil
}

// swap 은 현재 바이너리를 스테이징된 새 버전으로 바꾼다.
//
// Unix 와 Windows 모두 실행 중인 실행 파일의 rename 을 허용한다(Windows 가 금지하는 것은 삭제와
// 덮어쓰기이며 rename 은 포함되지 않음). 그래서 여기서는 플랫폼을 나눌 필요도, 먼저 자신을 멈출 필요도 없다.
func swap(p Paths) error {
	// Windows 의 rename 은 이미 있는 대상을 덮어쓰지 않으므로, 지난 업그레이드가 남긴 .old 를 먼저 지워야 한다.
	if err := os.Remove(p.Old); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("이전 백업 %s 정리: %w", p.Old, err)
	}
	if err := os.Rename(p.Current, p.Old); err != nil {
		return fmt.Errorf("현재 버전 백업: %w", err)
	}
	if err := os.Rename(p.New, p.Current); err != nil {
		// 교체에 실패했는데 현재 버전은 이미 옮겨졌다면, 반드시 그대로 되돌려 놓아야 한다. 그렇지 않으면 다음 시작 때 실행 파일이 없다.
		if rerr := os.Rename(p.Old, p.Current); rerr != nil {
			return fmt.Errorf("새 버전 넣기 실패(%v), 현재 버전 복원도 실패: %w", err, rerr)
		}
		return fmt.Errorf("새 버전 넣기: %w", err)
	}
	_ = os.Remove(p.Sum)
	return nil
}

// rollback 은 swap 이 백업한 기존 버전을 되돌려 놓는다.
func rollback(p Paths) error {
	if _, err := os.Stat(p.Old); err != nil {
		return fmt.Errorf("롤백할 백업 %s 가 없습니다: %w", p.Old, err)
	}
	// 시작하지 못하는 새 버전은 바로 지우지 않고 .failed 로 옮겨 조사용으로 남긴다.
	failed := p.Current + ".failed"
	_ = os.Remove(failed)
	if err := os.Rename(p.Current, failed); err != nil {
		return fmt.Errorf("실패한 버전 옮기기: %w", err)
	}
	if err := os.Rename(p.Old, p.Current); err != nil {
		return fmt.Errorf("기존 버전 복원: %w", err)
	}
	return nil
}

// Rollback 은 /api/update/rollback 의 구현이다: 이전 버전으로 직접 되돌린다.
// 교체만 하며, 재시작은 마찬가지로 감시 스크립트에 맡긴다(호출자가 이어서 ExitRestart 로 종료).
func Rollback() error {
	p, err := ResolvePaths()
	if err != nil {
		return err
	}
	if _, err := os.Stat(p.Old); err != nil {
		return errors.New("롤백할 이전 버전이 없습니다(" + p.Old + " 없음)")
	}
	cleanStaged(p)
	if err := smokeTest(p.Old); err != nil {
		return fmt.Errorf("이전 버전을 실행할 수 없어 롤백을 거부합니다: %w", err)
	}
	// 현재와 백업을 맞바꾼다: 롤백한 뒤에도 다시 되돌릴 수 있다.
	tmp := p.Current + ".swap"
	_ = os.Remove(tmp)
	if err := os.Rename(p.Current, tmp); err != nil {
		return fmt.Errorf("현재 버전 옮기기: %w", err)
	}
	if err := os.Rename(p.Old, p.Current); err != nil {
		_ = os.Rename(tmp, p.Current)
		return fmt.Errorf("이전 버전 넣기: %w", err)
	}
	if err := os.Rename(tmp, p.Old); err != nil {
		log.Printf("[update] 롤백 후 백업 정리 실패(실행에는 영향 없음): %v", err)
	}
	_ = os.Remove(p.Marker)
	return nil
}

// HasBackup 은 롤백할 이전 버전이 있는지 보고하며, 프론트엔드가 롤백 버튼을 보일지 결정하는 데 쓴다.
func HasBackup() bool {
	p, err := ResolvePaths()
	if err != nil {
		return false
	}
	_, err = os.Stat(p.Old)
	return err == nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "알 수 없는 버전"
	}
	return s
}
