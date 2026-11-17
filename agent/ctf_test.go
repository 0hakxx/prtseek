package agent

import (
	"strings"
	"testing"
)

func TestCTFSolveAliasesShareMethodAndWriteupStaysSeparate(t *testing.T) {
	for _, tc := range []struct{ key, marker string }{
		{"solve", "停止条件"},
		{"triage", "材料清点"},
		{"command", "停止条件"},
		{"solver", "Crypto"},
		{"writeup", "EXP"},
	} {
		prompt := chatSystem(tc.key, t.TempDir(), t.TempDir())
		if !strings.Contains(prompt, tc.marker) || (tc.key != "writeup" && !strings.Contains(prompt, CTFEvidencePriority)) {
			t.Errorf("%s missing CTF method", tc.key)
		}
	}
	if strings.Contains(chatSystem("writeup", t.TempDir(), t.TempDir()), CTFEvidencePriority) {
		t.Fatal("writing a WP inherited solve-until-flag instructions")
	}
	if ctfRoleMethod("ctf_verifier") != "" {
		t.Fatal("retired verifier regained a method card")
	}
}
