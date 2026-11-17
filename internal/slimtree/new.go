package slimtree

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/cloudflare/artifact-fs/internal/apfsclone"
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
	track bool
	path  string
}

// newWorktree makes an apfsclone worktree on o.branch, cloned from the base
// worktree, and writes its record. It checks out o.branch when it exists, and
// otherwise makes it at o.from, else at HEAD. It resolves o.from and HEAD in
// the current directory, as git worktree add does. It removes the worktree
// and the branch it made when it fails, except when only the post-checkout
// hook failed, which leaves a complete worktree, as git worktree add does. It
// prints the worktree's path to stdout.
func newWorktree(ctx context.Context, home *state.Home, o newOptions, stdout, stderr io.Writer) error {
	if o.track && o.from == "" {
		return errors.New("--track needs --from, the branch to track")
	}
	out, err := apfsclone.Git(ctx, ".", "check-ref-format", "--branch", o.branch)
	if err != nil {
		return err
	}
	// check-ref-format expands @{-1}.
	if strings.TrimSpace(string(out)) != o.branch {
		return fmt.Errorf("%s is not a branch name", o.branch)
	}
	repo, err := commonDir(ctx, "slimtree new")
	if err != nil {
		return err
	}
	base, err := baseWorktree(ctx, repo)
	if err != nil {
		return err
	}
	dest := filepath.Join(filepath.Dir(base), filepath.Base(base)+"-"+path.Base(o.branch))
	if o.path != "" {
		if dest, err = state.Canonical(o.path); err != nil {
			return err
		}
	}
	lock, err := home.Lock(ctx, dest)
	if err != nil {
		return err
	}
	defer lock.Unlock()

	existing, err := refCommit(ctx, ".", "refs/heads/"+o.branch)
	if err != nil {
		return err
	}
	// start is the commit a new branch starts at, and "" for a branch that
	// exists.
	var start string
	commit, addArgs := o.branch, []string(nil)
	switch {
	case existing != "" && o.from != "":
		return fmt.Errorf("the branch %s exists; drop --from to check it out", o.branch)
	case existing != "":
	default:
		from := o.from
		if from == "" {
			from = "HEAD"
		}
		if start, err = refCommit(ctx, ".", from+"^{commit}"); err != nil {
			return err
		}
		if start == "" {
			return fmt.Errorf("%s names no commit", from)
		}
		commit, addArgs = start, []string{"-b", o.branch}
		if o.track {
			// git worktree add resolves its start in the base, where
			// @{upstream} and @{-1} can name another branch.
			out, err := apfsclone.Git(ctx, ".", "rev-parse", "--symbolic-full-name", o.from)
			if err != nil {
				return err
			}
			if commit = strings.TrimSpace(string(out)); commit == "" {
				return fmt.Errorf("--track needs --from to name a branch, and %s names none", o.from)
			}
			addArgs = []string{"--track", "-b", o.branch}
		}
	}

	result, err := apfsclone.Add(ctx, apfsclone.Options{Base: base, Path: dest, Commit: commit, AddArgs: addArgs,
		Clone: cloneFile, Stdout: stderr, Stderr: stderr})
	var hook *apfsclone.HookError
	var incomplete *apfsclone.IncompleteError
	made := errors.As(err, &incomplete)
	if err == nil || errors.As(err, &hook) {
		made = true
		r := state.Record{Backend: string(apfsClone), Path: dest, CommonDir: repo, Branch: o.branch,
			Created: time.Now().UTC(), SlimtreeVersion: version.String()}
		recordErr := home.Write(r)
		if recordErr == nil {
			fmt.Fprintf(stderr, "slimtree: cloned %d files from %s, wrote %d from Git\n", result.Cloned, base, result.Written)
			fmt.Fprintln(stdout, dest)
			return err
		}
		err = errors.Join(err, fmt.Errorf("writing the record of %s: %w", dest, recordErr))
	}
	if rollbackErr := rollBack(ctx, base, dest, made, o.branch, start, o.track); rollbackErr != nil {
		return errors.Join(err, rollbackErr)
	}
	return err
}

// rollBack removes what a failed new made: the worktree dest when made is
// set, then the branch when start is the commit new made it at and the
// branch still points there, with the tracking config new made when track is
// set. It outlives ctx's cancellation, for at most rollbackTimeout.
func rollBack(ctx context.Context, base, dest string, made bool, branch, start string, track bool) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
	defer cancel()
	if made {
		if _, err := apfsclone.Git(ctx, base, "worktree", "remove", "--force", dest); err != nil {
			return fmt.Errorf("slimtree left the incomplete worktree %s; remove it with git worktree remove --force %s, and the branch %s with git branch -D %s: %w", dest, dest, branch, branch, err)
		}
	}
	if start == "" {
		return nil
	}
	ref := "refs/heads/" + branch
	now, err := refCommit(ctx, base, ref)
	if err != nil {
		return err
	}
	switch now {
	// git worktree add made no branch.
	case "":
		return nil
	case start:
	default:
		return fmt.Errorf("slimtree kept the branch %s, which it made at %s and which now points at %s", branch, start, now)
	}
	if _, err := apfsclone.Git(ctx, base, "update-ref", "-d", ref, start); err != nil {
		return err
	}
	if !track {
		return nil
	}
	_, err = apfsclone.Git(ctx, base, "config", "--remove-section", "branch."+branch)
	return err
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
	if common, top, ok := strings.Cut(strings.TrimSpace(string(out)), "\n"); ok && canonicalIs(common, repo) && canonicalIs(top, base) {
		return base, nil
	}
	return "", fmt.Errorf("the base %s is not the top directory of a worktree of %s; set git config slimtree.base to one", base, repo)
}

// canonicalIs reports whether path, made canonical, is want.
func canonicalIs(path, want string) bool {
	c, err := state.Canonical(path)
	return err == nil && c == want
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
