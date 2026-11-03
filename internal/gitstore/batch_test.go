package gitstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestBlobToCacheLogsLazyFetchStderrWithoutPrompting(t *testing.T) {
	askpassRuns := isolateLazyFetchEnv(t)
	tmp := t.TempDir()
	gitDir, _, blobOID := partialClone(t, tmp, "blob:none")
	run(t, "git", "--git-dir", gitDir, "remote", "set-url", "origin", unauthorizedRemote(t))

	var logs bytes.Buffer
	store := New(slog.New(slog.NewJSONHandler(&logs, nil)))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cfg := model.RepoConfig{ID: "x", GitDir: gitDir}
	if _, err := store.BlobToCache(ctx, cfg, blobOID, filepath.Join(tmp, "cache", blobOID)); err == nil {
		t.Fatal("BlobToCache fetched a blob the promisor remote refused")
	}
	store.Close()
	if !strings.Contains(logs.String(), `"msg":"git cat-file stderr"`) || !strings.Contains(logs.String(), "terminal prompts disabled") {
		t.Fatalf("logs = %s, want the lazy fetch's non-interactive failure", logs.String())
	}
	assertAskpassDidNotRun(t, askpassRuns)
}

func TestBuildTreeIndexLazyTreeFetchDoesNotPrompt(t *testing.T) {
	askpassRuns := isolateLazyFetchEnv(t)
	tmp := t.TempDir()
	gitDir, commit, _ := partialClone(t, tmp, "tree:0")
	run(t, "git", "--git-dir", gitDir, "remote", "set-url", "origin", unauthorizedRemote(t))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := New(nil).BuildTreeIndex(ctx, model.RepoConfig{ID: "x", GitDir: gitDir}, commit)
	if err == nil || !strings.Contains(err.Error(), "terminal prompts disabled") {
		t.Fatalf("BuildTreeIndex error = %v, want the lazy tree fetch's non-interactive failure", err)
	}
	assertAskpassDidNotRun(t, askpassRuns)
}

func TestCloneBloblessNonInteractiveSkipsSSHAskpass(t *testing.T) {
	askpassRuns := isolateLazyFetchEnv(t)
	tmp := t.TempDir()
	cfg := model.RepoConfig{Name: "repo", GitDir: filepath.Join(tmp, "repo.git"), RemoteURL: unauthorizedRemote(t), Branch: "main"}
	err := New(slog.New(slog.NewTextHandler(io.Discard, nil))).CloneBloblessNonInteractive(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "terminal prompts disabled") {
		t.Fatalf("CloneBloblessNonInteractive error = %v, want a non-interactive failure", err)
	}
	assertAskpassDidNotRun(t, askpassRuns)
}

// isolateLazyFetchEnv keeps the machine's Git config and prompt programs out of
// a test's Git commands. It sets SSH_ASKPASS to a program that records each run
// in the returned file.
func isolateLazyFetchEnv(t *testing.T) string {
	t.Helper()
	for _, key := range []string{"GIT_ASKPASS", "GIT_SSH", "GIT_SSH_COMMAND", "SSH_ASKPASS_REQUIRE"} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	dir := t.TempDir()
	runs := filepath.Join(dir, "askpass-runs")
	askpass := filepath.Join(dir, "askpass")
	if err := os.WriteFile(askpass, []byte("#!/bin/sh\necho \"$*\" >> '"+runs+"'\necho guessed\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSH_ASKPASS", askpass)
	return runs
}

func assertAskpassDidNotRun(t *testing.T, runs string) {
	t.Helper()
	if data, err := os.ReadFile(runs); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Git ran SSH_ASKPASS: %q, %v", data, err)
	}
}

// unauthorizedRemote returns an HTTP remote that answers every request with 401.
func unauthorizedRemote(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)
	return server.URL + "/repo.git"
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
}

func TestBlobToCacheRedactsBatchStderrBeforeBoundingLine(t *testing.T) {
	// The logged bound falls inside the password.
	prefix := strings.Repeat("x", maxGitErrorBytes-len("https://alice:s3cr"))
	logs := batchStderrLogs(t, "echo '"+prefix+"https://alice:s3cretpassword@example.com/repo.git' >&2")
	if !strings.Contains(logs, `"truncated":true`) {
		t.Fatalf("logs = %.200s, want a truncated line", logs)
	}
	if strings.Contains(logs, "s3cr") {
		t.Fatalf("logs leaked part of the password: %s", logs[len(logs)-300:])
	}
}

func TestBlobToCacheBoundsBatchStderrLine(t *testing.T) {
	logs := batchStderrLogs(t, `printf '%040000d\n' 0 >&2`)
	decoder := json.NewDecoder(strings.NewReader(logs))
	for decoder.More() {
		var entry struct {
			Line      string
			Truncated bool
		}
		if err := decoder.Decode(&entry); err != nil {
			t.Fatal(err)
		}
		if len(entry.Line) != maxGitErrorBytes || !entry.Truncated {
			t.Fatalf("logged %d bytes, truncated=%v; want %d bytes, truncated", len(entry.Line), entry.Truncated, maxGitErrorBytes)
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
	cfg := model.RepoConfig{ID: "x", GitDir: filepath.Join(tmp, "repo.git")}
	if _, err := store.BlobToCache(context.Background(), cfg, oid, filepath.Join(tmp, "cache", oid)); err == nil {
		t.Fatal("BlobToCache succeeded for a missing blob")
	}
	store.Close()
	return logs.String()
}
