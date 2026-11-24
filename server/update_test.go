package server

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Autumn-27/artex/selfupdate"
)

// releaseCache 는 GitHub 할당량을 보호하는 계층이다: 미인증 API 는 시간당 IP 당 60 회뿐이고,
// 상단바의 "새 버전 있음" 안내는 매 전체 로드마다 한 번씩 조회한다. 캐시가 무효화되면 사용자가
// 탭 몇 개를 더 열자마자 할당량이 소진되어, 정작 업데이트하고 싶을 때 조회가 안 된다.

func newTestCache(fetch func(context.Context, *http.Client) (*selfupdate.Release, error)) *releaseCache {
	return &releaseCache{fetch: fetch}
}

func TestReleaseCacheServesFromCache(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return &selfupdate.Release{TagName: "v0.3.8"}, nil
	})

	for range 5 {
		rel, err := c.get(t.Context(), nil, false, "")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if rel.TagName != "v0.3.8" {
			t.Fatalf("TagName = %q", rel.TagName)
		}
	}
	if calls != 1 {
		t.Errorf("5 번 조회에 1 번만 원본으로 가야 하는데 실제 %d 번", calls)
	}
}

func TestReleaseCacheForceBypasses(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return &selfupdate.Release{TagName: "v0.3.8"}, nil
	})

	if _, err := c.get(t.Context(), nil, false, ""); err != nil {
		t.Fatal(err)
	}
	// 사용자가 「업데이트 확인」을 누르면 반드시 실시간 결과를 받아야 한다. 그러지 않으면 방금 공개된 버전은 캐시 만료를 기다려야만 보인다.
	if _, err := c.get(t.Context(), nil, true, ""); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("force 는 캐시를 우회해야 하므로 원본 2 번을 기대했는데 실제 %d 번", calls)
	}
}

func TestReleaseCacheExpiresAfterTTL(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return &selfupdate.Release{TagName: "v0.3.8"}, nil
	})

	if _, err := c.get(t.Context(), nil, false, ""); err != nil {
		t.Fatal(err)
	}
	// DB 저장 시각을 막 지난 시점으로 앞당겨 TTL 만료를 흉내 낸다.
	c.at = time.Now().Add(-releaseTTL - time.Second)
	if _, err := c.get(t.Context(), nil, false, ""); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("TTL 만료 후에는 원본으로 다시 가야 하므로 2 번을 기대했는데 실제 %d 번", calls)
	}
}

func TestReleaseCacheUsesShorterTTLForErrors(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return nil, errors.New("github 도달 불가")
	})

	if _, err := c.get(t.Context(), nil, false, ""); err == nil {
		t.Fatal("오류 반환을 기대함")
	}
	// 실패 결과도 잠시 캐시해야 한다. 그러지 않으면 GitHub 도달 불가 시 매 페이지 로드마다 한 번씩 헛되이 타임아웃을 기다린다.
	if _, err := c.get(t.Context(), nil, false, ""); err == nil {
		t.Fatal("오류 반환을 기대함")
	}
	if calls != 1 {
		t.Errorf("오류는 짧게 캐시해야 하므로 원본 1 번을 기대했는데 실제 %d 번", calls)
	}

	// 그러나 오류의 TTL 은 반드시 성공보다 뚜렷이 짧아야 함. 네트워크가 복구되면 금방 자가 치유되어야 한다.
	if releaseErrTTL >= releaseTTL {
		t.Fatalf("오류 TTL(%v) 은 성공 TTL(%v) 보다 짧아야 함", releaseErrTTL, releaseTTL)
	}
	c.at = time.Now().Add(-releaseErrTTL - time.Second)
	if _, err := c.get(t.Context(), nil, false, ""); err == nil {
		t.Fatal("오류 반환을 기대함")
	}
	if calls != 2 {
		t.Errorf("오류 TTL 만료 후에는 재시도해야 하므로 2 번을 기대했는데 실제 %d 번", calls)
	}
}

func TestReleaseCacheDoesNotPoisonOnCallerCancel(t *testing.T) {
	good := &selfupdate.Release{TagName: "v0.3.8"}
	c := newTestCache(func(ctx context.Context, _ *http.Client) (*selfupdate.Release, error) {
		return good, nil
	})
	if _, err := c.get(t.Context(), nil, false, ""); err != nil {
		t.Fatal(err)
	}

	// 방문자가 탭을 닫으면 요청이 취소된다. 그것은 GitHub 에 문제가 있다는 뜻이 아니므로 「취소됨」을
	// 절대 캐시에 쓰면 안 된다——그러지 않으면 다음 30 분 동안 방문자마다 알 수 없는 오류를 받는다.
	c.fetch = func(ctx context.Context, _ *http.Client) (*selfupdate.Release, error) {
		return nil, ctx.Err()
	}
	c.at = time.Now().Add(-releaseTTL - time.Second) // 캐시 만료, 강제로 원본 가게 함

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.get(ctx, nil, false, ""); err == nil {
		t.Fatal("호출자가 이미 취소했으면 오류를 그대로 전달해야 함")
	}

	// 핵심 불변량: 취소된 그 한 번은 어떤 흔적도 남기면 안 된다——캐시에 「취소됨」 오류가 없고,
	// 지난번의 좋은 결과도 그대로 유지되어야 한다.
	if c.err != nil {
		t.Fatalf("취소 오류가 캐시에 써지면 안 되는데 %v 얻음", c.err)
	}
	if c.rel == nil || c.rel.TagName != "v0.3.8" {
		t.Fatalf("캐시는 지난번의 좋은 결과를 유지해야 하는데 %+v 얻음", c.rel)
	}

	// 그 취소가 어떤 새 데이터도 가져오지 않았으므로, 다음 방문자는 마땅히 원본으로 다시 가야 한다——
	// 그리고 정상적으로 결과를 받아야 하며, 지난번 취소에 영향받으면 안 된다.
	c.fetch = func(context.Context, *http.Client) (*selfupdate.Release, error) {
		return good, nil
	}
	rel, err := c.get(t.Context(), nil, false, "")
	if err != nil {
		t.Fatalf("취소 후의 정상 요청은 오류를 내면 안 됨: %v", err)
	}
	if rel == nil || rel.TagName != "v0.3.8" {
		t.Fatalf("정상 결과를 받아야 하는데 %+v 얻음", rel)
	}
}

func TestReleaseCacheChangesWithNetwork(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("direct unavailable")
		}
		return &selfupdate.Release{TagName: "v0.3.21"}, nil
	})
	if _, err := c.get(t.Context(), nil, false, ""); err == nil {
		t.Fatal("expected direct error")
	}
	if _, err := c.get(t.Context(), nil, false, "http://localhost:7897"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.get(t.Context(), nil, false, "http://localhost:7897"); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("same proxy should reuse cache; calls=%d", calls)
	}
	if _, err := c.get(t.Context(), nil, false, ""); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("switching back to direct must refetch; calls=%d", calls)
	}
}

func TestUpdateSettingsValidation(t *testing.T) {
	for _, tc := range []struct {
		proxy string
		valid bool
	}{
		{"", true}, {" http://127.0.0.1:7897 ", true}, {"localhost:7897", false},
	} {
		cfg := updateSettings{Proxy: tc.proxy}
		if err := cfg.validate(); (err == nil) != tc.valid {
			t.Errorf("validation: %v", err)
		}
	}
}
