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
	if err := blobcache.Install(dir, oid, staged); err != nil {
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

// --older-than evicts every blob last read before the cutoff, and a byte
// budget beside it evicts the least recently read blobs left until the cache
// fits.
func TestEvictCacheCLIEvictsBlobsReadBeforeTheCutoff(t *testing.T) {
	old, middle, recent := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	for _, tt := range []struct {
		name string
		args []string
		kept map[string]bool
		want string
	}{
		{"cutoff", []string{"--older-than", "48h"}, map[string]bool{old: false, middle: true, recent: true}, "evicted_blobs=1 evicted_bytes=10 hydrated_blobs=2 hydrated_bytes=20\n"},
		{"cutoff_and_budget", []string{"--older-than", "48h", "--max-bytes", "10"}, map[string]bool{old: false, middle: false, recent: true}, "evicted_blobs=2 evicted_bytes=20 hydrated_blobs=1 hydrated_bytes=10\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := registerRepoWithCache(t)
			now := time.Now()
			installBlob(t, dir, old, "0123456789", now.Add(-72*time.Hour))
			installBlob(t, dir, middle, "0123456789", now.Add(-30*time.Hour))
			installBlob(t, dir, recent, "0123456789", now.Add(-time.Hour))

			stdout, stderr, code := runCLI(t, append([]string{"evict-cache", "--name", "repo"}, tt.args...)...)
			if code != 0 {
				t.Fatalf("evict-cache exit = %d, stderr=%q", code, stderr)
			}
			if stdout != tt.want {
				t.Fatalf("stdout = %q, want %q", stdout, tt.want)
			}
			for oid, kept := range tt.kept {
				if _, err := os.Stat(blobcache.Path(dir, oid)); (err == nil) != kept {
					t.Fatalf("blob %s present = %v, want %v", oid[:2], err == nil, kept)
				}
			}
		})
	}
}

func TestEvictCacheCLIRequiresMaxBytesOrOlderThan(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{"neither", nil, "--max-bytes or --older-than required"},
		{"zero_cutoff", []string{"--older-than", "0s"}, "--older-than must be positive"},
		{"negative_cutoff", []string{"--older-than=-1h"}, "--older-than must be positive"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := registerRepoWithCache(t, strings.Repeat("a", 40))
			stdout, stderr, code := runCLI(t, append([]string{"evict-cache", "--name", "repo"}, tt.args...)...)
			if code == 0 {
				t.Fatalf("evict-cache %v exit = 0, stdout=%q", tt.args, stdout)
			}
			if !strings.Contains(stderr, tt.want) {
				t.Fatalf("stderr = %q, want %q", stderr, tt.want)
			}
			if _, err := os.Stat(blobcache.Path(dir, strings.Repeat("a", 40))); err != nil {
				t.Fatalf("blob after a refused evict-cache: %v", err)
			}
		})
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

// Without --max-bytes there is no byte budget, so the help shows no default
// for it: a default of 0 reads as the budget that empties the cache.
func TestEvictCacheCLIHelpShowsNoFlagDefaults(t *testing.T) {
	stdout, stderr, code := runCLI(t, "evict-cache", "--help")
	if code != 0 || !strings.Contains(stdout, "--older-than") {
		t.Fatalf("evict-cache --help exit = %d, stdout=%q, stderr=%q; want the help", code, stdout, stderr)
	}
	if strings.Contains(stdout, "default:") {
		t.Fatalf("evict-cache --help = %q, want no flag defaults", stdout)
	}
}
