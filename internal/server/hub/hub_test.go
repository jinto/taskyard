package hub_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/jinto/taskyard/internal/buildinfo"
	"github.com/jinto/taskyard/internal/protocol"
	"github.com/jinto/taskyard/internal/runner/link"
	"github.com/jinto/taskyard/internal/runner/spool"
	"github.com/jinto/taskyard/internal/server/hub"
	"github.com/jinto/taskyard/internal/server/store"
)

const testToken = "pair-me"

type rig struct {
	st    *store.Store
	hub   *hub.Hub
	srv   *httptest.Server
	sp    *spool.Spool
	link  *link.Link
	wsURL string
}

func newRig(t *testing.T, onCommand link.CommandHandler) *rig {
	t.Helper()

	st, err := store.Open(filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	h := hub.New(st, testToken)
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", h.ServeWS)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	sp, err := spool.Open(filepath.Join(t.TempDir(), "spool.db"))
	if err != nil {
		t.Fatalf("spool.Open: %v", err)
	}
	t.Cleanup(func() { _ = sp.Close() })

	if onCommand == nil {
		onCommand = func(context.Context, protocol.Envelope) error { return nil }
	}

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	l, err := link.New(link.Config{
		ServerURL:    wsURL,
		RunnerID:     "runner-1",
		PairingToken: testToken,
		Spool:        sp,
		Capabilities: []string{protocol.CapClaudeCode},
		OnCommand:    onCommand,
	})
	if err != nil {
		t.Fatalf("link.New: %v", err)
	}

	return &rig{st: st, hub: h, srv: srv, sp: sp, link: l, wsURL: wsURL}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestEventsFlowFromRunnerToServerAndGetAcked(t *testing.T) {
	r := newRig(t, nil)
	if err := r.st.UpsertRun(store.Run{ID: "run-1", State: store.StateRunning, Kind: "structured"}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.link.Run(ctx)

	waitFor(t, "runner connection", r.hub.Connected)

	for i := 0; i < 5; i++ {
		env, _ := protocol.NewEvent(protocol.EvMessageDelta, "run-1", 0, map[string]int{"i": i})
		if err := r.link.Publish("run-1", env); err != nil {
			t.Fatalf("Publish: %v", err)
		}
	}

	waitFor(t, "server to apply 5 events", func() bool {
		run, err := r.st.GetRun("run-1")
		return err == nil && run.LastAckedSeq == 5
	})

	waitFor(t, "spool to drain after ack", func() bool {
		n, err := r.sp.Pending("run-1")
		return err == nil && n == 0
	})
}

// TestEventsForUnknownRunAreAckedAndDropped: 서버 원장에 없는 Run의 이벤트는
// 저장하지 않되 ack는 해야 한다. ack가 없으면 러너의 spool이 영원히 남아
// 재연결마다 같은 이벤트를 다시 보낸다(findings 2번 livelock). 서버 DB를
// 지운 채 러너 spool이 살아 있는 경우가 이 경로다.
func TestEventsForUnknownRunAreAckedAndDropped(t *testing.T) {
	r := newRig(t, nil)
	// 원장에 run-ghost 를 만들지 않는다.

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.link.Run(ctx)
	waitFor(t, "runner connection", r.hub.Connected)

	for i := 0; i < 3; i++ {
		env, _ := protocol.NewEvent(protocol.EvMessageDelta, "run-ghost", 0, map[string]int{"i": i})
		if err := r.link.Publish("run-ghost", env); err != nil {
			t.Fatal(err)
		}
	}

	waitFor(t, "spool to drain although the run is unknown", func() bool {
		n, err := r.sp.Pending("run-ghost")
		return err == nil && n == 0
	})

	events, err := r.st.Events("run-ghost", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("stored %d events for an unknown run, want 0", len(events))
	}
	points, err := r.st.ResumePoints()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := points["run-ghost"]; ok {
		t.Fatal("an unknown run must not gain a ledger row")
	}
}

func TestReconnectResendsUnackedEventsWithoutDuplication(t *testing.T) {
	r := newRig(t, nil)
	if err := r.st.UpsertRun(store.Run{ID: "run-1", State: store.StateRunning, Kind: "structured"}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.link.Run(ctx)
	waitFor(t, "runner connection", r.hub.Connected)

	for i := 0; i < 3; i++ {
		env, _ := protocol.NewEvent(protocol.EvMessageDelta, "run-1", 0, map[string]int{"i": i})
		if err := r.link.Publish("run-1", env); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "first batch applied", func() bool {
		run, err := r.st.GetRun("run-1")
		return err == nil && run.LastAckedSeq == 3
	})

	// 연결을 강제로 끊고, 끊긴 동안 이벤트를 더 쌓는다.
	r.hub.DropConnection()
	waitFor(t, "hub to notice the drop", func() bool { return !r.hub.Connected() })

	for i := 3; i < 8; i++ {
		env, _ := protocol.NewEvent(protocol.EvMessageDelta, "run-1", 0, map[string]int{"i": i})
		if err := r.link.Publish("run-1", env); err != nil {
			t.Fatalf("Publish while disconnected: %v", err)
		}
	}

	waitFor(t, "reconnect and drain", func() bool {
		run, err := r.st.GetRun("run-1")
		return err == nil && run.LastAckedSeq == 8
	})

	events, err := r.st.Events("run-1", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 8 {
		t.Fatalf("stored %d events, want exactly 8 (no loss, no duplicates)", len(events))
	}
	for i, e := range events {
		if want := uint64(i + 1); e.Seq != want {
			t.Fatalf("events[%d].Seq = %d, want %d", i, e.Seq, want)
		}
	}
}

func TestCommandReachesRunnerHandler(t *testing.T) {
	got := make(chan protocol.Envelope, 1)
	r := newRig(t, func(_ context.Context, env protocol.Envelope) error {
		got <- env
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.link.Run(ctx)
	waitFor(t, "runner connection", r.hub.Connected)

	cmd, _ := protocol.NewCommand(protocol.CmdRunStart, "run-1", map[string]string{"prompt": "hello"})
	if err := r.hub.SendCommand(cmd); err != nil {
		t.Fatalf("SendCommand: %v", err)
	}

	select {
	case env := <-got:
		if env.Type != protocol.CmdRunStart {
			t.Fatalf("Type = %q, want %q", env.Type, protocol.CmdRunStart)
		}
		if env.ID != cmd.ID {
			t.Fatalf("command_id = %q, want %q", env.ID, cmd.ID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("command never reached the runner handler")
	}
}

func TestWrongPairingTokenIsRejected(t *testing.T) {
	r := newRig(t, nil)

	sp, err := spool.Open(filepath.Join(t.TempDir(), "other.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()

	bad, err := link.New(link.Config{
		ServerURL:    r.wsURL,
		RunnerID:     "intruder",
		PairingToken: "wrong-token",
		Spool:        sp,
		OnCommand:    func(context.Context, protocol.Envelope) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = bad.Run(ctx)

	if r.hub.Connected() {
		t.Fatal("hub accepted a runner with a wrong pairing token")
	}
}

// TestHeartbeatIsNotAppliedAsEvent은 heartbeat 봉투(run_id·seq 없음)가
// store.ApplyEvent까지 흘러들어가지 않는지 확인한다. link.go의 writeLoop이
// 보내는 heartbeat와 정확히 같은 모양의 봉투를 raw websocket으로 직접 써서,
// hub의 readLoop이 실제로 거치는 경로를 그대로 태운다.
func TestHeartbeatIsNotAppliedAsEvent(t *testing.T) {
	r := newRig(t, nil)
	if err := r.st.UpsertRun(store.Run{ID: "run-1", State: store.StateRunning, Kind: "structured"}); err != nil {
		t.Fatal(err)
	}

	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	defer slog.SetDefault(prevLogger)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, r.wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	helloBody, err := json.Marshal(protocol.Hello{
		ProtocolVersion: buildinfo.ProtocolVersion(),
		RunnerID:        "runner-hb",
		PairingToken:    testToken,
	})
	if err != nil {
		t.Fatal(err)
	}
	writeTestEnvelope(t, ctx, conn, protocol.Envelope{
		V:    buildinfo.ProtocolVersion(),
		Kind: protocol.KindHello,
		TS:   time.Now().UTC(),
		Body: helloBody,
	})
	if env := readTestEnvelope(t, ctx, conn); env.Kind != protocol.KindWelcome {
		t.Fatalf("first reply kind = %q, want %q", env.Kind, protocol.KindWelcome)
	}

	// link.go의 writeLoop이 보내는 것과 정확히 같은 모양: run_id도, seq도 없다.
	heartbeat, err := protocol.NewEvent(protocol.EvHeartbeat, "", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	writeTestEnvelope(t, ctx, conn, heartbeat)

	// 뒤이어 정상 이벤트를 하나 보내고 그 ack을 기다린다. readLoop은 한
	// 고루틴에서 순차적으로 메시지를 처리하므로, 이 ack을 받았다는 것은
	// heartbeat 처리가 이미 끝났다는 뜻이다.
	realEvent, err := protocol.NewEvent(protocol.EvMessageDelta, "run-1", 1, map[string]int{"i": 0})
	if err != nil {
		t.Fatal(err)
	}
	writeTestEnvelope(t, ctx, conn, realEvent)

	ack := readTestEnvelope(t, ctx, conn)
	if ack.Kind != protocol.KindAck || ack.Seq != 1 {
		t.Fatalf("ack = %+v, want kind=%q seq=1", ack, protocol.KindAck)
	}

	if strings.Contains(logBuf.String(), "apply event failed") {
		t.Fatalf("heartbeat reached ApplyEvent and logged an error:\n%s", logBuf.String())
	}
}

func writeTestEnvelope(t *testing.T, ctx context.Context, conn *websocket.Conn, env protocol.Envelope) {
	t.Helper()
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
		t.Fatalf("write envelope: %v", err)
	}
}

func readTestEnvelope(t *testing.T, ctx context.Context, conn *websocket.Conn) protocol.Envelope {
	t.Helper()
	_, raw, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read envelope: %v", err)
	}
	var env protocol.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	return env
}

// TestSettledSignalDoesNotBlockReadLoop: 아무도 Settled()를 읽지 않아도 이벤트는
// 전부 ack된다(신호는 크기 1, non-blocking). 읽으면 신호가 와 있다. 접속 훅도
// 불린다.
func TestSettledSignalDoesNotBlockReadLoop(t *testing.T) {
	r := newRig(t, nil)
	connected := make(chan struct{}, 1)
	r.hub.OnConnect = func() { connected <- struct{}{} }
	if err := r.st.UpsertRun(store.Run{ID: "run-1", State: store.StateRunning, Kind: "structured"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.link.Run(ctx)
	waitFor(t, "runner connection", r.hub.Connected)
	select {
	case <-connected:
	case <-time.After(5 * time.Second):
		t.Fatal("OnConnect was not called")
	}

	for i := 0; i < 10; i++ {
		env, _ := protocol.NewEvent(protocol.EvRunStateChanged, "run-1", 0, map[string]any{"body": map[string]any{"state": "running"}})
		if err := r.link.Publish("run-1", env); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "10 events acked with nobody reading Settled()", func() bool {
		run, err := r.st.GetRun("run-1")
		return err == nil && run.LastAckedSeq == 10
	})
	select {
	case <-r.hub.Settled():
	default:
		t.Fatal("Settled() should have a pending signal")
	}
}
