package selfupdate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Repo 는 실제 상위 출처와 그 배포 패키지 프로토콜을 유지한다. prtseek 빌드는 이 채널로 업데이트하는 것이 금지되어 있다.
// 설정 항목으로 만들지 않고 하드코딩한다: 업데이트 출처를 설정할 수 있게 하면 설정을 바꿀 수 있는 누구에게나
// 원격 코드 실행 통로를 주는 셈이며, 침투 테스트 플랫폼에서 이 구멍은 열 수 없다.
const Repo = "RuoJi6/ARTEX"

// latestURL 은 GitHub 의 "최신 정식 버전" 인터페이스다. prerelease 와 draft 는 자동으로 건너뛴다.
const latestURL = "https://api.github.com/repos/" + Repo + "/releases/latest"

// allowedHosts 는 업그레이드 경로가 접근할 수 있는 도메인을 제한한다. 아래의 checkRedirect 와 함께,
// 어느 단계든 목록 밖의 호스트로 리디렉션되면 바로 실패한다——이것이 DNS 오염 / 중간자가
// 바이너리를 바꿔치기하는 것을 막는 첫 번째 관문이며, 두 번째는 SHA256SUMS 비교다.
var allowedHosts = map[string]bool{
	"api.github.com":                       true,
	"github.com":                           true,
	"objects.githubusercontent.com":        true, // release 자산이 실제로 저장되는 객체 스토리지
	"release-assets.githubusercontent.com": true,
	"raw.githubusercontent.com":            true,
}

// Release 는 GitHub Release 에서 우리가 관심 있는 필드다.
type Release struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	Body        string    `json:"body"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
	HTMLURL     string    `json:"html_url"`
	Assets      []Asset   `json:"assets"`
}

// Asset 은 Release 에 붙은 파일 하나다.
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

// ParseProxy validates an optional update proxy without including credentials in errors.
func ParseProxy(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("프록시 주소 형식이 유효하지 않습니다")
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, fmt.Errorf("프록시 프로토콜은 http, https, socks5 또는 socks5h 여야 합니다")
	}
	if u.Hostname() == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("프록시 주소에는 호스트가 있어야 하며 경로, 쿼리, 프래그먼트를 포함할 수 없습니다")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("프록시 포트는 1–65535 사이여야 합니다")
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return nil, fmt.Errorf("프록시 포트는 비워 둘 수 없습니다")
	}
	return u, nil
}

// NewClient validates the proxy before opening any connection. Empty means direct,
// regardless of HTTP_PROXY / HTTPS_PROXY in the process environment.
func NewClient(proxy string) (*http.Client, error) {
	pu, err := ParseProxy(proxy)
	if err != nil {
		return nil, err
	}
	tr := &http.Transport{
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: 15 * time.Second,
	}
	if pu != nil {
		tr.Proxy = http.ProxyURL(pu)
	}

	return &http.Client{
		Transport: tr,
		Timeout:   30 * time.Minute, // 패키지 전체를 다운로드하므로 요청 단위 시간 초과로 막으면 안 됨
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("리디렉션 횟수가 너무 많습니다")
			}
			return checkURL(req.URL)
		},
	}, nil
}

// checkURL 은 https + 도메인 화이트리스트를 강제한다.
func checkURL(u *url.URL) error {
	if u.Scheme != "https" {
		return fmt.Errorf("HTTPS 가 아닌 주소를 거부합니다: %s", u.Scheme+"://"+u.Host)
	}
	if !allowedHosts[strings.ToLower(u.Hostname())] {
		return fmt.Errorf("GitHub 가 아닌 도메인을 거부합니다: %s", u.Hostname())
	}
	return nil
}

// FetchLatest 는 최신 정식 버전을 조회한다.
func FetchLatest(ctx context.Context, c *http.Client) (*Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, latestURL, nil)
	if err != nil {
		return nil, err
	}
	if err := checkURL(req.URL); err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "prtseek-selfupdate")

	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GitHub 접근 실패(버전 및 업데이트에서 업데이트 프록시를 설정할 수 있습니다): %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusForbidden, resp.StatusCode == http.StatusTooManyRequests:
		// 인증하지 않은 GitHub API 는 IP 당 시간당 60 회이며, 출구 IP 를 공유하면 쉽게 걸린다.
		return nil, fmt.Errorf("GitHub 인터페이스 속도 제한(시간당 60 회)입니다. 잠시 후 다시 시도하세요")
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("저장소 %s 에 접근할 수 없거나 아직 정식 버전을 공개하지 않았습니다", Repo)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("GitHub 가 %d 를 반환했습니다", resp.StatusCode)
	}

	var rel Release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("Release 파싱 실패: %w", err)
	}
	if strings.TrimSpace(rel.TagName) == "" {
		return nil, fmt.Errorf("Release 에 tag 가 없습니다")
	}
	return &rel, nil
}

// AssetName 은 현재 플랫폼에 해당하는 배포 패키지 이름을 반환하며, build.sh 의 package_binary 와 일치시킨다:
// artex-<버전>-<os>-<arch>.zip(버전 번호에 v 접두사 없음).
func AssetName(tag, goos, goarch string) string {
	return fmt.Sprintf("artex-%s-%s-%s.zip", strings.TrimPrefix(tag, "v"), goos, goarch)
}

// FindAsset 은 Release 에서 이름으로 자산을 찾는다.
func (r *Release) FindAsset(name string) (Asset, bool) {
	for _, a := range r.Assets {
		if strings.EqualFold(a.Name, name) {
			return a, true
		}
	}
	return Asset{}, false
}
