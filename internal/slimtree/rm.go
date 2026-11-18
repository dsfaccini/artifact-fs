package slimtree

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/cloudflare/artifact-fs/internal/apfsclone"
	"github.com/cloudflare/artifact-fs/internal/slimtree/doctor"
	"github.com/cloudflare/artifact-fs/internal/slimtree/state"
)

// goneListed, when set, runs once rm has listed the worktrees of a gone
// worktree's repository. Tests use it to cancel the command there.
var goneListed func()

// removeWorktree removes the worktree arg names, with its ignored files, as
// git worktree remove does, and its record, then prunes. removeArtifactFS
// removes an artifactfs worktree. Without o.force, it refuses an apfsclone
// worktree in which git status lists a change or an untracked file, whatever
// the config hides, and one whose directory is gone, which can be on a volume
// that is not mounted. With o.check, it stops after the refusals. It first
// writes the worktree's HEAD to a recovery ref in the repository, and never
// deletes a branch.
func removeWorktree(ctx context.Context, home *state.Home, probe doctor.Probe, goos, arg string, o rmOptions, stdout, stderr io.Writer) error {
	lock, err := home.Lock(ctx, "")
	if err != nil {
		return err
	}
	defer lock.Unlock()
	r, err := home.Resolve(arg)
	if err != nil {
		return err
	}
	switch r.Backend {
	case string(artifactFS):
		if err := removeArtifactFS(ctx, home, probe, goos, r, o, stdout, stderr); err != nil || o.check {
			return err
		}
		prune(ctx, home, time.Now()).write(stderr)
		return nil
	case string(apfsClone):
	default:
		return fmt.Errorf("slimtree rm %s: unknown backend %q", r.Path, r.Backend)
	}
	if len(o.excludes) > 0 {
		return fmt.Errorf("--exclude leaves files out of an artifactfs worktree's archive, and rm archives no files of the apfsclone worktree %s", r.Path)
	}
	if err := doctor.Require(probe, goos, r.Backend, "rm", doctor.Needs{}); err != nil {
		return err
	}

	if err := refuseMissingRepo(r, o); err != nil {
		return err
	}
	head, registered := "", true
	switch _, err := os.Lstat(r.Path); {
	case errors.Is(err, fs.ErrNotExist):
		if !o.force {
			return fmt.Errorf("%s is gone; if its volume is not mounted, mount it and rerun. slimtree rm --force removes git's registration of it and its record", r.Path)
		}
		listed, err := worktreeList(ctx, r.CommonDir)
		if goneListed != nil {
			goneListed()
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if errors.Is(err, errWorktreeListTimeout) {
			return err
		}
		// A repository git cannot read holds no registration. One that is
		// missing gets here only with --force twice.
		var w gitWorktree
		w, registered = listed[r.Path]
		head = w.head
	case err != nil:
		return err
	default:
		out, err := apfsclone.Git(ctx, r.Path, "rev-parse", "--path-format=absolute", "--git-common-dir", "--show-toplevel", "HEAD^{commit}")
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		if err != nil {
			return fmt.Errorf("slimtree removes nothing at %s, which is not a worktree of %s: %w", r.Path, r.CommonDir, err)
		}
		if len(lines) != 3 || !state.SamePath(lines[0], r.CommonDir) || !state.SamePath(lines[1], r.Path) {
			return fmt.Errorf("slimtree removes nothing at %s, which is not a worktree of %s", r.Path, r.CommonDir)
		}
		head = lines[2]
		// git worktree remove hides what the config hides, such as untracked
		// files under status.showUntrackedFiles=no, and then deletes them.
		// --no-optional-locks keeps git status from refreshing the index
		// under index.lock, so the check writes nothing and stays cancelable.
		status, err := apfsclone.Git(ctx, r.Path, "--no-optional-locks", "status", "--porcelain", "--untracked-files=normal", "--ignore-submodules=none")
		if err != nil {
			return err
		}
		if len(status) > 0 && !o.force {
			return fmt.Errorf("git status lists changes in %s; commit them, or discard them with slimtree rm --force", r.Path)
		}
	}
	if o.check {
		fmt.Fprintf(stdout, "%s can be removed\n", r.Path)
		return nil
	}

	ref := ""
	if head != "" {
		r.Head = head
		a, err := home.Archive(r)
		if err != nil {
			return err
		}
		ref = a.Ref
		if _, err := gitWrite(ctx, r.CommonDir, "update-ref", ref, head, ""); err != nil {
			return err
		}
	}
	if registered {
		args := []string{"worktree", "remove", r.Path}
		if o.force {
			args = []string{"worktree", "remove", "--force", r.Path}
		}
		if _, err := gitWrite(ctx, r.CommonDir, args...); err != nil {
			return err
		}
	}
	if err := home.Remove(r.Path); err != nil {
		return fmt.Errorf("removed %s, but not its record: %w", r.Path, err)
	}
	if ref == "" {
		fmt.Fprintf(stdout, "removed %s\n", r.Path)
	} else {
		fmt.Fprintf(stdout, "removed %s; %s keeps its HEAD %s\n", r.Path, ref, head)
	}
	prune(ctx, home, time.Now()).write(stderr)
	return nil
}
