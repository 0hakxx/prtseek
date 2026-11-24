package agent

import (
	"fmt"

	"github.com/Autumn-27/norma/agentcore"
)

// SessionResumeError stops a run before its first model or tool call when its
// persisted conversation cannot be read. Retrying the whole intent must not
// silently turn that conversation into a fresh run and repeat prior effects.
type SessionResumeError struct {
	SessionID string
	Cause     error
}

func (e *SessionResumeError) Error() string {
	return fmt.Sprintf("대화 %q 의 기록을 읽지 못해 이번 턴을 중지했으며, 모델 호출이나 도구 실행은 하지 않았습니다. 기록 파일을 확인한 뒤 계속하세요.", e.SessionID)
}

func (e *SessionResumeError) Unwrap() error { return e.Cause }

func resumeSession(s *agentcore.Session, sessionID string) error {
	if err := s.Resume(sessionID); err != nil {
		return &SessionResumeError{SessionID: sessionID, Cause: err}
	}
	// The SDK already treats an absent transcript as a new session. Other read
	// errors, including a corrupt JSONL line, must reach the caller unchanged.
	return nil
}
