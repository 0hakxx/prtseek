package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	actool "github.com/Autumn-27/norma/tool"
)

// Writeup has no shell, network, generic MCP or unrestricted filesystem tool.
// Historical material comes from the server's read-only citation tools; its own
// report and script text can only be read/written through this rooted handle.
func ctfWriteupWorkspaceTools(workDir string) []actool.CoreTool {
	pathSchema := map[string]any{"type": "string", "description": "自己工作目录内的相对文件路径，例如 WRITEUP.md；不能读取或修改其他会话"}
	validPath := func(name string) bool {
		return filepath.IsLocal(name) && name != "." && path.Clean(name) == name && !strings.ContainsAny(name, "\\:\x00\r\n") && !strings.HasPrefix(name, ".")
	}
	openRoot := func() (*os.Root, error) {
		for _, name := range []string{filepath.Dir(workDir), workDir} {
			info, err := os.Lstat(name)
			if err != nil {
				return nil, err
			}
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return nil, os.ErrPermission
			}
		}
		return os.OpenRoot(workDir)
	}
	return []actool.CoreTool{
		writeTool("Write", "在自己的工作目录保存 WP 或脚本文本；不会执行脚本，也不能修改其他会话。", map[string]any{
			"type": "object", "properties": map[string]any{"file_path": pathSchema, "content": map[string]any{"type": "string"}}, "required": []string{"file_path", "content"},
		}, func(ctx context.Context, raw json.RawMessage) (actool.Result, error) {
			var req struct {
				Path    string `json:"file_path"`
				Content string `json:"content"`
			}
			if ctx.Err() != nil || len(raw) > 2<<20 || json.Unmarshal(raw, &req) != nil || !validPath(req.Path) || len(req.Content) > 512<<10 {
				return actool.Errorf("需要工作目录内的相对路径和不超过512KiB的文本"), nil
			}
			root, err := openRoot()
			if err != nil {
				return actool.Errorf("工作目录不可用"), nil
			}
			defer root.Close()
			if err = root.MkdirAll(filepath.Dir(req.Path), 0700); err != nil {
				return actool.Errorf("无法创建文件目录"), nil
			}
			// Replace the local directory entry instead of truncating an existing
			// inode, which could be a hard link to another conversation's file.
			var nonce [12]byte
			if _, err = rand.Read(nonce[:]); err != nil {
				return actool.Errorf("无法准备报告文件"), nil
			}
			temp := path.Join(filepath.Dir(req.Path), ".wp-"+hex.EncodeToString(nonce[:]))
			f, err := root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if err != nil {
				return actool.Errorf("不能写入该路径"), nil
			}
			defer root.Remove(temp)
			_, writeErr := io.WriteString(f, req.Content)
			closeErr := f.Close()
			if writeErr != nil || closeErr != nil {
				return actool.Errorf("保存文件失败"), nil
			}
			if err = root.Rename(temp, req.Path); err != nil {
				return actool.Errorf("无法保存报告文件"), nil
			}
			return jsonResult(map[string]any{"path": req.Path, "bytes": len(req.Content), "scope": "own_workspace"})
		}),
		readTool("Read", "读取自己工作目录内的报告或脚本文本；历史会话资料使用 writeup_read_artifact。", map[string]any{
			"type": "object", "properties": map[string]any{"file_path": pathSchema}, "required": []string{"file_path"},
		}, func(ctx context.Context, raw json.RawMessage) (actool.Result, error) {
			var req struct {
				Path string `json:"file_path"`
			}
			if ctx.Err() != nil || json.Unmarshal(raw, &req) != nil || !validPath(req.Path) {
				return actool.Errorf("需要自己工作目录内的相对路径"), nil
			}
			root, err := openRoot()
			if err != nil {
				return actool.Errorf("工作目录不可用"), nil
			}
			defer root.Close()
			f, err := root.Open(req.Path)
			if err != nil {
				return actool.Errorf("不能读取该路径"), nil
			}
			defer f.Close()
			info, err := f.Stat()
			if err != nil || !info.Mode().IsRegular() {
				return actool.Errorf("只支持普通文本文件"), nil
			}
			content, err := io.ReadAll(io.LimitReader(f, (64<<10)+1))
			if err != nil {
				return actool.Errorf("读取失败"), nil
			}
			truncated := len(content) > 64<<10
			if truncated {
				content = content[:64<<10]
			}
			return jsonResult(map[string]any{"path": req.Path, "content": RedactResearchText(string(content)), "truncated": truncated, "scope": "own_workspace"})
		}),
	}
}
