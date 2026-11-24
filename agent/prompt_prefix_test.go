package agent

import (
	"strings"
	"testing"
	"time"
)

// The prefix cache only pays off when the rendered system prompt is byte-identical
// across turns, so {{.Now}} is pinned per session and {{.NowTurn}} carries the live
// clock. These tests pin the clock instead of sleeping on wall time.

func withClock(t *testing.T, at time.Time) *time.Time {
	t.Helper()
	cur := at
	prev := clockNow
	clockNow = func() time.Time { return cur }
	t.Cleanup(func() {
		clockNow = prev
		resetSessionStamps()
	})
	return &cur
}

func TestSessionNowIsStablePerSession(t *testing.T) {
	resetSessionStamps()
	cur := withClock(t, time.Date(2026, 11, 21, 9, 0, 0, 0, time.Local))

	first := sessionNow("conv-1")
	*cur = cur.Add(90 * time.Minute)
	if got := sessionNow("conv-1"); got != first {
		t.Fatalf("session stamp drifted within a conversation: %q → %q", first, got)
	}
	if got := sessionNow("conv-2"); got == first {
		t.Fatalf("separate sessions must not share a stamp: %q", got)
	}
}

// A one-shot render (no session) keeps the live clock, which template preview relies on.
func TestSessionNowEmptySessionIsLive(t *testing.T) {
	resetSessionStamps()
	cur := withClock(t, time.Date(2026, 11, 21, 9, 0, 0, 0, time.Local))

	if got, want := sessionNow(""), cur.Format(stampLayout); got != want {
		t.Fatalf("empty session should render the live clock: %q != %q", got, want)
	}
}

// The first line of a prompt referencing both variables must survive a second turn
// untouched while the {{.NowTurn}} line moves.
func TestChatSystemKeepsPrefixStableAcrossTurns(t *testing.T) {
	resetSessionStamps()
	prev := PromptOverride
	t.Cleanup(func() { PromptOverride = prev })

	PromptOverride = func(key string) (string, bool) {
		if key == "solver" {
			return "대화 시작: {{.Now}}\n현재 시각: {{.NowTurn}}", true
		}
		return "", false
	}
	cur := withClock(t, time.Date(2026, 11, 21, 9, 0, 0, 0, time.Local))

	first := strings.Split(chatSystemFor("solver", "/data", "/data/ws", "conv-1"), "\n")
	*cur = cur.Add(42 * time.Minute)
	second := strings.Split(chatSystemFor("solver", "/data", "/data/ws", "conv-1"), "\n")

	if len(first) < 2 || len(second) < 2 {
		t.Fatalf("prompt layout changed: %q / %q", first, second)
	}
	if first[0] != second[0] {
		t.Fatalf("stable prefix moved between turns: %q → %q", first[0], second[0])
	}
	if first[1] == second[1] {
		t.Fatalf("{{.NowTurn}} did not refresh: %q", first[1])
	}
}
