package watcher

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLinkedWorktreeLooseAndPackedReferences(t *testing.T) {
	for _, absolute := range []bool{false, true} {
		name := "relative-commondir"
		if absolute {
			name = "absolute-commondir"
		}
		t.Run(name, func(t *testing.T) {
			common := t.TempDir()
			gitDir := filepath.Join(common, "worktrees", "topic")
			for _, dir := range []string{gitDir, filepath.Join(common, "refs", "heads")} {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			commonValue := "../.."
			if absolute {
				commonValue = common
			}
			write := func(path, value string) {
				t.Helper()
				if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			write(filepath.Join(gitDir, "commondir"), commonValue+"\n")
			head := filepath.Join(gitDir, "HEAD")
			loose := filepath.Join(common, "refs", "heads", "main")
			packed := filepath.Join(common, "packed-refs")
			write(head, "ref: refs/heads/main\n")
			write(loose, "commit-1\n")
			poller := New(time.Second)
			if poller.headChanged(head) {
				t.Fatal("startup must prime state")
			}
			write(loose, "commit-2\n")
			if !poller.headChanged(head) {
				t.Fatal("missed common loose-ref update")
			}
			write(packed, "# pack-refs with: peeled\ncommit-2 refs/heads/main\nother refs/heads/other\n")
			if err := os.Remove(loose); err != nil {
				t.Fatal(err)
			}
			if poller.headChanged(head) {
				t.Fatal("packing an unchanged ref must preserve the baseline")
			}
			write(packed, "commit-3 refs/heads/main\nother refs/heads/other\n")
			if !poller.headChanged(head) {
				t.Fatal("missed packed active-ref update")
			}
			write(packed, "commit-3 refs/heads/main\nnew-other refs/heads/other\n")
			if poller.headChanged(head) {
				t.Fatal("unrelated packed branch caused reconciliation")
			}
			write(head, "ref: refs/heads/other\n")
			if !poller.headChanged(head) {
				t.Fatal("missed switch to packed branch")
			}
			write(packed, "commit-3 refs/heads/main\nnext-other refs/heads/other\n")
			if !poller.headChanged(head) {
				t.Fatal("missed first packed update after branch switch")
			}
			write(head, "detached-commit\n")
			if !poller.headChanged(head) {
				t.Fatal("missed detached HEAD")
			}
			write(head, "next-detached-commit\n")
			if !poller.headChanged(head) {
				t.Fatal("missed detached HEAD advance")
			}
		})
	}
}

func TestLinkedWorktreePrivateReferenceNamespace(t *testing.T) {
	common := t.TempDir()
	gitDir := filepath.Join(common, "worktrees", "topic")
	privateRef := filepath.Join(gitDir, "refs", "bisect", "test")
	if err := os.MkdirAll(filepath.Dir(privateRef), 0o755); err != nil {
		t.Fatal(err)
	}
	for path, value := range map[string]string{
		filepath.Join(gitDir, "commondir"): "../..\n",
		filepath.Join(gitDir, "HEAD"):      "ref: refs/bisect/test\n",
		privateRef:                         "private-1\n",
	} {
		if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	poller := New(time.Second)
	head := filepath.Join(gitDir, "HEAD")
	if poller.headChanged(head) {
		t.Fatal("startup must prime state")
	}
	if err := os.WriteFile(privateRef, []byte("private-2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !poller.headChanged(head) {
		t.Fatal("missed per-worktree reference update")
	}
}
