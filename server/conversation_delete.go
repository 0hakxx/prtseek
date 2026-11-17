package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/sidequestion"
)

// Admission covers short setup/upload writes as well as the chatBusy runner.
// Checking existence here closes the gap after an HTTP handler read its row.
func (s *Server) beginConversationFileOperation(id int64) error {
	s.chatMu.Lock()
	defer s.chatMu.Unlock()
	if s.conversationDeleting[id] {
		return errors.New("会话正在删除，请稍后重试")
	}
	c, err := s.m.pg.GetConversation(id)
	if err != nil {
		return err
	}
	if c == nil {
		return errors.New("会话已不存在")
	}
	if s.conversationFileOps == nil {
		s.conversationFileOps = map[int64]int{}
	}
	s.conversationFileOps[id]++
	return nil
}

func (s *Server) endConversationFileOperation(id int64) {
	s.chatMu.Lock()
	defer s.chatMu.Unlock()
	s.conversationFileOps[id]--
	if s.conversationFileOps[id] <= 0 {
		delete(s.conversationFileOps, id)
	}
}

// File-panel writes to session directories use the same admission barrier.
func (s *Server) beginWorkspaceConversationWrite(abs string) (func(), error) {
	parts := strings.Split(filepath.ToSlash(s.wsRel(abs)), "/")
	if len(parts) < 2 || !strings.EqualFold(parts[0], "sessions") {
		return func() {}, nil
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(strings.ToLower(parts[1]), "conv-"), 10, 64)
	if err != nil || id <= 0 {
		return func() {}, nil
	}
	if err = s.beginConversationFileOperation(id); err != nil {
		return nil, err
	}
	return func() { s.endConversationFileOperation(id) }, nil
}

// Exact IDs, not a prefix glob: deleting conv-1 must never select conv-10.
// Rooted removal unlinks a target symlink without traversing its destination.
func deleteConversationFiles(dataDir string, id int64) error {
	if id <= 0 {
		return errors.New("invalid conversation ID")
	}
	base, err := filepath.Abs(dataDir)
	if err != nil {
		return err
	}
	info, err := os.Lstat(base)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("会话数据根目录不可安全访问")
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		return err
	}
	defer root.Close()
	// Validate all parents before deleting anything. No traversal through a
	// replaced sessions/transcripts/noa directory is permitted.
	for _, parent := range []string{"sessions", "transcripts", "noa"} {
		info, err := root.Lstat(parent)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s 不是安全的会话数据目录", parent)
		}
	}
	n := strconv.FormatInt(id, 10)
	for _, relative := range []string{"sessions/conv-" + n, "sessions/" + n, "transcripts/conv-" + n + ".jsonl", "transcripts/conv-" + n, "noa/chat-conv-" + n} {
		full := filepath.Join(base, filepath.FromSlash(relative))
		if !strings.HasPrefix(full, base+string(os.PathSeparator)) {
			return errors.New("会话清理路径越界")
		}
		if err := root.RemoveAll(filepath.FromSlash(relative)); err != nil {
			return fmt.Errorf("清理 %s 失败: %w", relative, err)
		}
	}
	return nil
}

func (s *Server) deleteConversationWithFiles(ctx context.Context, id int64) error {
	if id <= 0 {
		return errors.New("invalid conversation ID")
	}
	s.chatMu.Lock()
	if s.conversationDeleting[id] {
		s.chatMu.Unlock()
		return errors.New("会话正在删除")
	}
	if s.conversationDeleting == nil {
		s.conversationDeleting = map[int64]bool{}
	}
	s.conversationDeleting[id] = true
	s.chatMu.Unlock()
	defer func() { s.chatMu.Lock(); delete(s.conversationDeleting, id); s.chatMu.Unlock() }()
	c, err := s.m.pg.GetConversation(id)
	if err != nil {
		return err
	}
	if c == nil {
		return errors.New("conversation not found")
	}
	if run, err := s.m.pg.PersonalBackgroundRun(id, ""); err != nil {
		return err
	} else if run != nil {
		return errors.New("此会话关联后台执行记录，请保留会话以便回查结果")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		s.cancelConversation(id)
		s.chatMu.Lock()
		busy := s.chatBusy[s.convBusyKey(id)] || s.conversationFileOps[id] > 0
		s.chatMu.Unlock()
		if !busy {
			break
		}
		select {
		case <-ctx.Done():
			return errors.New("会话仍在停止或写入中，文件与记录尚未删除，请稍后重试")
		case <-ticker.C:
		}
	}
	// Approval side questions identify their parent by approval ID instead of
	// conversation ID. Drain both kinds before removing the owning DB row.
	approvals := map[int64]bool{}
	rows, err := s.m.pg.QueryContext(ctx, `SELECT id FROM intercept_pending WHERE conversation_id=$1`, id)
	if err != nil {
		return err
	}
	for rows.Next() {
		var approval int64
		if err := rows.Scan(&approval); err != nil {
			rows.Close()
			return err
		}
		approvals[approval] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	match := func(p sidequestion.Parent) bool { return p.ConversationID == id || approvals[p.ApprovalID] }
	if s.side != nil {
		s.side.commands.Lock()
		defer s.side.commands.Unlock()
		for _, done := range s.cancelSideWhere(match) {
			select {
			case <-done:
			case <-ctx.Done():
				return errors.New("会话旁路任务仍在停止，请稍后重试")
			}
		}
		s.side.mu.Lock()
		for key, snap := range s.side.pending {
			if match(snap.Parent) {
				delete(s.side.pending, key)
				delete(s.side.seen, key)
			}
		}
		for key, snap := range s.side.latest {
			if match(snap.Parent) {
				delete(s.side.latest, key)
				delete(s.side.seen, key)
			}
		}
		s.side.mu.Unlock()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Keep the row visible on any filesystem error so the user can retry. A
	// successful response means the files have physically been removed.
	if err := deleteConversationFiles(s.m.dir, id); err != nil {
		return fmt.Errorf("工作区未能完整清理，会话记录保留：%w", err)
	}
	return s.m.pg.DeleteConversation(id)
}
