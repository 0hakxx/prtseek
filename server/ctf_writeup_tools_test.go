package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Autumn-27/artex/db"
)

func TestWriteupReadConversationsPagesVisibleSourcesOnly(t *testing.T) {
	s, first := ctfRegressionServer(t)
	second, err := s.m.pg.CreateConversation("solve", "second local fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	// The package fixture can contain conversations from earlier tests. Both
	// new rows must lead the descending list; older rows still make more pages.
	var conversationCount int
	if err := s.m.pg.QueryRow(`SELECT count(*) FROM conversations`).Scan(&conversationCount); err != nil {
		t.Fatal(err)
	}
	tools := s.ctfWriteupTools()
	for _, tool := range tools {
		if !tool.IsReadOnly(nil) || !tool.IsConcurrencySafe(nil) {
			t.Fatalf("cross-conversation tool is not read-only: %s", tool.Name())
		}
	}
	list := workflowTool(t, tools, "writeup_list_conversations")
	page := workflowCall(t, t.Context(), list, map[string]any{"limit": 1}, false)
	var listed struct {
		Conversations []struct {
			ID int64 `json:"conversation_id"`
		} `json:"conversations"`
		Next int  `json:"next_offset"`
		More bool `json:"has_more"`
	}
	if err := json.Unmarshal([]byte(page), &listed); err != nil || len(listed.Conversations) != 1 || listed.Conversations[0].ID != second.ID || !listed.More || listed.Next != 1 {
		t.Fatal("wrong conversation page", page, err)
	}
	page = workflowCall(t, t.Context(), list, map[string]any{"limit": 1, "offset": listed.Next}, false)
	if err := json.Unmarshal([]byte(page), &listed); err != nil || len(listed.Conversations) != 1 || listed.Conversations[0].ID != first.ID || listed.More != (conversationCount > 2) || listed.Next != 2 {
		t.Fatal("wrong second conversation page", page, err)
	}
	activities := []db.Activity{
		{Kind: "user", Detail: "please document this local fixture"},
		{Kind: "thinking", Detail: "hidden-thought-marker"},
		{Kind: "reasoning", Detail: "hidden-reason-marker"},
		{Kind: "system", Detail: "hidden-system-marker"},
		{Kind: "prompt", Detail: "hidden-prompt-marker"},
		{Kind: "usage", Detail: "not-a-user-message"},
		{Kind: "text", Detail: "visible answer flag{synthetic-fixture}"},
		{Kind: "tool_use", Tool: "fixture", Detail: `{"api_key":"private-api-value","nested":{"Authorization":"Bearer private-header-value"},"prompt":"summarize the fixture"}`},
		{Kind: "tool_result", Tool: "fixture", Detail: `{"output":"observed fixture result","reasoning_content":"hidden-json-reason-marker","cookie":"private-cookie-value"}`},
		{Kind: "result", Detail: "final visible explanation"},
	}
	for _, activity := range activities {
		if _, err := s.m.pg.AppendConvActivity(second.ID, activity); err != nil {
			t.Fatal(err)
		}
	}
	read := workflowTool(t, tools, "writeup_read_conversation")
	var all strings.Builder
	var cursor int64
	var visible int
	for range 4 {
		page = workflowCall(t, t.Context(), read, map[string]any{"conversation_id": second.ID, "after_id": cursor, "limit": 2}, false)
		var p struct {
			ConversationID int64 `json:"conversation_id"`
			Activities     []struct {
				ID      int64  `json:"activity_id"`
				Content string `json:"content"`
			} `json:"activities"`
			Next int64 `json:"next_after_id"`
			More bool  `json:"has_more"`
		}
		if err := json.Unmarshal([]byte(page), &p); err != nil || p.ConversationID != second.ID || !strings.Contains(page, "reference_only") {
			t.Fatal("missing provenance", page, err)
		}
		for _, activity := range p.Activities {
			if activity.ID <= cursor {
				t.Fatal("activity cursor did not advance")
			}
			cursor = activity.ID
		}
		if p.Next != cursor {
			t.Fatal("incorrect next cursor", page)
		}
		visible += len(p.Activities)
		all.WriteString(page)
		if !p.More {
			break
		}
	}
	if visible != 5 || !strings.Contains(all.String(), "flag{synthetic-fixture}") || !strings.Contains(all.String(), "observed fixture result") {
		t.Fatal("visible messages missing", visible, all.String())
	}
	for _, denied := range []string{"hidden-thought", "hidden-reason", "hidden-system", "hidden-prompt", "hidden-json-reason", "private-api-value", "private-header-value", "private-cookie-value", "not-a-user-message"} {
		if strings.Contains(all.String(), denied) {
			t.Fatalf("private data leaked: %s", denied)
		}
	}
	var count int
	if err := s.m.pg.QueryRow(`SELECT COUNT(*) FROM conversation_activities WHERE conversation_id=$1`, second.ID).Scan(&count); err != nil || count != len(activities) {
		t.Fatal("read tools changed source activities", count, err)
	}
}

func TestWriteupArtifactReadAndListingAreBoundedAndScoped(t *testing.T) {
	s, c := ctfRegressionServer(t)
	root := s.conversationWorkDir(c.ID)
	for name, content := range map[string]string{
		"answer.md":                  "flag{local-answer}\nAuthorization: Bearer private-token-012345\n",
		"outputs/explanation.txt":    "fixture explanation",
		".research/memory.json":      "hidden-research-memory",
		".research/evidence/raw.txt": "hidden-internal-evidence",
		"jwt.key":                    "private-signing-key",
		"credentials.json":           "private-credential-file",
		".env":                       "private-env-file",
		"large.txt":                  strings.Repeat("a", 70000),
		"split-secret.json":          `{"api_key":"private-half-secret-value-and-more"}`,
		"binary.bin":                 "a\x00b",
	} {
		filename := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(filename), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	tools := s.ctfWriteupTools()
	list := workflowTool(t, tools, "writeup_list_artifacts")
	listed := workflowCall(t, t.Context(), list, map[string]any{"conversation_id": c.ID, "limit": 100}, false)
	for _, denied := range []string{".research", "jwt.key", "credentials.json", ".env"} {
		if strings.Contains(listed, denied) {
			t.Fatalf("private file listed: %s", denied)
		}
	}
	if !strings.Contains(listed, "outputs/explanation.txt") {
		t.Fatal("nested artifact missing", listed)
	}
	read := workflowTool(t, tools, "writeup_read_artifact")
	answer := workflowCall(t, t.Context(), read, map[string]any{"conversation_id": c.ID, "path": "answer.md"}, false)
	if !strings.Contains(answer, "flag{local-answer}") || strings.Contains(answer, "private-token") || !strings.Contains(answer, "reference_only") || !strings.Contains(answer, `"path":"answer.md"`) {
		t.Fatal("answer provenance or redaction failed", answer)
	}
	large := workflowCall(t, t.Context(), read, map[string]any{"conversation_id": c.ID, "path": "large.txt", "max_bytes": 100}, false)
	var preview struct {
		Content   string `json:"content"`
		Truncated bool   `json:"truncated"`
	}
	if err := json.Unmarshal([]byte(large), &preview); err != nil || len(preview.Content) != 100 || !preview.Truncated {
		t.Fatal("unbounded preview", large, err)
	}
	partial := workflowCall(t, t.Context(), read, map[string]any{"conversation_id": c.ID, "path": "split-secret.json", "max_bytes": 33}, false)
	if strings.Contains(partial, "private-half") {
		t.Fatal("partially quoted credential leaked", partial)
	}
	for _, denied := range []string{"../outside.txt", "outputs/../answer.md", "C:/outside.txt", "outputs\\explanation.txt", ".research/memory.json", ".research/evidence/raw.txt", "jwt.key", "credentials.json", ".env", "binary.bin"} {
		workflowCall(t, t.Context(), read, map[string]any{"conversation_id": c.ID, "path": denied}, true)
	}
	workflowCall(t, t.Context(), read, map[string]any{"conversation_id": 987654, "path": "answer.md"}, true)
	workflowCall(t, t.Context(), read, map[string]any{"conversation_id": c.ID, "path": "large.txt", "max_bytes": 65537}, true)
	unchanged, err := os.ReadFile(filepath.Join(root, "answer.md"))
	if err != nil || !strings.Contains(string(unchanged), "private-token-012345") {
		t.Fatal("reader modified original artifact", err)
	}
}

func TestWriteupArtifactRejectsSymlinks(t *testing.T) {
	s, c := ctfRegressionServer(t)
	root := s.conversationWorkDir(c.ID)
	if err := os.MkdirAll(filepath.Join(root, ".research"), 0700); err != nil {
		t.Fatal(err)
	}
	hidden := filepath.Join(root, ".research", "memory.txt")
	if err := os.WriteFile(hidden, []byte("hidden local fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(hidden, filepath.Join(root, "alias.txt")); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	out := t.TempDir()
	if err := os.WriteFile(filepath.Join(out, "outside.txt"), []byte("outside local fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(out, filepath.Join(root, "outside")); err != nil {
		t.Fatal(err)
	}
	tools := s.ctfWriteupTools()
	read := workflowTool(t, tools, "writeup_read_artifact")
	for _, name := range []string{"alias.txt", "outside/outside.txt"} {
		workflowCall(t, t.Context(), read, map[string]any{"conversation_id": c.ID, "path": name}, true)
	}
	listed := workflowCall(t, t.Context(), workflowTool(t, tools, "writeup_list_artifacts"), map[string]any{"conversation_id": c.ID}, false)
	if strings.Contains(listed, "alias.txt") || strings.Contains(listed, "outside") {
		t.Fatal("symlink was listed", listed)
	}
}

func TestWriteupReadRejectsCancelledAndInvalidRequests(t *testing.T) {
	s, c := ctfRegressionServer(t)
	tools := s.ctfWriteupTools()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, tool := range tools {
		workflowCall(t, ctx, tool, map[string]any{"conversation_id": c.ID, "path": "answer.md"}, true)
	}
	workflowCall(t, t.Context(), workflowTool(t, tools, "writeup_read_conversation"), map[string]any{"conversation_id": c.ID, "after_id": -1}, true)
	workflowCall(t, t.Context(), workflowTool(t, tools, "writeup_list_conversations"), map[string]any{"limit": 51}, true)
	workflowCall(t, t.Context(), workflowTool(t, tools, "writeup_list_artifacts"), map[string]any{"conversation_id": c.ID, "offset": -1}, true)
}

func TestWriteupActivityPaginationReachesTailWithoutSplittingSecrets(t *testing.T) {
	s, c := ctfRegressionServer(t)
	other, err := s.m.pg.CreateConversation("solve", "unrelated fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	content := strings.Repeat("前", 9000) + "\napi_key=\"private-pagination-secret-" + strings.Repeat("z", 17000) + "\"\n" + strings.Repeat("后", 9000) + "\nACTIVITY-TAIL-REACHABLE"
	id, err := s.m.pg.AppendConvActivity(c.ID, db.Activity{Kind: "tool_result", Tool: "fixture", Detail: content})
	if err != nil {
		t.Fatal(err)
	}
	hiddenID, err := s.m.pg.AppendConvActivity(c.ID, db.Activity{Kind: "thinking", Detail: "never-readable-fixture"})
	if err != nil {
		t.Fatal(err)
	}
	tools := s.ctfWriteupTools()
	preview := workflowCall(t, t.Context(), workflowTool(t, tools, "writeup_read_conversation"), map[string]any{"conversation_id": c.ID}, false)
	var initial struct {
		Activities []struct {
			Content   string `json:"content"`
			Next      int    `json:"next_offset_chars"`
			Truncated bool   `json:"truncated"`
		} `json:"activities"`
	}
	if err := json.Unmarshal([]byte(preview), &initial); err != nil || len(initial.Activities) != 1 || !initial.Activities[0].Truncated || initial.Activities[0].Next <= 0 {
		t.Fatal("preview did not advertise continuation", preview, err)
	}
	read := workflowTool(t, tools, "writeup_read_activity")
	workflowCall(t, t.Context(), read, map[string]any{"conversation_id": other.ID, "activity_id": id}, true)
	workflowCall(t, t.Context(), read, map[string]any{"conversation_id": c.ID, "activity_id": hiddenID}, true)
	workflowCall(t, t.Context(), read, map[string]any{"conversation_id": c.ID, "activity_id": id, "offset_chars": -1}, true)
	var all strings.Builder
	all.WriteString(initial.Activities[0].Content)
	offset := initial.Activities[0].Next
	finished := false
	for range 20 {
		raw := workflowCall(t, t.Context(), read, map[string]any{"conversation_id": c.ID, "activity_id": id, "offset_chars": offset, "limit_chars": 1024}, false)
		var page struct {
			Content string `json:"content"`
			Next    int    `json:"next_offset_chars"`
			More    bool   `json:"has_more"`
		}
		if err := json.Unmarshal([]byte(raw), &page); err != nil || !utf8.ValidString(page.Content) || page.Next <= offset {
			t.Fatal("invalid character page", raw, err)
		}
		all.WriteString(page.Content)
		offset = page.Next
		if !page.More {
			finished = true
			break
		}
	}
	if !finished || !strings.HasSuffix(all.String(), "ACTIVITY-TAIL-REACHABLE") || strings.Contains(all.String(), "private-pagination") || strings.Contains(all.String(), "zzzz") || !strings.Contains(all.String(), "REDACTED_CREDENTIAL") {
		t.Fatal("long activity tail or whole-record redaction failed", finished, len(all.String()))
	}
}

func TestWriteupArtifactPaginationReachesTailAfterRedaction(t *testing.T) {
	s, c := ctfRegressionServer(t)
	root := s.conversationWorkDir(c.ID)
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	content := strings.Repeat("x", 70000) + "\npassword=\"private-artifact-page-secret\"\n" + strings.Repeat("结", 2000) + "\nARTIFACT-TAIL-REACHABLE"
	if err := os.WriteFile(filepath.Join(root, "long.txt"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	read := workflowTool(t, s.ctfWriteupTools(), "writeup_read_artifact")
	var all strings.Builder
	offset := 0
	finished := false
	for range 20 {
		raw := workflowCall(t, t.Context(), read, map[string]any{"conversation_id": c.ID, "path": "long.txt", "offset_chars": offset, "limit_chars": 5000, "max_bytes": 6000}, false)
		var page struct {
			Content string `json:"content"`
			Next    int    `json:"next_offset_chars"`
			More    bool   `json:"has_more"`
		}
		if err := json.Unmarshal([]byte(raw), &page); err != nil || !utf8.ValidString(page.Content) || len(page.Content) > 6000 || page.Next <= offset {
			t.Fatal("invalid artifact page", raw, err)
		}
		all.WriteString(page.Content)
		offset = page.Next
		if !page.More {
			finished = true
			break
		}
	}
	if !finished || !strings.HasSuffix(all.String(), "ARTIFACT-TAIL-REACHABLE") || strings.Contains(all.String(), "private-artifact") || !strings.Contains(all.String(), "REDACTED_CREDENTIAL") {
		t.Fatal("long artifact tail or redaction failed", finished, len(all.String()))
	}
	workflowCall(t, t.Context(), read, map[string]any{"conversation_id": c.ID, "path": "long.txt", "offset_chars": -1}, true)
}
