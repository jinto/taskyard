package lifecycle_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/jinto/taskyard/internal/runner/lifecycle"
)

func TestResolverReturnsSameManagerForSamePath(t *testing.T) {
	repo, worktrees := newRepo(t)
	r, err := lifecycle.NewRepoResolver([]string{repo}, worktrees)
	if err != nil {
		t.Fatal(err)
	}

	a, err := r.Manager(repo)
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.Manager(repo)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatal("same path resolved to different Manager instances")
	}
	if r.First() != a {
		t.Fatal("First() should be the only allowed repo's manager")
	}

	// worktree 루트는 저장소별로 갈라진다. 같은 run_id라도 저장소가 다르면 경로가 다르다.
	other, _ := newRepo(t)
	r2, err := lifecycle.NewRepoResolver([]string{repo, other}, worktrees)
	if err != nil {
		t.Fatal(err)
	}
	ma, _ := r2.Manager(repo)
	mb, _ := r2.Manager(other)
	if ma.WorktreePath("run-1") == mb.WorktreePath("run-1") {
		t.Fatalf("two repos share a worktree path: %s", ma.WorktreePath("run-1"))
	}
	for _, m := range []interface{ WorktreePath(string) string }{ma, mb} {
		rel, err := filepath.Rel(worktrees, m.WorktreePath("run-1"))
		if err != nil || filepath.IsAbs(rel) || rel == ".." || len(rel) > 0 && rel[0] == '.' {
			t.Fatalf("worktree path %s is not under root %s", m.WorktreePath("run-1"), worktrees)
		}
	}
}

func TestResolverResolvesSymlinkAlias(t *testing.T) {
	repo, worktrees := newRepo(t)

	// macOS의 /tmp ↔ /private/tmp처럼, 같은 디렉터리를 가리키는 다른 표기.
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(repo, alias); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	r, err := lifecycle.NewRepoResolver([]string{alias}, worktrees)
	if err != nil {
		t.Fatalf("NewRepoResolver with symlinked allow-list entry: %v", err)
	}
	viaReal, err := r.Manager(repo)
	if err != nil {
		t.Fatalf("real path was rejected although its alias is allowed: %v", err)
	}
	viaAlias, err := r.Manager(alias)
	if err != nil {
		t.Fatal(err)
	}
	if viaReal != viaAlias {
		t.Fatal("alias and real path resolved to different managers")
	}
}

func TestResolverRejectsRelativeAndMissingPaths(t *testing.T) {
	repo, worktrees := newRepo(t)
	r, err := lifecycle.NewRepoResolver([]string{repo}, worktrees)
	if err != nil {
		t.Fatal(err)
	}

	for _, p := range []string{"relative/repo", filepath.Join(t.TempDir(), "does-not-exist"), filepath.Dir(repo)} {
		if _, err := r.Manager(p); !errors.Is(err, lifecycle.ErrRepoNotAllowed) {
			t.Errorf("Manager(%q) err = %v, want ErrRepoNotAllowed", p, err)
		}
	}
}

func TestNewResolverFailsOnBadAllowList(t *testing.T) {
	_, worktrees := newRepo(t)
	for _, allowed := range [][]string{
		{},
		{"relative/repo"},
		{filepath.Join(t.TempDir(), "missing")},
	} {
		if _, err := lifecycle.NewRepoResolver(allowed, worktrees); err == nil {
			t.Errorf("NewRepoResolver(%v) succeeded; want error", allowed)
		}
	}
}
