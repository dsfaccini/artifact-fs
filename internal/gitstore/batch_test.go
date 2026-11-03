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
	config := runOutput(t, "git", "--git-dir", cfg.GitDir, "config", "--list", "--local")
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
	if got := runOutput(t, "git", "--git-dir", cfg.GitDir, "config", "--list", "--local"); got != config {
		t.Fatalf("repo config changed:\nbefore:\n%s\nafter:\n%s", config, got)
	}
	out := runOutput(t, "env", "GIT_NO_LAZY_FETCH=1", "git", "--git-dir", cfg.GitDir, "cat-file", "--batch-check", "--batch-all-objects")
	for _, oid := range oids {
		if !strings.Contains(out, oid+" blob") {
			t.Fatalf("blob %s missing after FetchMissingBlobs:\n%s", oid, out)
		}
	}
}

func TestFetchMissingBlobsKeepsRemoteConfigWhenOriginIsNotThePromisor(t *testing.T) {
	t.Parallel()
	remote := gittest.PartialCloneRemote(t, map[string]string{"a.txt": "a\n", "b.txt": "b\n"})
	// The user cloned blobless from upstream, then added origin for the same
	// repository.
	gitDir := filepath.Join(t.TempDir(), "repo.git")
	run(t, "git", "clone", "-q", "--bare", "--filter=blob:none", "--origin", "upstream", remote, gitDir)
	run(t, "git", "--git-dir", gitDir, "remote", "add", "origin", remote)
	config := runOutput(t, "git", "--git-dir", gitDir, "config", "--list", "--local")
	var oids []string
	for _, name := range []string{"a.txt", "b.txt"} {
		oids = append(oids, strings.TrimSpace(runOutput(t, "git", "--git-dir", gitDir, "rev-parse", "HEAD:"+name)))
	}
	store := New(nil)
	defer store.Close()

	if err := store.FetchMissingBlobs(context.Background(), model.RepoConfig{ID: "x", Name: "x", GitDir: gitDir}, oids); err != nil {
		t.Fatalf("FetchMissingBlobs: %v", err)
	}
	// A filtered fetch from origin would record origin as a promisor.
	if got := runOutput(t, "git", "--git-dir", gitDir, "config", "--list", "--local"); got != config {
		t.Fatalf("repo config changed:\nbefore:\n%s\nafter:\n%s", config, got)
	}
	out := runOutput(t, "env", "GIT_NO_LAZY_FETCH=1", "git", "--git-dir", gitDir, "cat-file", "--batch-check", "--batch-all-objects")
	for _, oid := range oids {
		if !strings.Contains(out, oid+" blob") {
			t.Fatalf("blob %s missing after FetchMissingBlobs:\n%s", oid, out)
		}
	}
}

func TestPromisorRemoteFollowsLazyFetchOrder(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		config string
		want   string
	}{
		{name: "none", config: "", want: ""},
		{name: "promisor true", config: "[remote \"a\"]\n\tpromisor = true\n", want: "a"},
		{name: "empty promisor value is false", config: "[remote \"a\"]\n\tpromisor = \n[remote \"b\"]\n\tpromisor = true\n", want: "b"},
		{name: "promisor false", config: "[remote \"a\"]\n\tpromisor = off\n[remote \"b\"]\n\tpromisor = yes\n", want: "b"},
		{name: "nonzero number is true", config: "[remote \"a\"]\n\tpromisor = 2\n", want: "a"},
		{name: "partial clone filter alone", config: "[remote \"a\"]\n\tpartialclonefilter = blob:none\n[remote \"b\"]\n\tpromisor = true\n", want: "a"},
		{name: "partialClone remote goes last", config: "[extensions]\n\tpartialClone = a\n[remote \"a\"]\n\tpromisor = true\n[remote \"b\"]\n\tpromisor = true\n", want: "b"},
		{name: "partialClone remote alone", config: "[extensions]\n\tpartialClone = a\n", want: "a"},
		{name: "dotted remote name", config: "[remote \"up.stream\"]\n\tpromisor = true\n", want: "up.stream"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gitDir := filepath.Join(t.TempDir(), "repo.git")
			run(t, "git", "init", "-q", "--bare", gitDir)
			f, err := os.OpenFile(filepath.Join(gitDir, "config"), os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.WriteString(tt.config); err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
			got, err := promisorRemote(context.Background(), gitDir)
			if err != nil {
				t.Fatalf("promisorRemote: %v", err)
			}
			if got != tt.want {
				t.Fatalf("promisorRemote = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFetchMissingBlobsKeepsRemoteConfigWhenAnEarlierRemoteIsNotAPromisor(t *testing.T) {
	t.Parallel()
	remote := gittest.PartialCloneRemote(t, map[string]string{"a.txt": "a\n", "b.txt": "b\n"})
	gitDir := filepath.Join(t.TempDir(), "repo.git")
	run(t, "git", "clone", "-q", "--bare", "--filter=blob:none", "--origin", "upstream", remote, gitDir)
	run(t, "git", "--git-dir", gitDir, "remote", "add", "a", remote)
	// Git reads the empty value as false, so upstream, listed after a, is the
	// promisor.
	run(t, "git", "--git-dir", gitDir, "config", "remote.a.promisor", "")
	run(t, "git", "--git-dir", gitDir, "config", "--unset", "remote.upstream.promisor")
	run(t, "git", "--git-dir", gitDir, "config", "--unset", "remote.upstream.partialclonefilter")
	f, err := os.OpenFile(filepath.Join(gitDir, "config"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("[remote \"upstream\"]\n\tpromisor = true\n\tpartialclonefilter = blob:none\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	config := runOutput(t, "git", "--git-dir", gitDir, "config", "--list", "--local")
	var oids []string
	for _, name := range []string{"a.txt", "b.txt"} {
		oids = append(oids, strings.TrimSpace(runOutput(t, "git", "--git-dir", gitDir, "rev-parse", "HEAD:"+name)))
	}
	store := New(nil)
	defer store.Close()

	if err := store.FetchMissingBlobs(context.Background(), model.RepoConfig{ID: "x", Name: "x", GitDir: gitDir}, oids); err != nil {
		t.Fatalf("FetchMissingBlobs: %v", err)
	}
	// A filtered fetch from a would record a as a promisor.
	if got := runOutput(t, "git", "--git-dir", gitDir, "config", "--list", "--local"); got != config {
		t.Fatalf("repo config changed:\nbefore:\n%s\nafter:\n%s", config, got)
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
