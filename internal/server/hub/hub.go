// Package hub는 Runner의 아웃바운드 WebSocket 연결을 받는다.
//
// 스파이크는 Runner 한 대만 다룬다. 연결이 하나뿐이므로 라우팅 대신
// 뮤텍스로 보호되는 현재 연결 하나를 들고 있는다.
package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/jinto/taskyard/internal/protocol"
	"github.com/jinto/taskyard/internal/server/store"
)

// ErrNoRunner는 연결된 Runner가 없을 때 반환한다.
var ErrNoRunner = errors.New("no runner connected")

// readTimeout을 넘겨도 아무 메시지가 없으면 연결이 죽은 것으로 본다.
// Runner는 이보다 훨씬 짧은 주기로 heartbeat를 보낸다.
const readTimeout = 45 * time.Second

type Hub struct {
	st           *store.Store
	pairingToken string

	mu   sync.Mutex
	conn *websocket.Conn

	subMu sync.Mutex
	subs  map[chan protocol.Envelope]struct{}
}

func New(st *store.Store, pairingToken string) *Hub {
	return &Hub{
		st:           st,
		pairingToken: pairingToken,
		subs:         map[chan protocol.Envelope]struct{}{},
	}
}

func (h *Hub) Connected() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.conn != nil
}

// DropConnection은 현재 연결을 강제로 끊는다. 테스트에서 단절을 흉내내고,
// 운영에서는 Runner 자격증명 취소에 쓴다.
func (h *Hub) DropConnection() {
	h.mu.Lock()
	conn := h.conn
	h.conn = nil
	h.mu.Unlock()

	if conn != nil {
		_ = conn.Close(websocket.StatusGoingAway, "dropped by server")
	}
}

// Subscribe는 적용된 이벤트를 받는 채널과 해지 함수를 돌려준다.
func (h *Hub) Subscribe() (<-chan protocol.Envelope, func()) {
	ch := make(chan protocol.Envelope, 256)

	h.subMu.Lock()
	h.subs[ch] = struct{}{}
	h.subMu.Unlock()

	return ch, func() {
		h.subMu.Lock()
		if _, ok := h.subs[ch]; ok {
			delete(h.subs, ch)
			close(ch)
		}
		h.subMu.Unlock()
	}
}

func (h *Hub) fanout(env protocol.Envelope) {
	h.subMu.Lock()
	defer h.subMu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- env:
		default:
			// 느린 구독자 때문에 이벤트 적용이 막히면 안 된다. 흘린다.
			slog.Warn("dropping event for slow subscriber", "run_id", env.RunID, "seq", env.Seq)
		}
	}
}

// SendCommand는 연결된 Runner에 명령을 보낸다.
func (h *Hub) SendCommand(env protocol.Envelope) error {
	h.mu.Lock()
	conn := h.conn
	h.mu.Unlock()

	if conn == nil {
		return ErrNoRunner
	}

	raw, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal command: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
		return fmt.Errorf("write command: %w", err)
	}
	return nil
}

// ServeWS는 Runner의 연결을 받아 handshake 후 메시지 루프를 돈다.
func (h *Hub) ServeWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Runner는 브라우저가 아니므로 Origin 검사를 요구하지 않는다.
		InsecureSkipVerify: true,
	})
	if err != nil {
		slog.Error("websocket accept failed", "err", err)
		return
	}
	defer conn.CloseNow()

	ctx := r.Context()

	hello, err := h.readHello(ctx, conn)
	if err != nil {
		slog.Warn("handshake rejected", "err", err)
		_ = conn.Close(websocket.StatusPolicyViolation, err.Error())
		return
	}

	welcome, err := protocol.Negotiate(hello)
	if err != nil {
		slog.Warn("negotiation failed", "runner", hello.RunnerID, "err", err)
		_ = conn.Close(websocket.StatusPolicyViolation, err.Error())
		return
	}

	points, err := h.st.ResumePoints()
	if err != nil {
		slog.Error("resume points failed", "err", err)
		_ = conn.Close(websocket.StatusInternalError, "resume points")
		return
	}
	welcome.ResumeFrom = points

	if err := h.writeEnvelope(ctx, conn, protocol.Envelope{
		V:    welcome.ProtocolVersion,
		Kind: protocol.KindWelcome,
		TS:   time.Now().UTC(),
		Body: mustJSON(welcome),
	}); err != nil {
		slog.Error("write welcome failed", "err", err)
		return
	}

	h.mu.Lock()
	h.conn = conn
	h.mu.Unlock()
	slog.Info("runner connected", "runner", hello.RunnerID)

	defer func() {
		h.mu.Lock()
		if h.conn == conn {
			h.conn = nil
		}
		h.mu.Unlock()
		slog.Info("runner disconnected", "runner", hello.RunnerID)
	}()

	h.readLoop(ctx, conn)
}

func (h *Hub) readHello(ctx context.Context, conn *websocket.Conn) (protocol.Hello, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	_, raw, err := conn.Read(ctx)
	if err != nil {
		return protocol.Hello{}, fmt.Errorf("read hello: %w", err)
	}

	var env protocol.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return protocol.Hello{}, fmt.Errorf("unmarshal hello envelope: %w", err)
	}
	if env.Kind != protocol.KindHello {
		return protocol.Hello{}, fmt.Errorf("first message kind = %q, want %q", env.Kind, protocol.KindHello)
	}

	var hello protocol.Hello
	if err := json.Unmarshal(env.Body, &hello); err != nil {
		return protocol.Hello{}, fmt.Errorf("unmarshal hello body: %w", err)
	}
	if hello.PairingToken != h.pairingToken {
		return protocol.Hello{}, errors.New("invalid pairing token")
	}
	return hello, nil
}

func (h *Hub) readLoop(ctx context.Context, conn *websocket.Conn) {
	for {
		readCtx, cancel := context.WithTimeout(ctx, readTimeout)
		_, raw, err := conn.Read(readCtx)
		cancel()
		if err != nil {
			return
		}

		var env protocol.Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			slog.Warn("dropping malformed message", "err", err)
			continue
		}

		if env.Kind != protocol.KindEvent || env.Type == protocol.EvHeartbeat {
			// heartbeat 등 이벤트가 아닌 것은 읽은 것만으로 생존 신호가 된다.
			continue
		}

		accepted, ack, err := h.st.ApplyEvent(env)
		if err != nil {
			slog.Error("apply event failed", "run_id", env.RunID, "seq", env.Seq, "err", err)
			continue
		}
		if accepted {
			h.fanout(env)
		}

		if err := h.writeEnvelope(ctx, conn, protocol.Envelope{
			V:     env.V,
			Kind:  protocol.KindAck,
			RunID: env.RunID,
			Seq:   ack,
			TS:    time.Now().UTC(),
		}); err != nil {
			slog.Warn("write ack failed", "err", err)
			return
		}
	}
}

func (h *Hub) writeEnvelope(ctx context.Context, conn *websocket.Conn, env protocol.Envelope) error {
	raw, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}
	writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return conn.Write(writeCtx, websocket.MessageText, raw)
}

func mustJSON(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("marshal: %v", err))
	}
	return raw
}
