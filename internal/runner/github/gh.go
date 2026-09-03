// Package github은 gh CLI의 얇은 래퍼다. 사용자의 gh 로그인을 그대로 쓴다
// (PRD GH-05) — 토큰을 따로 받지 않고, 프롬프트만 막는다.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

type Client struct {
	// Binary는 gh 실행 파일이다. 비어 있으면 PATH의 gh. 테스트는 가짜를 준다.
	Binary  string
	Timeout time.Duration
}

// PR은 gh pr view의 요약이다. Checks는 statusCheckRollup을 줄인 것
// (none/pending/success/failure), Review는 reviewDecision 그대로.
type PR struct {
	URL    string
	Number int
	State  string
	Checks string
	Review string
}

func (c Client) run(ctx context.Context, dir string, args ...string) (string, error) {
	bin := c.Binary
	if bin == "" {
		bin = "gh"
	}
	timeout := c.Timeout
	if timeout == 0 {
		timeout = time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("gh %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// View는 브랜치의 PR을 찾는다. 없으면 ok=false, 오류 아님.
func (c Client) View(ctx context.Context, dir, branch string) (PR, bool, error) {
	out, err := c.run(ctx, dir, "pr", "view", branch, "--json", "number,url,state,statusCheckRollup,reviewDecision")
	if err != nil {
		if strings.Contains(err.Error(), "no pull requests found") {
			return PR{}, false, nil
		}
		return PR{}, false, err
	}
	var raw struct {
		Number            int               `json:"number"`
		URL               string            `json:"url"`
		State             string            `json:"state"`
		ReviewDecision    string            `json:"reviewDecision"`
		StatusCheckRollup []json.RawMessage `json:"statusCheckRollup"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return PR{}, false, fmt.Errorf("parse gh pr view: %w", err)
	}
	return PR{
		URL: raw.URL, Number: raw.Number, State: raw.State,
		Checks: summarizeChecks(raw.StatusCheckRollup), Review: raw.ReviewDecision,
	}, true, nil
}

// Create는 PR을 만들고 View로 다시 읽어 돌려준다 — gh pr create는 URL만
// 출력하므로 번호·상태는 view가 정본이다.
func (c Client) Create(ctx context.Context, dir, base, head, title, body string) (PR, error) {
	f, err := os.CreateTemp("", "taskyard-pr-body-*.md")
	if err != nil {
		return PR{}, err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(body); err != nil {
		f.Close()
		return PR{}, err
	}
	f.Close()

	if _, err := c.run(ctx, dir, "pr", "create", "--base", base, "--head", head, "--title", title, "--body-file", f.Name()); err != nil {
		return PR{}, err
	}
	pr, ok, err := c.View(ctx, dir, head)
	if err != nil {
		return PR{}, err
	}
	if !ok {
		return PR{}, fmt.Errorf("gh pr create succeeded but pr view finds no PR for %s", head)
	}
	return pr, nil
}

// summarizeChecks는 statusCheckRollup(CheckRun: status·conclusion, StatusContext:
// state)을 하나로 줄인다. 비어 있으면 none, 하나라도 실패면 failure, 아니면
// 하나라도 미완이면 pending, 나머지(SUCCESS/NEUTRAL/SKIPPED)는 success.
func summarizeChecks(items []json.RawMessage) string {
	if len(items) == 0 {
		return "none"
	}
	pending := false
	for _, it := range items {
		var c struct{ Status, Conclusion, State string }
		_ = json.Unmarshal(it, &c)
		switch strings.ToUpper(c.Conclusion) {
		case "FAILURE", "ERROR", "TIMED_OUT", "CANCELLED", "ACTION_REQUIRED":
			return "failure"
		}
		switch strings.ToUpper(c.State) {
		case "FAILURE", "ERROR":
			return "failure"
		case "PENDING", "EXPECTED":
			pending = true
		}
		if c.Status != "" && strings.ToUpper(c.Status) != "COMPLETED" {
			pending = true
		}
	}
	if pending {
		return "pending"
	}
	return "success"
}
