package slimtree

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/cloudflare/artifact-fs/internal/apfsclone"
	"github.com/cloudflare/artifact-fs/internal/daemon"
	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/cloudflare/artifact-fs/internal/overlay"
	"github.com/cloudflare/artifact-fs/internal/slimtree/doctor"
	"github.com/cloudflare/artifact-fs/internal/slimtree/state"
)

// stateArchiveName is the file in an artifactfs worktree's archive that
// holds its state root and its git dir.
const stateArchiveName = "state.tar.gz"

// rmOptions are rm's flags.
type rmOptions struct {
	// force discards the changes git status lists, and removes a worktree
	// whose directory is gone or that no daemon serves.
	force bool
	// check runs only the refusals, and changes nothing.
	check bool
	// excludes are globs. An artifactfs worktree's archive leaves out the
	// overlay file of each path with a component that one of them matches.
	excludes []string
}

// checkExcludes refuses a pattern that path.Match cannot match against one
// path component.
func checkExcludes(excludes []string) error {
	for _, p := range excludes {
		if strings.Contains(p, "/") {
			return fmt.Errorf("--exclude %s: a pattern matches one path component, so it holds no slash", p)
		}
		if _, err := path.Match(p, ""); err != nil {
			return fmt.Errorf("--exclude %s: %w", p, err)
		}
	}
	return nil
}

// removeArtifactFS removes the artifactfs worktree r for removeWorktree,
// which holds the global lock. Only through the mount can git status tell
// whether r holds changes, so without force it refuses a worktree that its
// recorded daemon does not serve, and one in which git status lists
// anything. It refuses, with force too, while processes other than the
// daemon use the mount: a stop would leave them in a dead mount. It then
// stops the daemon and unmounts its mount, and removes the worktree with
// finishRemoval. A failure after the stop leaves a worktree that rm --force
// removes.
func removeArtifactFS(ctx context.Context, home *state.Home, probe doctor.Probe, goos string, r state.Record, o rmOptions, stdout, stderr io.Writer) error {
	if err := healRemount(ctx, home, &r, !o.check); err != nil {
		return err
	}
	alive, err := daemonAlive(r)
	if err != nil {
		return err
	}
	id, err := identify(r.Path)
	if err != nil {
		return err
	}
	if id != "" && string(id) != r.MountIdentity {
		return fmt.Errorf("%s holds another mount than the one slimtree recorded; slimtree left it", r.Path)
	}
	if err := doctor.Require(probe, goos, r.Backend, "rm", doctor.Needs{Mount: id != "" && !o.check}); err != nil {
		return err
	}
	switch {
	case alive && id != "":
		if !o.force {
			if err := checkMounted(ctx, r); err != nil {
				return err
			}
			// --no-optional-locks keeps git status from refreshing the index.
			status, err := apfsclone.Git(ctx, r.Path, "--no-optional-locks", "status", "--porcelain", "--untracked-files=normal", "--ignore-submodules=none")
			if err != nil {
				return err
			}
			if len(status) > 0 {
				return fmt.Errorf("git status lists changes in %s; commit them, or discard them with slimtree rm --force, which archives its files", r.Path)
			}
		}
		if err := refuseHolders(ctx, r); err != nil {
			return err
		}
	case o.force:
	case id == "" && !exists(r.Path):
		// Nothing is mounted at r.Path, so the lstat reaches no mount.
		return fmt.Errorf("%s is gone; if its volume is not mounted, mount it and rerun. slimtree rm --force removes git's registration of it and its record, and archives its files", r.Path)
	default:
		return fmt.Errorf("the daemon of %s does not serve it, so git status cannot check it for changes; slimtree resume %s mounts it, or slimtree rm --force removes it unchecked and archives its files", r.Path, r.Path)
	}
	if o.check {
		fmt.Fprintf(stdout, "%s can be removed\n", r.Path)
		return nil
	}

	if err := stopDaemon(ctx, r); err != nil {
		return err
	}
	// The overlay and the git dir change no more, so the archive keeps them
	// as they are. Another daemon on the state root could still change them.
	if pid, running, err := daemon.Running(r.StateDir); err != nil {
		return err
	} else if running {
		return fmt.Errorf("pid %d, a daemon slimtree did not record, serves %s; stop it, then rm again", pid, r.StateDir)
	}
	if err := finishRemoval(ctx, home, r, o.excludes, stdout, stderr); err != nil {
		return fmt.Errorf("%w; slimtree stopped %s, and slimtree rm --force %s removes it", err, r.Path, r.Path)
	}
	return nil
}

// finishRemoval removes r, an artifactfs worktree that no daemon serves and
// nothing is mounted at. It archives r's record and, in stateArchiveName, its
// state root and git dir, writes its HEAD to the recovery ref, and puts its
// gitfile back. git worktree remove --force then removes the worktree and
// its git dir. Last, it removes r's record and prunes. It refuses a mount
// point that holds files: no mount serves them and no archive keeps them,
// and git worktree remove would delete them. Each step leaves a state that
// a later run finishes.
func finishRemoval(ctx context.Context, home *state.Home, r state.Record, excludes []string, stdout, stderr io.Writer) error {
	gitfile := filepath.Join(home.RecordDir(r.Path), gitfileName)
	entries, err := os.ReadDir(r.Path)
	gone := errors.Is(err, fs.ErrNotExist)
	switch {
	case gone:
	case err != nil:
		return err
	// An earlier run that put the gitfile back leaves it alone.
	case len(entries) > 1 || len(entries) == 1 && (entries[0].Name() != ".git" || exists(gitfile)):
		return fmt.Errorf("%s holds files that no mount serves, which git worktree remove would delete; move them away", r.Path)
	}
	listed, err := worktreeList(ctx, r.CommonDir)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if err != nil {
		// A repository that is gone holds no registration.
		if _, statErr := os.Stat(r.CommonDir); !errors.Is(statErr, fs.ErrNotExist) {
			return err
		}
	}
	w, registered := listed[r.Path]
	r.Head = w.head
	a, err := home.Archive(r)
	if err != nil {
		return err
	}
	if err := a.WriteFile(stateArchiveName, func(out io.Writer) error { return writeStateArchive(ctx, out, r, excludes, stderr) }); err != nil {
		return fmt.Errorf("archiving the state of %s: %w", r.Path, err)
	}
	if r.Head != "" {
		if _, err := apfsclone.Git(ctx, r.CommonDir, "update-ref", a.Ref, r.Head, ""); err != nil {
			return err
		}
		if got, err := refCommit(ctx, r.CommonDir, a.Ref); err != nil {
			return err
		} else if got != r.Head {
			return fmt.Errorf("the recovery ref %s points at %q, not at %s, the HEAD of %s", a.Ref, got, r.Head, r.Path)
		}
	}
	switch {
	case registered:
		if !gone && exists(gitfile) {
			if err := moveFile(gitfile, filepath.Join(r.Path, ".git")); err != nil {
				return err
			}
		} else if !gone && len(entries) == 0 {
			// git worktree remove refuses a directory without its gitfile,
			// and removes the registration of a worktree that is gone.
			if err := os.Remove(r.Path); err != nil {
				return err
			}
		}
		if w.locked {
			if _, err := apfsclone.Git(ctx, r.CommonDir, "worktree", "unlock", r.Path); err != nil {
				return err
			}
		}
		if _, err := apfsclone.Git(ctx, r.CommonDir, "worktree", "remove", "--force", r.Path); err != nil {
			return err
		}
	case !gone:
		// git registers no worktree at r.Path, so slimtree removes the mount
		// point, with a gitfile an earlier run put back. os.Remove removes
		// only a file or an empty directory.
		if err := os.Remove(filepath.Join(r.Path, ".git")); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := os.Remove(r.Path); err != nil {
			return err
		}
	}
	if err := home.Remove(r.Path); err != nil {
		return fmt.Errorf("removed %s, but not its record: %w", r.Path, err)
	}
	files := filepath.Join(a.Dir, stateArchiveName)
	if r.Head == "" {
		fmt.Fprintf(stdout, "removed %s; %s keeps its files\n", r.Path, files)
	} else {
		fmt.Fprintf(stdout, "removed %s; %s keeps its HEAD %s, and %s its files\n", r.Path, a.Ref, r.Head, files)
	}
	return nil
}

// writeStateArchive writes to w a gzipped tar of r's state root, below
// state/, and of its git dir, below gitdir/. It leaves out the overlay file
// of each path with a component that one of excludes matches, and each file
// that is not a regular file, a directory or a symlink, such as the
// daemon's control socket.
func writeStateArchive(ctx context.Context, w io.Writer, r state.Record, excludes []string, stderr io.Writer) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	if err := addTree(ctx, tw, r.StateDir, "state", excludedOverlayFiles(ctx, r.StateDir, excludes, stderr)); err != nil {
		return err
	}
	if err := addTree(ctx, tw, r.GitDir, "gitdir", nil); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// excludedOverlayFiles returns, relative to stateDir, the overlay file of
// each overlay entry in stateDir whose path has a component that one of
// excludes matches. The overlay keeps each file under a generated name, so
// only its database tells the file's path. It warns on stderr about each
// database it cannot read, and then the archive keeps every file.
func excludedOverlayFiles(ctx context.Context, stateDir string, excludes []string, stderr io.Writer) map[string]bool {
	excluded := map[string]bool{}
	if len(excludes) == 0 {
		return excluded
	}
	// Glob fails only on a malformed pattern.
	dbs, _ := filepath.Glob(filepath.Join(stateDir, "overlays", "*", "meta.sqlite"))
	for _, db := range dbs {
		backing, err := overlay.ReadBackingPaths(ctx, model.RepoPaths{OverlayDBPath: db})
		if err != nil {
			fmt.Fprintf(stderr, "slimtree: the archive keeps every overlay file, because slimtree cannot read %s: %v\n", db, err)
			continue
		}
		for p, file := range backing {
			if !matchesComponent(p, excludes) {
				continue
			}
			if rel, err := filepath.Rel(stateDir, file); err == nil && filepath.IsLocal(rel) {
				excluded[rel] = true
			}
		}
	}
	return excluded
}

// matchesComponent reports whether a component of the slash-separated p
// matches one of patterns.
func matchesComponent(p string, patterns []string) bool {
	for component := range strings.SplitSeq(p, "/") {
		for _, pattern := range patterns {
			if ok, _ := path.Match(pattern, component); ok {
				return true
			}
		}
	}
	return false
}

// addTree writes the tree at root to tw below prefix, without the paths
// relative to root that skip holds, and without each file that is not a
// regular file, a directory or a symlink. A root that is gone adds nothing.
func addTree(ctx context.Context, tw *tar.Writer, root, prefix string, skip map[string]bool) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if p == root && errors.Is(err, fs.ErrNotExist) {
			return filepath.SkipAll
		}
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil || skip[rel] {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		link := ""
		switch {
		case info.Mode().IsRegular(), info.IsDir():
		case info.Mode()&fs.ModeSymlink != 0:
			if link, err = os.Readlink(p); err != nil {
				return err
			}
		default:
			return nil
		}
		h, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		h.Name = path.Join(prefix, filepath.ToSlash(rel))
		if info.IsDir() {
			h.Name += "/"
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
}
