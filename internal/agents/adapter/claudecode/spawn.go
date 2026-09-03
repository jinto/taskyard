package claudecode

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// ApproveToolName은 Claude Code에 넘기는 권한 도구의 이름이다.
// MCP 명명 규칙 mcp__<server>__<tool>을 따른다.
const ApproveToolName = "mcp__taskyard__approve"

// bannedEnvKeys는 Agent 프로세스에서 제거할 과금 경로 변수다(PRD §13.2).
// Claude Code 공식 문서(https://code.claude.com/docs/en/env-vars)를 기준으로,
// 구독 로그인이 아니라 API 키·클라우드 제공자·임의 엔드포인트로 과금을
// 우회시킬 수 있는 변수만 담는다. 타임아웃이나 기능 플래그 같은 동작
// 튜닝 변수는 대상이 아니다.
var bannedEnvKeys = []string{
	// 직접 API 키/토큰: 구독 대신 이 값으로 과금한다.
	"ANTHROPIC_API_KEY",
	"ANTHROPIC_AUTH_TOKEN",
	"OPENAI_API_KEY",
	"CODEX_API_KEY",

	// 제공자 전환 스위치: 켜지면 구독이 아니라 해당 클라우드 계정으로 과금된다.
	"CLAUDE_CODE_USE_BEDROCK",       // Amazon Bedrock 사용
	"CLAUDE_CODE_USE_VERTEX",        // Google Cloud Agent Platform(Vertex AI) 사용
	"CLAUDE_CODE_USE_FOUNDRY",       // Microsoft Foundry 사용
	"CLAUDE_CODE_USE_ANTHROPIC_AWS", // Claude Platform on AWS 사용
	"CLAUDE_CODE_USE_MANTLE",        // Amazon Bedrock Mantle 엔드포인트 사용

	// 엔드포인트 재지정: 임의 프록시/게이트웨이로 요청을 돌린다.
	"ANTHROPIC_BASE_URL",
	"ANTHROPIC_BEDROCK_BASE_URL",
	"ANTHROPIC_BEDROCK_MANTLE_BASE_URL",
	"ANTHROPIC_VERTEX_BASE_URL",
	"ANTHROPIC_FOUNDRY_BASE_URL",
	"ANTHROPIC_AWS_BASE_URL",

	// 제공자별 인증 정보: 이 값이 있으면 해당 제공자로 과금된다.
	"AWS_BEARER_TOKEN_BEDROCK",
	"ANTHROPIC_AWS_API_KEY",
	"ANTHROPIC_AWS_WORKSPACE_ID",
	"ANTHROPIC_FOUNDRY_API_KEY",
	"ANTHROPIC_FOUNDRY_AUTH_TOKEN",

	// Workload Identity Federation / Anthropic 프로필: 문서에 "/login 자격증명보다
	// 우선한다"고 명시되어 있다. 즉 /login 구독 로그인이 남아 있어도 이 값들이
	// 있으면 과금이 다른 신원으로 넘어간다.
	"ANTHROPIC_FEDERATION_RULE_ID",
	"ANTHROPIC_ORGANIZATION_ID",
	"ANTHROPIC_WORKSPACE_ID",
	"ANTHROPIC_PROFILE",
}

type SpawnOptions struct {
	Prompt          string
	WorkDir         string
	ResumeSessionID string
	BrokerURL       string
	BrokerToken     string
	// AllowedTools는 승인 없이 통과시킬 도구 패턴이다(PRD §11.6.3). 예: "Edit",
	// "Bash(go test:*)". 그 밖의 도구는 여전히 브로커를 거친다.
	AllowedTools []string
}

// allowedToolPattern은 허용 도구 항목의 문법이다: 도구 이름, 선택적으로
// 괄호 안의 패턴("Bash(go test:*)"처럼 괄호 안 공백은 된다). MCP 도구는
// "mcp__claude-in-chrome__navigate"처럼 하이픈이, 서버 전체 허용은
// "mcp__github__*"처럼 별표가 든다. 쉼표는 안
// 된다 — 쉼표로 이어 값 하나로 넘기니까. 이름은 글자로 시작해야 하므로
// "-"로 시작하는 플래그 모양은 걸러진다.
var allowedToolPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_*-]*(\([^(),\s][^(),\n]*\))?$`)

// CheckAllowedTools는 항목 전부가 문법에 맞는지 본다. 웹 설정 폼과 BuildArgs가
// 같은 규칙을 쓴다.
func CheckAllowedTools(tools []string) error {
	for _, t := range tools {
		if !allowedToolPattern.MatchString(t) {
			return fmt.Errorf("claudecode: invalid allowed tool %q (want Name or Name(pattern), no commas or newlines)", t)
		}
	}
	return nil
}

// BuildArgs는 `claude`에 넘길 인자를 만든다. 여기가 PRD §13.2.1의
// 플래그 정책이 강제되는 단 하나의 지점이다.
func BuildArgs(opts SpawnOptions) ([]string, error) {
	if opts.Prompt == "" {
		return nil, errors.New("claudecode: Prompt is required")
	}
	// Prompt는 "-p" 다음 값으로, ResumeSessionID는 "--resume" 다음 값으로 그대로
	// argv에 들어간다. "-"로 시작하면 값이 아니라 별도 플래그(예: "--bare")로
	// 파싱될 수 있어 이 함수의 플래그 정책 자체가 무력화된다.
	if strings.HasPrefix(opts.Prompt, "-") {
		return nil, errors.New("claudecode: Prompt must not start with '-'; it could be parsed as a CLI flag")
	}
	if opts.BrokerURL == "" {
		return nil, errors.New("claudecode: BrokerURL is required; without it approvals never reach the user")
	}
	if strings.HasPrefix(opts.ResumeSessionID, "-") {
		return nil, errors.New("claudecode: ResumeSessionID must not start with '-'; it could be parsed as a CLI flag")
	}
	if err := CheckAllowedTools(opts.AllowedTools); err != nil {
		return nil, err
	}

	mcpConfig, err := brokerMCPConfig(opts.BrokerURL, opts.BrokerToken)
	if err != nil {
		return nil, err
	}

	args := []string{
		"-p", opts.Prompt,
		"--output-format", "stream-json",
		"--verbose",
		// 사용자 전역 설정이 auto여도 분류기가 승인을 가로채지 못하게 한다.
		"--permission-mode", "default",
		// 브로커 외의 MCP 서버를 차단한다.
		"--strict-mcp-config",
		"--mcp-config", mcpConfig,
		"--permission-prompt-tool", ApproveToolName,
	}

	if opts.ResumeSessionID != "" {
		args = append(args, "--resume", opts.ResumeSessionID)
	}
	if len(opts.AllowedTools) > 0 {
		// 가변 인자로 넘기면 뒤따르는 플래그까지 값으로 먹을 수 있어 쉼표로 잇는다.
		args = append(args, "--allowedTools", strings.Join(opts.AllowedTools, ","))
	}
	return args, nil
}

func brokerMCPConfig(url, token string) (string, error) {
	cfg := map[string]any{
		"mcpServers": map[string]any{
			"taskyard": map[string]any{
				"type":    "http",
				"url":     url,
				"headers": map[string]string{"Authorization": "Bearer " + token},
				// HTTP MCP 서버의 요청 타이머는 max(60s, 이 서버의 timeout,
				// MCP_TIMEOUT)이고 MCP_TOOL_TIMEOUT의 기본값은 그 비교에 안
				// 들어간다(공식 문서). 승인은 사람의 속도라 서버 단위로 하루.
				"timeout": mcpToolTimeoutMS,
			},
		},
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("marshal mcp config: %w", err)
	}
	return string(raw), nil
}

// mcpToolTimeout은 Claude Code가 MCP 도구 호출 하나를 기다리는 한도다(ms).
// 승인 브로커도 MCP 도구라서 이 한도 안에 사람이 눌러야 한다 — 기본 60초는
// 사람의 속도가 아니다. 지나면 "timed out" 오류가 에이전트에 돌아가고 같은
// 승인을 다시 묻는다(2026-09-04 관측). 하루로 올린다.
const mcpToolTimeoutMS = 86_400_000

// AgentEnv는 에이전트 프로세스의 환경이다: 과금 변수를 빼고(ScrubEnv) MCP
// 도구 호출 한도를 올린다. 사용자가 더 길게 정해 뒀으면 그것을 존중하고,
// 짧거나 없으면 하루로 — 승인 대기가 그보다 짧아지면 안 된다.
func AgentEnv(env []string) []string {
	out := ScrubEnv(env)
	kept := out[:0]
	for _, entry := range out {
		if v, ok := strings.CutPrefix(entry, "MCP_TOOL_TIMEOUT="); ok {
			if n, err := strconv.Atoi(v); err == nil && n >= mcpToolTimeoutMS {
				kept = append(kept, entry)
			}
			continue
		}
		kept = append(kept, entry)
	}
	for _, entry := range kept {
		if strings.HasPrefix(entry, "MCP_TOOL_TIMEOUT=") {
			return kept
		}
	}
	return append(kept, "MCP_TOOL_TIMEOUT="+strconv.Itoa(mcpToolTimeoutMS))
}

// ScrubEnv는 API 과금용 변수를 제거한 환경을 돌려준다.
func ScrubEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, found := strings.Cut(entry, "=")
		if !found {
			continue
		}
		banned := false
		for _, b := range bannedEnvKeys {
			if key == b {
				banned = true
				break
			}
		}
		if !banned {
			out = append(out, entry)
		}
	}
	return out
}
