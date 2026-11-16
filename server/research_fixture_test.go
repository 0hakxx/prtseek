package server

import (
	"encoding/json"
	"net/http"
	"testing"
)

// An explicit missing-input outcome lets accounting/dispatch fixtures finish
// without teaching the runtime to accept arbitrary model text as completion.
func fixtureResearchBlocked(w http.ResponseWriter) {
	args := `{"status":"blocked","summary":"Fixture requires the next input artifact","blocker":"missing_input","resume_when":"Supply the next fixture artifact"}`
	_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "fixture_status", "type": "function", "function": map[string]any{"name": "research_status", "arguments": args}}}}, "finish_reason": "tool_calls"}}, "usage": map[string]any{"prompt_tokens": 100, "completion_tokens": 10}})
}

func TestCTFModelCallLimitConfiguration(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  int
	}{{"", 200}, {"2000", 2000}, {"0", 200}, {"-1", 200}, {"100001", 200}, {"invalid", 200}} {
		t.Setenv("prtseek_CTF_MODEL_CALL_LIMIT", tc.value)
		if got := ctfModelCallLimit(); got != tc.want {
			t.Fatalf("%q: %d", tc.value, got)
		}
	}
}
