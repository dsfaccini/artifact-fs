package gitstore

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/gittest"
	"github.com/cloudflare/artifact-fs/internal/model"
)

func TestBlobToCacheBinarySafe(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	run(t, "git", "init", repo)
	// Write a file ending with a newline (should be preserved)
	os.WriteFile(filepath.Join(repo, "file.txt"), []byte("line\n"), 0o644)
	run(t, "git", "-C", repo, "add", "file.txt")
	run(t, "git", "-C", repo, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "init")

	cfg := model.RepoConfig{ID: "x", GitDir: filepath.Join(repo, ".git"), BlobCacheDir: filepath.Join(tmp, "cache")}
	store := New(nil)
	ctx := context.Background()
	oid, _, _ := store.ResolveHEAD(ctx, cfg)
	nodes, _ := store.BuildTreeIndex(ctx, cfg, oid)
	var blobOID string
	for _, n := range nodes {
		if n.Path == "file.txt" {
			blobOID = n.ObjectOID
		}
	}
	if blobOID == "" {
		t.Fatal("no blob OID found")
	}
	dst := filepath.Join(tmp, "cache", blobOID)
	size, err := store.BlobToCache(ctx, cfg, blobOID, dst)
	if err != nil {
		t.Fatalf("BlobToCache: %v", err)
	}
	if size != 5 {
		t.Fatalf("expected size 5 (line\\n), got %d", size)
	}
	data, _ := os.ReadFile(dst)
	if string(data) != "line\n" {
		t.Fatalf("expected 'line\\n', got %q", data)
	}
}

func TestFetchMissingBlobsRequestsOnlyMissingObjects(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	files := map[string]string{"a.txt": "a\n", "b.txt": "b\n", "c.txt": "c\n", "d.txt": "d\n"}
	cfg := model.RepoConfig{ID: "x", Name: "x", GitDir: filepath.Join(tmp, "repo.git"), RemoteURL: gittest.PartialCloneRemote(t, files), Branch: "main"}
	store := New(nil)
	defer store.Close()
	ctx := context.Background()
	if err := store.CloneBlobless(ctx, cfg); err != nil {
		t.Fatalf("CloneBlobless: %v", err)
	}
	var oids []string
	for _, name := range []string{"a.txt", "b.txt", "c.txt", "d.txt"} {
		oids = append(oids, strings.TrimSpace(runOutput(t, "git", "--git-dir", cfg.GitDir, "rev-parse", "HEAD:"+name)))
	}
	if err := store.FetchMissingBlobs(ctx, cfg, oids[:2]); err != nil {
		t.Fatalf("FetchMissingBlobs(a, b): %v", err)
	}
	packed := packedObjects(t, cfg.GitDir)

	if err := store.FetchMissingBlobs(ctx, cfg, oids); err != nil {
		t.Fatalf("FetchMissingBlobs(a, b, c, d): %v", err)
	}
	// A fetch that also asked for a and b would download them again.
	if got := packedObjects(t, cfg.GitDir); got != packed+2 {
		t.Fatalf("packed objects = %d, want %d: only c and d were missing", got, packed+2)
	}
	out := runOutput(t, "env", "GIT_NO_LAZY_FETCH=1", "git", "--git-dir", cfg.GitDir, "cat-file", "--batch-check", "--batch-all-objects")
	for _, oid := range oids {
		if !strings.Contains(out, oid+" blob") {
			t.Fatalf("blob %s missing after FetchMissingBlobs:\n%s", oid, out)
		}
	}
}

// packedObjects counts the objects in gitDir's packs, a duplicate once per pack.
func packedObjects(t *testing.T, gitDir string) int {
	t.Helper()
	out := runOutput(t, "git", "--git-dir", gitDir, "count-objects", "-v")
	for line := range strings.Lines(out) {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), "in-pack: "); ok {
			n, err := strconv.Atoi(value)
			if err != nil {
				t.Fatal(err)
			}
			return n
		}
	}
	t.Fatalf("count-objects printed no in-pack count:\n%s", out)
	return 0
}

func TestVerifyBlobHashesWithoutGit(t *testing.T) {
	for _, objectFormat := range []string{"sha1", "sha256"} {
		t.Run(objectFormat, func(t *testing.T) {
			tmp := t.TempDir()
			repo := filepath.Join(tmp, "repo")
			run(t, "git", "init", "--object-format="+objectFormat, repo)
			cachePath := filepath.Join(tmp, "blob")
			if err := os.WriteFile(cachePath, []byte("cached\r\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			oid := strings.TrimSpace(runOutput(t, "git", "-C", repo, "hash-object", "-w", "--no-filters", cachePath))
			cfg := model.RepoConfig{ID: "x", GitDir: filepath.Join(repo, ".git")}
			trace := filepath.Join(tmp, "git-trace")
			t.Setenv("GIT_TRACE", trace)
			store := New(nil)
			defer store.Close()

			ok, err := store.VerifyBlob(context.Background(), cfg, oid, cachePath)
			if err != nil || !ok {
				t.Fatalf("VerifyBlob(matching) = %v, %v; want true", ok, err)
			}
			if err := os.WriteFile(cachePath, []byte("changed\r\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			ok, err = store.VerifyBlob(context.Background(), cfg, oid, cachePath)
			if err != nil || ok {
				t.Fatalf("VerifyBlob(changed) = %v, %v; want false", ok, err)
			}
			if out, err := os.ReadFile(trace); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("VerifyBlob ran git: %s", out)
			}
		})
	}
}

func TestBlobToCacheHonorsCanceledContext(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	run(t, "git", "init", repo)
	os.WriteFile(filepath.Join(repo, "file.txt"), []byte("line\n"), 0o644)
	run(t, "git", "-C", repo, "add", "file.txt")
	run(t, "git", "-C", repo, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "init")

	cfg := model.RepoConfig{ID: "x", GitDir: filepath.Join(repo, ".git"), BlobCacheDir: filepath.Join(tmp, "cache")}
	store := New(nil)
	oid, _, err := store.ResolveHEAD(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := store.BuildTreeIndex(context.Background(), cfg, oid)
	if err != nil {
		t.Fatal(err)
	}
	var blobOID string
	for _, n := range nodes {
		if n.Path == "file.txt" {
			blobOID = n.ObjectOID
		}
	}
	if blobOID == "" {
		t.Fatal("no blob OID found")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dst := filepath.Join(tmp, "cache", blobOID)
	_, err = store.BlobToCache(ctx, cfg, blobOID, dst)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(dst); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cache file should not be written after cancellation: %v", err)
	}
}

func TestReadBlobRespectsMaxBytes(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	run(t, "git", "init", repo)
	os.WriteFile(filepath.Join(repo, "file.txt"), []byte("line\n"), 0o644)
	run(t, "git", "-C", repo, "add", "file.txt")
	run(t, "git", "-C", repo, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "init")

	cfg := model.RepoConfig{ID: "x", GitDir: filepath.Join(repo, ".git")}
	store := New(nil)
	ctx := context.Background()
	oid, _, _ := store.ResolveHEAD(ctx, cfg)
	nodes, _ := store.BuildTreeIndex(ctx, cfg, oid)
	var blobOID string
	for _, n := range nodes {
		if n.Path == "file.txt" {
			blobOID = n.ObjectOID
		}
	}
	if blobOID == "" {
		t.Fatal("no blob OID found")
	}

	data, err := store.ReadBlob(ctx, cfg, blobOID, 5)
	if err != nil {
		t.Fatalf("ReadBlob at limit: %v", err)
	}
	if string(data) != "line\n" {
		t.Fatalf("data = %q, want line\\n", data)
	}
	_, err = store.ReadBlob(ctx, cfg, blobOID, 4)
	if !errors.Is(err, model.ErrBlobTooLarge) {
		t.Fatalf("err = %v, want ErrBlobTooLarge", err)
	}
	data, err = store.ReadBlob(ctx, cfg, blobOID, 5)
	if err != nil {
		t.Fatalf("ReadBlob after oversized read: %v", err)
	}
	if string(data) != "line\n" {
		t.Fatalf("data after oversized read = %q, want line\\n", data)
	}
}

func TestBatchPoolBoundsConcurrentProcesses(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	run(t, "git", "init", repo)
	p := &batchPool{
		gitDir:  filepath.Join(repo, ".git"),
		logger:  slog.Default(),
		maxSize: 1,
		all:     map[*batchCatFile]struct{}{},
		changed: make(chan struct{}),
	}
	first, err := p.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := p.acquire(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second acquire err = %v, want deadline exceeded", err)
	}
	p.release(first)
	second, err := p.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p.release(second)
	p.closeAll()
}
