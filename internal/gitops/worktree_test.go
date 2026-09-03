package gitops

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// newRepo는 커밋 하나가 있는 저장소를 만든다.
func newRepo(t *testing.T) (repoPath, worktreeRoot string) {
	t.Helper()
	base := t.TempDir()
	repoPath = filepath.Join(base, "repo")
	worktreeRoot = filepath.Join(base, "worktrees")

	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, repoPath, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repoPath, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, repoPath, "add", "README.md")
	run(t, repoPath, "commit", "-q", "-m", "initial")
	return repoPath, worktreeRoot
}

func TestBranchAndPathAreDeterministic(t *testing.T) {
	repo, root := newRepo(t)
	m := New(repo, root)

	if got, want := m.BranchName("run-1"), "taskyard/run/run-1"; got != want {
		t.Errorf("BranchName = %q, want %q", got, want)
	}
	if m.WorktreePath("run-1") != m.WorktreePath("run-1") {
		t.Error("WorktreePath is not stable across calls")
	}
}

func TestEnsureCreatesWorktreeOnBranch(t *testing.T) {
	repo, root := newRepo(t)
	m := New(repo, root)

	ws, err := m.Ensure(context.Background(), "run-1", "main")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !ws.Created {
		t.Error("Created = false on first Ensure")
	}
	if _, err := os.Stat(filepath.Join(ws.Path, "README.md")); err != nil {
		t.Fatalf("worktree is missing repo content: %v", err)
	}

	branch := strings.TrimSpace(run(t, ws.Path, "rev-parse", "--abbrev-ref", "HEAD"))
	if branch != "taskyard/run/run-1" {
		t.Fatalf("worktree branch = %q, want taskyard/run/run-1", branch)
	}
}

func TestEnsureIsIdempotent(t *testing.T) {
	repo, root := newRepo(t)
	m := New(repo, root)
	ctx := context.Background()

	first, err := m.Ensure(ctx, "run-1", "main")
	if err != nil {
		t.Fatal(err)
	}

	// 같은 run.start 명령이 재전송돼도 worktree가 또 생기면 안 된다(GH-09).
	second, err := m.Ensure(ctx, "run-1", "main")
	if err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	if second.Created {
		t.Error("Created = true on repeat Ensure; must reuse")
	}
	if second.Path != first.Path {
		t.Errorf("path changed: %q then %q", first.Path, second.Path)
	}

	out := run(t, repo, "worktree", "list")
	if n := strings.Count(out, "taskyard/run/run-1"); n != 1 {
		t.Fatalf("worktree list mentions the branch %d times, want 1:\n%s", n, out)
	}
}

func TestStatusReportsDirtyPaths(t *testing.T) {
	repo, root := newRepo(t)
	m := New(repo, root)
	ctx := context.Background()

	ws, err := m.Ensure(ctx, "run-1", "main")
	if err != nil {
		t.Fatal(err)
	}

	clean, err := m.Status(ctx, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if clean.Dirty {
		t.Error("fresh worktree reported dirty")
	}

	if err := os.WriteFile(filepath.Join(ws.Path, "new.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	dirty, err := m.Status(ctx, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if !dirty.Dirty {
		t.Fatal("worktree with a new file reported clean")
	}
	if len(dirty.ChangedPaths) != 1 || dirty.ChangedPaths[0] != "new.txt" {
		t.Fatalf("ChangedPaths = %v, want [new.txt]", dirty.ChangedPaths)
	}
}

func TestSalvageCommitsUncommittedWork(t *testing.T) {
	repo, root := newRepo(t)
	m := New(repo, root)
	ctx := context.Background()

	ws, err := m.Ensure(ctx, "run-1", "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws.Path, "wip.txt"), []byte("half done\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sha, saved, err := m.Salvage(ctx, "run-1")
	if err != nil {
		t.Fatalf("Salvage: %v", err)
	}
	if !saved {
		t.Fatal("saved = false despite uncommitted changes")
	}
	if sha == "" {
		t.Fatal("Salvage returned an empty sha")
	}

	after, err := m.Status(ctx, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if after.Dirty {
		t.Error("worktree still dirty after Salvage")
	}

	body := run(t, ws.Path, "show", "--stat", "--format=%s", sha)
	if !strings.Contains(body, "wip.txt") {
		t.Fatalf("salvage commit does not contain wip.txt:\n%s", body)
	}
}

func TestSalvageIsNoOpOnCleanWorktree(t *testing.T) {
	repo, root := newRepo(t)
	m := New(repo, root)
	ctx := context.Background()

	if _, err := m.Ensure(ctx, "run-1", "main"); err != nil {
		t.Fatal(err)
	}

	sha, saved, err := m.Salvage(ctx, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if saved {
		t.Errorf("saved = true on a clean worktree (sha %q)", sha)
	}
}

func TestDiffShowsChangesAgainstBase(t *testing.T) {
	repo, root := newRepo(t)
	m := New(repo, root)
	ctx := context.Background()

	ws, err := m.Ensure(ctx, "run-1", "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws.Path, "README.md"), []byte("hello\nworld\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	diff, err := m.Diff(ctx, "run-1", "main")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if !strings.Contains(diff, "+world") {
		t.Fatalf("diff does not show the added line:\n%s", diff)
	}
}

func TestStatusReportsRenamedPathAsNewName(t *testing.T) {
	repo, root := newRepo(t)
	m := New(repo, root)
	ctx := context.Background()

	ws, err := m.Ensure(ctx, "run-1", "main")
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(ws.Path, "old.txt"), []byte("content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, ws.Path, "add", "old.txt")
	run(t, ws.Path, "commit", "-q", "-m", "add old.txt")

	run(t, ws.Path, "mv", "old.txt", "new.txt")

	status, err := m.Status(ctx, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if !status.Dirty {
		t.Fatal("rename not reported as dirty")
	}
	if len(status.ChangedPaths) != 1 || status.ChangedPaths[0] != "new.txt" {
		t.Fatalf("ChangedPaths = %v, want [new.txt]", status.ChangedPaths)
	}
}

func TestEnsureRecreatesWhenDirectoryMissingButBranchExists(t *testing.T) {
	repo, root := newRepo(t)
	m := New(repo, root)
	ctx := context.Background()

	first, err := m.Ensure(ctx, "run-1", "main")
	if err != nil {
		t.Fatal(err)
	}

	// worktree 디렉터리만 사라지고 branch는 남는 상황을 재현한다
	// (runner 재시작 후 reconciliation, Task 10). 삭제는 이 테스트의
	// 픽스처 구성용일 뿐이며, gitops 패키지 자체에는 삭제 경로가 없다.
	run(t, repo, "worktree", "remove", "--force", first.Path)

	second, err := m.Ensure(ctx, "run-1", "main")
	if err != nil {
		t.Fatalf("Ensure after directory loss: %v", err)
	}
	if second.Path != first.Path {
		t.Errorf("path changed: %q then %q", first.Path, second.Path)
	}
	if second.Branch != "taskyard/run/run-1" {
		t.Errorf("branch = %q, want taskyard/run/run-1", second.Branch)
	}

	branch := strings.TrimSpace(run(t, second.Path, "rev-parse", "--abbrev-ref", "HEAD"))
	if branch != "taskyard/run/run-1" {
		t.Fatalf("worktree branch = %q, want taskyard/run/run-1", branch)
	}
}

// ---- push·앞선 커밋·미push·삭제 (계획 2026-09-03-phase1-pr) ----

// newRepoWithOrigin은 newRepo에 bare 저장소를 origin으로 붙인다. push가
// 인터넷 없이 진짜로 돈다.
func newRepoWithOrigin(t *testing.T) (repoPath, worktreeRoot, bare string) {
	t.Helper()
	repoPath, worktreeRoot = newRepo(t)
	bare = filepath.Join(filepath.Dir(repoPath), "origin.git")
	run(t, filepath.Dir(repoPath), "init", "-q", "--bare", bare)
	run(t, repoPath, "remote", "add", "origin", bare)
	return repoPath, worktreeRoot, bare
}

func commitFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "add", name)
	run(t, dir, "commit", "-q", "-m", "add "+name)
}

func TestAheadOfCountsCommitsOverBase(t *testing.T) {
	repo, root := newRepo(t)
	m := New(repo, root)
	ws, err := m.Ensure(context.Background(), "run-1", "main")
	if err != nil {
		t.Fatal(err)
	}
	if n, err := m.AheadOf(context.Background(), "run-1", "main"); err != nil || n != 0 {
		t.Fatalf("AheadOf fresh = %d, %v; want 0", n, err)
	}
	commitFile(t, ws.Path, "a.txt", "a")
	if n, err := m.AheadOf(context.Background(), "run-1", "main"); err != nil || n != 1 {
		t.Fatalf("AheadOf after commit = %d, %v; want 1", n, err)
	}
}

func TestPushPublishesBranchAndTracksUpstream(t *testing.T) {
	repo, root, bare := newRepoWithOrigin(t)
	m := New(repo, root)
	ws, _ := m.Ensure(context.Background(), "run-1", "main")
	commitFile(t, ws.Path, "a.txt", "a")

	if _, err := m.Unpushed(context.Background(), "run-1"); err == nil {
		t.Fatal("Unpushed before push should fail: no upstream")
	}
	if err := m.Push(context.Background(), "run-1"); err != nil {
		t.Fatalf("Push: %v", err)
	}
	run(t, bare, "rev-parse", "--verify", "refs/heads/"+m.BranchName("run-1"))
	if n, err := m.Unpushed(context.Background(), "run-1"); err != nil || n != 0 {
		t.Fatalf("Unpushed after push = %d, %v; want 0", n, err)
	}
	commitFile(t, ws.Path, "b.txt", "b")
	if n, err := m.Unpushed(context.Background(), "run-1"); err != nil || n != 1 {
		t.Fatalf("Unpushed after local commit = %d, %v; want 1", n, err)
	}
	// 두 번째 push는 갱신 — 멱등하다.
	if err := m.Push(context.Background(), "run-1"); err != nil {
		t.Fatalf("second Push: %v", err)
	}
}

func TestPushFailsWithoutRemote(t *testing.T) {
	repo, root := newRepo(t)
	m := New(repo, root)
	ws, _ := m.Ensure(context.Background(), "run-1", "main")
	commitFile(t, ws.Path, "a.txt", "a")
	if err := m.Push(context.Background(), "run-1"); err == nil {
		t.Fatal("Push without origin should fail")
	}
}

func TestRemoveDeletesWorktreeButKeepsBranch(t *testing.T) {
	repo, root := newRepo(t)
	m := New(repo, root)
	ws, _ := m.Ensure(context.Background(), "run-1", "main")
	commitFile(t, ws.Path, "a.txt", "a")

	if err := m.Remove(context.Background(), "run-1"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(ws.Path); !os.IsNotExist(err) {
		t.Fatalf("worktree path still exists: %v", err)
	}
	if out := run(t, repo, "worktree", "list"); strings.Contains(out, ws.Path) {
		t.Fatalf("worktree still listed:\n%s", out)
	}
	run(t, repo, "rev-parse", "--verify", "refs/heads/"+ws.Branch)
}
