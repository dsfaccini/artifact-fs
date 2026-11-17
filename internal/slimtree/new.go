package slimtree

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/cloudflare/artifact-fs/internal/apfsclone"
	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/cloudflare/artifact-fs/internal/slimtree/state"
	"github.com/cloudflare/artifact-fs/internal/version"
)

// rollbackTimeout bounds a rollback. A rollback outlives the command's
// cancellation, and the canceled command ignores a second Ctrl-C.
const rollbackTimeout = time.Minute

// cloneFile, when set, clones in place of clonefile(2). Tests set it where
// clonefile(2) does not exist.
var cloneFile apfsclone.Clonefile

// newOptions are new's arguments.
type newOptions struct {
	branch string
	// from is the commit-ish a new branch starts at.
	from string
	// track makes a new branch track from.
	track   bool
	path    string
	backend backend
	// goos is the platform slimtree runs on.
	goos string
}

// newWorktree makes a worktree on o.branch with o.backend, writes its
// record, and prints its path to stdout. An apfsclone worktree is cloned from
// the base worktree, and an artifactfs worktree is mounted. It then runs
// the post-checkout hook, holding no lock, so the hook can run slimtree and a
// long hook stalls no other command. A failed hook leaves the worktree and its
// record, as git worktree add keeps its worktree.
func newWorktree(ctx context.Context, home *state.Home, o newOptions, stdout, stderr io.Writer) error {
	dest, head, err := makeWorktree(ctx, home, o, stdout, stderr)
	if err != nil {
		return err
	}
	return apfsclone.PostCheckout(ctx, dest, head, stderr, stderr)
}

// makeWorktree makes the worktree and its record for newWorktree, holding the
// global lock, and returns its path and the commit it checks out. It checks
// out o.branch when it exists, and otherwise makes it at o.from, else at
// HEAD. It resolves o.from and HEAD in the current directory, as git worktree
// add does. When it fails, it removes what it made.
func makeWorktree(ctx context.Context, home *state.Home, o newOptions, stdout, stderr io.Writer) (string, string, error) {
	if o.track && o.from == "" {
		return "", "", errors.New("--track needs --from, the branch to track")
	}
	out, err := apfsclone.Git(ctx, ".", "check-ref-format", "--branch", o.branch)
	if err != nil {
		return "", "", err
	}
	// check-ref-format expands @{-1}.
	if strings.TrimSpace(string(out)) != o.branch {
		return "", "", fmt.Errorf("%s is not a branch name", o.branch)
	}
	repo, err := commonDir(ctx, "slimtree new")
	if err != nil {
		return "", "", err
	}
	base, err := baseWorktree(ctx, repo)
	if err != nil {
		return "", "", err
	}
	dest := filepath.Join(filepath.Dir(base), filepath.Base(base)+"-"+path.Base(o.branch))
	if o.path != "" {
		if dest, err = state.Canonical(o.path); err != nil {
			return "", "", err
		}
	}
	// artifact-fs names the repo it mounts at dest after dest.
	if o.backend == artifactFS {
		if err := model.ValidateRepoName(filepath.Base(dest)); err != nil {
			return "", "", fmt.Errorf("slimtree cannot mount a worktree at %s: %w", dest, err)
		}
	}
	lock, err := home.Lock(ctx, dest)
	if err != nil {
		return "", "", err
	}
	defer lock.Unlock()

	existing, err := refCommit(ctx, ".", "refs/heads/"+o.branch)
	if err != nil {
		return "", "", err
	}
	rb := rollback{repo: repo, base: base, dest: dest, branch: o.branch}
	commit, addArgs := o.branch, []string(nil)
	switch {
	case existing != "" && o.from != "":
		return "", "", fmt.Errorf("the branch %s exists; drop --from to check it out", o.branch)
	case existing != "":
	default:
		from := o.from
		if from == "" {
			from = "HEAD"
		}
		if rb.start, err = refCommit(ctx, ".", from+"^{commit}"); err != nil {
			return "", "", err
		}
		if rb.start == "" {
			return "", "", fmt.Errorf("%s names no commit", from)
		}
		commit, addArgs = rb.start, []string{"-b", o.branch}
		if o.track {
			// git worktree add resolves its start in the base, where
			// @{upstream} and @{-1} can name another branch.
			out, err := apfsclone.Git(ctx, ".", "rev-parse", "--symbolic-full-name", o.from)
			if err != nil {
				return "", "", err
			}
			commit = strings.TrimSpace(string(out))
			if !strings.HasPrefix(commit, "refs/heads/") && !strings.HasPrefix(commit, "refs/remotes/") {
				return "", "", fmt.Errorf("--track needs --from to name a branch, and %s names none", o.from)
			}
			addArgs = []string{"--track", "-b", o.branch}
			if rb.unset, err = absentKeys(ctx, base, "branch."+o.branch, trackingKeys); err != nil {
				return "", "", err
			}
		}
	}
	// A rollback removes the worktree git registers at dest, which must then
	// be the one new made.
	listed, err := worktreeList(ctx, repo)
	if err != nil {
		return "", "", err
	}
	if _, ok := listed[dest]; ok {
		return "", "", fmt.Errorf("git already registers a worktree at %s", dest)
	}
	// git worktree add makes the directories above dest that are missing.
	for dir := filepath.Dir(dest); !exists(dir); dir = filepath.Dir(dir) {
		rb.missing = dir
	}

	if o.backend == artifactFS {
		head, err := newArtifactFS(ctx, home, &rb, o.goos, commit, addArgs, stderr)
		if err != nil {
			return "", "", err
		}
		fmt.Fprintln(stdout, dest)
		return dest, head, nil
	}
	result, err := apfsclone.Add(ctx, apfsclone.Options{Base: base, Path: dest, Commit: commit, AddArgs: addArgs,
		Clone: cloneFile, Stdout: stderr, Stderr: stderr})
	var incomplete *apfsclone.IncompleteError
	rb.added = err == nil || errors.As(err, &incomplete)
	if err == nil {
		r := state.Record{Backend: string(apfsClone), Path: dest, CommonDir: repo, Branch: o.branch,
			Created: time.Now().UTC(), SlimtreeVersion: version.String()}
		if err = home.Write(r); err == nil {
			fmt.Fprintf(stderr, "slimtree: cloned %d files from %s, wrote %d from Git\n", result.Cloned, base, result.Written)
			fmt.Fprintln(stdout, dest)
			return dest, result.Head, nil
		}
		err = fmt.Errorf("writing the record of %s: %w", dest, err)
	}
	if rollbackErr := rb.run(ctx); rollbackErr != nil {
		return "", "", errors.Join(err, rollbackErr)
	}
	return "", "", err
}

// trackingKeys are the keys of a branch's config section that git branch
// --track can write.
var trackingKeys = []string{"remote", "merge", "rebase"}

// absentKeys returns those of keys that the config section section in dir
// does not set.
func absentKeys(ctx context.Context, dir, section string, keys []string) ([]string, error) {
	var absent []string
	for _, k := range keys {
		_, err := apfsclone.Git(ctx, dir, "config", "--get", section+"."+k)
		var exit *exec.ExitError
		switch {
		case errors.As(err, &exit) && exit.ExitCode() == 1:
			absent = append(absent, k)
		case err != nil:
			return nil, err
		}
	}
	return absent, nil
}

// rollback is what a failed new can have made.
type rollback struct {
	// repo is the repository's common dir, and base its base worktree.
	repo, base, dest string
	// missing is the highest directory above dest that did not exist, or "".
	missing string
	branch  string
	// start is the commit new made branch at, or "" for a branch that
	// existed.
	start string
	// unset are the keys of branch's config section that were not set before
	// new made branch track another.
	unset []string
	// added is set when Add reports that its git worktree add registered
	// dest.
	added bool
}

// run removes what a failed new made: the worktree new registered, the
// directories above it that git worktree add made, and the branch while it
// still points at rb.start, with the tracking config new wrote. It outlives
// ctx's cancellation, for at most rollbackTimeout.
func (rb rollback) run(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
	defer cancel()
	// Whether git registered dest decides, not Add's error: a Ctrl-C can land
	// after git worktree add registered it, before Go reaped git.
	listed, err := worktreeList(ctx, rb.repo)
	if err != nil {
		return fmt.Errorf("slimtree cannot tell whether git registered %s, so it left it and the branch %s: %w", rb.dest, rb.branch, err)
	}
	// Another process can register dest after makeWorktree checked it, and
	// outside the global lock. A registration on another branch is never
	// new's: new never detaches, and git checks out a branch only once. One on
	// a branch that existed can be another process's, which git refused to
	// check out again for new, unless Add reports that it registered dest.
	switch w, ok := listed[rb.dest]; {
	case !ok || w.branch != rb.branch:
	case rb.start == "" && !rb.added:
		return fmt.Errorf("slimtree kept the worktree %s on %s, which another process can have made; if new made it, remove it with git worktree remove --force --force %s", rb.dest, rb.branch, rb.dest)
	default:
		// git worktree add locks its worktree as initializing until it ends,
		// so one that was stopped leaves the lock.
		if _, err := apfsclone.Git(ctx, rb.base, "worktree", "remove", "--force", "--force", rb.dest); err != nil {
			return fmt.Errorf("slimtree left the incomplete worktree %s; remove it with git worktree remove --force --force %s, and the branch %s with git branch -D %s: %w", rb.dest, rb.dest, rb.branch, rb.branch, err)
		}
	}
	for dir := filepath.Dir(rb.dest); rb.missing != ""; dir = filepath.Dir(dir) {
		// os.Remove removes only an empty directory.
		if os.Remove(dir) != nil || dir == rb.missing {
			break
		}
	}
	if rb.start == "" {
		return nil
	}
	ref := "refs/heads/" + rb.branch
	now, err := refCommit(ctx, rb.base, ref)
	if err != nil {
		return err
	}
	switch now {
	// git worktree add made no branch.
	case "":
		return nil
	case rb.start:
	default:
		return fmt.Errorf("slimtree kept the branch %s, which it made at %s and which now points at %s", rb.branch, rb.start, now)
	}
	if _, err := apfsclone.Git(ctx, rb.base, "update-ref", "-d", ref, rb.start); err != nil {
		return err
	}
	for _, k := range rb.unset {
		_, err := apfsclone.Git(ctx, rb.base, "config", "--unset-all", "branch."+rb.branch+"."+k)
		// git config exits 5 for a key it does not set.
		var exit *exec.ExitError
		if err != nil && !(errors.As(err, &exit) && exit.ExitCode() == 5) {
			return err
		}
	}
	return nil
}

// exists reports whether path exists.
func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// baseWorktree returns the worktree of repo, a canonical common dir, whose
// files new clones: the one git config slimtree.base names, else the main
// worktree.
func baseWorktree(ctx context.Context, repo string) (string, error) {
	out, err := apfsclone.Git(ctx, ".", "config", "--type=path", "--get", "slimtree.base")
	base := strings.TrimSpace(string(out))
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit) && exit.ExitCode() == 1:
		// Git's main worktree is the directory that holds the common dir.
		if filepath.Base(repo) != ".git" {
			return "", fmt.Errorf("%s has no main worktree to clone; set git config slimtree.base to a worktree of it", repo)
		}
		base = filepath.Dir(repo)
	case err != nil:
		return "", err
	case !filepath.IsAbs(base):
		return "", fmt.Errorf("slimtree.base %s is not an absolute path", base)
	}
	if base, err = state.Canonical(base); err != nil {
		return "", err
	}
	out, err = apfsclone.Git(ctx, base, "rev-parse", "--path-format=absolute", "--git-common-dir", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("the base %s: %w", base, err)
	}
	if common, top, ok := strings.Cut(strings.TrimSpace(string(out)), "\n"); ok && state.SamePath(common, repo) && state.SamePath(top, base) {
		return base, nil
	}
	return "", fmt.Errorf("the base %s is not the top directory of a worktree of %s; set git config slimtree.base to one", base, repo)
}

// refCommit returns the object ID that rev in dir names, or "" when it names
// none.
func refCommit(ctx context.Context, dir, rev string) (string, error) {
	out, err := apfsclone.Git(ctx, dir, "rev-parse", "--verify", "--quiet", "--end-of-options", rev)
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
