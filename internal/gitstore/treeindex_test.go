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

	"github.com/cloudflare/artifact-fs/internal/model"
)

func TestResolveHEADAndBuildTreeIndex(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	run(t, "git", "init", repo)
	os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello"), 0o644)
	run(t, "git", "-C", repo, "add", "README.md")
	run(t, "git", "-C", repo, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "init")

	cfg := model.RepoConfig{ID: "x", GitDir: filepath.Join(repo, ".git")}
	store := New(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	oid, ref, err := store.ResolveHEAD(ctx, cfg)
	if err != nil {
		t.Fatalf("ResolveHEAD: %v", err)
	}
	if oid == "" || ref == "" {
		t.Fatalf("expected oid/ref, got %q %q", oid, ref)
	}
	nodes, err := store.BuildTreeIndex(ctx, cfg, oid)
	if err != nil {
		t.Fatalf("BuildTreeIndex: %v", err)
	}
	found := false
	for _, n := range nodes {
		if n.Path == "README.md" {
			found = true
			if n.Type != "file" {
				t.Fatalf("expected type file, got %q", n.Type)
			}
		}
	}
	if !found {
		t.Fatalf("expected README.md in tree")
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
		_, err := store.BuildTreeIndex(ctx, model.RepoConfig{ID: "repo", GitDir: t.TempDir()}, oid)
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
		model.RepoConfig{ID: "repo", GitDir: t.TempDir()},
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

	cfg := model.RepoConfig{ID: "x", GitDir: filepath.Join(repo, ".git")}
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

	cfg := model.RepoConfig{ID: "x", GitDir: filepath.Join(repo, ".git")}
	store := New(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	oid, _, err := store.ResolveHEAD(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := store.BuildTreeIndex(ctx, cfg, oid)
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

func TestStreamTreeRecordsPreservesDeadline(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nexec sleep 10\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := streamTreeRecords(ctx, filepath.Join(tmp, "repo.git"), "HEAD", func(string) {})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("streamTreeRecords error = %v, want deadline exceeded", err)
	}
}
