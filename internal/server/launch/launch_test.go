package launch_test

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jinto/taskyard/internal/protocol"
	"github.com/jinto/taskyard/internal/server/launch"
	"github.com/jinto/taskyard/internal/server/store"
)

// fakeCommander는 hub 대신 명령을 받아 적는다.
type fakeCommander struct {
	mu        sync.Mutex
	connected bool
	sent      []protocol.Envelope
	fail      error
}

func (f *fakeCommander) Connected() bool { return f.connected }
func (f *fakeCommander) SendCommand(env protocol.Envelope) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	f.sent = append(f.sent, env)
	return nil
}
func (f *fakeCommander) starts(t *testing.T) []protocol.RunStartBody {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []protocol.RunStartBody
	for _, env := range f.sent {
		var b protocol.RunStartBody
		if err := json.Unmarshal(env.Body, &b); err != nil {
			t.Fatal(err)
		}
		out = append(out, b)
	}
	return out
}

func newLauncher(t *testing.T) (*store.Store, *fakeCommander, *launch.Launcher) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cmd := &fakeCommander{connected: true}
	return st, cmd, &launch.Launcher{Store: st, Commander: cmd}
}

func seed(t *testing.T, st *store.Store, body string, analyzeEnabled bool, skipBelow int) (store.Project, store.Task) {
	t.Helper()
	p, err := st.CreateProject(store.Project{
		Key: "shop", Name: "s", RepoPath: "/repos/shop", DefaultBranch: "main",
		ExecuteTemplate: "실행 {{issue}}\n보고서:\n{{stage1_report}}", AnalyzeTemplate: "분석 {{issue}}",
		AnalyzeEnabled: analyzeEnabled, AnalyzeSkipBelow: skipBelow, CreatePR: true, CleanupMerged: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	task, err := st.CreateTask(store.Task{ProjectID: p.ID, Title: "제목", Body: body})
	if err != nil {
		t.Fatal(err)
	}
	return p, task
}

func TestChooseStage(t *testing.T) {
	long := strings.Repeat("가", 300)
	cases := []struct {
		name    string
		enabled bool
		skip    int
		body    string
		choice  string
		want    string
	}{
		{"auto long body analyzes", true, 200, long, "auto", store.StageAnalyze},
		{"auto short body executes", true, 200, "짧다", "auto", store.StageExecute},
		{"auto disabled executes", false, 200, long, "auto", store.StageExecute},
		{"auto skip 0 always analyzes", true, 0, "", "auto", store.StageAnalyze},
		{"explicit analyze wins", true, 200, "짧다", "analyze", store.StageAnalyze},
		{"explicit execute wins", true, 200, long, "execute", store.StageExecute},
		{"unknown choice is auto", true, 200, long, "bogus", store.StageAnalyze},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := store.Project{AnalyzeEnabled: tc.enabled, AnalyzeSkipBelow: tc.skip}
			if got := launch.ChooseStage(p, store.Task{Body: tc.body}, tc.choice); got != tc.want {
				t.Fatalf("ChooseStage = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestStartAnalyzeSendsNoPRAndRendersAnalyzeTemplate(t *testing.T) {
	st, cmd, l := newLauncher(t)
	p, task := seed(t, st, "본문", true, 0)
	run, err := l.Start(p, task, launch.Options{Stage: store.StageAnalyze})
	if err != nil {
		t.Fatal(err)
	}
	if run.Stage != store.StageAnalyze || run.State != store.StateQueued {
		t.Fatalf("run = %+v", run)
	}
	starts := cmd.starts(t)
	if len(starts) != 1 || starts[0].PR != nil || !strings.HasPrefix(starts[0].Prompt, "분석 ") {
		t.Fatalf("run.start = %+v", starts)
	}
	if got, _ := st.GetTaskByID(task.ID); got.Status != store.TaskInProgress {
		t.Fatalf("task = %s", got.Status)
	}
}

func TestStartExecuteFillsStage1ReportFromReportRun(t *testing.T) {
	st, cmd, l := newLauncher(t)
	p, task := seed(t, st, "본문", true, 0)
	if err := st.UpsertRun(store.Run{ID: "a1", State: store.StateSucceeded, Kind: "structured", TaskID: task.ID, Stage: store.StageAnalyze}); err != nil {
		t.Fatal(err)
	}
	env, _ := protocol.NewEvent(protocol.EvArtifactAdded, "a1", 1, map[string]any{"body": protocol.ArtifactBody{Name: "analysis.md", Content: "# 설계\n이렇게"}})
	env.Seq = 1
	if _, _, err := st.ApplyEvent(env); err != nil {
		t.Fatal(err)
	}

	run, err := l.Start(p, task, launch.Options{Stage: store.StageExecute, ReportRunID: "a1"})
	if err != nil {
		t.Fatal(err)
	}
	if run.ReportRunID != "a1" {
		t.Fatalf("ReportRunID = %q", run.ReportRunID)
	}
	starts := cmd.starts(t)
	if len(starts) != 1 || !strings.Contains(starts[0].Prompt, "보고서:\n# 설계\n이렇게") || starts[0].PR == nil || starts[0].PR.Title != "제목" {
		t.Fatalf("run.start = %+v", starts)
	}

	// 보고서 없이 시작하면 빈 문자열.
	if _, _, err := st.ApplyEvent(stateEvent(t, run.ID, 1, store.StateSucceeded)); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Start(p, task, launch.Options{Stage: store.StageExecute}); err != nil {
		t.Fatal(err)
	}
	if starts := cmd.starts(t); !strings.HasSuffix(starts[1].Prompt, "보고서:\n") {
		t.Fatalf("prompt without report = %q", starts[1].Prompt)
	}
}

func stateEvent(t *testing.T, runID string, seq uint64, state string) protocol.Envelope {
	t.Helper()
	env, err := protocol.NewEvent(protocol.EvRunStateChanged, runID, seq, map[string]any{"body": map[string]any{"state": state}})
	if err != nil {
		t.Fatal(err)
	}
	env.Seq = seq
	return env
}

func TestStartRefusesWhileARunIsActive(t *testing.T) {
	st, _, l := newLauncher(t)
	p, task := seed(t, st, "본문", true, 0)
	if _, err := l.Start(p, task, launch.Options{Stage: store.StageExecute}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Start(p, task, launch.Options{Stage: store.StageExecute}); !errors.Is(err, launch.ErrRunActive) {
		t.Fatalf("err = %v, want ErrRunActive", err)
	}
}

func TestStartWhenRunnerDownFailsRunAndRestoresTask(t *testing.T) {
	st, cmd, l := newLauncher(t)
	p, task := seed(t, st, "본문", true, 0)
	cmd.fail = errors.New("not connected")
	_, err := l.Start(p, task, launch.Options{Stage: store.StageExecute})
	if !errors.Is(err, launch.ErrRunnerUnavailable) {
		t.Fatalf("err = %v, want ErrRunnerUnavailable", err)
	}
	runs, _ := st.RunsForTask(task.ID)
	if len(runs) != 1 || runs[0].State != store.StateFailed {
		t.Fatalf("runs = %+v; want one failed run", runs)
	}
	if got, _ := st.GetTaskByID(task.ID); got.Status != store.TaskBacklog {
		t.Fatalf("task = %s, want backlog restored", got.Status)
	}
}

func TestChainPendingIsIdempotent(t *testing.T) {
	st, cmd, l := newLauncher(t)
	p, task := seed(t, st, strings.Repeat("가", 300), true, 200)
	a, err := l.Start(p, task, launch.Options{Stage: store.StageAnalyze})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.ApplyEvent(stateEvent(t, a.ID, 1, store.StateSucceeded)); err != nil {
		t.Fatal(err)
	}

	l.ChainPending()
	l.ChainPending()
	runs, _ := st.RunsForTask(task.ID)
	if len(runs) != 2 || runs[0].Stage != store.StageExecute || runs[0].ReportRunID != a.ID {
		t.Fatalf("runs after chain = %+v; want one execute run reporting %s", runs, a.ID)
	}
	if n := len(cmd.starts(t)); n != 2 {
		t.Fatalf("run.start sent %d times, want 2 (analyze + execute)", n)
	}

	// 1단계를 재시도해 다시 성공하면(가장 최근) 새 2단계가 하나 더.
	if _, _, err := st.ApplyEvent(stateEvent(t, runs[0].ID, 1, store.StateFailed)); err != nil {
		t.Fatal(err)
	}
	a2, err := l.Start(p, task, launch.Options{Stage: store.StageAnalyze, Previous: *a})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.ApplyEvent(stateEvent(t, a2.ID, 1, store.StateSucceeded)); err != nil {
		t.Fatal(err)
	}
	l.ChainPending()
	runs, _ = st.RunsForTask(task.ID)
	if len(runs) != 4 || runs[0].Stage != store.StageExecute || runs[0].ReportRunID != a2.ID {
		t.Fatalf("runs after retry chain = %+v", runs)
	}
}

func TestChainPendingSkipsAttentionAndDisconnectedRunner(t *testing.T) {
	st, cmd, l := newLauncher(t)
	p, task := seed(t, st, "본문", true, 0)
	a, _ := l.Start(p, task, launch.Options{Stage: store.StageAnalyze})
	if _, _, err := st.ApplyEvent(stateEvent(t, a.ID, 1, store.StateNeedsAttention)); err != nil {
		t.Fatal(err)
	}
	l.ChainPending()
	if runs, _ := st.RunsForTask(task.ID); len(runs) != 1 {
		t.Fatalf("needs_attention must not chain: %+v", runs)
	}

	st2, cmd2, l2 := newLauncher(t)
	p2, task2 := seed(t, st2, "본문", true, 0)
	a2, _ := l2.Start(p2, task2, launch.Options{Stage: store.StageAnalyze})
	_, _, _ = st2.ApplyEvent(stateEvent(t, a2.ID, 1, store.StateSucceeded))
	cmd2.connected = false
	l2.ChainPending()
	if runs, _ := st2.RunsForTask(task2.ID); len(runs) != 1 {
		t.Fatalf("disconnected runner must not chain (no failed run either): %+v", runs)
	}
	_ = cmd
}

func TestChainPendingRacesWithManualStart(t *testing.T) {
	st, _, l := newLauncher(t)
	p, task := seed(t, st, "본문", true, 0)
	a, _ := l.Start(p, task, launch.Options{Stage: store.StageAnalyze})
	_, _, _ = st.ApplyEvent(stateEvent(t, a.ID, 1, store.StateSucceeded))

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); l.ChainPending() }()
		go func() {
			defer wg.Done()
			_, _ = l.Start(p, task, launch.Options{Stage: store.StageExecute, ReportRunID: a.ID})
		}()
	}
	wg.Wait()
	runs, _ := st.RunsForTask(task.ID)
	if len(runs) != 2 {
		t.Fatalf("expected exactly one execute run, got %d runs: %+v", len(runs), runs)
	}
}
