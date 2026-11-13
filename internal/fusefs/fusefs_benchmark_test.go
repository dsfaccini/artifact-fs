package fusefs

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/model"
	overlaystore "github.com/cloudflare/artifact-fs/internal/overlay"
	"github.com/cloudflare/artifact-fs/internal/snapshot"
	"github.com/dsfaccini/fuse/fuseops"
)

// lockTimer adds up how long reconciles hold the lock, which is how long
// SwitchGeneration keeps readers waiting.
type lockTimer struct {
	model.OverlayStore
	b         *testing.B
	wall, cpu time.Duration
}

func (o *lockTimer) ReconcileChecked(ctx context.Context, baseLookup func(string) (model.BaseNode, bool, error), switched model.SwitchedPaths, lock sync.Locker, switchBase func()) error {
	var wallStart time.Time
	var cpuStart time.Duration
	return o.OverlayStore.ReconcileChecked(ctx, baseLookup, switched, observedLock{
		Locker: lock,
		locked: func() {
			wallStart, cpuStart = time.Now(), cpuTime(o.b)
		},
		unlocking: func() {
			o.wall += time.Since(wallStart)
			o.cpu += cpuTime(o.b) - cpuStart
		},
	}, switchBase)
}

func cpuTime(b *testing.B) time.Duration {
	b.Helper()
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		b.Fatal(err)
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

// switchGeneration runs one SwitchGeneration and adds its process CPU time to
// cpu.
func switchGeneration(b *testing.B, r *Resolver, gen int64, cpu *time.Duration) {
	b.Helper()
	start := cpuTime(b)
	if _, err := r.SwitchGeneration(context.Background(), gen, 0); err != nil {
		b.Fatal(err)
	}
	*cpu += cpuTime(b) - start
}

// reportSwitch reports the process CPU time per switch, and the wall and CPU
// time the lock was held per switch. CPU time is steadier than wall time on a
// loaded machine.
func reportSwitch(b *testing.B, cpu time.Duration, timer *lockTimer) {
	n := float64(b.N)
	b.ReportMetric(float64(cpu.Nanoseconds())/n, "cpu-ns/op")
	b.ReportMetric(float64(timer.wall.Nanoseconds())/n, "lock-wall-ns/op")
	b.ReportMetric(float64(timer.cpu.Nanoseconds())/n, "lock-cpu-ns/op")
}

// BenchmarkSwitchGeneration20kOverlayEntries switches generation over 20k
// overlay creates.
//   - kept: the 100k-file generation lacks every create, so the switch looks
//     up every entry and keeps it.
//   - committed: the generation holds every create's content, as after a
//     commit, so the switch removes every entry and its backing file.
func BenchmarkSwitchGeneration20kOverlayEntries(b *testing.B) {
	const entries = 20_000
	ctx := context.Background()
	newSnapshot := func(b *testing.B, nodes []model.BaseNode) (*snapshot.Store, int64) {
		b.Helper()
		snap, err := snapshot.New(ctx, filepath.Join(b.TempDir(), "snapshot.sqlite"))
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() { _ = snap.Close() })
		gen, err := snap.PublishGeneration(ctx, "head", "main", append([]model.BaseNode{{Path: ".", Type: "dir", Mode: 0o40000, SizeState: "known"}}, nodes...))
		if err != nil {
			b.Fatal(err)
		}
		return snap, gen
	}
	newOverlay := func(b *testing.B, path func(i int) string, write bool) *overlaystore.Store {
		b.Helper()
		dir := b.TempDir()
		ov, err := overlaystore.New(ctx, model.RepoPaths{ID: "bench", OverlayDir: dir, OverlayDBPath: filepath.Join(dir, "overlay.sqlite")})
		if err != nil {
			b.Fatal(err)
		}
		for i := range entries {
			if _, err := ov.CreateFile(ctx, path(i), 0o644); err != nil {
				b.Fatal(err)
			}
			if !write {
				continue
			}
			if _, err := ov.WriteFile(ctx, path(i), 0, []byte(path(i))); err != nil {
				b.Fatal(err)
			}
		}
		return ov
	}

	b.Run("kept", func(b *testing.B) {
		var nodes []model.BaseNode
		for d := range 1000 {
			nodes = append(nodes, model.BaseNode{Path: fmt.Sprintf("pkg%d", d), Type: "dir", Mode: 0o40000, SizeState: "known"})
			for f := range 100 {
				nodes = append(nodes, model.BaseNode{Path: fmt.Sprintf("pkg%d/file%d.go", d, f), Type: "file", Mode: 0o100644, ObjectOID: fmt.Sprintf("%040x", d*100+f), SizeState: "known", SizeBytes: 10})
			}
		}
		snap, gen := newSnapshot(b, nodes)
		ov := newOverlay(b, func(i int) string { return fmt.Sprintf("node_modules/pkg%d/lib/f%d.js", i%500, i) }, false)
		b.Cleanup(func() { _ = ov.Close() })
		timer := &lockTimer{OverlayStore: ov, b: b}
		r := &Resolver{Snapshot: snap, Overlay: timer}
		r.SetGeneration(gen)
		var cpu time.Duration
		b.ResetTimer()
		for range b.N {
			switchGeneration(b, r, gen, &cpu)
		}
		reportSwitch(b, cpu, timer)
	})

	b.Run("committed", func(b *testing.B) {
		path := func(i int) string { return fmt.Sprintf("f%d.txt", i) }
		var nodes []model.BaseNode
		for i := range entries {
			nodes = append(nodes, model.BaseNode{Path: path(i), Type: "file", Mode: 0o100644, ObjectOID: blobOID(path(i)), SizeState: "known", SizeBytes: int64(len(path(i)))})
		}
		snap, gen := newSnapshot(b, nodes)
		timer := &lockTimer{b: b}
		var cpu time.Duration
		b.ResetTimer()
		for range b.N {
			b.StopTimer()
			ov := newOverlay(b, path, true)
			timer.OverlayStore = ov
			r := &Resolver{Snapshot: snap, Overlay: timer}
			r.SetGeneration(gen)
			b.StartTimer()
			switchGeneration(b, r, gen, &cpu)
			b.StopTimer()
			if entries, err := ov.ListAll(ctx); err != nil || len(entries) != 0 {
				b.Fatalf("overlay after the switch holds %d entries, %v; want none", len(entries), err)
			}
			_ = ov.Close()
			b.StartTimer()
		}
		reportSwitch(b, cpu, timer)
	})
}

// inodeTableTree returns n files, perDir per directory, with their
// directories. A directory path is 44 characters long and a file path 69,
// close to the 68 and 78 characters of the mean tracked file path of
// pydantic-ai and platform, which hold about 11 and 12 files per directory.
func inodeTableTree(n, perDir int) []model.BaseNode {
	nodes := make([]model.BaseNode, 0, n+n/perDir+1)
	for i := range n {
		dir := fmt.Sprintf("packages/module-%05d/src/components/widgets", i/perDir)
		if i%perDir == 0 {
			nodes = append(nodes, model.BaseNode{Path: dir, Type: "dir", Mode: 0o40000, SizeState: "known"})
		}
		path := fmt.Sprintf("%s/component-file-%05d.tsx", dir, i%100000)
		nodes = append(nodes, model.BaseNode{Path: path, Type: "file", Mode: 0o100644, ObjectOID: fmt.Sprintf("%040x", i), SizeState: "known", SizeBytes: 10})
	}
	return nodes
}

func heapInUse() uint64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// BenchmarkInodeTable reports the heap the inode table holds per path the
// kernel looked up once: held-B/path while the kernel holds every inode, and
// dormant-B/path once it has forgotten them all. dormant-B/path includes the
// capacity of the emptied inodes map, which Go never shrinks; table-B/path
// leaves it out. Each lookup allocates its path and blob strings, as
// LookUpInode does.
func BenchmarkInodeTable(b *testing.B) {
	for _, n := range []int{100_000, 300_000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			var paths, blobs []string
			for _, node := range inodeTableTree(n, 100) {
				paths, blobs = append(paths, node.Path), append(blobs, node.ObjectOID)
			}
			var held, dormant, table uint64
			for range b.N {
				before := heapInUse()
				fs := NewArtifactFuse(model.RepoPaths{}, nil)
				ids := make([]fuseops.InodeID, len(paths))
				fs.mu.Lock()
				for i, path := range paths {
					ids[i] = fs.allocInode(strings.Clone(path), Attrs{Type: "file", Mode: 0o644, Gen: 1, Blob: strings.Clone(blobs[i])}).ID
				}
				fs.mu.Unlock()
				idsSize := uint64(len(ids)) * 8
				held += heapInUse() - before - idsSize
				for _, id := range ids {
					_ = fs.ForgetInode(context.Background(), &fuseops.ForgetInodeOp{Inode: id, N: 1})
				}
				dormant += heapInUse() - before - idsSize
				if len(fs.pathToInode) != len(paths)+1 {
					b.Fatalf("table maps %d paths after FORGET, want %d", len(fs.pathToInode), len(paths)+1)
				}
				fs.inodes = map[fuseops.InodeID]*InodeRef{fuseops.RootInodeID: fs.inodes[fuseops.RootInodeID]}
				table += heapInUse() - before - idsSize
				runtime.KeepAlive(ids)
				runtime.KeepAlive(fs)
			}
			perPath := float64(b.N * len(paths))
			b.ReportMetric(float64(held)/perPath, "held-B/path")
			b.ReportMetric(float64(dormant)/perPath, "dormant-B/path")
			b.ReportMetric(float64(table)/perPath, "table-B/path")
			b.ReportMetric(float64(dormant)/float64(b.N)/(1<<20), "dormant-MiB")
		})
	}
}

// BenchmarkSwitchPassForgottenPaths times the pass a generation switch runs
// over paths the kernel looked up once and forgot, in the snapshot or in the
// overlay. The pass reads the overlay once and lists the snapshot children of
// each directory that holds one of the paths.
func BenchmarkSwitchPassForgottenPaths(b *testing.B) {
	overlayTree := func(format string) []string {
		var files []string
		for i := range 20_000 {
			files = append(files, fmt.Sprintf(format, i%500, i))
		}
		return files
	}
	for _, layout := range []struct {
		name    string
		nodes   []model.BaseNode
		overlay []string
	}{
		{name: "snapshot/100-per-dir/100k", nodes: inodeTableTree(100_000, 100)},
		{name: "snapshot/100-per-dir/300k", nodes: inodeTableTree(300_000, 100)},
		{name: "snapshot/10-per-dir/100k", nodes: inodeTableTree(100_000, 10)},
		{name: "overlay/node_modules", overlay: overlayTree("node_modules/pkg%d/lib/f%d.js")},
		{name: "overlay/venv", overlay: overlayTree(".venv/lib/python3.12/site-packages/pkg%d/sub/f%d.js")},
	} {
		b.Run(layout.name, func(b *testing.B) {
			benchSwitchPass(b, layout.nodes, layout.overlay)
		})
	}
}

// benchSwitchPass publishes nodes, creates overlayFiles, and maps every one of
// their paths and parent directories to an inode the kernel then forgets.
// Then it times the pass and reports paths, cpu-ns/op and cpu-ns/path.
func benchSwitchPass(b *testing.B, nodes []model.BaseNode, overlayFiles []string) {
	ctx := context.Background()
	snap, err := snapshot.New(ctx, filepath.Join(b.TempDir(), "snapshot.sqlite"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = snap.Close() })
	gen, err := snap.PublishGeneration(ctx, "head", "main", append([]model.BaseNode{{Path: ".", Type: "dir", Mode: 0o40000, SizeState: "known"}}, nodes...))
	if err != nil {
		b.Fatal(err)
	}
	dir := b.TempDir()
	ov, err := overlaystore.New(ctx, model.RepoPaths{ID: "bench", OverlayDir: dir, OverlayDBPath: filepath.Join(dir, "overlay.sqlite")})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = ov.Close() })
	paths := map[string]bool{}
	for _, n := range nodes {
		paths[n.Path] = true
	}
	for _, p := range overlayFiles {
		if _, err := ov.CreateFile(ctx, p, 0o644); err != nil {
			b.Fatal(err)
		}
		for q := p; q != "."; q = filepath.Dir(q) {
			paths[q] = true
		}
	}
	r := &Resolver{Snapshot: snap, Overlay: ov}
	r.SetGeneration(gen)
	fs := NewArtifactFuse(model.RepoPaths{}, &Engine{Resolver: r, Overlay: ov})
	var ids []fuseops.InodeID
	fs.mu.Lock()
	for p := range paths {
		ids = append(ids, fs.allocInode(p, Attrs{Type: "file", Mode: 0o644, Gen: gen}).ID)
	}
	fs.mu.Unlock()
	for _, id := range ids {
		_ = fs.ForgetInode(ctx, &fuseops.ForgetInodeOp{Inode: id, N: 1})
	}
	var cpu time.Duration
	b.ResetTimer()
	for range b.N {
		start := cpuTime(b)
		if err := fs.invalidateChanged(false); err != nil {
			b.Fatal(err)
		}
		cpu += cpuTime(b) - start
	}
	b.StopTimer()
	if len(fs.pathToInode) != len(paths)+1 {
		b.Fatalf("pass dropped mappings of paths that exist: %d left, want %d", len(fs.pathToInode), len(paths)+1)
	}
	b.ReportMetric(float64(len(paths)), "paths")
	b.ReportMetric(float64(cpu.Nanoseconds())/float64(b.N), "cpu-ns/op")
	b.ReportMetric(float64(cpu.Nanoseconds())/float64(b.N*len(paths)), "cpu-ns/path")
}

// listingHook runs onList before the snapshot's next listing.
type listingHook struct {
	model.SnapshotStore
	onList func()
}

func (s *listingHook) ListChildren(gen int64, dir string) ([]model.BaseNode, error) {
	fire(&s.onList)
	return s.SnapshotStore.ListChildren(gen, dir)
}

// BenchmarkMissingPathsHeap reports the live heap MissingPaths holds over an
// overlay of files, checking every overlay path and parent directory:
// listall-MiB for the overlay read alone, and peak-MiB when the first snapshot
// listing starts, after MissingPaths has read and grouped the overlay.
func BenchmarkMissingPathsHeap(b *testing.B) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, format string
		files, pkgs  int
	}{
		{"venv/20k", ".venv/lib/python3.12/site-packages/pkg%d/sub/f%d.js", 20_000, 500},
		{"node_modules/20k", "node_modules/pkg%d/lib/f%d.js", 20_000, 500},
		{"venv/60k", ".venv/lib/python3.12/site-packages/pkg%d/sub/f%d.js", 60_000, 1500},
	} {
		b.Run(tc.name, func(b *testing.B) {
			snap, err := snapshot.New(ctx, filepath.Join(b.TempDir(), "snapshot.sqlite"))
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { _ = snap.Close() })
			gen, err := snap.PublishGeneration(ctx, "head", "main", []model.BaseNode{{Path: ".", Type: "dir", Mode: 0o40000, SizeState: "known"}})
			if err != nil {
				b.Fatal(err)
			}
			dir := b.TempDir()
			ov, err := overlaystore.New(ctx, model.RepoPaths{ID: "bench", OverlayDir: dir, OverlayDBPath: filepath.Join(dir, "overlay.sqlite")})
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { _ = ov.Close() })
			set := map[string]bool{}
			for i := range tc.files {
				p := fmt.Sprintf(tc.format, i%tc.pkgs, i)
				if _, err := ov.CreateFile(ctx, p, 0o644); err != nil {
					b.Fatal(err)
				}
				for q := p; q != "."; q = filepath.Dir(q) {
					set[q] = true
				}
			}
			paths := make([]string, 0, len(set))
			for p := range set {
				paths = append(paths, p)
			}
			snapHook := &listingHook{SnapshotStore: snap}
			r := &Resolver{Snapshot: snapHook, Overlay: ov}
			r.SetGeneration(gen)
			var listAll, peak uint64
			b.ResetTimer()
			for range b.N {
				before := heapInUse()
				all, err := ov.ListAll(ctx)
				if err != nil {
					b.Fatal(err)
				}
				listAll += heapInUse() - before
				runtime.KeepAlive(all)
				all = nil
				before = heapInUse()
				snapHook.onList = func() { peak += heapInUse() - before }
				missing, err := r.MissingPaths(ctx, paths)
				if err != nil || len(missing) != 0 {
					b.Fatalf("MissingPaths = %d paths, %v; want none", len(missing), err)
				}
			}
			b.ReportMetric(float64(len(paths)), "paths")
			b.ReportMetric(float64(listAll)/float64(b.N)/(1<<20), "listall-MiB")
			b.ReportMetric(float64(peak)/float64(b.N)/(1<<20), "peak-MiB")
		})
	}
}

// BenchmarkMissingPathsOnePath times the switch pass's re-check, which holds
// handleOps: MissingPaths of one vanished path over an overlay of n files.
func BenchmarkMissingPathsOnePath(b *testing.B) {
	ctx := context.Background()
	for _, n := range []int{20_000, 60_000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			snap, err := snapshot.New(ctx, filepath.Join(b.TempDir(), "snapshot.sqlite"))
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { _ = snap.Close() })
			gen, err := snap.PublishGeneration(ctx, "head", "main", []model.BaseNode{{Path: ".", Type: "dir", Mode: 0o40000, SizeState: "known"}})
			if err != nil {
				b.Fatal(err)
			}
			dir := b.TempDir()
			ov, err := overlaystore.New(ctx, model.RepoPaths{ID: "bench", OverlayDir: dir, OverlayDBPath: filepath.Join(dir, "overlay.sqlite")})
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { _ = ov.Close() })
			for i := range n {
				if _, err := ov.CreateFile(ctx, fmt.Sprintf(".venv/lib/python3.12/site-packages/pkg%d/sub/f%d.js", i%(n/40), i), 0o644); err != nil {
					b.Fatal(err)
				}
			}
			r := &Resolver{Snapshot: snap, Overlay: ov}
			r.SetGeneration(gen)
			b.ResetTimer()
			for range b.N {
				missing, err := r.MissingPaths(ctx, []string{"src/gone.txt"})
				if err != nil || len(missing) != 1 {
					b.Fatalf("MissingPaths = %v, %v; want src/gone.txt", missing, err)
				}
			}
		})
	}
}

// switchPassTree returns a generation of 6,300 entries shaped like a source
// repository, as two variants: base, and next, which gives changed of base's
// files, spread over the tree, new blobs. Its 525 directories of about 11
// files lie in 5 top-level directories of 15 packages each. A directory's
// object ID derives from its entries, as a Git tree ID does, so a changed
// file changes the ID of every directory above it. It also returns base's
// leaf directories and files, each sorted.
func switchPassTree(changed int) (base, next []model.BaseNode, leaves, files []string) {
	const entries = 6_300
	dirs := []string{"src"}
	for t := range 5 {
		area := fmt.Sprintf("src/area-%d", t)
		dirs = append(dirs, area)
		for p := range 15 {
			pkg := fmt.Sprintf("%s/package-%02d", area, p)
			dirs = append(dirs, pkg)
			for m := range 7 {
				leaf := fmt.Sprintf("%s/module-%d", pkg, m)
				dirs, leaves = append(dirs, leaf), append(leaves, leaf)
			}
		}
	}
	for i := range entries - 1 - len(dirs) {
		files = append(files, fmt.Sprintf("%s/component-%04d.go", leaves[i%len(leaves)], i))
	}
	slices.Sort(files)
	build := func(changes int) []model.BaseNode {
		stride := len(files) / max(changes, 1)
		kids := map[string][]model.BaseNode{}
		for i, f := range files {
			oid := fmt.Sprintf("%040x", i)
			if i%stride == 0 && i/stride < changes {
				oid = fmt.Sprintf("%040x", len(files)+i)
			}
			kids[filepath.Dir(f)] = append(kids[filepath.Dir(f)], model.BaseNode{Path: f, Type: "file", Mode: 0o100644, ObjectOID: oid, SizeState: "known", SizeBytes: 10})
		}
		// dirs lists each directory before those below it.
		for _, d := range slices.Backward(dirs) {
			var listing strings.Builder
			for _, k := range kids[d] {
				fmt.Fprintf(&listing, "%s %s\n", k.Path, k.ObjectOID)
			}
			kids[filepath.Dir(d)] = append(kids[filepath.Dir(d)], model.BaseNode{Path: d, Type: "dir", Mode: 0o40000, ObjectOID: blobOID(listing.String()), SizeState: "known"})
		}
		nodes := []model.BaseNode{{Path: ".", Type: "dir", Mode: 0o755, SizeState: "known"}}
		for _, k := range kids {
			nodes = append(nodes, k...)
		}
		return nodes
	}
	return build(0), build(changed), leaves, files
}

// countingNotifier counts the invalidations a pass sends, and records the
// inodes it invalidated since the last take.
type countingNotifier struct {
	inodes, entries int
	invalidated     []fuseops.InodeID
}

func (n *countingNotifier) InvalidateInode(id fuseops.InodeID, _, _ int64) error {
	n.inodes++
	n.invalidated = append(n.invalidated, id)
	return nil
}

func (n *countingNotifier) InvalidateEntry(fuseops.InodeID, string) error {
	n.entries++
	return nil
}

// untrackedEntries is the number of node_modules entries the 20,000-held
// case adds to the 6,299 base inodes, the tree without its root.
const untrackedEntries = 20_000 - 6_299

// BenchmarkSwitchPass times the pass after a generation switch that tells
// the kernel what changed, over the 6,300-entry tree of switchPassTree. Each
// iteration publishes the tree's other variant as a new generation, prunes
// all but the last two, and switches to it, so changed files differ. The kernel holds held inodes: files spread over the tree with the
// directories above them, or past the 6,300 base entries, every base entry
// and untrackedEntries node_modules files and directories in the overlay.
// With one change, the changed file is held. After each pass, the kernel
// looks up each inode the pass invalidated again, outside the timed pass.
// The unheld-overlay cases add the node_modules entries to the overlay
// without holding them, and the forgotten case maps one of them to an inode
// the kernel then forgets. It reports pass-ns/op and pass-cpu-ns/op, the wall
// and process CPU time of the pass alone, and the invalidations each pass
// sends. CPU time is steadier than wall time on a loaded machine.
func BenchmarkSwitchPass(b *testing.B) {
	for _, held := range []int{100, 2_000, 20_000} {
		for _, changed := range []int{1, 500} {
			b.Run(fmt.Sprintf("held=%d/changed=%d", held, changed), func(b *testing.B) {
				benchSwitchPassHeld(b, held, changed, false, false)
			})
		}
	}
	for _, c := range []struct {
		held      int
		forgotten bool
	}{{100, false}, {2_000, false}, {100, true}} {
		name := fmt.Sprintf("held=%d/changed=1/unheld-overlay", c.held)
		if c.forgotten {
			name += "/1-forgotten"
		}
		b.Run(name, func(b *testing.B) {
			benchSwitchPassHeld(b, c.held, 1, true, c.forgotten)
		})
	}
}

func benchSwitchPassHeld(b *testing.B, held, changed int, unheldOverlay, forgotten bool) {
	ctx := context.Background()
	base, next, _, files := switchPassTree(changed)
	snap, err := snapshot.New(ctx, filepath.Join(b.TempDir(), "snapshot.sqlite"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = snap.Close() })
	first, err := snap.PublishGeneration(ctx, "first", "main", base)
	if err != nil {
		b.Fatal(err)
	}
	dir := b.TempDir()
	ov, err := overlaystore.New(ctx, model.RepoPaths{ID: "bench", OverlayDir: dir, OverlayDBPath: filepath.Join(dir, "overlay.sqlite")})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = ov.Close() })
	var paths []string
	seen := map[string]bool{".": true}
	// hold adds p and each directory above it the kernel does not yet hold,
	// the directories first.
	hold := func(p string) {
		var chain []string
		for q := p; !seen[q]; q = filepath.Dir(q) {
			seen[q] = true
			chain = append(chain, q)
		}
		slices.Reverse(chain)
		paths = append(paths, chain...)
	}
	// untracked adds untrackedEntries node_modules entries to the overlay,
	// and holds them when inKernel is true.
	untracked := func(inKernel bool) {
		made := map[string]bool{}
		mkdir := func(d string) {
			if made[d] {
				return
			}
			made[d] = true
			if err := ov.Mkdir(ctx, d, 0o755); err != nil {
				b.Fatal(err)
			}
			if inKernel {
				hold(d)
			}
		}
		for i := 0; len(made) < untrackedEntries; i++ {
			pkg := fmt.Sprintf("node_modules/pkg-%03d", i%500)
			for _, d := range []string{"node_modules", pkg, pkg + "/lib"} {
				mkdir(d)
			}
			f := fmt.Sprintf("%s/lib/file-%05d.js", pkg, i)
			if _, err := ov.CreateFile(ctx, f, 0o644); err != nil {
				b.Fatal(err)
			}
			made[f] = true
			if inKernel {
				hold(f)
			}
		}
	}
	if held < len(base) {
		stride := len(files) / held
		for i := 0; len(paths) < held; i += stride {
			hold(files[i])
		}
		// The last file held may bring directories past held; the
		// deepest go first, and no held path lies below them.
		paths = paths[:held]
		if unheldOverlay {
			untracked(false)
		}
	} else {
		for _, n := range base {
			hold(n.Path)
		}
		untracked(true)
	}
	r := &Resolver{Snapshot: snap, Overlay: ov}
	r.SetGeneration(first)
	fs := NewArtifactFuse(model.RepoPaths{}, &Engine{Resolver: r, Overlay: ov})
	notifier := &countingNotifier{}
	fs.setNotifier(notifier)
	fs.mu.Lock()
	for _, p := range paths {
		attrs, err := fs.engine.Attrs(nil, p)
		if err != nil {
			b.Fatal(err)
		}
		fs.allocInode(p, attrs)
	}
	var forget fuseops.InodeID
	if forgotten {
		forget = fs.allocInode("node_modules/pkg-000/lib/file-00000.js", Attrs{Type: "file", Mode: 0o644}).ID
	}
	fs.mu.Unlock()
	if forgotten {
		if err := fs.ForgetInode(ctx, &fuseops.ForgetInodeOp{Inode: forget, N: 1}); err != nil {
			b.Fatal(err)
		}
	}
	variants := [2][]model.BaseNode{next, base}
	var pass, passCPU time.Duration
	b.ResetTimer()
	for i := range b.N {
		gen, err := snap.PublishGeneration(ctx, "head", "main", variants[i%2])
		if err != nil {
			b.Fatal(err)
		}
		if err := snap.PruneGenerations(ctx, gen-1); err != nil {
			b.Fatal(err)
		}
		// The overlay holds only entries neither generation has, so a
		// reconcile would change nothing.
		r.SetGeneration(gen)
		start, startCPU := time.Now(), cpuTime(b)
		if err := fs.invalidateChanged(true); err != nil {
			b.Fatal(err)
		}
		pass, passCPU = pass+time.Since(start), passCPU+cpuTime(b)-startCPU
		// A kernel looks up an invalidated inode again before it uses it.
		for _, id := range notifier.invalidated {
			fs.mu.RLock()
			path := fs.inodes[id].Path
			fs.mu.RUnlock()
			attrs, err := fs.engine.Attrs(nil, path)
			if err != nil {
				b.Fatal(err)
			}
			if err := fs.setReported(id, attrs); err != nil {
				b.Fatal(err)
			}
		}
		notifier.invalidated = notifier.invalidated[:0]
	}
	b.StopTimer()
	b.ReportMetric(float64(len(paths)), "held")
	b.ReportMetric(float64(pass.Nanoseconds())/float64(b.N), "pass-ns/op")
	b.ReportMetric(float64(passCPU.Nanoseconds())/float64(b.N), "pass-cpu-ns/op")
	b.ReportMetric(float64(notifier.inodes)/float64(b.N), "inode-invals/op")
	b.ReportMetric(float64(notifier.entries)/float64(b.N), "entry-invals/op")
}
