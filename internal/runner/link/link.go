// Package link는 Runner에서 Server로 나가는 WebSocket 연결을 관리한다.
//
// 연결은 항상 Runner가 건다. 끊기면 백오프 후 다시 붙고, Welcome의
// ResumeFrom을 보고 spool에서 미확인 이벤트를 다시 흘려보낸다.
package link

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/jinto/taskyard/internal/buildinfo"
	"github.com/jinto/taskyard/internal/protocol"
	"github.com/jinto/taskyard/internal/runner/spool"
)

const (
	heartbeatInterval = 10 * time.Second
	minBackoff        = 200 * time.Millisecond
	maxBackoff        = 10 * time.Second
	resendBatch       = 200
)

// CommandHandler는 Server가 보낸 명령을 처리한다. 멱등성은 호출자 책임이다.
type CommandHandler func(ctx context.Context, env protocol.Envelope) error

type Config struct {
	ServerURL    string
	RunnerID     string
	PairingToken string
	Spool        *spool.Spool
	Capabilities []string
	OnCommand    CommandHandler
}

type Link struct {
	cfg Config

	mu   sync.Mutex
	conn *websocket.Conn

	// wake는 새 이벤트가 spool에 들어왔음을 전송 루프에 알린다.
	wake chan struct{}
}

func New(cfg Config) (*Link, error) {
	if cfg.ServerURL == "" {
		return nil, errors.New("link: ServerURL is required")
	}
	if cfg.RunnerID == "" {
		return nil, errors.New("link: RunnerID is required")
	}
	if cfg.Spool == nil {
		return nil, errors.New("link: Spool is required")
	}
	if cfg.OnCommand == nil {
		return nil, errors.New("link: OnCommand is required")
	}
	return &Link{cfg: cfg, wake: make(chan struct{}, 1)}, nil
}

// Publish는 이벤트를 spool에 적고 전송 루프를 깨운다. 연결이 끊겨 있어도
// 성공한다. 이것이 단절 중 이벤트를 잃지 않는 이유다.
func (l *Link) Publish(runID string, env protocol.Envelope) error {
	if _, err := l.cfg.Spool.Append(runID, env); err != nil {
		return fmt.Errorf("append to spool: %w", err)
	}
	select {
	case l.wake <- struct{}{}:
	default:
	}
	return nil
}

// Run은 ctx가 끝날 때까지 연결을 유지한다.
func (l *Link) Run(ctx context.Context) error {
	backoff := minBackoff
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		welcomed, err := l.session(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}

		if welcomed {
			// 이번 세션은 최소한 Welcome까지 받았다 — 즉 한 번은 정상적으로
			// 붙었다가 끊긴 것이다. 페널티를 초기화한다. 초기화하지 않으면
			// 과거에 여러 번 끊긴 적이 있는 Runner는 그 사이 세션이 아무리
			// 오래 건강했어도 다음 재연결마다 상한(10초)까지 기다리게 된다.
			backoff = minBackoff
		}
		slog.Warn("link session ended, reconnecting", "err", err, "backoff", backoff)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}

		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// session은 연결 하나의 수명이다. welcomed는 Welcome까지 받아 정상적으로
// 붙었는지를 알려준다 — Run이 이걸로 재연결 backoff를 초기화할지 정한다.
func (l *Link) session(ctx context.Context) (welcomed bool, err error) {
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	conn, _, err := websocket.Dial(dialCtx, l.cfg.ServerURL, nil)
	cancel()
	if err != nil {
		return false, fmt.Errorf("dial: %w", err)
	}
	defer conn.CloseNow()

	hello := protocol.Hello{
		ProtocolVersion: buildinfo.ProtocolVersion(),
		RunnerID:        l.cfg.RunnerID,
		PairingToken:    l.cfg.PairingToken,
		Capabilities:    l.cfg.Capabilities,
	}
	body, err := json.Marshal(hello)
	if err != nil {
		return false, fmt.Errorf("marshal hello: %w", err)
	}
	if err := writeEnvelope(ctx, conn, protocol.Envelope{
		V:    buildinfo.ProtocolVersion(),
		Kind: protocol.KindHello,
		TS:   time.Now().UTC(),
		Body: body,
	}); err != nil {
		return false, fmt.Errorf("write hello: %w", err)
	}

	welcome, err := readWelcome(ctx, conn)
	if err != nil {
		return false, fmt.Errorf("read welcome: %w", err)
	}
	slog.Info("connected to server", "server_version", welcome.ServerVersion, "runs", len(welcome.ResumeFrom))

	l.mu.Lock()
	l.conn = conn
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		if l.conn == conn {
			l.conn = nil
		}
		l.mu.Unlock()
	}()

	// Server가 이미 받은 지점까지 spool을 정리한 뒤 나머지를 다시 보낸다.
	for runID, seq := range welcome.ResumeFrom {
		if err := l.cfg.Spool.Ack(runID, seq); err != nil {
			return true, fmt.Errorf("trim spool for %s: %w", runID, err)
		}
	}

	sessionCtx, cancelSession := context.WithCancel(ctx)
	defer cancelSession()

	var wg sync.WaitGroup
	wg.Add(2)
	errCh := make(chan error, 2)

	go func() {
		defer wg.Done()
		errCh <- l.readLoop(sessionCtx, conn)
	}()
	go func() {
		defer wg.Done()
		errCh <- l.writeLoop(sessionCtx, conn)
	}()

	sessionErr := <-errCh
	cancelSession()
	_ = conn.Close(websocket.StatusNormalClosure, "session ending")
	wg.Wait()
	return true, sessionErr
}

func (l *Link) readLoop(ctx context.Context, conn *websocket.Conn) error {
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}

		var env protocol.Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			slog.Warn("dropping malformed server message", "err", err)
			continue
		}

		switch env.Kind {
		case protocol.KindAck:
			if err := l.cfg.Spool.Ack(env.RunID, env.Seq); err != nil {
				return fmt.Errorf("apply ack: %w", err)
			}
		case protocol.KindCommand:
			if err := l.cfg.OnCommand(ctx, env); err != nil {
				slog.Error("command handler failed", "command_id", env.ID, "type", env.Type, "err", err)
			}
		default:
			slog.Warn("ignoring unexpected message kind", "kind", env.Kind)
		}
	}
}

func (l *Link) writeLoop(ctx context.Context, conn *websocket.Conn) error {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	for {
		if err := l.drain(ctx, conn); err != nil {
			return err
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-l.wake:
		case <-ticker.C:
			if err := writeEnvelope(ctx, conn, protocol.Envelope{
				V:    buildinfo.ProtocolVersion(),
				Kind: protocol.KindEvent,
				Type: protocol.EvHeartbeat,
				TS:   time.Now().UTC(),
			}); err != nil {
				return fmt.Errorf("heartbeat: %w", err)
			}
		}
	}
}

// drain은 spool에 남은 미확인 이벤트를 모두 흘려보낸다. Ack이 도착하면
// readLoop이 spool에서 지우므로, 여기서는 남은 것만 반복해서 보낸다.
func (l *Link) drain(ctx context.Context, conn *websocket.Conn) error {
	runs, err := l.cfg.Spool.ActiveRuns()
	if err != nil {
		return fmt.Errorf("list active runs: %w", err)
	}

	for _, runID := range runs {
		batch, err := l.cfg.Spool.Since(runID, 0, resendBatch)
		if err != nil {
			return fmt.Errorf("read spool for %s: %w", runID, err)
		}
		for _, env := range batch {
			if err := writeEnvelope(ctx, conn, env); err != nil {
				return fmt.Errorf("write event: %w", err)
			}
		}
	}
	return nil
}

func writeEnvelope(ctx context.Context, conn *websocket.Conn, env protocol.Envelope) error {
	raw, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}
	writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return conn.Write(writeCtx, websocket.MessageText, raw)
}

func readWelcome(ctx context.Context, conn *websocket.Conn) (protocol.Welcome, error) {
	readCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	_, raw, err := conn.Read(readCtx)
	if err != nil {
		return protocol.Welcome{}, fmt.Errorf("read: %w", err)
	}

	var env protocol.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return protocol.Welcome{}, fmt.Errorf("unmarshal envelope: %w", err)
	}
	if env.Kind != protocol.KindWelcome {
		return protocol.Welcome{}, fmt.Errorf("kind = %q, want %q", env.Kind, protocol.KindWelcome)
	}

	var w protocol.Welcome
	if err := json.Unmarshal(env.Body, &w); err != nil {
		return protocol.Welcome{}, fmt.Errorf("unmarshal welcome: %w", err)
	}
	return w, nil
}
