// Package claudecode는 Claude Code의 headless 출력을 정규화 이벤트로 바꾼다.
//
// 입력은 `claude -p --output-format stream-json --verbose`의 NDJSON이다.
// 터미널 화면을 파싱하지 않는다. PRD §11.6.1의 표면을 그대로 쓴다.
package claudecode

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"

	"github.com/jinto/taskyard/internal/agents/adapter"
	"github.com/jinto/taskyard/internal/protocol"
)

// maxLineBytes는 한 NDJSON 줄의 상한이다. 큰 도구 결과가 그대로 실려 오므로
// bufio의 기본값(64KB)으로는 부족하다.
const maxLineBytes = 8 << 20 // 8MB

type Parser struct {
	session adapter.SessionInfo
}

func NewParser() *Parser { return &Parser{} }

// Session은 init 이벤트에서 읽은 세션 정보를 돌려준다. Parse 도중이나
// 이후에 호출한다.
func (p *Parser) Session() adapter.SessionInfo { return p.session }

// Parse는 NDJSON을 끝까지 읽으며 정규화 이벤트마다 emit을 호출한다.
// 깨진 줄 하나가 스트림 전체를 죽이지 않도록, 파싱 실패는 error 이벤트로
// 바꾸고 계속 읽는다. emit이 에러를 돌려주면 그때는 즉시 중단한다.
func (p *Parser) Parse(r io.Reader, emit func(adapter.Event) error) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineBytes)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		// scanner는 버퍼를 재사용한다. Raw로 들고 있으려면 복사해야 한다.
		raw := make([]byte, len(line))
		copy(raw, line)

		var msg streamMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			if emitErr := emit(adapter.Event{
				Type: protocol.EvError,
				Body: map[string]any{"reason": "unparseable stream line", "detail": err.Error()},
				Raw:  raw,
			}); emitErr != nil {
				return emitErr
			}
			continue
		}

		if err := p.dispatch(msg, raw, emit); err != nil {
			return err
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read stream: %w", err)
	}
	return nil
}

func (p *Parser) dispatch(msg streamMessage, raw json.RawMessage, emit func(adapter.Event) error) error {
	switch msg.Type {
	case "system":
		if msg.Subtype == "init" {
			p.session = adapter.SessionInfo{
				SessionID:    msg.SessionID,
				Model:        msg.Model,
				Version:      msg.ClaudeCodeVersion,
				APIKeySource: msg.APIKeySource,
			}
		}
		// hook_started, hook_response, plugin_install 등은 정규화 대상이 아니다.
		return nil

	case "assistant":
		return p.emitContentBlocks(msg, raw, emit)

	case "user":
		return p.emitContentBlocks(msg, raw, emit)

	case "rate_limit_event":
		info := msg.RateLimitInfo
		return emit(adapter.Event{
			Type: protocol.EvUsageUpdated,
			Body: map[string]any{
				"status":          info.Status,
				"rate_limit_type": info.RateLimitType,
				"resets_at":       info.ResetsAt,
				"using_overage":   info.IsUsingOverage,
				// 스케줄러는 이 값만 보고 paused_quota 전이를 판단한다(EX-06).
				"quota_exhausted": info.Status != "allowed",
			},
			Raw: raw,
		})

	case "result":
		return emit(adapter.Event{
			Type: protocol.EvTurnCompleted,
			Body: map[string]any{
				"result":         msg.Result,
				"is_error":       msg.IsError,
				"num_turns":      msg.NumTurns,
				"duration_ms":    msg.DurationMS,
				"total_cost_usd": msg.TotalCostUSD,
				"stop_reason":    msg.StopReason,
				"session_id":     msg.SessionID,
			},
			Raw: raw,
		})

	default:
		// stream_event(부분 델타) 등 아직 정규화하지 않는 종류.
		return nil
	}
}

func (p *Parser) emitContentBlocks(msg streamMessage, raw json.RawMessage, emit func(adapter.Event) error) error {
	for _, block := range msg.Message.Content {
		switch block.Type {
		case "text":
			if block.Text == "" {
				continue
			}
			if err := emit(adapter.Event{
				Type: protocol.EvMessageDelta,
				Body: map[string]any{
					"text":               block.Text,
					"parent_tool_use_id": msg.ParentToolUseID,
				},
				Raw: raw,
			}); err != nil {
				return err
			}

		case "tool_use":
			if err := emit(adapter.Event{
				Type: protocol.EvToolStarted,
				Body: map[string]any{
					"tool_use_id": block.ID,
					"tool_name":   block.Name,
					"input":       block.Input,
				},
				Raw: raw,
			}); err != nil {
				return err
			}

		case "tool_result":
			if err := emit(adapter.Event{
				Type: protocol.EvToolFinished,
				Body: map[string]any{
					"tool_use_id": block.ToolUseID,
					"is_error":    block.IsError,
				},
				Raw: raw,
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

// streamMessage는 stream-json 한 줄의 관심 필드만 담는다. 모르는 필드는
// 무시하되 원본은 Raw로 보존한다.
type streamMessage struct {
	Type              string `json:"type"`
	Subtype           string `json:"subtype"`
	SessionID         string `json:"session_id"`
	Model             string `json:"model"`
	ClaudeCodeVersion string `json:"claude_code_version"`
	APIKeySource      string `json:"apiKeySource"`
	ParentToolUseID   any    `json:"parent_tool_use_id"`

	Message struct {
		Role    string         `json:"role"`
		Content []contentBlock `json:"content"`
	} `json:"message"`

	RateLimitInfo struct {
		Status         string `json:"status"`
		RateLimitType  string `json:"rateLimitType"`
		ResetsAt       int64  `json:"resetsAt"`
		IsUsingOverage bool   `json:"isUsingOverage"`
	} `json:"rate_limit_info"`

	Result       string  `json:"result"`
	IsError      bool    `json:"is_error"`
	NumTurns     int     `json:"num_turns"`
	DurationMS   int64   `json:"duration_ms"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	StopReason   string  `json:"stop_reason"`
}

type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
}
