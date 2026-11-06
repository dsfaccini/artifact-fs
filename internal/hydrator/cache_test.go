package hydrator

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/blobcache"
	"github.com/cloudflare/artifact-fs/internal/model"
)

func TestOpenHydratedRehydratesBlobEvictedAfterValidation(t *testing.T) {
	cfg := model.RepoConfig{ID: "repo", BlobCacheDir: t.TempDir()}
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
		if res, err := blobcache.Evict(cfg.BlobCacheDir, 0); err != nil || res.Evicted != 1 {
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
	cfg := model.RepoConfig{ID: "repo", BlobCacheDir: t.TempDir()}
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
	cfg := model.RepoConfig{ID: "repo", BlobCacheDir: t.TempDir()}
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
