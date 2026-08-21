// Package approval은 Runner가 로컬호스트에 띄우는 MCP 권한 도구다.
//
// Claude Code는 --permission-prompt-tool로 지정된 MCP 도구를 호출해
// 승인을 묻는다. 호출은 사람이 결정할 때까지 블록되고, 결정이 오면
// PermissionResult JSON을 텍스트 블록에 담아 돌려준다.
//
// 요청 구조체의 필드 태그는 Task 8 Step 3에서 캡처한 실제 요청에 맞춘다.
// 캡처와 다르면 캡처가 이긴다.
package approval

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"

	"github.com/google/uuid"
)

// Request는 사람에게 올라갈 승인 요청 하나다.
type Request struct {
	ID       string          `json:"id"`
	ToolName string          `json:"tool_name"`
	Input    json.RawMessage `json:"input"`
}

// Decision은 사람이 내린 결정이다.
type Decision struct {
	Allow        bool            `json:"allow"`
	Message      string          `json:"message,omitempty"`
	UpdatedInput json.RawMessage `json:"updated_input,omitempty"`
}

type Broker struct {
	token string

	mu      sync.Mutex
	pending map[string]chan Decision

	requests chan Request
}

func New(token string) *Broker {
	return &Broker{
		token:    token,
		pending:  map[string]chan Decision{},
		requests: make(chan Request, 64),
	}
}

// Requests는 새 승인 요청 스트림이다.
func (b *Broker) Requests() <-chan Request { return b.requests }

// Decide는 대기 중인 요청에 결정을 전달한다.
func (b *Broker) Decide(requestID string, d Decision) error {
	b.mu.Lock()
	ch, ok := b.pending[requestID]
	if ok {
		delete(b.pending, requestID)
	}
	b.mu.Unlock()

	if !ok {
		return fmt.Errorf("approval: unknown request %q", requestID)
	}
	ch <- d
	return nil
}

func (b *Broker) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", b.serveMCP)
	return mux
}

type jsonrpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func (b *Broker) serveMCP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+b.token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var req jsonrpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	switch req.Method {
	case "initialize":
		b.reply(w, req.ID, map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "taskyard", "version": "0"},
		})

	case "tools/list":
		b.reply(w, req.ID, map[string]any{
			"tools": []map[string]any{{
				"name":        "approve",
				"description": "Ask the Taskyard user to approve or deny a tool call.",
				"inputSchema": map[string]any{"type": "object"},
			}},
		})

	case "tools/call":
		b.handleToolCall(w, req)

	default:
		// notifications/initialized 등 응답이 필요 없는 알림.
		w.WriteHeader(http.StatusAccepted)
	}
}

// toolCallArguments의 태그는 Step 3의 캡처에 맞춘다.
type toolCallArguments struct {
	ToolName string          `json:"tool_name"`
	Input    json.RawMessage `json:"input"`
}

func (b *Broker) handleToolCall(w http.ResponseWriter, req jsonrpcRequest) {
	var params struct {
		Name      string            `json:"name"`
		Arguments toolCallArguments `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		b.replyError(w, req.ID, fmt.Sprintf("bad params: %v", err))
		return
	}

	id := uuid.NewString()
	ch := make(chan Decision, 1)

	b.mu.Lock()
	b.pending[id] = ch
	b.mu.Unlock()

	slog.Info("approval requested", "id", id, "tool", params.Arguments.ToolName)

	b.requests <- Request{
		ID:       id,
		ToolName: params.Arguments.ToolName,
		Input:    params.Arguments.Input,
	}

	// 사람이 결정할 때까지 무기한 기다린다. Claude Code는 그동안 멈춘다.
	d := <-ch

	result, err := permissionResult(d, params.Arguments.Input)
	if err != nil {
		b.replyError(w, req.ID, err.Error())
		return
	}

	b.reply(w, req.ID, map[string]any{
		"content": []map[string]any{{"type": "text", "text": result}},
	})
}

// permissionResult는 결정을 Claude Code가 이해하는 PermissionResult JSON으로
// 바꾼다. allow는 반드시 updatedInput을 포함해야 한다.
func permissionResult(d Decision, original json.RawMessage) (string, error) {
	var payload map[string]any

	if d.Allow {
		updated := d.UpdatedInput
		if len(updated) == 0 {
			updated = original
		}
		if len(updated) == 0 {
			updated = json.RawMessage(`{}`)
		}
		payload = map[string]any{
			"behavior":     "allow",
			"updatedInput": json.RawMessage(updated),
		}
	} else {
		msg := d.Message
		if msg == "" {
			msg = "Denied by the Taskyard user."
		}
		payload = map[string]any{"behavior": "deny", "message": msg}
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal permission result: %w", err)
	}
	return string(raw), nil
}

func (b *Broker) reply(w http.ResponseWriter, id json.RawMessage, result any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	}); err != nil {
		slog.Error("write mcp reply failed", "err", err)
	}
}

func (b *Broker) replyError(w http.ResponseWriter, id json.RawMessage, msg string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": -32602, "message": msg},
	})
}
