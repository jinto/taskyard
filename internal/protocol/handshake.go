package protocol

import (
	"errors"

	"github.com/jinto/taskyard/internal/buildinfo"
)

// Runner가 광고하는 역량. 스파이크에서는 목록을 최소로 유지한다.
const (
	CapClaudeCode     = "adapter.claudecode"
	CapApprovalBroker = "approval.broker"
	CapGitWorktree    = "git.worktree"
)

var (
	// ErrProtocolMismatch는 major 버전이 다를 때 반환한다. 스파이크에서는
	// 정확히 일치할 때만 받아들인다.
	ErrProtocolMismatch = errors.New("protocol version mismatch")
	// ErrMissingRunnerID는 Runner가 자신을 식별하지 않았을 때 반환한다.
	ErrMissingRunnerID = errors.New("hello is missing runner_id")
)

// Hello는 Runner가 연결 직후 보내는 첫 메시지다.
type Hello struct {
	ProtocolVersion int      `json:"protocol_version"`
	RunnerID        string   `json:"runner_id"`
	PairingToken    string   `json:"pairing_token,omitempty"`
	Capabilities    []string `json:"capabilities"`
}

// Has는 Runner가 해당 역량을 광고했는지 확인한다.
func (h Hello) Has(capability string) bool {
	for _, c := range h.Capabilities {
		if c == capability {
			return true
		}
	}
	return false
}

// Welcome은 Server의 응답이다. ResumeFrom은 Run별로 Server가 마지막으로
// 연속 적용한 seq이며, Runner는 그 다음 seq부터 spool을 재전송한다.
type Welcome struct {
	ProtocolVersion int               `json:"protocol_version"`
	ServerVersion   string            `json:"server_version"`
	Capabilities    []string          `json:"capabilities"`
	ResumeFrom      map[string]uint64 `json:"resume_from"`
}

// Negotiate는 Hello를 검증하고 Welcome을 만든다. ResumeFrom은 비어 있으며
// 호출자가 store에서 채운다.
func Negotiate(h Hello) (Welcome, error) {
	if h.RunnerID == "" {
		return Welcome{}, ErrMissingRunnerID
	}
	if h.ProtocolVersion != buildinfo.ProtocolVersion() {
		return Welcome{}, ErrProtocolMismatch
	}
	return Welcome{
		ProtocolVersion: buildinfo.ProtocolVersion(),
		ServerVersion:   buildinfo.Version(),
		Capabilities:    []string{CapClaudeCode, CapApprovalBroker, CapGitWorktree},
		ResumeFrom:      map[string]uint64{},
	}, nil
}
