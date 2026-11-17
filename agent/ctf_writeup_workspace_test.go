package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCTFWriteupWorkspaceCannotWriteOtherConversations(t *testing.T) {
	root := t.TempDir()
	other := filepath.Join(t.TempDir(), "original.txt")
	if err := os.WriteFile(other, []byte("preserve source"), 0600); err != nil {
		t.Fatal(err)
	}
	tools := ctfWriteupWorkspaceTools(root)
	for _, name := range []string{"../original.txt", other, "C:/outside.txt", "nested/../../original.txt", ".research/state.json", "report.md:stream"} {
		raw, _ := json.Marshal(map[string]any{"file_path": name, "content": "replace"})
		result, err := tools[0].Call(t.Context(), raw, nil)
		if err != nil || !result.IsError {
			t.Fatalf("outside write accepted %q: %v %s", name, err, result.Flatten())
		}
	}
	raw, _ := json.Marshal(map[string]any{"file_path": "wp/WRITEUP.md", "content": "synthetic local writeup"})
	result, err := tools[0].Call(t.Context(), raw, nil)
	if err != nil || result.IsError {
		t.Fatal(err, result.Flatten())
	}
	result, err = tools[1].Call(t.Context(), json.RawMessage(`{"file_path":"wp/WRITEUP.md"}`), nil)
	if err != nil || result.IsError || !strings.Contains(result.Flatten(), "synthetic local writeup") {
		t.Fatal(err, result.Flatten())
	}
	data, _ := os.ReadFile(other)
	if string(data) != "preserve source" {
		t.Fatal("source conversation was modified")
	}
	if err := os.Link(other, filepath.Join(root, "linked.txt")); err == nil {
		raw, _ = json.Marshal(map[string]any{"file_path": "linked.txt", "content": "own replacement"})
		result, err = tools[0].Call(t.Context(), raw, nil)
		if err != nil || result.IsError {
			t.Fatal("could not replace local report entry", err, result.Flatten())
		}
		data, _ = os.ReadFile(other)
		if string(data) != "preserve source" {
			t.Fatal("writing a hard link modified another conversation")
		}
	}
	if err := os.Symlink(filepath.Dir(other), filepath.Join(root, "source")); err == nil {
		raw, _ = json.Marshal(map[string]any{"file_path": "source/original.txt", "content": "replace"})
		result, err = tools[0].Call(t.Context(), raw, nil)
		if err != nil || !result.IsError {
			t.Fatal("symlink escaped workspace", err, result.Flatten())
		}
	}
}
