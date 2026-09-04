package lifecycle

import (
	"log/slog"
	"os"
	"path/filepath"
	"sort"

	"github.com/jinto/taskyard/internal/protocol"
)

// 산출물(ST-06): 에이전트가 worktree의 .taskyard/artifacts/ 에 남긴 파일.
// attention·summary와 같은 패턴 — finish에서 salvage보다 먼저 읽고 지운다.
const (
	artifactsDir        = ".taskyard/artifacts"
	maxArtifactBytes    = 256 * 1024
	maxArtifactsPerRun  = 16
	maxArtifactsTotalMB = 1
)

// takeArtifacts는 산출물 디렉터리의 일반 파일을 이름순으로 읽고 디렉터리를
// 지운다. 하위 디렉터리는 무시하고, worktree 밖을 가리키는 symlink는
// readWorktreeFile이 거른다. 파일당 256KiB(넘으면 truncated), Run당 16개,
// 합계 1MiB. 예산이 다하면 나머지는 로그만 남긴다.
func takeArtifacts(worktree string) []protocol.ArtifactBody {
	dir := filepath.Join(worktree, artifactsDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	var out []protocol.ArtifactBody
	budget := maxArtifactsTotalMB * 1024 * 1024
	for _, name := range names {
		if len(out) >= maxArtifactsPerRun || budget <= 0 {
			slog.Warn("artifact budget exhausted; ignoring", "name", name)
			continue
		}
		content, truncated, ok := readWorktreeFile(worktree, filepath.Join(artifactsDir, name), maxArtifactBytes)
		if !ok {
			continue
		}
		if len(content) > budget {
			content, truncated = content[:budget], true
		}
		budget -= len(content)
		out = append(out, protocol.ArtifactBody{Name: name, Content: content, Truncated: truncated})
	}
	if err := os.RemoveAll(dir); err != nil {
		slog.Warn("could not remove artifacts directory", "err", err)
	}
	return out
}

func (m *Manager) emitArtifacts(runID string, arts []protocol.ArtifactBody) {
	for _, a := range arts {
		env, err := protocol.NewEvent(protocol.EvArtifactAdded, runID, 0, eventBody{Body: map[string]any{
			"name": a.Name, "content": a.Content, "truncated": a.Truncated,
		}})
		if err != nil {
			slog.Error("build artifact event failed", "err", err)
			continue
		}
		if err := m.cfg.Publish(runID, env); err != nil {
			slog.Error("publish artifact event failed", "run_id", runID, "name", a.Name, "err", err)
		}
	}
}
