package watcher

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
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
			poller := New()
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
	// Git writes the files here, so this subtest also covers the layout Git
	// itself leaves, which the ones above write by hand.
	t.Run("git", func(t *testing.T) {
		dir := t.TempDir()
		primary := filepath.Join(dir, "primary")
		linked := filepath.Join(dir, "linked")
		git := func(args ...string) {
			t.Helper()
			if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v\n%s", args, err, out)
			}
		}
		commit := func(worktree, message string) {
			t.Helper()
			git("-C", worktree, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-q", "--allow-empty", "-m", message)
		}
		git("init", "-q", "--initial-branch", "main", primary)
		commit(primary, "main 1")
		git("-C", primary, "branch", "other")
		git("-C", primary, "worktree", "add", "-q", "-b", "topic", linked)
		head := filepath.Join(primary, ".git", "worktrees", "linked", "HEAD")
		poller := New()
		if poller.headChanged(head) {
			t.Fatal("startup must prime state")
		}
		commit(linked, "topic 1")
		if !poller.headChanged(head) {
			t.Fatal("missed a commit in the linked worktree")
		}
		git("-C", primary, "pack-refs", "--all")
		if poller.headChanged(head) {
			t.Fatal("packing an unchanged ref must preserve the baseline")
		}
		commit(primary, "main 2")
		if poller.headChanged(head) {
			t.Fatal("a commit to another branch caused reconciliation")
		}
		commit(linked, "topic 2")
		git("-C", primary, "pack-refs", "--all")
		if !poller.headChanged(head) {
			t.Fatal("missed a packed update of the checked-out branch")
		}
		git("-C", linked, "switch", "-q", "other")
		if !poller.headChanged(head) {
			t.Fatal("missed a switch to a packed branch")
		}
		commit(linked, "other 1")
		if !poller.headChanged(head) {
			t.Fatal("missed a commit after the switch")
		}
	})
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
	poller := New()
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
