package slimtree

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"github.com/cloudflare/artifact-fs/internal/apfsclone"
	"github.com/cloudflare/artifact-fs/internal/slimtree/doctor"
	"github.com/cloudflare/artifact-fs/internal/slimtree/state"
)

// goneListed, when set, runs once rm has listed the worktrees of a gone
// worktree's repository. Tests use it to cancel the command there.
var goneListed func()

// removeWorktree removes the apfsclone worktree arg names, with its ignored
// files, as git worktree remove does, and its record. Without force, it
// refuses a worktree in which git status lists a change or an untracked file,
// whatever the config hides, and a worktree whose directory is gone, which
// can be on a volume that is not mounted. It first writes the worktree's HEAD
// to a recovery ref in the repository, and never deletes a branch.
func removeWorktree(ctx context.Context, home *state.Home, probe doctor.Probe, goos, arg string, force bool, stdout io.Writer) error {
	lock, err := home.Lock(ctx, "")
	if err != nil {
		return err
	}
	defer lock.Unlock()
	r, err := home.Resolve(arg)
	if err != nil {
		return err
	}
	if r.Backend != string(apfsClone) {
		return notInThisRelease("slimtree rm "+r.Path, r.Backend)
	}
	if err := doctor.Require(probe, goos, r.Backend, "rm", doctor.Needs{}); err != nil {
		return err
	}

	head, registered := "", true
	switch _, err := os.Lstat(r.Path); {
	case errors.Is(err, fs.ErrNotExist):
		if !force {
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
		// A repository git cannot read holds no registration.
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
		status, err := apfsclone.Git(ctx, r.Path, "status", "--porcelain", "--untracked-files=normal", "--ignore-submodules=none")
		if err != nil {
			return err
		}
		if len(status) > 0 && !force {
			return fmt.Errorf("git status lists changes in %s; commit them, or discard them with slimtree rm --force", r.Path)
		}
	}

	ref := ""
	if head != "" {
		if ref, err = home.Archive(r); err != nil {
			return err
		}
		if _, err := gitWrite(ctx, r.CommonDir, "update-ref", ref, head, ""); err != nil {
			return err
		}
	}
	if registered {
		args := []string{"worktree", "remove", r.Path}
		if force {
			args = []string{"worktree", "remove", "--force", r.Path}
		}
		if _, err := apfsclone.Git(ctx, r.CommonDir, args...); err != nil {
			return err
		}
	}
	if err := home.Remove(r.Path); err != nil {
		return fmt.Errorf("removed %s, but not its record: %w", r.Path, err)
	}
	if ref == "" {
		fmt.Fprintf(stdout, "removed %s\n", r.Path)
		return nil
	}
	fmt.Fprintf(stdout, "removed %s; %s keeps its HEAD %s\n", r.Path, ref, head)
	return nil
}
