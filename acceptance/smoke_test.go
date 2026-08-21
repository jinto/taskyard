//go:build smoke

// smoke는 실제 claude CLI를 호출한다. 사용자 구독 할당량을 소모하므로
// 기본 test 대상에서 제외돼 있다. `make smoke`로만 실행한다.
package acceptance

import (
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/jinto/taskyard/internal/agents/adapter"
	"github.com/jinto/taskyard/internal/agents/adapter/claudecode"
	"github.com/jinto/taskyard/internal/approval"
	"github.com/jinto/taskyard/internal/protocol"
)

func TestSmokeRealClaudeStaysOnSubscription(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude CLI not on PATH")
	}

	// --mcp-config는 Claude Code가 시작할 때 이 MCP 서버가 응답할 때까지
	// 기다리게 만든다(MCP_TIMEOUT, 기본 30초). 존재하지 않는 주소를 넘기면
	// 기동 자체가 그 이유로 멈추거나 실패해, 이 스모크가 검증하려는 것과
	// 무관한 이유로 죽는다. 실제 프롬프트는 단어 하나만 요청하므로 승인이
	// 실제로 발생하지는 않지만, 서버 자체는 살아 있어야 한다.
	broker := approval.New("unused")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for broker: %v", err)
	}
	defer ln.Close()
	go func() { _ = http.Serve(ln, broker.Handler()) }()
	brokerURL := "http://" + ln.Addr().String() + "/mcp"

	args, err := claudecode.BuildArgs(claudecode.SpawnOptions{
		Prompt:      "Reply with exactly the word: pong",
		BrokerURL:   brokerURL,
		BrokerToken: "unused",
	})
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("claude", args...)
	cmd.Dir = t.TempDir()
	cmd.Env = claudecode.ScrubEnv(os.Environ())

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	parser := claudecode.NewParser()
	var types []string
	parseErr := parser.Parse(stdout, func(e adapter.Event) error {
		types = append(types, e.Type)
		return nil
	})
	waitErr := cmd.Wait()

	if parseErr != nil {
		t.Fatalf("parse: %v", parseErr)
	}
	if waitErr != nil {
		t.Fatalf("claude exited with error: %v (events: %v)", waitErr, types)
	}

	session := parser.Session()
	if session.SessionID == "" {
		t.Error("no session id captured; --resume would be impossible")
	}
	// PRD §13.2의 구독 경계가 실제로 지켜지는지 확인하는 유일한 자동 검증이다.
	if session.UsesAPIKey() {
		t.Fatalf("run billed to an API key (apiKeySource=%q); the flag policy is broken", session.APIKeySource)
	}
	t.Logf("apiKeySource = %q", session.APIKeySource)

	if !strings.Contains(strings.Join(types, ","), protocol.EvTurnCompleted) {
		t.Fatalf("no turn_completed event; types = %v", types)
	}
}
