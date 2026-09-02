package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/jinto/taskyard/internal/approval"
	"github.com/jinto/taskyard/internal/buildinfo"
	"github.com/jinto/taskyard/internal/protocol"
	"github.com/jinto/taskyard/internal/runner/lifecycle"
	"github.com/jinto/taskyard/internal/runner/link"
	"github.com/jinto/taskyard/internal/runner/spool"
)

func main() {
	var (
		serverURL   = flag.String("server", "ws://127.0.0.1:8080/ws", "server websocket url")
		runnerID    = flag.String("id", "runner-1", "runner id")
		token       = flag.String("pairing-token", "", "pairing token issued by the server")
		dbPath      = flag.String("db", "taskyard-runner.db", "sqlite path")
		worktrees   = flag.String("worktrees", "", "worktree root")
		baseBranch  = flag.String("base-branch", "main", "default base branch when run.start names none")
		showVersion = flag.Bool("version", false, "print version and exit")
		allowRepos  []string
	)
	// 프로젝트마다 저장소가 다르므로 하나가 아니라 목록이다(PRD RN-03).
	// 여러 번 줄 수 있다: --allow-repo /a --allow-repo /b
	flag.Func("allow-repo", "absolute path of a repository this runner may work in (repeatable)", func(v string) error {
		allowRepos = append(allowRepos, v)
		return nil
	})
	flag.Parse()

	if *showVersion {
		fmt.Printf("taskyard-runner %s (protocol v%d)\n", buildinfo.Version(), buildinfo.ProtocolVersion())
		return
	}
	if len(allowRepos) == 0 || *worktrees == "" || *token == "" {
		fmt.Fprintln(os.Stderr, "taskyard-runner: --allow-repo (at least one), --worktrees and --pairing-token are required")
		os.Exit(2)
	}
	repos, err := lifecycle.NewRepoResolver(allowRepos, *worktrees)
	if err != nil {
		fmt.Fprintln(os.Stderr, "taskyard-runner:", err)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	sp, err := spool.Open(*dbPath)
	if err != nil {
		slog.Error("open spool failed", "err", err)
		os.Exit(1)
	}
	defer sp.Close()

	// 승인 브로커를 임의 포트로 로컬호스트에만 띄운다. 이것은 승인 경계다 —
	// 라우팅 가능한 주소로 열면 네트워크의 누구든 Agent 도구 호출을
	// 자기 스스로 승인시킬 수 있게 된다.
	brokerToken := randomToken()
	broker := approval.New(brokerToken)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		slog.Error("listen for broker failed", "err", err)
		os.Exit(1)
	}
	brokerURL := fmt.Sprintf("http://%s/mcp", ln.Addr().String())
	go func() {
		if err := http.Serve(ln, broker.Handler()); err != nil {
			slog.Error("broker stopped", "err", err)
		}
	}()
	slog.Info("approval broker listening", "url", brokerURL)

	// lifecycle과 link이 서로를 참조하므로 publish 클로저가 l을 늦게 읽는다.
	//
	// 불변식: l 대입은 반드시 lm.Start와 lm.Reconcile 호출보다 앞서야 한다.
	// 둘 다 이벤트를 발행할 수 있고, 발행은 이 클로저를 거쳐 l에 닿는다.
	// 순서가 뒤집히면 첫 이벤트에서 nil 역참조로 죽는다.
	var l *link.Link

	lm, err := lifecycle.New(lifecycle.Config{
		Spool:       sp,
		Repos:       repos,
		Broker:      broker,
		BaseBranch:  *baseBranch,
		BrokerURL:   brokerURL,
		BrokerToken: brokerToken,
		Publish: func(runID string, env protocol.Envelope) error {
			return l.Publish(runID, env)
		},
	})
	if err != nil {
		slog.Error("build lifecycle failed", "err", err)
		os.Exit(1)
	}

	l, err = link.New(link.Config{
		ServerURL:    *serverURL,
		RunnerID:     *runnerID,
		PairingToken: *token,
		Spool:        sp,
		Capabilities: []string{protocol.CapClaudeCode, protocol.CapApprovalBroker, protocol.CapGitWorktree},
		OnCommand:    lm.HandleCommand,
	})
	if err != nil {
		slog.Error("build link failed", "err", err)
		os.Exit(1)
	}

	// 여기부터 l이 유효하다. 위 불변식대로 발행자들을 그 뒤에 띄운다.
	go lm.Start(ctx)

	// 재시작 직후 남아 있는 Run의 실제 상태를 먼저 맞춘다(PRD §11.7).
	if err := lm.Reconcile(ctx); err != nil {
		slog.Error("reconcile failed", "err", err)
	}

	if err := l.Run(ctx); err != nil && ctx.Err() == nil {
		slog.Error("link stopped", "err", err)
		os.Exit(1)
	}
}

func randomToken() string {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		panic(fmt.Sprintf("read random: %v", err))
	}
	return hex.EncodeToString(buf)
}
