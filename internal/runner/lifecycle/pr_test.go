package lifecycle_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jinto/taskyard/internal/protocol"
	"github.com/jinto/taskyard/internal/runner/spool"
)

// fakeGH는 제어 디렉터리로 조종하는 가짜 gh다.
//
//	state  — 없거나 "none"이면 PR 없음, 아니면 OPEN/MERGED/CLOSED
//	calls  — 받은 인자를 한 줄씩 누적
//	title, body — pr create가 받은 제목과 본문 파일 내용
type fakeGH struct {
	bin string
	dir string
}

func newFakeGH(t *testing.T) *fakeGH {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
D=` + dir + `
printf '%s\n' "$*" >> "$D/calls"
case "$1 $2" in
  "pr view")
    S=$(cat "$D/state" 2>/dev/null || echo none)
    if [ "$S" = none ]; then echo "no pull requests found for branch" >&2; exit 1; fi
    printf '{"number":7,"url":"https://example.test/pull/7","state":"%s","mergedAt":null,"statusCheckRollup":[],"reviewDecision":"","headRefName":"x"}\n' "$S"
    ;;
  "pr create")
    while [ $# -gt 0 ]; do
      case "$1" in
        --title) printf '%s' "$2" > "$D/title"; shift ;;
        --body-file) cp "$2" "$D/body"; shift ;;
      esac
      shift
    done
    echo OPEN > "$D/state"
    echo "https://example.test/pull/7"
    ;;
  *) echo "unexpected gh $*" >&2; exit 2 ;;
esac
`
	bin := filepath.Join(dir, "gh")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return &fakeGH{bin: bin, dir: dir}
}

func (g *fakeGH) setState(t *testing.T, state string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(g.dir, "state"), []byte(state+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (g *fakeGH) calls() []string {
	raw, _ := os.ReadFile(filepath.Join(g.dir, "calls"))
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

func (g *fakeGH) creates() int {
	n := 0
	for _, c := range g.calls() {
		if strings.HasPrefix(c, "pr create") {
			n++
		}
	}
	return n
}

func (g *fakeGH) file(name string) string {
	raw, _ := os.ReadFile(filepath.Join(g.dir, name))
	return string(raw)
}

// scriptedAgent는 worktree에서 쉘 명령을 몇 줄 실행한 뒤 pong 스트림을
// 흘리고 exitCode로 끝나는 가짜 Agent다.
func scriptedAgent(t *testing.T, lines []string, exitCode int) string {
	t.Helper()
	abs, err := filepath.Abs(pongFixture)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "scripted-claude")
	script := "#!/bin/sh\nset -e\n" + strings.Join(lines, "\n") + "\ncat " + abs + "\nexit " + fmt.Sprint(exitCode) + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

const (
	writeSummary = "mkdir -p .taskyard && printf 'Shout를 추가했다\\n' > .taskyard/summary.md"
	commitOne    = "echo $$ > a.txt && git add a.txt && git -c user.name=t -c user.email=t@t commit -q -m 'agent commit'"
)

func prUpdatesOf(t *testing.T, col *collector, runID string) []protocol.PRUpdatedBody {
	t.Helper()
	var out []protocol.PRUpdatedBody
	for _, e := range col.snapshot() {
		if e.RunID != runID || e.Type != protocol.EvPRUpdated {
			continue
		}
		var wrapper struct {
			Body protocol.PRUpdatedBody `json:"body"`
		}
		if err := json.Unmarshal(e.Body, &wrapper); err != nil {
			t.Fatal(err)
		}
		out = append(out, wrapper.Body)
	}
	return out
}

func summaryOf(t *testing.T, col *collector, runID string) string {
	t.Helper()
	for _, e := range col.snapshot() {
		if e.RunID != runID || e.Type != protocol.EvRunStateChanged {
			continue
		}
		var wrapper struct {
			Body map[string]any `json:"body"`
		}
		_ = json.Unmarshal(e.Body, &wrapper)
		if s, _ := wrapper.Body["summary"].(string); s != "" {
			return s
		}
	}
	return ""
}

func originHasBranch(t *testing.T, repo, branch string) bool {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	cmd.Dir = filepath.Join(filepath.Dir(repo), "origin.git")
	return cmd.Run() == nil
}

func runRecord(t *testing.T, sp *spool.Spool, runID string) spool.RunRecord {
	t.Helper()
	for _, r := range mustLoadRuns(t, sp) {
		if r.RunID == runID {
			return r
		}
	}
	t.Fatalf("no record for %s", runID)
	return spool.RunRecord{}
}

var prSpec = &protocol.PRSpec{Title: "Shout(name) 함수를 추가한다", Body: "Taskyard 이슈 #3 · Run run-1\n\nGreet처럼 인사하되…"}

func startWithPR(t *testing.T, h *harness, runID string, pr *protocol.PRSpec, wsID string) {
	t.Helper()
	env := startCommand(t, runID, protocol.RunStartBody{Prompt: "p", RepoPath: h.repo, PR: pr, CleanupMerged: true, WorkspaceRunID: wsID})
	if err := h.m.HandleCommand(context.Background(), env); err != nil {
		t.Fatal(err)
	}
}

func TestFinishCreatesPRAndPublishesUpdate(t *testing.T) {
	col := &collector{}
	gh := newFakeGH(t)
	h := newHarness(t, col, withOrigin(), withGH(gh.bin), withBinary(scriptedAgent(t, []string{writeSummary, commitOne}, 0)))
	startWithPR(t, h, "run-1", prSpec, "")
	waitTerminal(t, col)

	if got := lastState(t, col, "run-1"); got.state != "succeeded" {
		t.Fatalf("state = %+v, want succeeded", got)
	}
	if !originHasBranch(t, h.repo, h.git.BranchName("run-1")) {
		t.Fatal("branch not pushed to origin")
	}
	if gh.creates() != 1 {
		t.Fatalf("gh calls = %v, want one pr create", gh.calls())
	}
	if gh.file("title") != prSpec.Title {
		t.Fatalf("title = %q", gh.file("title"))
	}
	body := gh.file("body")
	if !strings.Contains(body, "Shout를 추가했다") || !strings.Contains(body, prSpec.Body) {
		t.Fatalf("body = %q; want summary and server-provided body", body)
	}
	prs := prUpdatesOf(t, col, "run-1")
	if len(prs) != 1 || prs[0].Number != 7 || prs[0].State != "OPEN" || prs[0].URL == "" {
		t.Fatalf("pr.updated = %+v", prs)
	}
	// pr.updated는 종결보다 먼저다.
	types := col.types()
	if idx := indexOf(types, protocol.EvPRUpdated); idx < 0 || types[len(types)-1] != protocol.EvRunStateChanged || idx > len(types)-2 {
		t.Fatalf("event order = %v", types)
	}
	if summaryOf(t, col, "run-1") != "Shout를 추가했다\n" {
		t.Fatalf("summary in terminal event = %q", summaryOf(t, col, "run-1"))
	}
	if _, err := os.Stat(filepath.Join(h.git.WorktreePath("run-1"), ".taskyard/summary.md")); !os.IsNotExist(err) {
		t.Fatal("summary.md should have been taken (deleted)")
	}
	rec := runRecord(t, h.sp, "run-1")
	if rec.PRNumber != 7 || rec.PRState != "OPEN" || rec.PRURL == "" || !rec.CleanupMerged {
		t.Fatalf("record = %+v", rec)
	}
}

func indexOf(xs []string, x string) int {
	for i, v := range xs {
		if v == x {
			return i
		}
	}
	return -1
}

func TestFinishReusesExistingPR(t *testing.T) {
	col := &collector{}
	gh := newFakeGH(t)
	gh.setState(t, "OPEN")
	h := newHarness(t, col, withOrigin(), withGH(gh.bin), withBinary(scriptedAgent(t, []string{commitOne}, 0)))
	startWithPR(t, h, "run-1", prSpec, "")
	waitTerminal(t, col)

	if gh.creates() != 0 {
		t.Fatalf("gh calls = %v, want no pr create", gh.calls())
	}
	if prs := prUpdatesOf(t, col, "run-1"); len(prs) != 1 || prs[0].Number != 7 {
		t.Fatalf("pr.updated = %+v", prs)
	}
	if got := lastState(t, col, "run-1"); got.state != "succeeded" {
		t.Fatalf("state = %+v", got)
	}
}

func TestFinishWithoutCommitsSkipsPR(t *testing.T) {
	col := &collector{}
	gh := newFakeGH(t)
	h := newHarness(t, col, withOrigin(), withGH(gh.bin), withBinary(scriptedAgent(t, []string{writeSummary}, 0)))
	startWithPR(t, h, "run-1", prSpec, "")
	waitTerminal(t, col)

	got := lastState(t, col, "run-1")
	if got.state != "succeeded" || !strings.Contains(got.detail, "PR 없음") {
		t.Fatalf("state = %+v", got)
	}
	if len(gh.calls()) != 0 || originHasBranch(t, h.repo, h.git.BranchName("run-1")) {
		t.Fatalf("no push or gh expected; calls = %v", gh.calls())
	}
	if summaryOf(t, col, "run-1") == "" {
		t.Fatal("summary should still be reported")
	}
	if n, _ := h.git.AheadOf(context.Background(), "run-1", "main"); n != 0 {
		t.Fatalf("summary-only worktree produced %d commit(s)", n)
	}
}

func TestFailureSalvageDoesNotCommitTaskyardFiles(t *testing.T) {
	col := &collector{}
	h := newHarness(t, col, withBinary(scriptedAgent(t, []string{writeSummary, "echo dirty > dirty.txt"}, 1)))
	startWithPR(t, h, "run-1", nil, "")
	waitTerminal(t, col)

	if got := lastState(t, col, "run-1"); got.state != "failed" {
		t.Fatalf("state = %+v", got)
	}
	if col.count(protocol.EvFileChanged) != 1 {
		t.Fatalf("expected one salvage event, types = %v", col.types())
	}
	cmd := exec.Command("git", "show", "--name-only", "--format=", "HEAD")
	cmd.Dir = h.git.WorktreePath("run-1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), ".taskyard/summary.md") || !strings.Contains(string(out), "dirty.txt") {
		t.Fatalf("salvage commit files = %q", out)
	}
	if summaryOf(t, col, "run-1") == "" {
		t.Fatal("summary should be reported even on failure")
	}
}

func TestFinishWithoutRemoteNeedsAttention(t *testing.T) {
	col := &collector{}
	gh := newFakeGH(t)
	h := newHarness(t, col, withGH(gh.bin), withBinary(scriptedAgent(t, []string{commitOne}, 0)))
	startWithPR(t, h, "run-1", prSpec, "")
	waitTerminal(t, col)

	got := lastState(t, col, "run-1")
	if got.state != "needs_attention" || !strings.Contains(got.detail, "PR 생성 실패") {
		t.Fatalf("state = %+v", got)
	}
	if len(gh.calls()) != 0 {
		t.Fatalf("gh should not be called when push fails: %v", gh.calls())
	}
}

func TestFinishWithoutPRSpecSkipsPR(t *testing.T) {
	col := &collector{}
	gh := newFakeGH(t)
	h := newHarness(t, col, withOrigin(), withGH(gh.bin), withBinary(scriptedAgent(t, []string{commitOne}, 0)))
	startWithPR(t, h, "run-1", nil, "")
	waitTerminal(t, col)

	if got := lastState(t, col, "run-1"); got.state != "succeeded" {
		t.Fatalf("state = %+v", got)
	}
	if len(gh.calls()) != 0 || originHasBranch(t, h.repo, h.git.BranchName("run-1")) {
		t.Fatalf("no push or gh expected without PR spec; calls = %v", gh.calls())
	}
}

// trackedRun은 PR을 하나 만든 Run과, 그것을 추적하는 TrackPRs를 띄운다.
func trackedRun(t *testing.T, col *collector, gh *fakeGH) *harness {
	t.Helper()
	h := newHarness(t, col, withOrigin(), withGH(gh.bin), withPRPoll(20*time.Millisecond),
		withBinary(scriptedAgent(t, []string{commitOne}, 0)))
	startWithPR(t, h, "run-1", prSpec, "")
	waitTerminal(t, col)
	if len(prUpdatesOf(t, col, "run-1")) != 1 {
		t.Fatalf("precondition: one pr.updated, got %+v", prUpdatesOf(t, col, "run-1"))
	}
	return h
}

func track(t *testing.T, h *harness) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go h.m.TrackPRs(ctx)
}

func TestTrackPRsEmitsOnChangeAndCleansMerged(t *testing.T) {
	col := &collector{}
	gh := newFakeGH(t)
	h := trackedRun(t, col, gh)
	track(t, h)

	// OPEN 그대로 → 폴링은 하지만 이벤트는 없다.
	waitFor(t, "a poll", func() bool { return len(gh.calls()) >= 3 })
	time.Sleep(60 * time.Millisecond)
	if n := len(prUpdatesOf(t, col, "run-1")); n != 1 {
		t.Fatalf("unchanged PR produced %d pr.updated", n)
	}

	gh.setState(t, "MERGED")
	waitFor(t, "merged event", func() bool {
		prs := prUpdatesOf(t, col, "run-1")
		return len(prs) == 2 && prs[1].State == "MERGED"
	})
	prs := prUpdatesOf(t, col, "run-1")
	if !prs[1].WorktreeRemoved {
		t.Fatalf("merged event = %+v, want worktree_removed", prs[1])
	}
	if _, err := os.Stat(h.git.WorktreePath("run-1")); !os.IsNotExist(err) {
		t.Fatal("worktree should be removed after merge")
	}
	if rec := runRecord(t, h.sp, "run-1"); rec.PRState != "MERGED" || !rec.WorktreeRemoved {
		t.Fatalf("record = %+v", rec)
	}
	// 추적이 끝났다 — gh 호출이 더 늘지 않는다.
	before := len(gh.calls())
	time.Sleep(80 * time.Millisecond)
	if after := len(gh.calls()); after != before {
		t.Fatalf("still polling after MERGED: %d → %d calls", before, after)
	}
}

func TestCleanupKeepsDirtyOrUnpushedWorktree(t *testing.T) {
	for _, tc := range []struct {
		name string
		mess func(t *testing.T, wt string)
	}{
		{"dirty", func(t *testing.T, wt string) {
			if err := os.WriteFile(filepath.Join(wt, "default.profraw"), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"unpushed", func(t *testing.T, wt string) { commitFile(t, wt, "late.txt", "late") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			col := &collector{}
			gh := newFakeGH(t)
			h := trackedRun(t, col, gh)
			tc.mess(t, h.git.WorktreePath("run-1"))
			track(t, h)

			gh.setState(t, "MERGED")
			waitFor(t, "merged event", func() bool { return len(prUpdatesOf(t, col, "run-1")) == 2 })
			if prs := prUpdatesOf(t, col, "run-1"); prs[1].State != "MERGED" || prs[1].WorktreeRemoved {
				t.Fatalf("merged event = %+v, want worktree kept", prs[1])
			}
			if _, err := os.Stat(h.git.WorktreePath("run-1")); err != nil {
				t.Fatalf("worktree should be kept: %v", err)
			}
		})
	}
}

func TestTrackPRsReloadsFromSpoolAfterRestart(t *testing.T) {
	col := &collector{}
	gh := newFakeGH(t)
	h := trackedRun(t, col, gh)
	// 이벤트를 전부 ack — ActiveRuns는 비어 있어야 한다. 추적은 그와 무관하다.
	if err := h.sp.Ack("run-1", 1<<62); err != nil {
		t.Fatal(err)
	}
	if active, _ := h.sp.ActiveRuns(); len(active) != 0 {
		t.Fatalf("precondition: ActiveRuns = %v", active)
	}
	h.restart(t)
	track(t, h)

	gh.setState(t, "MERGED")
	waitFor(t, "merged event after restart", func() bool {
		prs := prUpdatesOf(t, col, "run-1")
		return len(prs) == 2 && prs[1].State == "MERGED"
	})
}

func TestTrackPRsFollowsLatestRunOnSharedBranch(t *testing.T) {
	col := &collector{}
	gh := newFakeGH(t)
	h := trackedRun(t, col, gh)

	// 이어서 재시도: 같은 worktree, 같은 브랜치 → 같은 PR에 붙는다.
	startWithPR(t, h, "run-2", prSpec, "run-1")
	waitFor(t, "run-2 terminal", func() bool { return len(stateEventsOf(t, col, "run-2")) >= 2 })
	if gh.creates() != 1 {
		t.Fatalf("retry created a second PR: %v", gh.calls())
	}
	if prs := prUpdatesOf(t, col, "run-2"); len(prs) != 1 || prs[0].Number != 7 {
		t.Fatalf("run-2 pr.updated = %+v", prs)
	}
	if rec := runRecord(t, h.sp, "run-1"); rec.PRState != "superseded" {
		t.Fatalf("run-1 record = %+v, want superseded", rec)
	}

	track(t, h)
	gh.setState(t, "MERGED")
	waitFor(t, "merged on run-2", func() bool {
		prs := prUpdatesOf(t, col, "run-2")
		return len(prs) == 2 && prs[1].State == "MERGED"
	})
	if n := len(prUpdatesOf(t, col, "run-1")); n != 1 {
		t.Fatalf("run-1 got %d pr.updated, want the original one only", n)
	}
}

func TestCrashAfterPushReattachesOnRetry(t *testing.T) {
	col := &collector{}
	gh := newFakeGH(t)
	h := trackedRun(t, col, gh)

	// push·create 직후, PR 필드를 저장하기 전에 죽은 것처럼 기록을 되돌린다.
	rec := runRecord(t, h.sp, "run-1")
	rec.State, rec.SessionID, rec.PID = "running", "", 0
	rec.PRURL, rec.PRNumber, rec.PRState = "", 0, ""
	if err := h.sp.SaveRun(rec); err != nil {
		t.Fatal(err)
	}
	h.restart(t)
	if err := h.m.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := lastState(t, col, "run-1"); got.state != "failed" {
		t.Fatalf("after reconcile = %+v, want failed (lost)", got)
	}

	startWithPR(t, h, "run-2", prSpec, "run-1")
	waitFor(t, "run-2 terminal", func() bool { return len(stateEventsOf(t, col, "run-2")) >= 2 })
	if gh.creates() != 1 {
		t.Fatalf("retry should reattach, not create: %v", gh.calls())
	}
	if prs := prUpdatesOf(t, col, "run-2"); len(prs) != 1 || prs[0].Number != 7 {
		t.Fatalf("run-2 pr.updated = %+v", prs)
	}
	if got := lastState(t, col, "run-2"); got.state != "succeeded" {
		t.Fatalf("run-2 state = %+v", got)
	}
}
