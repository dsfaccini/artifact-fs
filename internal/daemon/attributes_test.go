package daemon

import (
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/execfile"
	"github.com/cloudflare/artifact-fs/internal/gittest"
	"github.com/cloudflare/artifact-fs/internal/model"
)

// attributesFiles are a remote's files. docs/.gitattributes converts
// docs/readme.md, so the clean-index bootstrap leaves that path to Git.
var attributesFiles = map[string]string{
	".gitattributes":      "*.png binary\n",
	"docs/.gitattributes": "*.md text\n",
	"docs/readme.md":      "readme\n",
	"tracked.txt":         "base\n",
}

// The clean-index bootstrap reads every .gitattributes without lazy fetch.
// Each kind of preparation fetches the .gitattributes blobs that a blobless
// clone lacks, and no other blob, so the mount's bootstrap marks every path
// it may and the first git status reads no file. A sync and an adopted repo
// prepare in the CLI, before the daemon mounts them.
func TestPreparationFetchesTheAttributesTheBootstrapReads(t *testing.T) {
	t.Parallel()
	remote := gittest.PartialCloneRemote(t, attributesFiles)
	for _, tc := range []struct {
		name  string
		async bool
		// add registers the repo, as add-repo does, from dir.
		add func(t *testing.T, svc *Service, dir string) error
	}{
		{name: "sync", add: func(t *testing.T, svc *Service, _ string) error {
			return svc.ctl().AddRepoWithOptions(t.Context(), attributesRepo(remote), AddRepoOptions{})
		}},
		{name: "async", async: true, add: func(t *testing.T, svc *Service, _ string) error {
			return svc.ctl().AddRepoWithOptions(t.Context(), attributesRepo(remote), AddRepoOptions{Async: true})
		}},
		{name: "adopted", add: func(t *testing.T, svc *Service, dir string) error {
			gitDir, worktree := adoptableBloblessWorktree(t, remote, dir)
			cfg := model.RepoConfig{RepoPaths: model.RepoPaths{Name: "repo", GitDir: gitDir, MountPath: worktree}, RepoSpec: model.RepoSpec{AdoptGitDir: true}, Enabled: true}
			return svc.ctl().AddRepo(t.Context(), cfg)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			dir := t.TempDir()
			svc, err := New(ctx, filepath.Join(dir, "state"), discardLogger())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = svc.Close() })
			svc.mount = fakeMount
			if err := tc.add(t, svc, dir); err != nil {
				t.Fatal(err)
			}
			stored, err := svc.registry.GetRepo(ctx, "repo")
			if err != nil {
				t.Fatal(err)
			}
			if !tc.async {
				requireLocalBlobs(t, stored.GitDir, "after add-repo")
			}
			if err := svc.syncRepos(ctx); err != nil {
				t.Fatal(err)
			}
			svc.prepareWorkers.Wait()
			if rt := runningRuntime(svc, stored.ID); rt == nil {
				t.Fatal("repo is not mounted after syncRepos")
			}
			requireLocalBlobs(t, stored.GitDir, "after the mount")
			want := "h .gitattributes\nh docs/.gitattributes\nH docs/readme.md\nh tracked.txt"
			if got := fsmonitorValidEntries(t, stored.GitDir); got != want {
				t.Fatalf("fsmonitor-valid entries = %q, want %q", got, want)
			}
		})
	}
}

// The daemon mounts on its sync loop, which serves every repo, so a mount
// contacts no remote. A .gitattributes blob that no preparation fetched makes
// the bootstrap skip, and the daemon logs why.
func TestMountFetchesNoGitAttributes(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	dir := t.TempDir()
	remote := gittest.PartialCloneRemote(t, attributesFiles)
	logs := &lockedBuffer{}
	svc, err := New(ctx, filepath.Join(dir, "state"), slog.New(slog.NewJSONHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	svc.mount = fakeMount
	if err := svc.ctl().AddRepoWithOptions(ctx, attributesRepo(remote), AddRepoOptions{}); err != nil {
		t.Fatal(err)
	}
	stored, err := svc.registry.GetRepo(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	requireLocalBlobs(t, stored.GitDir, "after add-repo")

	// HEAD moves, while no daemon runs, to a commit whose .gitattributes
	// blob the clone lacks. The remote still serves it.
	work := filepath.Join(dir, "work")
	runCmd(t, "git", "clone", "-q", remote, work)
	commitFile(t, work, ".gitattributes", "*.png binary\n*.txt -text\n")
	runCmd(t, "git", "-C", work, "push", "-q", "origin", "main")
	runCmd(t, "git", "--git-dir", stored.GitDir, "fetch", "-q", "origin")
	runCmd(t, "git", "--git-dir", stored.GitDir, "update-ref", "refs/heads/main", "refs/remotes/origin/main")
	runCmd(t, "git", "--git-dir", stored.GitDir, "read-tree", "HEAD")
	if blobLocal(stored.GitDir, ".gitattributes") {
		t.Fatal("the fetch of the moved HEAD brought its .gitattributes blob")
	}

	if err := svc.syncRepos(ctx); err != nil {
		t.Fatal(err)
	}
	if rt := runningRuntime(svc, stored.ID); rt == nil {
		t.Fatal("repo is not mounted after syncRepos")
	}
	if blobLocal(stored.GitDir, ".gitattributes") {
		t.Fatal("the mount fetched the .gitattributes blob")
	}
	want := "H .gitattributes\nH docs/.gitattributes\nH docs/readme.md\nH tracked.txt"
	if got := fsmonitorValidEntries(t, stored.GitDir); got != want {
		t.Fatalf("fsmonitor-valid entries = %q, want %q", got, want)
	}
	if got := logs.String(); !strings.Contains(got, "git status clean baseline bootstrap skipped") || !strings.Contains(got, "the blob of .gitattributes is not in the object store") {
		t.Fatalf("the daemon did not log why the bootstrap skipped: %s", got)
	}
}

// attributesRepo is the config add-repo registers for a clone of remote.
func attributesRepo(remote string) model.RepoConfig {
	return model.RepoConfig{RepoPaths: model.RepoPaths{Name: "repo"}, RepoSpec: model.RepoSpec{RemoteURL: remote, Branch: "main"}, RefreshPolicy: model.RefreshPolicy{RefreshInterval: time.Hour}, Enabled: true}
}

// adoptableBloblessWorktree makes, under dir, a blobless clone of remote that
// holds no blob, and a linked worktree of it ready for adoption: its index
// holds HEAD's tree, and its gitfile moved out of its empty directory. It
// returns the worktree's git dir and directory.
func adoptableBloblessWorktree(t *testing.T, remote, dir string) (string, string) {
	t.Helper()
	source, worktree := filepath.Join(dir, "source"), filepath.Join(dir, "worktree")
	runCmd(t, "git", "clone", "-q", "--filter=blob:none", "--no-checkout", remote, source)
	runCmd(t, "git", "-C", source, "worktree", "add", "-q", "--no-checkout", "-b", "topic", worktree)
	gitDir := strings.TrimSpace(runCmdOutput(t, "git", "-C", worktree, "rev-parse", "--path-format=absolute", "--git-dir"))
	runCmd(t, "git", "--git-dir", gitDir, "read-tree", "HEAD")
	if err := os.Rename(filepath.Join(worktree, ".git"), filepath.Join(dir, "gitfile")); err != nil {
		t.Fatal(err)
	}
	for path := range attributesFiles {
		if blobLocal(gitDir, path) {
			t.Fatalf("blobless clone holds the blob of %s", path)
		}
	}
	return gitDir, worktree
}

// requireLocalBlobs fails t unless gitDir holds the blob of each
// .gitattributes of attributesFiles at HEAD, and of no other file.
func requireLocalBlobs(t *testing.T, gitDir, when string) {
	t.Helper()
	for _, path := range slices.Sorted(maps.Keys(attributesFiles)) {
		want := filepath.Base(path) == ".gitattributes"
		if got := blobLocal(gitDir, path); got != want {
			t.Fatalf("%s, the blob of %s is local = %v, want %v", when, path, got, want)
		}
	}
}

// blobLocal reports whether the object store of gitDir holds the blob of path
// at HEAD, without fetching it.
func blobLocal(gitDir, path string) bool {
	cmd := exec.Command("git", "--git-dir", gitDir, "cat-file", "-e", "HEAD:"+path)
	cmd.Env = append(os.Environ(), "GIT_NO_LAZY_FETCH=1")
	return cmd.Run() == nil
}

// fsmonitorValidEntries lists gitDir's index through a hook that reports no
// changes. ls-files -f prints an FSMonitor-valid entry in lowercase.
func fsmonitorValidEntries(t *testing.T, gitDir string) string {
	t.Helper()
	hook := filepath.Join(t.TempDir(), "fsmonitor")
	if err := execfile.Write(hook, []byte("#!/bin/sh\nprintf 'token\\0'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(runCmdOutput(t, "git", "--git-dir", gitDir, "-c", "core.fsmonitor="+hook, "ls-files", "-f"))
}
