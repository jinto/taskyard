// Package protocol은 Server와 Runner가 주고받는 메시지 형식을 정의한다.
//
// 모든 메시지는 하나의 봉투(Envelope)에 담긴다. 명령은 고유한 ID를 가지고
// Runner가 중복 수신해도 한 번만 적용하며, 이벤트는 Run별로 단조 증가하는
// seq를 가져 Server가 멱등하게 적용한다. PRD §11.5를 따른다.
package protocol

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jinto/taskyard/internal/buildinfo"
)

// 봉투 종류.
const (
	KindCommand = "command"
	KindEvent   = "event"
	KindAck     = "ack"
	KindHello   = "hello"
	KindWelcome = "welcome"
)

// Server → Runner 명령 타입.
const (
	CmdRunStart         = "run.start"
	CmdRunCancel        = "run.cancel"
	CmdRunReconcile     = "run.reconcile"
	CmdApprovalDecision = "approval.decision"
)

// Runner → Server 이벤트 타입. 어댑터가 Provider 이벤트를 여기로 정규화한다.
const (
	EvRunStateChanged   = "run.state_changed"
	EvMessageDelta      = "message_delta"
	EvToolStarted       = "tool_started"
	EvToolFinished      = "tool_finished"
	EvFileChanged       = "file_changed"
	EvApprovalRequested = "approval_requested"
	EvUsageUpdated      = "usage_updated"
	EvTurnCompleted     = "turn_completed"
	EvError             = "error"
	EvHeartbeat         = "runner.heartbeat"
)

// Envelope은 모든 메시지의 공통 껍데기다.
type Envelope struct {
	V     int             `json:"v"`
	Kind  string          `json:"kind"`
	ID    string          `json:"id"`
	Type  string          `json:"type"`
	RunID string          `json:"run_id,omitempty"`
	Seq   uint64          `json:"seq,omitempty"`
	TS    time.Time       `json:"ts"`
	Body  json.RawMessage `json:"body,omitempty"`
}

func marshalBody(body any) (json.RawMessage, error) {
	if body == nil {
		return nil, nil
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal body: %w", err)
	}
	return raw, nil
}

// NewCommand는 새 명령 봉투를 만든다. ID는 Runner의 멱등성 판정 키다.
func NewCommand(cmdType, runID string, body any) (Envelope, error) {
	raw, err := marshalBody(body)
	if err != nil {
		return Envelope{}, err
	}
	return Envelope{
		V:     buildinfo.ProtocolVersion(),
		Kind:  KindCommand,
		ID:    uuid.NewString(),
		Type:  cmdType,
		RunID: runID,
		TS:    time.Now().UTC(),
		Body:  raw,
	}, nil
}

// NewEvent는 새 이벤트 봉투를 만든다. seq는 호출자가 spool에서 발급받아 넘긴다.
func NewEvent(evType, runID string, seq uint64, body any) (Envelope, error) {
	raw, err := marshalBody(body)
	if err != nil {
		return Envelope{}, err
	}
	return Envelope{
		V:     buildinfo.ProtocolVersion(),
		Kind:  KindEvent,
		ID:    uuid.NewString(),
		Type:  evType,
		RunID: runID,
		Seq:   seq,
		TS:    time.Now().UTC(),
		Body:  raw,
	}, nil
}
