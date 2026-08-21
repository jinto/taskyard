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

// bannedEnvKeys는 Agent 프로세스에서 제거할 API 과금용 변수다(PRD §13.2).
var bannedEnvKeys = []string{
	"ANTHROPIC_API_KEY",
	"ANTHROPIC_AUTH_TOKEN",
	"OPENAI_API_KEY",
	"CODEX_API_KEY",
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
	if opts.BrokerURL == "" {
		return nil, errors.New("claudecode: BrokerURL is required; without it approvals never reach the user")
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
