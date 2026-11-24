// Package selfupdate retains the upstream ARTEX update protocol for legacy builds: GitHub Release 에서 새 버전
// 바이너리를 받아 검증하고 스테이징한 뒤, 다음 시작 때 원자적으로 교체한다.
//
// 전체 분담(start.sh / start.bat 참고):
//
//	시작 스크립트  = 단순한 감시 루프이며, "프로세스가 종료되면 종료 코드에 따라 다시 띄울지"만 담당
//	이 패키지      = 틀리기 쉬운 모든 로직(다운로드 / SHA256 검증 / 스모크 / 교체 / 실패 롤백)
//
// 교체를 스크립트가 아니라 Go 에 두는 이유는, sha256 검증과 스모크 테스트를 sh 와 bat 에서
// 두 벌로 써야 하는데(sha256sum / shasum / certutil), 이것이야말로 절대 틀리면 안 되는 부분이기 때문이다——실행되지 않는
// 바이너리로 바꿔 놓으면 감시 프로세스가 충실하게 계속 다시 띄우고, 사용자는 기기에 직접 들어가 손으로 살려야 한다.
//
// 한 번의 완전한 업그레이드는 세 번의 프로세스 시작을 거친다:
//
//	① 기존 server 가 /api/update/apply 를 받음 → 다운로드 검증 → prtseek.new 스테이징 → exit 75
//	② 스크립트가 기존 버전을 다시 띄움 → Bootstrap 이 prtseek.new 를 발견 → 검증+스모크 → 교체 → exit 75
//	③ 스크립트가 다시 띄움, 이때는 이미 새 버전 → Bootstrap 이 시도를 한 번 기록 → 시작 성공 후 표시 지움
//
// 어느 단계든 실패하면 기존 버전으로 돌아간다: ② 검증을 통과하지 못하면 스테이징 파일을 지우고 기존 버전을 계속 실행한다. ③ 3 회 연속으로
// 표시를 지울 때까지 버티지 못하면(시작하자마자 충돌) 자동으로 prtseek.old 를 되돌려 놓는다.
package selfupdate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ExitRestart 는 "감시 프로세스가 나를 다시 띄워 달라"는 종료 코드(EX_TEMPFAIL)다. 시작 스크립트는 이를 보면
// 즉시 다시 실행하며 충돌 백오프에 넣지 않는다. 0 은 사용자의 정상 중지(스크립트가 루프 종료)이며, 나머지는 모두 충돌로 본다.
const ExitRestart = 75

// maxAttempts 는 교체 후 허용되는 시작 시도 횟수다. 새 버전은 시작할 때마다 횟수를 +1 하며,
// settleDelay 를 버티면 표시를 지운다. maxAttempts 번 연속 충돌하면 새 버전이 아예 시작되지 않는다는 뜻이므로 자동 롤백한다.
const maxAttempts = 3

// Paths 는 한 번의 업그레이드에 관련된 모든 파일이며, 모두 **실행 파일이 있는 디렉터리** 아래에 둔다.
// 일부러 CWD 를 쓰지 않는다: 서비스로 실행할 때 작업 디렉터리는 / 나 임의의 경로일 수 있어, CWD 를 쓰면 스테이징 파일이
// 엉뚱한 곳에 생겨 교체 로직이 바로 무효가 된다.
type Paths struct {
	Dir     string // 실행 파일이 있는 디렉터리
	Current string // 현재 실행 중인 바이너리        prtseek   / prtseek.exe
	New     string // 스테이징된 새 버전            prtseek.new  / prtseek.new.exe
	Sum     string // 새 버전의 sha256(hex)  prtseek.new.sha256 / prtseek.new.exe.sha256
	Old     string // 교체 전에 백업한 기존 버전      prtseek.old  / prtseek.old.exe
	Marker  string // 업그레이드 상태 표시            prtseek.upgrade.json
}

// ResolvePaths 는 현재 실행 파일을 기준으로 모든 업그레이드 경로를 도출한다.
//
// Windows 에서는 .new/.old 에도 .exe 접미사가 붙어야 한다. 그렇지 않으면 스모크 테스트와 교체 후 실행이 모두 실패하므로,
// 접미사를 먼저 떼고 붙여야 두 플랫폼의 이름이 대칭이 된다.
func ResolvePaths() (Paths, error) {
	exe, err := os.Executable()
	if err != nil {
		return Paths{}, fmt.Errorf("실행 파일 위치 찾기: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	dir := filepath.Dir(exe)
	name := filepath.Base(exe)
	ext := filepath.Ext(name) // Windows 에서는 ".exe", Unix 에서는 보통 비어 있음
	stem := strings.TrimSuffix(name, ext)

	join := func(suffix string) string { return filepath.Join(dir, stem+suffix+ext) }
	return Paths{
		Dir:     dir,
		Current: exe,
		New:     join(".new"),
		Sum:     join(".new") + ".sha256",
		Old:     join(".old"),
		Marker:  filepath.Join(dir, stem+".upgrade.json"),
	}, nil
}

// marker 는 한 번의 교체 진행 상황을 기록하며, 새 버전이 시작되지 않을 때 자동 롤백을 트리거하는 데 쓴다.
type marker struct {
	From     string `json:"from"`     // 업그레이드 전 버전
	To       string `json:"to"`       // 대상 버전
	Attempts int    `json:"attempts"` // 교체 후 시작을 시도한 횟수
	StagedAt int64  `json:"staged_at"`
}

func readMarker(path string) (marker, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return marker{}, false
	}
	var m marker
	if json.Unmarshal(b, &m) != nil {
		return marker{}, false
	}
	return m, true
}

func writeMarker(path string, m marker) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// cleanStaged 는 스테이징 파일을 지운다. 교체 성공, 검증 실패, 사용자 취소 모두 이것을 거쳐, 남은
// prtseek.new 가 다음 시작 때 다시 시도되지 않게 한다.
func cleanStaged(p Paths) {
	_ = os.Remove(p.New)
	_ = os.Remove(p.Sum)
}

// CompareVersions 는 두 버전 번호를 비교해 -1/0/1(a<b / a==b / a>b)을 반환한다.
// ok=false 는 적어도 한쪽이 비교 가능한 버전 번호가 아니라는 뜻이다(예: 로컬 개발 빌드의 "dev" 나
// git describe 가 만든 "0.3.7-2-gabc1234-dirty"). 이때 호출자는 원클릭 업데이트를 꺼야 한다.
// 그렇지 않으면 개발 중인 빌드를 정식 버전으로 "업그레이드"해 커밋하지 않은 변경을 덮어쓴다.
func CompareVersions(a, b string) (int, bool) {
	av, aok := parseVersion(a)
	bv, bok := parseVersion(b)
	if !aok || !bok {
		return 0, false
	}
	for i := range 3 {
		if av[i] != bv[i] {
			if av[i] < bv[i] {
				return -1, true
			}
			return 1, true
		}
	}
	return 0, true
}

// parseVersion 은 "v0.3.7" / "0.3.7" 형식의 버전 번호를 [3]int 로 파싱한다.
//
// 순수한 세 구간 형식만 받는다: build.sh 는 tag 가 아닌 빌드에서 git describe 로
// "0.3.7-2-gabc1234" 같은 접미사가 붙은 버전을 만드는데, 이는 반드시 비교 불가로 판정되어야 하며
// 0.3.7 로 취급되면 안 된다 —— 그렇지 않으면 개발 빌드가 "이미 최신"으로 오판되거나 정식 버전으로 덮어써진다.
func parseVersion(s string) ([3]int, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	if s == "" {
		return [3]int{}, false
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return [3]int{}, false
	}
	var out [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}

// InDocker 는 프로세스가 컨테이너 안에서 실행되는지 보고한다. Docker 에서 교체는 컨테이너의 쓰기 가능 계층에 쓰므로,
// `docker compose up -d` 로 컨테이너를 다시 만들면 이미지에 포함된 버전으로 돌아간다——이는 예상된 동작이지만
// (그때 사용자는 원래 새 이미지를 받고 있음), 프론트엔드가 이를 바탕으로 분명히 설명할 수 있어야 한다.
func InDocker() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	b, err := os.ReadFile("/proc/1/cgroup")
	if err != nil {
		return false
	}
	s := string(b)
	return strings.Contains(s, "docker") || strings.Contains(s, "containerd")
}
