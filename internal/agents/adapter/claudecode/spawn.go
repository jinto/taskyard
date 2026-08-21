package claudecode

import (
	"encoding/json"
	"errors"
	"fmt"
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
}

type SpawnOptions struct {
	Prompt          string
	WorkDir         string
	ResumeSessionID string
	BrokerURL       string
	BrokerToken     string
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
	return args, nil
}

func brokerMCPConfig(url, token string) (string, error) {
	cfg := map[string]any{
		"mcpServers": map[string]any{
			"taskyard": map[string]any{
				"type":    "http",
				"url":     url,
				"headers": map[string]string{"Authorization": "Bearer " + token},
			},
		},
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("marshal mcp config: %w", err)
	}
	return string(raw), nil
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
