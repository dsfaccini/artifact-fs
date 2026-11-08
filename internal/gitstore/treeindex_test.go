package gitstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/gittest"
	"github.com/cloudflare/artifact-fs/internal/model"
)

func TestResolveHEADAndBuildTreeIndex(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	run(t, "git", "init", "--initial-branch", "main", repo)
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", repo, "add", "README.md")
	run(t, "git", "-C", repo, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "init")

	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{ID: "x", GitDir: filepath.Join(repo, ".git")}}
	store := New(nil)
	oid, ref, err := store.ResolveHEAD(t.Context(), cfg)
	if err != nil {
		t.Fatalf("ResolveHEAD: %v", err)
	}
	if want := strings.TrimSpace(runOutput(t, "git", "-C", repo, "rev-parse", "HEAD")); oid != want || ref != "main" {
		t.Fatalf("ResolveHEAD = %q, %q; want %q, main", oid, ref, want)
	}
	nodes, err := store.BuildTreeIndex(t.Context(), cfg, oid)
	if err != nil {
		t.Fatalf("BuildTreeIndex: %v", err)
	}
	blob := strings.TrimSpace(runOutput(t, "git", "-C", repo, "rev-parse", "HEAD:README.md"))
	i := slices.IndexFunc(nodes, func(n model.BaseNode) bool { return n.Path == "README.md" })
	if i < 0 {
		t.Fatalf("README.md not in tree %+v", nodes)
	}
	if n := nodes[i]; n.Type != "file" || n.ObjectOID != blob || n.SizeState != "known" || n.SizeBytes != 5 {
		t.Fatalf("README.md = %+v, want a file of blob %s with known size 5", n, blob)
	}
}

// A HEAD write between ResolveHEAD's reads must not pair one state's commit
// with the other state's ref.
func TestResolveHEADPairsTheCommitWithItsRef(t *testing.T) {
	t.Parallel()
	repo := filepath.Join(t.TempDir(), "repo")
	run(t, "git", "init", "--initial-branch", "main", repo)
	commit := func(message string) string {
		t.Helper()
		run(t, "git", "-C", repo, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", message)
		return strings.TrimSpace(runOutput(t, "git", "-C", repo, "rev-parse", "HEAD"))
	}
	mainOID := commit("main")
	featureOID := commit("feature")
	run(t, "git", "-C", repo, "update-ref", "refs/heads/feature", featureOID)
	run(t, "git", "-C", repo, "update-ref", "refs/heads/main", mainOID)

	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{ID: "x", GitDir: filepath.Join(repo, ".git")}}
	store := New(nil)
	switched := false
	store.testHookBetweenHEADReads = func() {
		if !switched {
			switched = true
			run(t, "git", "-C", repo, "symbolic-ref", "HEAD", "refs/heads/feature")
		}
	}
	oid, ref, err := store.ResolveHEAD(t.Context(), cfg)
	if err != nil {
		t.Fatalf("ResolveHEAD: %v", err)
	}
	if !switched {
		t.Fatal("ResolveHEAD never ran the hook between its reads")
	}
	if oid != featureOID || ref != "feature" {
		t.Fatalf("ResolveHEAD = %q, %q; want %q, feature (main is %q)", oid, ref, featureOID, mainOID)
	}
}

func TestBuildTreeIndexPropagatesBatchCancellation(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	started := filepath.Join(tmp, "cat-file-started")
	oid := strings.Repeat("a", 40)
	script := fmt.Sprintf("#!/bin/sh\ncase \"$1\" in\nls-tree) printf '100644 blob %s\\tfile.txt\\0' ;;\ncat-file) : > \"$AFS_CAT_FILE_STARTED\"; exec sleep 30 ;;\nesac\n", oid)
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AFS_CAT_FILE_STARTED", started)

	var warnings bytes.Buffer
	store := New(slog.New(slog.NewTextHandler(&warnings, &slog.HandlerOptions{Level: slog.LevelWarn})))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		_, err := store.BuildTreeIndex(ctx, model.RepoConfig{RepoPaths: model.RepoPaths{ID: "repo", GitDir: t.TempDir()}}, oid)
		errCh <- err
	}()
	// Cancel only once size resolution runs, after ls-tree has finished.
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		select {
		case err := <-errCh:
			t.Fatalf("BuildTreeIndex returned before cat-file started: %v", err)
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("BuildTreeIndex error = %v, want context canceled", err)
	}
	// Size resolution failures are non-fatal and logged as warnings. A canceled build is not one.
	if warnings.Len() != 0 {
		t.Fatalf("cancellation was logged as a warning:\n%s", warnings.String())
	}
}

func TestBatchResolveSizesDisablesLazyFetch(t *testing.T) {
	t.Setenv("GIT_NO_LAZY_FETCH", "0")
	bin := t.TempDir()
	script := `#!/bin/sh
if [ "$GIT_NO_LAZY_FETCH" != "1" ]; then
	echo "GIT_NO_LAZY_FETCH=$GIT_NO_LAZY_FETCH" >&2
	exit 97
fi
if [ "$1" != "cat-file" ] || [ "$2" != "--batch-check" ] || [ "$3" != "--buffer" ]; then
	echo "unexpected arguments: $*" >&2
	exit 98
fi
while IFS= read -r oid; do
	printf '%s blob 7\n' "$oid"
done
`
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	oid := strings.Repeat("a", 40)
	nodes := []model.BaseNode{
		{ObjectOID: oid, SizeState: "unknown"},
		{ObjectOID: oid, SizeState: "unknown"},
	}
	err := New(nil).batchResolveSizes(
		context.Background(),
		model.RepoConfig{RepoPaths: model.RepoPaths{ID: "repo", GitDir: t.TempDir()}},
		nodes,
		[]string{oid},
		map[string][]int{oid: {0, 1}},
	)
	if err != nil {
		t.Fatalf("batchResolveSizes: %v", err)
	}
	for i, node := range nodes {
		if node.SizeState != "known" || node.SizeBytes != 7 {
			t.Fatalf("node %d size = %q/%d, want known/7", i, node.SizeState, node.SizeBytes)
		}
	}
}

func TestBuildTreeIndexBoundsLazyFetchStderr(t *testing.T) {
	// A lazy tree fetch's server shapes ls-tree's stderr, which redaction reads.
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nprintf 'fatal: %0100000d\\n' 0 >&2\nexit 128\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err := New(nil).BuildTreeIndex(context.Background(), model.RepoConfig{RepoPaths: model.RepoPaths{ID: "repo", GitDir: t.TempDir()}}, strings.Repeat("a", 40))
	if err == nil || !strings.HasSuffix(err.Error(), gitErrorTruncatedLabel) || len(err.Error()) > maxGitErrorBytes+len("\n"+gitErrorTruncatedLabel) {
		t.Fatalf("BuildTreeIndex error has %d bytes, want at most %d and the truncation label", len(fmt.Sprint(err)), maxGitErrorBytes)
	}
}

func TestBuildTreeIndexPreservesGitlinkAsDirectory(t *testing.T) {
	t.Parallel()
	repo := filepath.Join(t.TempDir(), "repo")
	run(t, "git", "init", repo)
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", repo, "add", "README.md")
	run(t, "git", "-C", repo, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "init")
	gitlinkOID := strings.TrimSpace(runOutput(t, "git", "-C", repo, "rev-parse", "HEAD"))
	run(t, "git", "-C", repo, "update-index", "--add", "--cacheinfo", "160000,"+gitlinkOID+",vendor/dependency")
	run(t, "git", "-C", repo, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "add gitlink")

	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{ID: "x", GitDir: filepath.Join(repo, ".git")}}
	store := New(nil)
	headOID := strings.TrimSpace(runOutput(t, "git", "-C", repo, "rev-parse", "HEAD"))
	nodes, err := store.BuildTreeIndex(context.Background(), cfg, headOID)
	if err != nil {
		t.Fatalf("BuildTreeIndex: %v", err)
	}
	for _, node := range nodes {
		if node.Path != "vendor/dependency" {
			continue
		}
		if node.Type != "dir" || node.Mode != 0o160000 || node.ObjectOID != gitlinkOID {
			t.Fatalf("gitlink node = %#v", node)
		}
		if node.SizeState != "known" || node.SizeBytes != 0 {
			t.Fatalf("gitlink size = %q/%d, want known/0", node.SizeState, node.SizeBytes)
		}
		return
	}
	t.Fatal("gitlink missing from tree")
}

func TestBuildTreeIndexNonASCIIPaths(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	run(t, "git", "init", repo)
	// Create files with non-ASCII names that git would C-quote without -z.
	os.WriteFile(filepath.Join(repo, "café.txt"), []byte("latte"), 0o644)
	os.WriteFile(filepath.Join(repo, "日本語.md"), []byte("hello"), 0o644)
	run(t, "git", "-C", repo, "add", ".")
	run(t, "git", "-C", repo, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "non-ascii files")

	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{ID: "x", GitDir: filepath.Join(repo, ".git")}}
	store := New(nil)
	oid, _, err := store.ResolveHEAD(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := store.BuildTreeIndex(t.Context(), cfg, oid)
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]bool{}
	for _, n := range nodes {
		paths[n.Path] = true
	}
	if !paths["café.txt"] {
		t.Fatalf("expected café.txt in tree, got paths: %v", paths)
	}
	if !paths["日本語.md"] {
		t.Fatalf("expected 日本語.md in tree, got paths: %v", paths)
	}
}

func TestReadNullDelimitedRejectsPartialFinalRecord(t *testing.T) {
	t.Parallel()
	var records []string
	err := readNullDelimited(strings.NewReader("first\x00partial"), func(record string) {
		records = append(records, record)
	})
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v, want io.ErrUnexpectedEOF", err)
	}
	if !slices.Equal(records, []string{"first"}) {
		t.Fatalf("records = %v, want [first]", records)
	}
}

// The deadline expires once the fake ls-tree runs, so it ends ls-tree itself.
func TestBuildTreeIndexPreservesDeadline(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	started := filepath.Join(tmp, "ls-tree-started")
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nif [ \"$1\" = ls-tree ]; then\n  : > \"$AFS_LS_TREE_STARTED\"\n  exec sleep 10\nfi\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AFS_LS_TREE_STARTED", started)

	ctx, cancel := gittest.ExpireOnFile(context.Background(), started)
	defer cancel()
	store := New(nil)
	defer store.Close()
	_, err := store.BuildTreeIndex(ctx, model.RepoConfig{RepoPaths: model.RepoPaths{ID: "repo", GitDir: filepath.Join(tmp, "repo.git")}}, strings.Repeat("a", 40))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("BuildTreeIndex error = %v, want deadline exceeded", err)
	}
}
