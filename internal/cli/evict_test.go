package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/blobcache"
	"github.com/cloudflare/artifact-fs/internal/registry"
)

// registerRepoWithCache registers repo and writes one 10-byte blob per oid
// into its blob cache, accessed in the order given. It returns the cache dir.
func registerRepoWithCache(t *testing.T, oids ...string) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("ARTIFACT_FS_ROOT", root)
	ctx := context.Background()
	var stdout, stderr bytes.Buffer
	if code := Run(ctx, []string{"add-repo", "--name", "repo", "--remote", "https://github.com/example/repo.git", "--async"}, &stdout, &stderr); code != 0 {
		t.Fatalf("add-repo exit = %d, stderr=%q", code, stderr.String())
	}
	reg, err := registry.New(ctx, filepath.Join(root, "config", "repos.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := reg.GetRepo(ctx, "repo")
	if closeErr := reg.Close(); err != nil || closeErr != nil {
		t.Fatalf("get repo: %v, close: %v", err, closeErr)
	}
	accessed := time.Now().Add(-time.Hour)
	for _, oid := range oids {
		accessed = accessed.Add(time.Minute)
		installBlob(t, cfg.BlobCacheDir, oid, "0123456789", accessed)
	}
	return cfg.BlobCacheDir
}

// installBlob installs content as oid's blob in dir, last accessed at accessed.
func installBlob(t *testing.T, dir, oid, content string, accessed time.Time) {
	t.Helper()
	staged, err := blobcache.Stage(dir, oid)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := blobcache.Install(dir, []blobcache.Staged{{OID: oid, Path: staged}}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(blobcache.Path(dir, oid), accessed, accessed); err != nil {
		t.Fatal(err)
	}
}

func runCLI(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), args, &stdout, &stderr)
	return stdout.String(), stderr.String(), code
}

func TestEvictCacheCLIEvictsLeastRecentlyAccessedBlobs(t *testing.T) {
	oldest, middle, newest := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	dir := registerRepoWithCache(t, oldest, middle, newest)

	stdout, stderr, code := runCLI(t, "evict-cache", "--name", "repo", "--max-bytes", "20")
	if code != 0 {
		t.Fatalf("evict-cache exit = %d, stderr=%q", code, stderr)
	}
	if want := "evicted_blobs=1 evicted_bytes=10 hydrated_blobs=2 hydrated_bytes=20\n"; stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
	for oid, kept := range map[string]bool{oldest: false, middle: true, newest: true} {
		if _, err := os.Stat(blobcache.Path(dir, oid)); (err == nil) != kept {
			t.Fatalf("blob %s present = %v, want %v", oid[:2], err == nil, kept)
		}
	}
	stdout, stderr, code = runCLI(t, "status", "--name", "repo")
	if code != 0 || !strings.Contains(stdout, " hydrated_blobs=2 hydrated_bytes=20 ") {
		t.Fatalf("status exit = %d, stdout=%q, stderr=%q; want 2 blobs, 20 bytes", code, stdout, stderr)
	}
}

// An empty file's blob holds no bytes, and --max-bytes 0 evicts it too.
func TestEvictCacheCLIZeroBudgetEmptiesTheCache(t *testing.T) {
	const emptyBlob = "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391"
	for _, tt := range []struct {
		name string
		oids []string
	}{
		{"empty_blob_accessed_last", []string{strings.Repeat("a", 40)}},
		{"only_an_empty_blob", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := registerRepoWithCache(t, tt.oids...)
			installBlob(t, dir, emptyBlob, "", time.Now())
			stdout, stderr, code := runCLI(t, "evict-cache", "--name", "repo", "--max-bytes", "0")
			if code != 0 {
				t.Fatalf("evict-cache exit = %d, stderr=%q", code, stderr)
			}
			if !strings.HasSuffix(stdout, " hydrated_blobs=0 hydrated_bytes=0\n") {
				t.Fatalf("stdout = %q, want an empty cache", stdout)
			}
			if _, err := os.Stat(blobcache.Path(dir, emptyBlob)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("empty blob after evict-cache --max-bytes 0: %v", err)
			}
		})
	}
}

func TestEvictCacheCLIRequiresMaxBytes(t *testing.T) {
	registerRepoWithCache(t, strings.Repeat("a", 40))
	stdout, stderr, code := runCLI(t, "evict-cache", "--name", "repo")
	if code == 0 {
		t.Fatalf("evict-cache without --max-bytes exit = 0, stdout=%q", stdout)
	}
	if !strings.Contains(stderr, "max-bytes") {
		t.Fatalf("stderr = %q, want it to name max-bytes", stderr)
	}
}

func TestStatusCLIReadsCacheCountersWithoutWalking(t *testing.T) {
	dir := registerRepoWithCache(t, strings.Repeat("a", 40))
	// A blob written behind the counters' back shows that status reads the
	// counters instead of walking the cache.
	unseen := blobcache.Path(dir, strings.Repeat("b", 40))
	if err := os.MkdirAll(filepath.Dir(unseen), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unseen, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code := runCLI(t, "status", "--name", "repo")
	if code != 0 || !strings.Contains(stdout, " hydrated_blobs=1 hydrated_bytes=10 ") {
		t.Fatalf("status exit = %d, stdout=%q, stderr=%q; want 1 blob, 10 bytes", code, stdout, stderr)
	}
}
