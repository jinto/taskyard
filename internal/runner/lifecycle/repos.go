package lifecycle

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"sync"

	"github.com/jinto/taskyard/internal/gitops"
)

// ErrRepoNotAllowed는 run.start가 가리킨 저장소가 허용 목록에 없을 때다.
// 명령 처리 오류가 아니라 그 Run의 실패다.
var ErrRepoNotAllowed = errors.New("repository not allowed")

// RepoResolver는 Runner가 허용한 저장소 목록이고, 경로에서 그 저장소의
// gitops.Manager를 찾아 준다(PRD RN-03). 프로젝트마다 저장소가 다르므로
// Runner는 하나가 아니라 목록을 안다.
//
// 경로는 filepath.Abs → filepath.EvalSymlinks로 정규화한 뒤 비교한다.
// macOS의 /tmp ↔ /private/tmp처럼 같은 디렉터리의 다른 표기가 같은
// 저장소로 풀려야 한다.
type RepoResolver struct {
	worktreeRoot string
	order        []string // 정규화된 허용 경로. 첫 원소가 기본값
	allowed      map[string]bool

	mu       sync.Mutex
	managers map[string]*gitops.Manager
}

// NewRepoResolver는 허용 목록의 각 경로를 정규화해 보관한다. 비절대 경로나
// 존재하지 않는 경로가 있으면 오류다 — 기동 시점에 잘못된 설정을 알린다.
func NewRepoResolver(allowed []string, worktreeRoot string) (*RepoResolver, error) {
	if len(allowed) == 0 {
		return nil, errors.New("lifecycle: at least one allowed repository is required")
	}
	r := &RepoResolver{
		worktreeRoot: worktreeRoot,
		allowed:      map[string]bool{},
		managers:     map[string]*gitops.Manager{},
	}
	for _, p := range allowed {
		canon, err := canonicalRepoPath(p)
		if err != nil {
			return nil, fmt.Errorf("allowed repository %q: %w", p, err)
		}
		if r.allowed[canon] {
			continue
		}
		r.allowed[canon] = true
		r.order = append(r.order, canon)
	}
	return r, nil
}

func canonicalRepoPath(p string) (string, error) {
	if !filepath.IsAbs(p) {
		return "", errors.New("path is not absolute")
	}
	canon, err := filepath.EvalSymlinks(filepath.Clean(p))
	if err != nil {
		return "", err
	}
	return canon, nil
}

// Manager는 repoPath가 허용 목록에 있으면 그 저장소의 관리자를 돌려준다.
// 같은 저장소면 같은 인스턴스다. 정규화에 실패하거나 목록에 없으면
// ErrRepoNotAllowed.
func (r *RepoResolver) Manager(repoPath string) (*gitops.Manager, error) {
	canon, err := canonicalRepoPath(repoPath)
	if err != nil {
		return nil, fmt.Errorf("%w: %s (%v)", ErrRepoNotAllowed, repoPath, err)
	}
	if !r.allowed[canon] {
		return nil, fmt.Errorf("%w: %s", ErrRepoNotAllowed, repoPath)
	}
	return r.managerFor(canon), nil
}

// First는 허용 목록의 첫 저장소다. repo_path가 없는 명령과 RepoPath가 없는
// Phase 0 원장 기록의 기본값이다.
func (r *RepoResolver) First() *gitops.Manager {
	return r.managerFor(r.order[0])
}

// RepoPathOf는 관리자가 맡은 저장소의 정규화 경로다. 원장에 기록하는 값이다.
func (r *RepoResolver) RepoPathOf(m *gitops.Manager) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for canon, mgr := range r.managers {
		if mgr == m {
			return canon
		}
	}
	return ""
}

func (r *RepoResolver) managerFor(canon string) *gitops.Manager {
	r.mu.Lock()
	defer r.mu.Unlock()
	if m, ok := r.managers[canon]; ok {
		return m
	}
	// 저장소별 worktree 루트. 이름이 같은 저장소 둘이 충돌하지 않도록
	// 경로 해시를 붙인다.
	sum := sha1.Sum([]byte(canon))
	root := filepath.Join(r.worktreeRoot, filepath.Base(canon)+"-"+hex.EncodeToString(sum[:])[:8])
	m := gitops.New(canon, root)
	r.managers[canon] = m
	return m
}

// resolve는 run.start와 원장 기록이 공통으로 쓰는 해석이다. 빈 경로는 첫
// 허용 저장소다.
func (r *RepoResolver) resolve(repoPath string) (*gitops.Manager, string, error) {
	if repoPath == "" {
		m := r.First()
		return m, r.order[0], nil
	}
	m, err := r.Manager(repoPath)
	if err != nil {
		return nil, "", err
	}
	return m, r.RepoPathOf(m), nil
}
