package claudecode

import (
	"strings"
	"testing"
)

func argsString(args []string) string { return strings.Join(args, " ") }

func hasFlagValue(args []string, flag, value string) bool {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

func baseOpts() SpawnOptions {
	return SpawnOptions{
		Prompt:      "do the thing",
		WorkDir:     "/tmp/wt/run-1",
		BrokerURL:   "http://127.0.0.1:9999/mcp",
		BrokerToken: "secret",
	}
}

func TestBuildArgsIncludesMandatoryFlags(t *testing.T) {
	args, err := BuildArgs(baseOpts())
	if err != nil {
		t.Fatalf("BuildArgs: %v", err)
	}

	if !hasFlagValue(args, "--output-format", "stream-json") {
		t.Error("missing --output-format stream-json")
	}
	if !hasFlag(args, "--verbose") {
		t.Error("missing --verbose (stream-json requires it)")
	}
	if !hasFlagValue(args, "--permission-prompt-tool", "mcp__taskyard__approve") {
		t.Error("missing --permission-prompt-tool")
	}
	if !hasFlag(args, "--strict-mcp-config") {
		t.Error("missing --strict-mcp-config")
	}
	if !hasFlagValue(args, "--permission-mode", "default") {
		t.Error("missing --permission-mode default")
	}
}

func TestBuildArgsNeverPassesBare(t *testing.T) {
	args, err := BuildArgs(baseOpts())
	if err != nil {
		t.Fatal(err)
	}
	// --bare는 구독 로그인을 건너뛰고 ANTHROPIC_API_KEY를 요구한다.
	// Taskyard의 과금 모델을 깨므로 절대 나오면 안 된다(PRD §13.2.1).
	if hasFlag(args, "--bare") {
		t.Fatalf("--bare must never appear: %s", argsString(args))
	}
}

func TestBuildArgsForcesDefaultPermissionMode(t *testing.T) {
	// 사용자 전역 설정이 auto여도 분류기가 도구를 먼저 승인하면
	// 승인 브로커가 호출되지 않는다. 명시적으로 덮어써야 한다.
	args, err := BuildArgs(baseOpts())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--permission-mode" && args[i+1] != "default" {
			t.Fatalf("--permission-mode %s, want default", args[i+1])
		}
	}
}

func TestBuildArgsEmbedsBrokerInMCPConfig(t *testing.T) {
	args, err := BuildArgs(baseOpts())
	if err != nil {
		t.Fatal(err)
	}

	var cfg string
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--mcp-config" {
			cfg = args[i+1]
		}
	}
	if cfg == "" {
		t.Fatal("missing --mcp-config")
	}
	for _, want := range []string{`"taskyard"`, `"http"`, "http://127.0.0.1:9999/mcp", "Bearer secret"} {
		if !strings.Contains(cfg, want) {
			t.Errorf("mcp config missing %q:\n%s", want, cfg)
		}
	}
}

func TestBuildArgsAddsResumeWhenSessionKnown(t *testing.T) {
	opts := baseOpts()
	opts.ResumeSessionID = "sess-123"

	args, err := BuildArgs(opts)
	if err != nil {
		t.Fatal(err)
	}
	if !hasFlagValue(args, "--resume", "sess-123") {
		t.Errorf("missing --resume sess-123: %s", argsString(args))
	}
}

func TestBuildArgsRejectsMissingBroker(t *testing.T) {
	opts := baseOpts()
	opts.BrokerURL = ""

	if _, err := BuildArgs(opts); err == nil {
		t.Fatal("BuildArgs accepted an empty BrokerURL; approvals would silently break")
	}
}

func TestScrubEnvRemovesBillingKeys(t *testing.T) {
	in := []string{
		"PATH=/usr/bin",
		"ANTHROPIC_API_KEY=sk-secret",
		"ANTHROPIC_AUTH_TOKEN=tok",
		"OPENAI_API_KEY=sk-other",
		"CODEX_API_KEY=sk-codex",
		"HOME=/Users/dev",
	}

	got := ScrubEnv(in)

	for _, e := range got {
		for _, banned := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "OPENAI_API_KEY", "CODEX_API_KEY"} {
			if strings.HasPrefix(e, banned+"=") {
				t.Errorf("%s survived scrubbing", banned)
			}
		}
	}
	if len(got) != 2 {
		t.Fatalf("kept %d vars (%v), want PATH and HOME only", len(got), got)
	}
}
