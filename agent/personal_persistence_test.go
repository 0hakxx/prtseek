package agent

import "testing"

func TestPersonalPersistentGoal(t *testing.T) {
	for _, tc := range []struct {
		message string
		want    bool
	}{
		{"계속 풀이 멈추지 말고 flag 획득까지", true},
		{"flag 획득 멈추지 마", true},
		{"CTF 상황: 지연(3) flag 획득", true},
		{"Keep going until you get the flag", true},
		{"이 CTF 문제를 분석", false},
		{"계속", true},
		{"일반 채팅", false},
	} {
		if got := PersonalPersistentGoal(tc.message); got != tc.want {
			t.Errorf("PersonalPersistentGoal(%q) = %v; want %v", tc.message, got, tc.want)
		}
	}
}
