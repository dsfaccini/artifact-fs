package hydrator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/blobcache"
	"github.com/cloudflare/artifact-fs/internal/model"
)

func TestOpenHydratedRehydratesBlobEvictedAfterValidation(t *testing.T) {
	cfg := model.RepoPaths{ID: "repo", BlobCacheDir: t.TempDir()}
	node := model.BaseNode{RepoID: cfg.ID, Path: "a.txt", ObjectOID: strings.Repeat("ab", 20), SizeState: "known", SizeBytes: 7}
	fetcher := &fakeBlobFetcher{payload: []byte("content"), verifyOK: true}
	h := New(fetcher)
	h.Start(1, cfg)
	defer h.Stop()
	ctx := context.Background()

	held, _, err := h.OpenHydrated(ctx, cfg, node)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	evicted := false
	testHookBeforeOpen = func(string) {
		if evicted {
			return
		}
		evicted = true
		if res, err := blobcache.Evict(context.Background(), cfg.BlobCacheDir, 0); err != nil || res.Evicted != 1 {
			t.Errorf("evict = %+v, %v; want one blob evicted", res, err)
		}
	}
	defer func() { testHookBeforeOpen = nil }()

	reopened, size, err := h.OpenHydrated(ctx, cfg, node)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if !evicted {
		t.Fatal("hook did not evict the blob")
	}
	if data, err := io.ReadAll(reopened); err != nil || string(data) != "content" || size != 7 {
		t.Fatalf("reopened data = %q, size %d, err %v; want content, 7", data, size, err)
	}
	if fetcher.calls != 2 {
		t.Fatalf("BlobToCache calls = %d, want 2: the evicted blob hydrates again", fetcher.calls)
	}
	if data, err := io.ReadAll(held); err != nil || string(data) != "content" {
		t.Fatalf("handle opened before eviction read %q, %v; want content", data, err)
	}
	if st, err := blobcache.ReadStats(cfg.BlobCacheDir); err != nil || st != (blobcache.Stats{Blobs: 1, Bytes: 7}) {
		t.Fatalf("cache stats = %+v, %v; want 1 blob, 7 bytes", st, err)
	}
}

func TestStartMovesFlatCacheBlobsIntoShards(t *testing.T) {
	cfg := model.RepoPaths{ID: "repo", BlobCacheDir: t.TempDir()}
	node := model.BaseNode{RepoID: cfg.ID, Path: "a.txt", ObjectOID: strings.Repeat("cd", 20), SizeState: "known", SizeBytes: 7}
	if err := os.WriteFile(filepath.Join(cfg.BlobCacheDir, node.ObjectOID), []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	fetcher := &fakeBlobFetcher{payload: []byte("refetch"), verifyOK: true}
	h := New(fetcher)
	h.Start(1, cfg)
	defer h.Stop()

	path, size, err := h.EnsureHydrated(context.Background(), cfg, node)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(cfg.BlobCacheDir, node.ObjectOID[:2], node.ObjectOID)
	if path != want || size != 7 {
		t.Fatalf("EnsureHydrated = %q, %d; want %q, 7", path, size, want)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "content" {
		t.Fatalf("cached blob = %q, %v; want the flat blob's content", data, err)
	}
	if fetcher.calls != 0 {
		t.Fatalf("BlobToCache calls = %d, want 0: the flat blob is reused", fetcher.calls)
	}
	if st, err := blobcache.ReadStats(cfg.BlobCacheDir); err != nil || st != (blobcache.Stats{Blobs: 1, Bytes: 7}) {
		t.Fatalf("cache stats = %+v, %v; want 1 blob, 7 bytes", st, err)
	}
}

func TestVerifiedCacheFilesStayBounded(t *testing.T) {
	cfg := model.RepoPaths{ID: "repo", BlobCacheDir: t.TempDir()}
	h := New(&fakeBlobFetcher{payload: []byte("content"), verifyOK: true})
	h.maxVerified = 2
	h.Start(1, cfg)
	defer h.Stop()
	for i := range 5 {
		node := model.BaseNode{RepoID: cfg.ID, ObjectOID: strings.Repeat(string(rune('a'+i)), 40), SizeState: "known", SizeBytes: 7}
		if _, _, err := h.EnsureHydrated(context.Background(), cfg, node); err != nil {
			t.Fatal(err)
		}
	}
	h.mu.Lock()
	n := len(h.verified)
	h.mu.Unlock()
	if n > 2 {
		t.Fatalf("verified entries = %d, want at most 2", n)
	}
}

func TestFailedBlobWriteNeverReachesTheCachePath(t *testing.T) {
	cfg := model.RepoPaths{ID: "repo", BlobCacheDir: t.TempDir()}
	node := model.BaseNode{RepoID: cfg.ID, Path: "a.txt", ObjectOID: strings.Repeat("ef", 20), SizeState: "known", SizeBytes: 7}
	h := New(&fakeBlobFetcher{payload: []byte("partial"), failAfterWrite: errors.New("short read")})
	h.Start(1, cfg)
	defer h.Stop()

	if _, _, err := h.EnsureHydrated(context.Background(), cfg, node); err == nil {
		t.Fatal("EnsureHydrated succeeded after a failed write")
	}
	if _, err := os.Stat(blobcache.Path(cfg.BlobCacheDir, node.ObjectOID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cache path stat err = %v, want not exist", err)
	}
	if st, err := blobcache.ReadStats(cfg.BlobCacheDir); err != nil || st != (blobcache.Stats{}) {
		t.Fatalf("cache stats = %+v, %v; want none", st, err)
	}
}

// One worker copies maxInstallBatch+2 prefetch blobs and pauses in the last
// copy. By then the first maxInstallBatch blobs are installed as one full
// batch, and the next blob waits staged until a read of it installs it.
func TestStagedBlobsInstallAtAFullBatchOrARead(t *testing.T) {
	cfg := model.RepoPaths{ID: "repo", BlobCacheDir: t.TempDir()}
	payload := []byte("content")
	tasks := make([]model.HydrationTask, maxInstallBatch+2)
	now := time.Now()
	for i := range tasks {
		tasks[i] = model.HydrationTask{
			RepoID:     cfg.ID,
			Path:       fmt.Sprintf("dir/file-%03d.txt", i),
			ObjectOID:  fmt.Sprintf("%040x", i+1),
			SizeState:  "known",
			SizeBytes:  int64(len(payload)),
			Priority:   PriorityLikelyText,
			Reason:     "prefetch",
			EnqueuedAt: now.Add(time.Duration(i)),
		}
	}
	last := tasks[len(tasks)-1].ObjectOID
	copying := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	h := New(&fakeBlobFetcher{payload: payload, blobToCache: func(ctx context.Context, oid string) error {
		if oid != last {
			return nil
		}
		close(copying)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}})
	h.EnqueueBatch(tasks)
	h.Start(1, cfg)
	defer h.Stop()
	<-copying

	full := blobcache.Stats{Blobs: maxInstallBatch, Bytes: maxInstallBatch * int64(len(payload))}
	if st, err := blobcache.ReadStats(cfg.BlobCacheDir); err != nil || st != full {
		t.Fatalf("cache stats = %+v, %v; want the full batch, %+v", st, err, full)
	}
	next := tasks[maxInstallBatch]
	if _, err := os.Stat(blobcache.Path(cfg.BlobCacheDir, next.ObjectOID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("blob after the full batch: stat err %v; want it staged, not installed", err)
	}
	path, size, err := h.EnsureHydrated(context.Background(), cfg, taskBaseNode(next))
	if err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "content" || size != 7 {
		t.Fatalf("read of the staged blob = %q, size %d, err %v; want content, 7", data, size, err)
	}
	want := blobcache.Stats{Blobs: full.Blobs + 1, Bytes: full.Bytes + 7}
	if st, err := blobcache.ReadStats(cfg.BlobCacheDir); err != nil || st != want {
		t.Fatalf("cache stats after the read = %+v, %v; want %+v", st, err, want)
	}
}
