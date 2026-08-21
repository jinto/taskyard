// Package gitops는 Run별 격리 작업공간을 만든다.
//
// branch와 worktree 경로를 run_id에서 결정론적으로 파생하는 것이 핵심이다.
// 같은 run.start 명령이 재전송돼도 새로 만들지 않고 그대로 재사용한다
// (PRD GH-09). 그리고 어떤 경우에도 worktree를 자동 삭제하지 않는다
// (PRD §8.7.1).
package gitops

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type Manager struct {
	repoPath     string
	worktreeRoot string
}

func New(repoPath, worktreeRoot string) *Manager {
	return &Manager{repoPath: repoPath, worktreeRoot: worktreeRoot}
}

// Workspace는 한 Run이 쓰는 작업공간이다.
type Workspace struct {
	RunID   string
	Branch  string
	Path    string
	Created bool
}

// Status는 worktree의 현재 변경 상태다.
type Status struct {
	Dirty        bool
	ChangedPaths []string
}

func (m *Manager) BranchName(runID string) string {
	return "taskyard/run/" + runID
}

func (m *Manager) WorktreePath(runID string) string {
	return filepath.Join(m.worktreeRoot, runID)
}

func (m *Manager) git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, stderr.String())
	}
	return stdout.String(), nil
}

func (m *Manager) branchExists(ctx context.Context, branch string) bool {
	_, err := m.git(ctx, m.repoPath, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

// Ensure는 Run의 worktree를 보장한다. 이미 있으면 그대로 쓴다.
func (m *Manager) Ensure(ctx context.Context, runID, baseBranch string) (Workspace, error) {
	branch := m.BranchName(runID)
	path := m.WorktreePath(runID)

	ws := Workspace{RunID: runID, Branch: branch, Path: path}

	if info, err := os.Stat(path); err == nil && info.IsDir() {
		// 경로가 이미 있다. 기대한 branch에 있는지만 확인하고 재사용한다.
		out, err := m.git(ctx, path, "rev-parse", "--abbrev-ref", "HEAD")
		if err != nil {
			return Workspace{}, fmt.Errorf("inspect existing worktree at %s: %w", path, err)
		}
		if got := strings.TrimSpace(out); got != branch {
			return Workspace{}, fmt.Errorf("worktree at %s is on branch %q, want %q", path, got, branch)
		}
		return ws, nil
	}

	if err := os.MkdirAll(m.worktreeRoot, 0o755); err != nil {
		return Workspace{}, fmt.Errorf("create worktree root: %w", err)
	}

	args := []string{"worktree", "add"}
	if m.branchExists(ctx, branch) {
		// 이전 Run이 남긴 branch가 있으면 새로 만들지 않고 붙인다.
		args = append(args, path, branch)
	} else {
		args = append(args, "-b", branch, path, baseBranch)
	}

	if _, err := m.git(ctx, m.repoPath, args...); err != nil {
		return Workspace{}, fmt.Errorf("add worktree for %s: %w", runID, err)
	}

	ws.Created = true
	return ws, nil
}

// Status는 미커밋 변경을 조사한다.
func (m *Manager) Status(ctx context.Context, runID string) (Status, error) {
	out, err := m.git(ctx, m.WorktreePath(runID), "status", "--porcelain")
	if err != nil {
		return Status{}, fmt.Errorf("status for %s: %w", runID, err)
	}

	var paths []string
	for _, line := range strings.Split(out, "\n") {
		if len(line) < 4 {
			continue
		}
		// porcelain v1: 상태 두 글자 + 공백 + 경로
		path := strings.TrimSpace(line[3:])
		// rename(R)/copy(C)는 "old -> new" 형태다. 호출자에게 의미 있는
		// 것은 새 경로이므로 그 부분만 취한다.
		if idx := strings.Index(path, " -> "); idx != -1 {
			path = path[idx+len(" -> "):]
		}
		paths = append(paths, path)
	}
	return Status{Dirty: len(paths) > 0, ChangedPaths: paths}, nil
}

// Diff는 base branch 대비 변경을 통합 diff로 돌려준다. 커밋 여부와
// 무관하게 현재 작업 트리 상태를 본다.
func (m *Manager) Diff(ctx context.Context, runID, baseBranch string) (string, error) {
	out, err := m.git(ctx, m.WorktreePath(runID), "diff", baseBranch)
	if err != nil {
		return "", fmt.Errorf("diff for %s: %w", runID, err)
	}
	return out, nil
}

// Salvage는 미커밋 변경을 Run branch에 커밋해 보존한다. 깨끗하면 아무것도
// 하지 않는다. Run이 실패·취소·lost로 끝나기 전에 반드시 호출한다.
func (m *Manager) Salvage(ctx context.Context, runID string) (string, bool, error) {
	status, err := m.Status(ctx, runID)
	if err != nil {
		return "", false, err
	}
	if !status.Dirty {
		return "", false, nil
	}

	path := m.WorktreePath(runID)
	if _, err := m.git(ctx, path, "add", "-A"); err != nil {
		return "", false, fmt.Errorf("stage salvage for %s: %w", runID, err)
	}

	msg := fmt.Sprintf("taskyard salvage %s", runID)
	if _, err := m.git(ctx, path,
		"-c", "user.name=taskyard",
		"-c", "user.email=taskyard@localhost",
		"commit", "-m", msg,
	); err != nil {
		return "", false, fmt.Errorf("commit salvage for %s: %w", runID, err)
	}

	out, err := m.git(ctx, path, "rev-parse", "HEAD")
	if err != nil {
		return "", false, fmt.Errorf("read salvage sha for %s: %w", runID, err)
	}
	return strings.TrimSpace(out), true, nil
}
