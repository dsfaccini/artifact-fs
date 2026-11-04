package fusefs

import (
	"context"
	"fmt"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/model"
	overlaystore "github.com/cloudflare/artifact-fs/internal/overlay"
	"github.com/cloudflare/artifact-fs/internal/snapshot"
)

// applyTimer adds up how long each reconcile spends applying, which is how
// long SwitchGeneration holds the transition write lock.
type applyTimer struct {
	model.OverlayStore
	applying time.Duration
}

func (o *applyTimer) ReconcileChecked(ctx context.Context, baseLookup func(string) (model.BaseNode, bool, error), swap func(apply func() error) error) error {
	return o.OverlayStore.ReconcileChecked(ctx, baseLookup, func(apply func() error) error {
		return swap(func() error {
			start := time.Now()
			err := apply()
			o.applying += time.Since(start)
			return err
		})
	})
}

func cpuTime(b *testing.B) time.Duration {
	b.Helper()
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		b.Fatal(err)
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

// BenchmarkSwitchGeneration20kOverlayEntries switches a 100k-file snapshot
// with 20k overlay creates the snapshot lacks, so each switch looks up every
// entry and keeps it. lock-ns/op is how long readers wait. cpu-ns/op is the
// process CPU time, which a loaded machine distorts less than wall time.
func BenchmarkSwitchGeneration20kOverlayEntries(b *testing.B) {
	ctx := context.Background()
	dir := b.TempDir()
	snap, err := snapshot.New(ctx, filepath.Join(dir, "snapshot.sqlite"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = snap.Close() })
	nodes := []model.BaseNode{{Path: ".", Type: "dir", Mode: 0o40000, SizeState: "known"}}
	for d := range 1000 {
		nodes = append(nodes, model.BaseNode{Path: fmt.Sprintf("pkg%d", d), Type: "dir", Mode: 0o40000, SizeState: "known"})
		for f := range 100 {
			nodes = append(nodes, model.BaseNode{Path: fmt.Sprintf("pkg%d/file%d.go", d, f), Type: "file", Mode: 0o100644, ObjectOID: fmt.Sprintf("%040x", d*100+f), SizeState: "known", SizeBytes: 10})
		}
	}
	gen, err := snap.PublishGeneration(ctx, "head", "main", nodes)
	if err != nil {
		b.Fatal(err)
	}
	ov, err := overlaystore.New(ctx, model.RepoConfig{ID: "bench", OverlayDir: dir, OverlayDBPath: filepath.Join(dir, "overlay.sqlite")})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = ov.Close() })
	for i := range 20_000 {
		if _, err := ov.CreateFile(ctx, fmt.Sprintf("node_modules/pkg%d/lib/f%d.js", i%500, i), 0o644); err != nil {
			b.Fatal(err)
		}
	}
	timer := &applyTimer{OverlayStore: ov}
	r := &Resolver{Snapshot: snap, Overlay: timer}
	r.SetGeneration(gen)
	b.ResetTimer()
	cpuStart := cpuTime(b)
	for range b.N {
		if err := r.SwitchGeneration(ctx, gen, 0); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(timer.applying.Nanoseconds())/float64(b.N), "lock-ns/op")
	b.ReportMetric(float64((cpuTime(b)-cpuStart).Nanoseconds())/float64(b.N), "cpu-ns/op")
}
