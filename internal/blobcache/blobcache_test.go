package blobcache

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// install adds a blob of size bytes for oid, last accessed at accessed.
func install(t *testing.T, dir, oid string, size int, accessed time.Time) string {
	t.Helper()
	staged, err := Stage(dir, oid)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, []byte(strings.Repeat("x", size)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Install(dir, oid, staged); err != nil {
		t.Fatal(err)
	}
	path := Path(dir, oid)
	if err := os.Chtimes(path, accessed, accessed); err != nil {
		t.Fatal(err)
	}
	return path
}

func requireStats(t *testing.T, dir string, want Stats) {
	t.Helper()
	if got, err := ReadStats(dir); err != nil || got != want {
		t.Fatalf("stats = %+v, %v; want %+v", got, err, want)
	}
}

func TestEvictCountsBlobsRemovedBeforeAFailedRemove(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	old := time.Now().Add(-time.Hour)
	install(t, dir, strings.Repeat("a", 40), 10, old)
	locked := install(t, dir, strings.Repeat("b", 40), 10, old.Add(time.Minute))
	if err := os.Chmod(filepath.Dir(locked), 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(filepath.Dir(locked), 0o700)

	if _, err := Evict(context.Background(), dir, 0); err == nil {
		t.Fatal("Evict succeeded, want the read-only shard's remove error")
	}
	requireStats(t, dir, Stats{Blobs: 1, Bytes: 10})
}

func TestEvictRecountsBeforeEvicting(t *testing.T) {
	dir := t.TempDir()
	install(t, dir, strings.Repeat("a", 40), 10, time.Now())
	uncounted := Path(dir, strings.Repeat("b", 40))
	if err := os.MkdirAll(filepath.Dir(uncounted), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(uncounted, []byte(strings.Repeat("x", 1000)), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(uncounted, old, old); err != nil {
		t.Fatal(err)
	}

	res, err := Evict(context.Background(), dir, 100)
	if err != nil {
		t.Fatal(err)
	}
	if res.Evicted != 1 || res.EvictedBytes != 1000 {
		t.Fatalf("evicted %d blobs, %d bytes; want the uncounted 1000-byte blob", res.Evicted, res.EvictedBytes)
	}
	requireStats(t, dir, Stats{Blobs: 1, Bytes: 10})
}

func TestEvictStopsWhenContextEnds(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-time.Hour)
	install(t, dir, strings.Repeat("a", 40), 10, old)
	install(t, dir, strings.Repeat("b", 40), 10, old.Add(time.Minute))
	ctx, cancel := context.WithCancel(context.Background())
	testHookBeforeEvictBatch = cancel
	defer func() { testHookBeforeEvictBatch = nil }()

	res, err := Evict(ctx, dir, 0)
	if !errors.Is(err, context.Canceled) || res.Evicted != 0 {
		t.Fatalf("Evict = %+v, %v; want nothing evicted and context.Canceled", res, err)
	}
	requireStats(t, dir, Stats{Blobs: 2, Bytes: 20})
}

func TestEvictKeepsBlobChangedAfterListing(t *testing.T) {
	for name, change := range map[string]func(t *testing.T, dir, oid, path string){
		"read": func(t *testing.T, _, _, path string) {
			now := time.Now()
			if err := os.Chtimes(path, now, now); err != nil {
				t.Fatal(err)
			}
		},
		"hydrated again": func(t *testing.T, dir, oid, path string) {
			st, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			install(t, dir, oid, 10, accessTime(st))
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			oldest := strings.Repeat("a", 40)
			old := time.Now().Add(-time.Hour)
			oldestPath := install(t, dir, oldest, 10, old)
			install(t, dir, strings.Repeat("b", 40), 10, old.Add(time.Minute))
			testHookBeforeEvictBatch = func() { change(t, dir, oldest, oldestPath) }
			defer func() { testHookBeforeEvictBatch = nil }()

			res, err := Evict(context.Background(), dir, 10)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(oldestPath); err != nil || res.Evicted != 1 {
				t.Fatalf("Evict = %+v; oldest blob stat err %v; want the changed oldest blob kept and the other evicted", res, err)
			}
			requireStats(t, dir, Stats{Blobs: 1, Bytes: 10})
		})
	}
}

// cancelOnCall is a context whose Err reports nil until its nth call, then
// context.Canceled, so cancellation lands partway through a walk.
type cancelOnCall struct {
	context.Context
	calls, n int
}

func (c *cancelOnCall) Err() error {
	c.calls++
	if c.calls >= c.n {
		return context.Canceled
	}
	return nil
}

func TestEvictLeavesCountersWhenContextEndsDuringRecount(t *testing.T) {
	dir := t.TempDir()
	install(t, dir, strings.Repeat("a", 40), 10, time.Now())
	// An uncounted blob in a second shard makes a finished recount visible in
	// the counters.
	uncounted := Path(dir, strings.Repeat("b", 40))
	if err := os.MkdirAll(filepath.Dir(uncounted), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(uncounted, []byte(strings.Repeat("x", 1000)), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := &cancelOnCall{Context: context.Background(), n: 2}

	res, err := Evict(ctx, dir, 0)
	if !errors.Is(err, context.Canceled) || res.Evicted != 0 {
		t.Fatalf("Evict = %+v, %v; want nothing evicted and context.Canceled", res, err)
	}
	requireStats(t, dir, Stats{Blobs: 1, Bytes: 10})
}

func TestEvictStopsMovingFlatBlobsWhenContextEnds(t *testing.T) {
	dir := t.TempDir()
	first, second := strings.Repeat("a", 40), strings.Repeat("b", 40)
	for _, oid := range []string{first, second} {
		if err := os.WriteFile(filepath.Join(dir, oid), []byte(oid), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx := &cancelOnCall{Context: context.Background(), n: 2}

	if _, err := Evict(ctx, dir, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("Evict err = %v, want context.Canceled", err)
	}
	if data, err := os.ReadFile(Path(dir, first)); err != nil || string(data) != first {
		t.Fatalf("moved blob = %q, %v; want its content in its shard", data, err)
	}
	if _, err := os.Stat(filepath.Join(dir, second)); err != nil {
		t.Fatalf("second flat blob: %v; want it left in place", err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, statsName)); err != nil || len(data) >= statsSize {
		t.Fatalf("stats file = %d bytes, %v; want no counters written", len(data), err)
	}
}

// Installing a blob over a cached copy replaces the copy in the counters.
func TestInstallOverACachedBlobCountsItOnce(t *testing.T) {
	dir := t.TempDir()
	oid := strings.Repeat("a", 40)
	now := time.Now()
	install(t, dir, oid, 10, now)
	requireStats(t, dir, Stats{Blobs: 1, Bytes: 10})

	install(t, dir, oid, 4, now)
	requireStats(t, dir, Stats{Blobs: 1, Bytes: 4})
}
