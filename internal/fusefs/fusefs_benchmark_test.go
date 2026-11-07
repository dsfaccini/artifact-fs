package fusefs

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
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

func (o *lockTimer) ReconcileChecked(ctx context.Context, baseLookup func(string) (model.BaseNode, bool, error), lock sync.Locker, switchBase func()) error {
	var wallStart time.Time
	var cpuStart time.Duration
	return o.OverlayStore.ReconcileChecked(ctx, baseLookup, observedLock{
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
	if err := r.SwitchGeneration(context.Background(), gen, 0); err != nil {
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
		ov, err := overlaystore.New(ctx, model.RepoConfig{ID: "bench", OverlayDir: dir, OverlayDBPath: filepath.Join(dir, "overlay.sqlite")})
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
			if dirty, err := ov.DirtyCount(ctx); err != nil || dirty != 0 {
				b.Fatalf("overlay after the switch holds %d entries, %v; want none", dirty, err)
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
				fs := NewArtifactFuse(model.RepoConfig{}, nil)
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
	ov, err := overlaystore.New(ctx, model.RepoConfig{ID: "bench", OverlayDir: dir, OverlayDBPath: filepath.Join(dir, "overlay.sqlite")})
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
	fs := NewArtifactFuse(model.RepoConfig{}, &Engine{Resolver: r, Overlay: ov})
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
