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

// nodeType returns n's type: file, dir or symlink.
func (n ResolvedNode) nodeType() string {
	if n.FromOverlay {
		return n.Overlay.NodeType()
	}
	return n.Base.Type
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

// OverlayView is every overlay entry from one read, sorted by path, with the
// generation that readers resolved against at the read.
type OverlayView struct {
	gen     int64
	entries []model.OverlayEntry
}

// ReadOverlay reads every overlay entry together with the generation.
func (r *Resolver) ReadOverlay(ctx context.Context) (OverlayView, error) {
	r.transition.RLock()
	gen := r.Generation()
	entries, err := r.Overlay.ListByPrefix(ctx, ".")
	r.transition.RUnlock()
	if err != nil {
		return OverlayView{}, err
	}
	slices.SortFunc(entries, func(a, b model.OverlayEntry) int { return strings.Compare(a.Path, b.Path) })
	return OverlayView{gen: gen, entries: entries}, nil
}

// entry returns the entry at path.
func (v OverlayView) entry(path string) (model.OverlayEntry, bool) {
	i, found := slices.BinarySearchFunc(v.entries, path, func(e model.OverlayEntry, p string) int { return strings.Compare(e.Path, p) })
	if !found {
		return model.OverlayEntry{}, false
	}
	return v.entries[i], true
}

// below returns the entries below dir. Sorted by path, they are one run,
// which below finds without copying them.
func (v OverlayView) below(dir string) []model.OverlayEntry {
	if dir == "." {
		return v.entries
	}
	from := func(prefix string) int {
		return sort.Search(len(v.entries), func(i int) bool { return v.entries[i].Path >= prefix })
	}
	// '0' follows '/', so [dir/, dir0) holds exactly the paths below dir.
	return v.entries[from(dir+"/"):from(dir+"0")]
}

// MissingPaths returns those of paths that the merged view lacks, as the
// listing of each one's parent directory shows it. See MissingPathsIn.
func (r *Resolver) MissingPaths(ctx context.Context, paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	v, err := r.ReadOverlay(ctx)
	if err != nil {
		return nil, err
	}
	return r.MissingPathsIn(v, paths)
}

// MissingPathsIn returns those of paths that the merged view of v lacks, as
// the listing of each one's parent directory shows it. It lists the snapshot
// children of each parent once, which costs far less than resolving each
// path. A parent whose listing fails leaves its children out of the result,
// and the first such error is returned.
func (r *Resolver) MissingPathsIn(v OverlayView, paths []string) ([]string, error) {
	byDir := map[string][]string{}
	for _, p := range paths {
		dir := model.CleanPath(filepath.Dir(p))
		byDir[dir] = append(byDir[dir], p)
	}
	var missing []string
	var firstErr error
	for dir, kids := range byDir {
		// A published generation never changes, so its listing needs no lock.
		children, err := r.Snapshot.ListChildren(v.gen, dir)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			if firstErr == nil {
				firstErr = fmt.Errorf("list %s: %w", dir, err)
			}
			continue
		}
		names := map[string]bool{}
		for _, e := range mergeChildren(dir, children, v.below(dir)) {
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

// listCutoff is the most paths below a changed directory for which
// ChangedPaths returns them all rather than list the directory: resolving
// them takes a query each, and listing the directory two, one per generation.
const listCutoff = 2

// ChangedPaths returns those of paths whose base type, mode or blob can
// differ between generations from and to: each that the generations give
// different ones or only one of them holds, and each below a changed
// directory that it does not list. It walks down from the root, listing a
// directory's children in both generations when more than listCutoff of
// paths lie below it. A directory with the same tree ID in both generations
// holds the same entries in both, so no path below it changed. ok is false
// when from holds no root entries, as after a prune.
func (r *Resolver) ChangedPaths(from, to int64, paths []string) (changed map[string]bool, ok bool, err error) {
	sorted := slices.Sorted(slices.Values(paths))
	// below counts the paths below each directory above one of them.
	below := map[string]int{}
	for _, p := range sorted {
		for d := filepath.Dir(p); ; d = filepath.Dir(d) {
			below[d]++
			if d == "." {
				break
			}
		}
	}
	changed = map[string]bool{}
	walk := func(dirs []string, dir string) []string {
		switch n := below[dir]; {
		case n > listCutoff:
			return append(dirs, dir)
		case n > 0:
			at := func(prefix string) int {
				i, _ := slices.BinarySearch(sorted, prefix)
				return i
			}
			// '0' follows '/', so [dir/, dir0) holds exactly the paths below dir.
			for _, p := range sorted[at(dir+"/"):at(dir+"0")] {
				changed[p] = true
			}
		}
		return dirs
	}
	for dirs := []string{"."}; len(dirs) > 0; {
		dir := dirs[len(dirs)-1]
		dirs = dirs[:len(dirs)-1]
		before, err := r.Snapshot.ListChildren(from, dir)
		if err != nil || dir == "." && len(before) == 0 {
			return nil, false, err
		}
		after, err := r.Snapshot.ListChildren(to, dir)
		if err != nil {
			return nil, false, err
		}
		added := make(map[string]model.BaseNode, len(after))
		for _, n := range after {
			added[n.Path] = n
		}
		for _, a := range before {
			b, kept := added[a.Path]
			delete(added, a.Path)
			if !kept || a.Type != b.Type || a.Mode != b.Mode || baseBlob(a) != baseBlob(b) {
				changed[a.Path] = true
			}
			if (a.Type == "dir" || kept && b.Type == "dir") && !(kept && sameTree(a, b)) {
				dirs = walk(dirs, a.Path)
			}
		}
		for _, b := range added {
			changed[b.Path] = true
			if b.Type == "dir" {
				dirs = walk(dirs, b.Path)
			}
		}
	}
	// A prune deletes a generation in one statement, so from still holding
	// its root entries shows every listing above read it whole.
	if before, err := r.Snapshot.ListChildren(from, "."); err != nil || len(before) == 0 {
		return nil, false, err
	}
	return changed, true, nil
}

// sameTree reports whether a and b are directories with the same tree ID,
// which gives them the same entries. An empty ID identifies no tree.
func sameTree(a, b model.BaseNode) bool {
	return a.Type == "dir" && b.Type == "dir" && a.ObjectOID != "" && a.ObjectOID == b.ObjectOID
}

// resolveIn resolves path as resolvePath does, against v: the overlay as v
// read it, and the generation it read with it.
func (r *Resolver) resolveIn(v OverlayView, path string) (ResolvedNode, error) {
	if ov, ok := v.entry(path); ok {
		if ov.IsDeleted() {
			return ResolvedNode{}, fs.ErrNotExist
		}
		return ResolvedNode{FromOverlay: true, Overlay: ov}, nil
	}
	if n, ok, err := r.Snapshot.LookupNode(context.Background(), v.gen, path); err != nil {
		return ResolvedNode{}, err
	} else if ok {
		return ResolvedNode{Base: n}, nil
	}
	if len(v.below(path)) > 0 {
		return ResolvedNode{Base: implicitDir(path)}, nil
	}
	return ResolvedNode{}, fs.ErrNotExist
}
