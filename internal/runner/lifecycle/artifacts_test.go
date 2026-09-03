package lifecycle_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jinto/taskyard/internal/protocol"
)

func artifactsOf(t *testing.T, col *collector, runID string) []protocol.ArtifactBody {
	t.Helper()
	var out []protocol.ArtifactBody
	for _, e := range col.snapshot() {
		if e.RunID != runID || e.Type != protocol.EvArtifactAdded {
			continue
		}
		var wrapper struct {
			Body protocol.ArtifactBody `json:"body"`
		}
		if err := json.Unmarshal(e.Body, &wrapper); err != nil {
			t.Fatal(err)
		}
		out = append(out, wrapper.Body)
	}
	return out
}

const writeArtifacts = "mkdir -p .taskyard/artifacts && printf '# 분석\\n' > .taskyard/artifacts/analysis.md && printf 'n\\n' > .taskyard/artifacts/notes.txt"

func TestArtifactsAreCollectedBeforeTerminal(t *testing.T) {
	col := &collector{}
	h := newHarness(t, col, withBinary(scriptedAgent(t, []string{writeArtifacts}, 0)))
	startWithPR(t, h, "run-1", nil, "")
	waitTerminal(t, col)

	arts := artifactsOf(t, col, "run-1")
	if len(arts) != 2 || arts[0].Name != "analysis.md" || arts[0].Content != "# 분석\n" || arts[1].Name != "notes.txt" {
		t.Fatalf("artifacts = %+v; want analysis.md then notes.txt", arts)
	}
	types := col.types()
	if last := types[len(types)-1]; last != protocol.EvRunStateChanged {
		t.Fatalf("terminal must come last, got %v", types)
	}
	if _, err := os.Stat(filepath.Join(h.git.WorktreePath("run-1"), ".taskyard/artifacts")); !os.IsNotExist(err) {
		t.Fatal("artifacts directory should be taken (deleted)")
	}
	if got := lastState(t, col, "run-1"); got.state != "succeeded" {
		t.Fatalf("state = %+v", got)
	}
}

func TestArtifactsIgnoreSubdirsSymlinksAndCapSize(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	lines := []string{
		"mkdir -p .taskyard/artifacts/sub",
		"echo deep > .taskyard/artifacts/sub/deep.txt",
		"ln -s " + secret + " .taskyard/artifacts/link.txt",
		"head -c 307200 /dev/zero | tr '\\0' x > .taskyard/artifacts/big.txt",
		"for i in $(seq -w 1 17); do echo $i > .taskyard/artifacts/f$i.txt; done",
	}
	col := &collector{}
	h := newHarness(t, col, withBinary(scriptedAgent(t, lines, 0)))
	startWithPR(t, h, "run-1", nil, "")
	waitTerminal(t, col)

	arts := artifactsOf(t, col, "run-1")
	if len(arts) != 16 {
		t.Fatalf("got %d artifacts, want 16 (cap)", len(arts))
	}
	names := map[string]protocol.ArtifactBody{}
	for _, a := range arts {
		names[a.Name] = a
		if strings.Contains(a.Content, "SECRET") {
			t.Fatal("symlink escaping the worktree was read")
		}
	}
	if _, ok := names["deep.txt"]; ok {
		t.Fatal("subdirectory file was collected")
	}
	if _, ok := names["link.txt"]; ok {
		t.Fatal("escaping symlink was collected")
	}
	big, ok := names["big.txt"]
	if !ok || !big.Truncated || len(big.Content) != 256*1024 {
		t.Fatalf("big.txt = truncated=%v len=%d ok=%v; want 256KiB truncated", big.Truncated, len(big.Content), ok)
	}
}

func TestFailureSalvageDoesNotCommitArtifacts(t *testing.T) {
	col := &collector{}
	h := newHarness(t, col, withBinary(scriptedAgent(t, []string{writeArtifacts, "echo dirty > dirty.txt"}, 1)))
	startWithPR(t, h, "run-1", nil, "")
	waitTerminal(t, col)

	if got := lastState(t, col, "run-1"); got.state != "failed" {
		t.Fatalf("state = %+v", got)
	}
	if len(artifactsOf(t, col, "run-1")) != 2 {
		t.Fatal("artifacts of a failed run should still be reported")
	}
	cmd := exec.Command("git", "show", "--name-only", "--format=", "HEAD")
	cmd.Dir = h.git.WorktreePath("run-1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), ".taskyard/") || !strings.Contains(string(out), "dirty.txt") {
		t.Fatalf("salvage commit files = %q", out)
	}
}
