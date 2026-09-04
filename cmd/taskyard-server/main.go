package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"

	"github.com/jinto/taskyard/internal/buildinfo"
	"github.com/jinto/taskyard/internal/server/hub"
	"github.com/jinto/taskyard/internal/server/launch"
	"github.com/jinto/taskyard/internal/server/store"
	"github.com/jinto/taskyard/internal/server/web"
)

func main() {
	var (
		addr        = flag.String("addr", "127.0.0.1:8080", "listen address")
		dbPath      = flag.String("db", "taskyard-server.db", "sqlite path")
		token       = flag.String("pairing-token", "", "one-time pairing token for the runner")
		showVersion = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("taskyard-server %s (protocol v%d)\n", buildinfo.Version(), buildinfo.ProtocolVersion())
		return
	}
	if *token == "" {
		fmt.Fprintln(os.Stderr, "taskyard-server: --pairing-token is required")
		os.Exit(2)
	}

	if err := os.MkdirAll(filepath.Dir(*dbPath), 0o755); err != nil {
		slog.Error("create db directory failed", "err", err)
		os.Exit(1)
	}
	st, err := store.Open(*dbPath)
	if err != nil {
		slog.Error("open store failed", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	h := hub.New(st, *token)

	// 1단계 성공 뒤의 자동 이어 실행. hub의 정착 신호·서버 시작·러너 접속에서
	// 같은 조정을 돈다(계획 2026-09-04-phase1-stages).
	launcher := &launch.Launcher{Store: st, Commander: h}
	h.OnConnect = launcher.ChainPending
	go launcher.Run(context.Background(), h)

	ui, err := web.New(st, h)
	if err != nil {
		slog.Error("build web failed", "err", err)
		os.Exit(1)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", h.ServeWS)
	mux.Handle("/", ui.Routes())

	slog.Info("taskyard-server listening", "addr", *addr)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		slog.Error("server stopped", "err", err)
		os.Exit(1)
	}
}
