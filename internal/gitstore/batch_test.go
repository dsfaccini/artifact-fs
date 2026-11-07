package gitstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
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

	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{ID: "x", GitDir: filepath.Join(repo, ".git"), BlobCacheDir: filepath.Join(tmp, "cache")}}
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
	size, err := store.BlobToCache(ctx, cfg.RepoPaths, blobOID, dst)
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
	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{ID: "x", Name: "x", GitDir: filepath.Join(tmp, "repo.git")}, RepoSpec: model.RepoSpec{RemoteURL: gittest.PartialCloneRemote(t, files), Branch: "main"}}
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
	if err := store.FetchMissingBlobs(ctx, cfg.RepoPaths, oids[:2]); err != nil {
		t.Fatalf("FetchMissingBlobs(a, b): %v", err)
	}
	packed := packedObjects(t, cfg.GitDir)

	if err := store.FetchMissingBlobs(ctx, cfg.RepoPaths, oids); err != nil {
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

func TestFetchMissingBlobsLogsEveryFailureButACancel(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		// mode tells the fake git what its fetch does.
		mode    string
		context func(marker string) (context.Context, context.CancelFunc)
		wantErr error
		wantLog bool
	}{
		{name: "hydrator stop cancels the fetch", mode: "block", context: cancelOnFile, wantErr: context.Canceled},
		{name: "fetch times out", mode: "block", context: func(marker string) (context.Context, context.CancelFunc) {
			return gittest.ExpireOnFile(context.Background(), marker)
		}, wantErr: context.DeadlineExceeded, wantLog: true},
		{name: "remote refuses the fetch", mode: "fail", context: func(string) (context.Context, context.CancelFunc) {
			return context.WithCancel(context.Background())
		}, wantLog: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmp := t.TempDir()
			cfg := model.RepoConfig{RepoPaths: model.RepoPaths{ID: "x", Name: "x", GitDir: filepath.Join(tmp, "repo.git")}, RepoSpec: model.RepoSpec{RemoteURL: gittest.PartialCloneRemote(t, map[string]string{"a.txt": "a\n", "b.txt": "b\n"}), Branch: "main"}}
			var logs bytes.Buffer
			store := New(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
			defer store.Close()
			if err := store.CloneBlobless(context.Background(), cfg); err != nil {
				t.Fatalf("CloneBlobless: %v", err)
			}
			var oids []string
			for _, name := range []string{"a.txt", "b.txt"} {
				oids = append(oids, strings.TrimSpace(runOutput(t, "git", "--git-dir", cfg.GitDir, "rev-parse", "HEAD:"+name)))
			}
			// The fake runs every git command but fetch. Its fetch blocks,
			// bounded by sleep 60, or fails.
			bin := filepath.Join(tmp, "bin")
			if err := os.Mkdir(bin, 0o755); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(tmp, "fetch-started")
			script := "#!/bin/sh\nfor arg in \"$@\"; do\n  if [ \"$arg\" = fetch ]; then\n" +
				"    if [ " + tt.mode + " = fail ]; then echo 'fatal: remote refused' >&2; exit 128; fi\n" +
				"    : > " + marker + "; exec sleep 60\n  fi\ndone\nexec " + realGit + " \"$@\"\n"
			if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			ctx, cancel := tt.context(marker)
			defer cancel()

			err := store.FetchMissingBlobs(ctx, cfg.RepoPaths, oids)
			if err == nil || tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("FetchMissingBlobs error = %v, want %v", err, tt.wantErr)
			}
			if got := strings.Contains(logs.String(), "batch blob fetch failed"); got != tt.wantLog {
				t.Fatalf("warning logged = %t, want %t; logs:\n%s", got, tt.wantLog, logs.String())
			}
		})
	}
}

func TestFetchMissingBlobsRedactsFetchStderr(t *testing.T) {
	// Git anonymizes HTTP URLs itself, so only a fake git can print a
	// credentialed URL the way a remote helper or SSH may.
	logs, _ := failedFetchMissingBlobs(t, `echo "fatal: unable to access 'https://alice:s3cret@example.com/repo.git/': HTTP 401" >&2`)
	if !strings.Contains(logs, "unable to access") {
		t.Fatalf("logs = %s, want the fetch's stderr", logs)
	}
	if strings.Contains(logs, "s3cret") {
		t.Fatalf("logs leaked the password: %s", logs)
	}
	if strings.Contains(logs, "example.com/repo.git") {
		t.Fatalf("logs kept the remote's location: %s", logs)
	}
}

func TestFetchMissingBlobsBoundsFetchStderr(t *testing.T) {
	_, err := failedFetchMissingBlobs(t, `awk 'BEGIN { for (i = 0; i < 10000; i++) printf "word "; print "" }' >&2`)
	msg := err.Error()
	if !strings.HasSuffix(msg, gitErrorTruncatedLabel) || len(msg) > maxGitErrorBytes+len(gitErrorTruncatedLabel)+1 {
		t.Fatalf("error holds %d bytes of fetch stderr; want at most %d bytes, truncated", len(msg), maxGitErrorBytes)
	}
}

// failedFetchMissingBlobs runs FetchMissingBlobs on two blobs against a fake
// git whose fetch runs stderrScript and fails, and returns the logs and the
// error. Its cat-file reports every blob missing, and its config names origin
// as the promisor remote.
func failedFetchMissingBlobs(t *testing.T, stderrScript string) (string, error) {
	t.Helper()
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	fakeGit := "#!/bin/sh\ncase \" $* \" in\n" +
		"*\" cat-file \"*) while read -r oid; do echo \"$oid missing\"; done ;;\n" +
		"*\" config \"*) printf 'remote.origin.promisor\\ntrue\\000' ;;\n" +
		"*\" fetch \"*) " + stderrScript + "; exit 128 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(fakeGit), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	var logs bytes.Buffer
	store := New(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer store.Close()
	cfg := model.RepoPaths{ID: "x", Name: "x", GitDir: filepath.Join(tmp, "repo.git")}
	err := store.FetchMissingBlobs(context.Background(), cfg, []string{strings.Repeat("a", 40), strings.Repeat("b", 40)})
	if err == nil {
		t.Fatal("FetchMissingBlobs succeeded against a fetch that fails")
	}
	return logs.String(), err
}

// cancelOnFile returns a context that is cancelled once path exists, as the
// hydrator's Stop cancels a batch fetch.
func cancelOnFile(path string) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for ctx.Err() == nil {
			if _, err := os.Stat(path); err == nil {
				cancel()
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	return ctx, cancel
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

	if err := store.FetchMissingBlobs(context.Background(), model.RepoPaths{ID: "x", Name: "x", GitDir: gitDir}, oids); err != nil {
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

	if err := store.FetchMissingBlobs(context.Background(), model.RepoPaths{ID: "x", Name: "x", GitDir: gitDir}, oids); err != nil {
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
			cfg := model.RepoPaths{ID: "x", GitDir: filepath.Join(repo, ".git")}
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

	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{ID: "x", GitDir: filepath.Join(repo, ".git"), BlobCacheDir: filepath.Join(tmp, "cache")}}
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
	_, err = store.BlobToCache(ctx, cfg.RepoPaths, blobOID, dst)
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

	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{ID: "x", GitDir: filepath.Join(repo, ".git")}}
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

	data, err := store.ReadBlob(ctx, cfg.RepoPaths, blobOID, 5)
	if err != nil {
		t.Fatalf("ReadBlob at limit: %v", err)
	}
	if string(data) != "line\n" {
		t.Fatalf("data = %q, want line\\n", data)
	}
	_, err = store.ReadBlob(ctx, cfg.RepoPaths, blobOID, 4)
	if !errors.Is(err, model.ErrBlobTooLarge) {
		t.Fatalf("err = %v, want ErrBlobTooLarge", err)
	}
	data, err = store.ReadBlob(ctx, cfg.RepoPaths, blobOID, 5)
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

func TestBlobToCacheLogsLazyFetchStderrWithoutPrompting(t *testing.T) {
	askpassRuns := gittest.IsolatePrompts(t)
	tmp := t.TempDir()
	gitDir, _, blobOID := partialClone(t, tmp, "blob:none")
	run(t, "git", "--git-dir", gitDir, "remote", "set-url", "origin", gittest.UnauthorizedRemote(t))

	var logs bytes.Buffer
	store := New(slog.New(slog.NewJSONHandler(&logs, nil)))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cfg := model.RepoPaths{ID: "x", GitDir: gitDir}
	if _, err := store.BlobToCache(ctx, cfg, blobOID, filepath.Join(tmp, "cache", blobOID)); err == nil {
		t.Fatal("BlobToCache fetched a blob the promisor remote refused")
	}
	store.Close()
	if !strings.Contains(logs.String(), `"msg":"git cat-file stderr"`) || !strings.Contains(logs.String(), "terminal prompts disabled") {
		t.Fatalf("logs = %s, want the lazy fetch's non-interactive failure", logs.String())
	}
	gittest.RequireNoPrompt(t, askpassRuns)
}

func TestBuildTreeIndexLazyTreeFetchDoesNotPrompt(t *testing.T) {
	askpassRuns := gittest.IsolatePrompts(t)
	tmp := t.TempDir()
	gitDir, commit, _ := partialClone(t, tmp, "tree:0")
	run(t, "git", "--git-dir", gitDir, "remote", "set-url", "origin", gittest.UnauthorizedRemote(t))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := New(nil).BuildTreeIndex(ctx, model.RepoConfig{RepoPaths: model.RepoPaths{ID: "x", GitDir: gitDir}}, commit)
	if err == nil || !strings.Contains(err.Error(), "terminal prompts disabled") {
		t.Fatalf("BuildTreeIndex error = %v, want the lazy tree fetch's non-interactive failure", err)
	}
	gittest.RequireNoPrompt(t, askpassRuns)
}

func TestCloneBloblessNonInteractiveSkipsSSHAskpass(t *testing.T) {
	askpassRuns := gittest.IsolatePrompts(t)
	tmp := t.TempDir()
	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{Name: "repo", GitDir: filepath.Join(tmp, "repo.git")}, RepoSpec: model.RepoSpec{RemoteURL: gittest.UnauthorizedRemote(t), Branch: "main"}}
	err := New(slog.New(slog.NewTextHandler(io.Discard, nil))).CloneBloblessNonInteractive(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "terminal prompts disabled") {
		t.Fatalf("CloneBloblessNonInteractive error = %v, want a non-interactive failure", err)
	}
	gittest.RequireNoPrompt(t, askpassRuns)
}

// partialClone clones a one-file repository with filter into dir and returns
// the clone's Git dir, its HEAD commit and the file's blob.
func partialClone(t *testing.T, dir string, filter string) (gitDir, commit, blob string) {
	t.Helper()
	src := filepath.Join(dir, "src")
	run(t, "git", "init", "--initial-branch", "main", src)
	if err := os.WriteFile(filepath.Join(src, "file.txt"), []byte("data\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", src, "add", "file.txt")
	run(t, "git", "-C", src, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "init")
	run(t, "git", "-C", src, "config", "uploadpack.allowFilter", "true")
	clone := filepath.Join(dir, "clone")
	run(t, "git", "clone", "--filter="+filter, "--no-checkout", "file://"+src, clone)
	commit = strings.TrimSpace(runOutput(t, "git", "-C", src, "rev-parse", "HEAD"))
	blob = strings.TrimSpace(runOutput(t, "git", "-C", src, "rev-parse", "HEAD:file.txt"))
	return filepath.Join(clone, ".git"), commit, blob
}

func TestBlobToCacheRedactsBatchStderr(t *testing.T) {
	// Git anonymizes HTTP URLs itself, so only a fake git can print a
	// credentialed URL the way a remote helper or SSH may.
	logs := batchStderrLogs(t, `echo "fatal: unable to access 'https://alice:s3cret@example.com/repo.git/': HTTP 401" >&2`)
	if !strings.Contains(logs, "unable to access") {
		t.Fatalf("logs = %s, want the batch process's stderr", logs)
	}
	if strings.Contains(logs, "s3cret") {
		t.Fatalf("logs leaked the password: %s", logs)
	}
	if strings.Contains(logs, "example.com/repo.git") {
		t.Fatalf("logs kept the remote's location: %s", logs)
	}
}

func TestBlobToCacheRedactsNoMoreOfBatchStderrLineThanItLogs(t *testing.T) {
	// The server shapes this stderr, and redaction's cost grows faster than a
	// line's length. Redacting the token shrinks the line far below the logged
	// bound, so text after it reaches the log only when redaction saw it.
	logs := batchStderrLogs(t, `printf 'fatal: token=%030000d unbuffered-tail\n' 0 >&2`)
	if !strings.Contains(logs, `"truncated":true`) {
		t.Fatalf("logs = %.300s, want a truncated line", logs)
	}
	if strings.Contains(logs, "unbuffered-tail") {
		t.Fatalf("redaction saw text past the logged bound: %.300s", logs)
	}
}

func TestBlobToCacheDropsPartialTokenAtBatchStderrBufferBound(t *testing.T) {
	// The buffer bound falls inside the password, and redacting the token
	// before it shrinks the line below the logged bound.
	head := "token=" + strings.Repeat("a", maxGitErrorBytes-len("token=")-len(" alice:s3cr")) + " alice:s3cr"
	logs := batchStderrLogs(t, "printf '%s' '"+head+"' >&2; echo 'etpassword@example.com/repo.git' >&2")
	if !strings.Contains(logs, `"truncated":true`) {
		t.Fatalf("logs = %.300s, want a truncated line", logs)
	}
	if strings.Contains(logs, "s3cr") {
		t.Fatalf("logs leaked part of the password: %.300s", logs)
	}
}

func TestBlobToCacheLogsOverflowingBatchStderrTokenAsTruncated(t *testing.T) {
	logs := batchStderrLogs(t, `printf 'fatal:%070000d\n' 0 >&2`)
	if !strings.Contains(logs, `"line":"","truncated":true`) {
		t.Fatalf("logs = %.300s, want an empty truncated line", logs)
	}
}

func TestBlobToCacheLogsBatchStderrWithoutTrailingNewline(t *testing.T) {
	logs := batchStderrLogs(t, "printf 'fatal: last line has no newline' >&2")
	if !strings.Contains(logs, "fatal: last line has no newline") {
		t.Fatalf("logs = %s, want the unterminated last line", logs)
	}
}

func TestBlobToCacheBoundsBatchStderrLine(t *testing.T) {
	logs := batchStderrLogs(t, `awk 'BEGIN { for (i = 0; i < 10000; i++) printf "word "; print "" }' >&2`)
	decoder := json.NewDecoder(strings.NewReader(logs))
	for decoder.More() {
		var entry struct {
			Line      string
			Truncated bool
		}
		if err := decoder.Decode(&entry); err != nil {
			t.Fatal(err)
		}
		if entry.Line == "" || len(entry.Line) > maxGitErrorBytes || !entry.Truncated {
			t.Fatalf("logged %d bytes, truncated=%v; want at most %d bytes, truncated", len(entry.Line), entry.Truncated, maxGitErrorBytes)
		}
	}
	if logs == "" {
		t.Fatal("no stderr was logged")
	}
}

// batchStderrLogs runs BlobToCache against a fake git whose cat-file --batch
// runs stderrScript, then reports every blob missing, and returns the logs.
func batchStderrLogs(t *testing.T, stderrScript string) string {
	t.Helper()
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	fakeGit := "#!/bin/sh\n" + stderrScript + "\nwhile read -r oid; do echo \"$oid missing\"; done\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(fakeGit), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	var logs bytes.Buffer
	store := New(slog.New(slog.NewJSONHandler(&logs, nil)))
	oid := strings.Repeat("a", 40)
	cfg := model.RepoPaths{ID: "x", GitDir: filepath.Join(tmp, "repo.git")}
	if _, err := store.BlobToCache(context.Background(), cfg, oid, filepath.Join(tmp, "cache", oid)); err == nil {
		t.Fatal("BlobToCache succeeded for a missing blob")
	}
	store.Close()
	return logs.String()
}

func TestBlobToCacheRemovesDstAfterAFailedWrite(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	// cat-file announces 10 bytes and sends 3, a cut-short stream a local
	// repository cannot produce.
	fakeGit := "#!/bin/sh\nread -r oid\necho \"$oid blob 10\"\nprintf abc\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(fakeGit), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	store := New(nil)
	defer store.Close()
	// The hydrator passes a file blobcache.Stage already created.
	dst := filepath.Join(tmp, "staged")
	if err := os.WriteFile(dst, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := model.RepoPaths{ID: "x", GitDir: filepath.Join(tmp, "repo.git")}
	if _, err := store.BlobToCache(context.Background(), cfg, strings.Repeat("a", 40), dst); err == nil {
		t.Fatal("BlobToCache succeeded on a cut-short blob")
	}
	if _, err := os.Stat(dst); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dst stat err = %v, want it removed", err)
	}
}
