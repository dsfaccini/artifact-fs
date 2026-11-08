package watcher

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Poller struct {
	// mu serializes polls and Rebase.
	mu              sync.Mutex
	prev            map[string]string
	firstSeenChange map[string]struct{}
}

func New() *Poller {
	return &Poller{prev: map[string]string{}, firstSeenChange: map[string]struct{}{}}
}

// Watch polls gitDir's HEAD and the ref it names on each tick, and calls fn
// when either changed since the last poll or Rebase.
func (p *Poller) Watch(ctx context.Context, gitDir string, ticks <-chan time.Time, fn func()) {
	headPath := filepath.Join(gitDir, "HEAD")
	// Prime first, then reconcile. A change during reconciliation is observed by
	// the next poll instead of falling into the startup baseline window.
	p.Rebase(gitDir)
	fn()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			p.mu.Lock()
			changed := p.headChanged(headPath)
			p.mu.Unlock()
			if changed {
				fn()
			}
		}
	}
}

// Rebase makes gitDir's HEAD and the ref it names, as they are now, the state
// the next poll compares with.
func (p *Poller) Rebase(gitDir string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	_ = p.headChanged(filepath.Join(gitDir, "HEAD"))
}

func (p *Poller) headChanged(headPath string) bool {
	headChanged := p.changed(headPath)
	ref, ok := p.headRefPath(headPath)
	if headChanged {
		if ok {
			_ = p.currentRefChanged(ref)
		}
		return true
	}
	return ok && p.currentRefChanged(ref)
}

type headReference struct {
	path       string
	name       string
	packedPath string
}

func (p *Poller) currentRefChanged(ref headReference) bool {
	data, err := os.ReadFile(ref.path)
	value := strings.TrimSpace(string(data))
	found := err == nil
	if err != nil && !os.IsNotExist(err) {
		return false
	}
	if !found {
		packed, err := os.ReadFile(ref.packedPath)
		if err != nil && !os.IsNotExist(err) {
			return false
		}
		for line := range strings.SplitSeq(string(packed), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 2 && fields[1] == ref.name {
				value, found = fields[0], true
				break
			}
		}
	}
	if !found {
		_, hadValue := p.prev[ref.path]
		delete(p.prev, ref.path)
		p.firstSeenChange[ref.path] = struct{}{}
		return hadValue
	}
	previous, seen := p.prev[ref.path]
	p.prev[ref.path] = value
	_, changeOnFirstSeen := p.firstSeenChange[ref.path]
	delete(p.firstSeenChange, ref.path)
	return (seen && previous != value) || (!seen && changeOnFirstSeen)
}

func (p *Poller) headRefPath(headPath string) (headReference, bool) {
	data, err := os.ReadFile(headPath)
	if err != nil {
		return headReference{}, false
	}
	line := strings.TrimSpace(string(data))
	ref, ok := strings.CutPrefix(line, "ref: ")
	if !ok {
		return headReference{}, false
	}
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return headReference{}, false
	}
	gitDir := filepath.Dir(headPath)
	// Git keeps these namespaces private; ordinary branch refs and packed-refs
	// live in the common directory of a linked worktree.
	if !strings.HasPrefix(ref, "refs/bisect/") && !strings.HasPrefix(ref, "refs/worktree/") && !strings.HasPrefix(ref, "refs/rewritten/") {
		if common, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil {
			commonDir := strings.TrimSpace(string(common))
			if filepath.IsAbs(commonDir) {
				gitDir = commonDir
			} else {
				gitDir = filepath.Join(gitDir, commonDir)
			}
		}
	}
	return headReference{path: filepath.Join(gitDir, filepath.FromSlash(ref)), name: ref, packedPath: filepath.Join(gitDir, "packed-refs")}, true
}

func (p *Poller) changed(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	value := string(data)
	prev, ok := p.prev[path]
	p.prev[path] = value
	if !ok {
		if _, changeOnFirstSeen := p.firstSeenChange[path]; changeOnFirstSeen {
			delete(p.firstSeenChange, path)
			return true
		}
		return false
	}
	delete(p.firstSeenChange, path)
	return value != prev
}
