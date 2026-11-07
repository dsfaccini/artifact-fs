package fusefs

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/model"
	overlaystore "github.com/cloudflare/artifact-fs/internal/overlay"
	"github.com/cloudflare/artifact-fs/internal/snapshot"
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
			if dirty, err := ov.DirtyCount(ctx); err != nil || dirty != 0 {
				b.Fatalf("overlay after the switch holds %d entries, %v; want none", dirty, err)
			}
			_ = ov.Close()
			b.StartTimer()
		}
		reportSwitch(b, cpu, timer)
	})
}
