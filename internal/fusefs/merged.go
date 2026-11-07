package fusefs

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloudflare/artifact-fs/internal/model"
)

// childName extracts the immediate child name of entryPath under parent.
// Returns ("", false) if entryPath is not a descendant of parent.
func childName(parent, entryPath string) (string, bool) {
	var rel string
	if parent == "." {
		rel = entryPath
	} else {
		var ok bool
		rel, ok = strings.CutPrefix(entryPath, parent+"/")
		if !ok {
			return "", false
		}
	}
	if rel == "" {
		return "", false
	}
	rel, _, _ = strings.Cut(rel, "/")
	return rel, true
}

type Resolver struct {
	generation atomic.Int64
	commitTime atomic.Int64 // unix seconds of HEAD commit
	transition sync.RWMutex
	Snapshot   model.SnapshotStore
	Overlay    model.OverlayStore
}

func (r *Resolver) SetGeneration(gen int64) { r.generation.Store(gen) }
func (r *Resolver) Generation() int64       { return r.generation.Load() }
func (r *Resolver) SetCommitTime(ts int64)  { r.commitTime.Store(ts) }
func (r *Resolver) CommitTime() int64       { return r.commitTime.Load() }

// SwitchGeneration reconciles the overlay with generation gen, then makes gen
// and its commit time commitTime what readers resolve against.
//
// A reader never sees the overlay disagree with the generation it resolves
// against: a read under the transition read lock sees the old overlay with the
// old generation, or the reconciled overlay with gen. The overlay looks up and
// hashes its entries before SwitchGeneration takes the transition write lock,
// which covers only applying the result and the switch.
func (r *Resolver) SwitchGeneration(ctx context.Context, gen int64, commitTime int64) error {
	baseLookup := func(path string) (model.BaseNode, bool, error) {
		return r.Snapshot.LookupNode(ctx, gen, path)
	}
	return r.Overlay.ReconcileChecked(ctx, baseLookup, &r.transition, func() {
		r.SetCommitTime(commitTime)
		r.SetGeneration(gen)
	})
}

type ResolvedNode struct {
	FromOverlay bool
	Base        model.BaseNode
	Overlay     model.OverlayEntry
}

func (r *Resolver) ResolvePath(path string) (ResolvedNode, error) {
	r.transition.RLock()
	defer r.transition.RUnlock()
	return r.resolvePath(path)
}

func (r *Resolver) ResolvePathState(path string) (ResolvedNode, int64, int64, error) {
	r.transition.RLock()
	defer r.transition.RUnlock()
	n, err := r.resolvePath(path)
	return n, r.Generation(), r.CommitTime(), err
}

func (r *Resolver) resolvePath(path string) (ResolvedNode, error) {
	path = model.CleanPath(path)
	if ov, ok, err := r.Overlay.Lookup(context.Background(), path); err != nil {
		return ResolvedNode{}, err
	} else if ok {
		if ov.IsDeleted() {
			return ResolvedNode{}, fs.ErrNotExist
		}
		return ResolvedNode{FromOverlay: true, Overlay: ov}, nil
	}
	if n, ok, err := r.Snapshot.LookupNode(context.Background(), r.Generation(), path); err != nil {
		return ResolvedNode{}, err
	} else if ok {
		return ResolvedNode{Base: n}, nil
	}
	// The listing of path's parent shows path as a directory when overlay
	// entries lie below it, as mergeChildren decides.
	if below, err := r.Overlay.HasEntryBelow(context.Background(), path); err != nil {
		return ResolvedNode{}, err
	} else if below {
		return ResolvedNode{Base: implicitDir(path)}, nil
	}
	return ResolvedNode{}, fs.ErrNotExist
}

// implicitDir is the directory at path that only overlay entries below it
// imply, such as one a HEAD change removed while it held untracked files.
func implicitDir(path string) model.BaseNode {
	return model.BaseNode{Path: path, Type: "dir", Mode: 0o755, SizeState: "known"}
}

func (r *Resolver) Getattr(path string) (mode uint32, size int64, nodeType string, mtime time.Time, ctime time.Time, err error) {
	r.transition.RLock()
	defer r.transition.RUnlock()
	n, err := r.resolvePath(path)
	if err != nil {
		return 0, 0, "", time.Time{}, time.Time{}, err
	}
	if n.FromOverlay {
		typ := n.Overlay.NodeType()
		mt := time.Unix(0, n.Overlay.MtimeUnixNs)
		ct := time.Unix(0, n.Overlay.CtimeUnixNs)
		return n.Overlay.Mode, n.Overlay.SizeBytes, typ, mt, ct, nil
	}
	mode = normalizeMode(n.Base.Mode, n.Base.Type)
	// Base files use the HEAD commit timestamp for mtime so tools like
	// make see a stable, meaningful value.
	ct := r.CommitTime()
	if ct == 0 {
		ct = r.Generation() // fallback: commit time unavailable
	}
	mt := time.Unix(ct, 0)
	return mode, n.Base.SizeBytes, n.Base.Type, mt, mt, nil
}

// normalizeMode ensures sane permission bits. Git tree entries have mode 040000
// which has zero permission bits after masking; directories need at least 0o755.
func normalizeMode(mode uint32, typ string) uint32 {
	perms := mode & 0o777
	if typ == "dir" && perms == 0 {
		return 0o755
	}
	if (typ == "file" || typ == "symlink") && perms == 0 {
		return 0o644
	}
	return mode
}

// ReaddirTyped returns directory entries with name and type, so the FUSE
// adapter doesn't need to call Getattr per child.
func (r *Resolver) ReaddirTyped(ctx context.Context, path string) ([]ReaddirEntry, error) {
	r.transition.RLock()
	defer r.transition.RUnlock()
	return r.readdirTypedAt(ctx, path, r.Generation())
}

func (r *Resolver) ReaddirSnapshot(ctx context.Context, path string) ([]ReaddirEntry, int64, int64, error) {
	r.transition.RLock()
	defer r.transition.RUnlock()
	gen := r.Generation()
	entries, err := r.readdirTypedAt(ctx, path, gen)
	return entries, gen, r.CommitTime(), err
}

func (r *Resolver) readdirTypedAt(ctx context.Context, path string, generation int64) ([]ReaddirEntry, error) {
	path = model.CleanPath(path)
	children, err := r.Snapshot.ListChildren(generation, path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	ovEntries, err := r.Overlay.ListByPrefix(ctx, path)
	if err != nil {
		return nil, err
	}
	out := mergeChildren(path, children, ovEntries)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// mergeChildren merges children, the snapshot children of the directory at
// path, with ovEntries, the overlay entries below it.
func mergeChildren(path string, children []model.BaseNode, ovEntries []model.OverlayEntry) []ReaddirEntry {
	type entry struct {
		name        string
		typ         string
		base        model.BaseNode
		overlay     model.OverlayEntry
		fromOverlay bool
	}
	set := map[string]entry{}
	for _, c := range children {
		name := filepath.Base(c.Path)
		if name != "." {
			set[name] = entry{name: name, typ: c.Type, base: c}
		}
	}
	// ovEntries holds every entry below path, so a child's own entry, when it
	// has one, is among them.
	own := map[string]model.OverlayEntry{}
	implicitDirs := map[string]bool{}
	for _, e := range ovEntries {
		if e.Path == path {
			continue
		}
		name, ok := childName(path, e.Path)
		if !ok {
			continue
		}
		if e.Path == model.CleanPath(filepath.Join(path, name)) {
			own[name] = e
		} else {
			implicitDirs[name] = true
		}
	}
	for name, ov := range own {
		if ov.IsDeleted() {
			delete(set, name)
			continue
		}
		set[name] = entry{name: name, typ: ov.NodeType(), overlay: ov, fromOverlay: true}
	}
	for name := range implicitDirs {
		if _, ok := own[name]; ok {
			continue
		}
		if _, ok := set[name]; !ok {
			set[name] = entry{name: name, typ: "dir", base: implicitDir(model.CleanPath(filepath.Join(path, name)))}
		}
	}
	out := make([]ReaddirEntry, 0, len(set))
	for _, e := range set {
		if e.fromOverlay {
			out = append(out, ReaddirEntry{
				Name:        e.name,
				Type:        e.typ,
				Mode:        e.overlay.Mode,
				SizeBytes:   e.overlay.SizeBytes,
				FromOverlay: true,
				MtimeUnixNs: e.overlay.MtimeUnixNs,
				CtimeUnixNs: e.overlay.CtimeUnixNs,
			})
			continue
		}
		out = append(out, ReaddirEntry{Name: e.name, Type: e.typ, Mode: e.base.Mode, ObjectOID: e.base.ObjectOID, SizeState: e.base.SizeState, SizeBytes: e.base.SizeBytes})
	}
	return out
}

// MissingPaths returns those of paths that the merged view lacks, as the
// listing of each one's parent directory shows it. It reads the overlay once
// and the snapshot children of each parent once, which costs far less than
// resolving each path. A parent whose listing fails leaves its children out of
// the result, and the first such error is returned.
func (r *Resolver) MissingPaths(ctx context.Context, paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	byDir := map[string][]string{}
	for _, p := range paths {
		dir := model.CleanPath(filepath.Dir(p))
		byDir[dir] = append(byDir[dir], p)
	}
	r.transition.RLock()
	gen := r.Generation()
	entries, err := r.Overlay.ListByPrefix(ctx, ".")
	r.transition.RUnlock()
	if err != nil {
		return nil, err
	}
	// Sorted by path, the entries below a directory are one run, which below
	// finds without copying them.
	slices.SortFunc(entries, func(a, b model.OverlayEntry) int { return strings.Compare(a.Path, b.Path) })
	below := func(dir string) []model.OverlayEntry {
		if dir == "." {
			return entries
		}
		from := func(prefix string) int {
			return sort.Search(len(entries), func(i int) bool { return entries[i].Path >= prefix })
		}
		// '0' follows '/', so [dir/, dir0) holds exactly the paths below dir.
		return entries[from(dir+"/"):from(dir+"0")]
	}
	var missing []string
	var firstErr error
	for dir, kids := range byDir {
		// A published generation never changes, so its listing needs no lock.
		children, err := r.Snapshot.ListChildren(gen, dir)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			if firstErr == nil {
				firstErr = fmt.Errorf("list %s: %w", dir, err)
			}
			continue
		}
		names := map[string]bool{}
		for _, e := range mergeChildren(dir, children, below(dir)) {
			names[e.Name] = true
		}
		for _, p := range kids {
			if !names[filepath.Base(p)] {
				missing = append(missing, p)
			}
		}
	}
	return missing, firstErr
}
