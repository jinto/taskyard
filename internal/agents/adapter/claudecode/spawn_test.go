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

func TestBuildArgsRejectsLeadingDashPrompt(t *testing.T) {
	opts := baseOpts()
	// Prompt는 "-p" 다음 값으로 그대로 argv에 들어간다. "-"로 시작하면
	// claude가 이를 값이 아니라 별도 플래그(예: "--bare")로 파싱할 수 있다.
	opts.Prompt = "--dangerously-skip-permissions"

	if _, err := BuildArgs(opts); err == nil {
		t.Fatal("BuildArgs accepted a Prompt starting with '-'; it could be parsed as a CLI flag")
	}
}

func TestBuildArgsRejectsLeadingDashResumeSessionID(t *testing.T) {
	opts := baseOpts()
	opts.ResumeSessionID = "--bare"

	if _, err := BuildArgs(opts); err == nil {
		t.Fatal("BuildArgs accepted a ResumeSessionID starting with '-'; it could be parsed as a CLI flag")
	}
}

// 사전 허용 도구(PRD §11.6.3). 쉼표로 이어 --allowedTools 값 하나로 넘긴다 —
// 가변 인자로 넘기면 뒤따르는 플래그까지 값으로 먹을 수 있다.
func TestBuildArgsAddsAllowedToolsAsOneValue(t *testing.T) {
	opts := baseOpts()
	opts.AllowedTools = []string{"Edit", "Bash(go test:*)", "mcp__claude-in-chrome__*"}

	args, err := BuildArgs(opts)
	if err != nil {
		t.Fatal(err)
	}
	if !hasFlagValue(args, "--allowedTools", "Edit,Bash(go test:*),mcp__claude-in-chrome__*") {
		t.Errorf("missing joined --allowedTools: %s", argsString(args))
	}
}

func TestBuildArgsOmitsAllowedToolsWhenEmpty(t *testing.T) {
	args, err := BuildArgs(baseOpts())
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range args {
		if a == "--allowedTools" {
			t.Fatalf("--allowedTools present with no tools: %s", argsString(args))
		}
	}
}

func TestBuildArgsRejectsMalformedAllowedTools(t *testing.T) {
	for _, bad := range []string{"--dangerously-skip-permissions", "-x", "Bash(git log", "Edit,Write", "", "  ", "Bash(a) Write", "Ed\nit"} {
		opts := baseOpts()
		opts.AllowedTools = []string{"Edit", bad}
		if _, err := BuildArgs(opts); err == nil {
			t.Errorf("BuildArgs accepted allowed tool %q", bad)
		}
	}
}

func TestScrubEnvRemovesBillingKeys(t *testing.T) {
	// 과금 우회 후보 변수 목록. https://code.claude.com/docs/en/env-vars 기준으로,
	// 구독이 아니라 API 키/클라우드 제공자/임의 엔드포인트로 과금을 돌리는
	// 변수만 담는다(see spawn.go의 bannedEnvKeys 주석).
	bannedKeys := []string{
		"ANTHROPIC_API_KEY",
		"ANTHROPIC_AUTH_TOKEN",
		"OPENAI_API_KEY",
		"CODEX_API_KEY",
		"CLAUDE_CODE_USE_BEDROCK",
		"CLAUDE_CODE_USE_VERTEX",
		"CLAUDE_CODE_USE_FOUNDRY",
		"CLAUDE_CODE_USE_ANTHROPIC_AWS",
		"CLAUDE_CODE_USE_MANTLE",
		"ANTHROPIC_BASE_URL",
		"ANTHROPIC_BEDROCK_BASE_URL",
		"ANTHROPIC_BEDROCK_MANTLE_BASE_URL",
		"ANTHROPIC_VERTEX_BASE_URL",
		"ANTHROPIC_FOUNDRY_BASE_URL",
		"ANTHROPIC_AWS_BASE_URL",
		"AWS_BEARER_TOKEN_BEDROCK",
		"ANTHROPIC_AWS_API_KEY",
		"ANTHROPIC_AWS_WORKSPACE_ID",
		"ANTHROPIC_FOUNDRY_API_KEY",
		"ANTHROPIC_FOUNDRY_AUTH_TOKEN",
		"ANTHROPIC_FEDERATION_RULE_ID",
		"ANTHROPIC_ORGANIZATION_ID",
		"ANTHROPIC_WORKSPACE_ID",
		"ANTHROPIC_PROFILE",
	}

	in := []string{"PATH=/usr/bin"}
	for _, k := range bannedKeys {
		in = append(in, k+"=leaked-value")
	}
	in = append(in, "HOME=/Users/dev")

	got := ScrubEnv(in)

	for _, e := range got {
		for _, banned := range bannedKeys {
			if strings.HasPrefix(e, banned+"=") {
				t.Errorf("%s survived scrubbing", banned)
			}
		}
	}
	if len(got) != 2 {
		t.Fatalf("kept %d vars (%v), want PATH and HOME only", len(got), got)
	}
}
