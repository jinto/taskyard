package hub_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
