// Package apfsclone makes a Git worktree whose files are APFS clones of a base
// worktree's files.
//
// Add runs git worktree add --no-checkout, fills the index with read-tree
// HEAD, then writes the files:
//
//   - A tracked path is cloned from the base when it is a regular file (mode
//     100644 or 100755), the base's HEAD has the same mode and blob there, and
//     git status in the base does not list it.
//   - checkout-index writes every other entry (changed blob, file modified in
//     the base, symlink, gitlink) and every failed clone. It writes each
//     .gitattributes first and never clones one, because it reads the work
//     tree's .gitattributes while it writes the other paths.
//   - Each clone must be a regular file with its entry's executable bit. A
//     clone that is not is rewritten from Git. diff-files ignores the
//     executable bit under core.fileMode=false, which a plain checkout still
//     writes.
//   - update-index --refresh then hashes every file, because read-tree records
//     no stat data. A file that does not match its blob is rewritten from Git,
//     and diff-files must then be empty. Refresh hashes through Git's clean
//     filter, so it compares raw bytes only for a checkout that converts no
//     files.
//
// Clones keep the base files' mtimes, unlike a plain checkout. A file
// rewritten in full (an atomic save, git checkout, a branch switch) becomes a
// private copy and stays one.
package apfsclone

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/cloudflare/artifact-fs/internal/auth"
	"github.com/cloudflare/artifact-fs/internal/gitstore"
)

// RegistrationLock is the file in the common git dir that Add flocks from git
// worktree add through read-tree, so concurrent Adds register one at a time.
// Stock Git can fail a registration that overlaps another one. A plain git
// worktree add takes no part in the lock. Keep the name: other tools that
// make apfsclone worktrees take the same lock.
const RegistrationLock = "apfsclone-registration.lock"

// A Clonefile clones the file src to dst, which must not exist, as
// clonefile(2) with CLONE_NOFOLLOW does: a symlink at src is cloned as a
// symlink.
type Clonefile func(src, dst string) error

// Options are Add's arguments.
type Options struct {
	// Base is the top directory of the worktree whose files Add clones.
	Base string
	// Path is the new worktree's path.
	Path string
	// Commit is the commit-ish the new worktree checks out.
	Commit string
	// AddArgs are the options of git worktree add, such as -b <branch> or
	// --track, which Add passes before Path.
	AddArgs []string
	// Clone clones one file. nil uses clonefile(2), which only macOS has.
	Clone Clonefile
	// Stdout and Stderr receive the output of git worktree add and of the
	// post-checkout hook.
	Stdout, Stderr io.Writer
}

// Result counts the files of a worktree that Add made.
type Result struct {
	// Cloned counts the files that are clones of the base's files.
	Cloned int
	// Written counts the entries that Git wrote.
	Written int
}

// An IncompleteError is a failure after git worktree add registered the
// worktree, which then exists but is incomplete.
type IncompleteError struct {
	Path string
	Err  error
}

func (e *IncompleteError) Error() string {
	return fmt.Sprintf("apfsclone could not fill %s: %v. The worktree is incomplete", e.Path, e.Err)
}

func (e *IncompleteError) Unwrap() error { return e.Err }

// entry is a tree entry's mode and object ID.
type entry struct{ mode, oid string }

// tree is a commit's tree entries, by path, and their paths in ls-tree order.
type tree struct {
	entries map[string]entry
	paths   []string
}

// Add makes the worktree o.Path from o.Base, as git worktree add with o.AddArgs
// and o.Commit does, with the base's files cloned where Add can show they
// match. Before it creates anything, it refuses a commit-ish that names no
// commit, and a base and a worktree parent that are not on one APFS volume.
// After the worktree is filled it runs the post-checkout hook with the
// arguments git worktree add passes it, and returns the hook's failure.
func Add(ctx context.Context, o Options) (Result, error) {
	clone := o.Clone
	if clone == nil {
		clone = clonefile
	}
	base, err := filepath.Abs(o.Base)
	if err != nil {
		return Result{}, err
	}
	dest, err := filepath.Abs(o.Path)
	if err != nil {
		return Result{}, err
	}
	refuse := func(err error) error {
		return fmt.Errorf("apfsclone cannot clone %s into %s: %w", base, dest, err)
	}
	baseHead, err := treeEntries(ctx, base)
	if err != nil {
		return Result{}, refuse(err)
	}
	dirty, err := dirtyPaths(ctx, base)
	if err != nil {
		return Result{}, refuse(err)
	}
	if _, err := git(ctx, base, nil, "rev-parse", "--verify", "--quiet", o.Commit+"^{commit}"); err != nil {
		return Result{}, refuse(fmt.Errorf("%s names no commit", o.Commit))
	}
	if err := preflight(clone, base, dest, baseHead); err != nil {
		return Result{}, refuse(err)
	}
	out, err := git(ctx, base, nil, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return Result{}, refuse(err)
	}
	commonDir := strings.TrimSpace(string(out))

	if err := register(ctx, o, base, dest, filepath.Join(commonDir, RegistrationLock)); err != nil {
		return Result{}, err
	}
	out, err = git(ctx, dest, nil, "rev-parse", "HEAD")
	if err != nil {
		return Result{}, &IncompleteError{dest, err}
	}
	head := strings.TrimSpace(string(out))
	result, err := populate(ctx, clone, base, dest, baseHead, dirty)
	if err != nil {
		return Result{}, &IncompleteError{dest, err}
	}
	// The lock is released, so the hook can make a worktree. The hook runs in
	// this process's group, as git worktree add runs it, so it can read the
	// terminal. A background group would stop it at the first read.
	hook := exec.CommandContext(ctx, "git", "-C", dest, "hook", "run", "--ignore-missing", "post-checkout", "--",
		strings.Repeat("0", len(head)), head, "1")
	hook.Cancel = func() error { return hook.Process.Signal(syscall.SIGTERM) }
	hook.Stdout, hook.Stderr = o.Stdout, o.Stderr
	if err := hook.Run(); err != nil {
		return result, fmt.Errorf("the post-checkout hook in %s failed: %w", dest, err)
	}
	return result, nil
}

// register runs git worktree add --no-checkout and read-tree HEAD holding the
// flock on lockPath. A failed read-tree returns an IncompleteError.
func register(ctx context.Context, o Options, base, dest, lockPath string) error {
	unlock, err := lockFile(ctx, lockPath)
	if err != nil {
		return fmt.Errorf("apfsclone cannot clone %s into %s: %w", base, dest, err)
	}
	defer unlock()
	args := append([]string{"-C", base, "worktree", "add", "--no-checkout"}, o.AddArgs...)
	add := exec.CommandContext(ctx, "git", append(args, dest, o.Commit)...)
	configureGit(add)
	add.Stdout, add.Stderr = o.Stdout, o.Stderr
	if err := add.Run(); err != nil {
		return fmt.Errorf("git worktree add %s: %w", dest, err)
	}
	if _, err := git(ctx, dest, nil, "read-tree", "HEAD"); err != nil {
		return &IncompleteError{dest, err}
	}
	return nil
}

// lockFile takes an exclusive flock on path, which it creates when missing.
// The returned func releases it. A canceled ctx ends the wait.
func lockFile(ctx context.Context, path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o666)
	if err != nil {
		return nil, err
	}
	acquired := make(chan error, 1)
	go func() {
		for {
			err := unix.Flock(int(f.Fd()), unix.LOCK_EX)
			if !errors.Is(err, unix.EINTR) {
				acquired <- err
				return
			}
		}
	}()
	select {
	case err := <-acquired:
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("flock %s: %w", path, err)
		}
		return func() { f.Close() }, nil
	case <-ctx.Done():
		// Closing the file releases a flock that lands later.
		go func() {
			<-acquired
			f.Close()
		}()
		return nil, ctx.Err()
	}
}

// preflight refuses a base and a parent of dest that are not on one volume,
// or whose filesystem cannot clone, before anything is created. It probes
// with a clone of the base's first regular file into dest's nearest existing
// ancestor.
func preflight(clone Clonefile, base, dest string, baseHead tree) error {
	parent := filepath.Dir(dest)
	for !exists(parent) {
		parent = filepath.Dir(parent)
	}
	var baseStat, parentStat unix.Stat_t
	if err := unix.Stat(base, &baseStat); err != nil {
		return &os.PathError{Op: "stat", Path: base, Err: err}
	}
	if err := unix.Stat(parent, &parentStat); err != nil {
		return &os.PathError{Op: "stat", Path: parent, Err: err}
	}
	if baseStat.Dev != parentStat.Dev {
		return fmt.Errorf("%s is on another volume than %s", parent, base)
	}
	source := ""
	for _, p := range baseHead.paths {
		if !regular(baseHead.entries[p].mode) {
			continue
		}
		candidate := filepath.Join(base, filepath.FromSlash(p))
		if fi, err := os.Lstat(candidate); err == nil && fi.Mode().IsRegular() {
			source = candidate
			break
		}
	}
	if source == "" {
		return nil
	}
	probe := filepath.Join(parent, ".apfsclone-probe-"+rand.Text())
	err := clone(source, probe)
	switch {
	case err == nil:
		return os.Remove(probe)
	case errors.Is(err, unix.ENOTSUP), errors.Is(err, unix.EOPNOTSUPP):
		return fmt.Errorf("the filesystem of %s does not support clones; it is not APFS", base)
	case errors.Is(err, unix.EXDEV):
		return fmt.Errorf("%s is on another volume than %s", parent, base)
	}
	return fmt.Errorf("cloning %s into %s failed: %w", source, parent, err)
}

// populate writes dest's files, cloning from base the paths that baseHead and
// dirty show to match, and rewrites from Git each clone that does not match
// its entry.
func populate(ctx context.Context, clone Clonefile, base, dest string, baseHead tree, dirty map[string]bool) (Result, error) {
	target, err := treeEntries(ctx, dest)
	if err != nil {
		return Result{}, err
	}
	var attributes, toClone, write []string
	for _, p := range target.paths {
		if path.Base(p) == ".gitattributes" {
			attributes = append(attributes, p)
			continue
		}
		e := target.entries[p]
		if b, ok := baseHead.entries[p]; ok && regular(e.mode) && b == e && !dirty[p] {
			toClone = append(toClone, p)
		} else {
			write = append(write, p)
		}
	}
	if err := checkoutIndex(ctx, dest, attributes); err != nil {
		return Result{}, err
	}

	made := map[string]bool{}
	var cloned []string
	for _, p := range toClone {
		if parent := path.Dir(p); parent != "." && !made[parent] {
			if err := os.MkdirAll(filepath.Join(dest, filepath.FromSlash(parent)), 0o777); err != nil {
				return Result{}, err
			}
			made[parent] = true
		}
		if clone(filepath.Join(base, filepath.FromSlash(p)), filepath.Join(dest, filepath.FromSlash(p))) == nil {
			cloned = append(cloned, p)
		} else {
			write = append(write, p)
		}
	}
	if err := checkoutIndex(ctx, dest, write); err != nil {
		return Result{}, err
	}
	misshapen, err := misshapenClones(dest, target, cloned)
	if err != nil {
		return Result{}, err
	}
	if err := checkoutIndex(ctx, dest, misshapen); err != nil {
		return Result{}, err
	}

	if _, err := git(ctx, dest, nil, "update-index", "-q", "--refresh"); err != nil {
		return Result{}, err
	}
	mismatched, err := diffFiles(ctx, dest)
	if err != nil {
		return Result{}, err
	}
	if err := checkoutIndex(ctx, dest, mismatched); err != nil {
		return Result{}, err
	}
	if still, err := diffFiles(ctx, dest); err != nil {
		return Result{}, err
	} else if len(still) > 0 {
		return Result{}, errors.New("tracked files still differ from the index after rewriting them from Git")
	}
	rewritten := map[string]bool{}
	for _, p := range append(misshapen, mismatched...) {
		rewritten[p] = true
	}
	kept := 0
	for _, p := range cloned {
		if !rewritten[p] {
			kept++
		}
	}
	return Result{Cloned: kept, Written: len(target.paths) - kept}, nil
}

// misshapenClones returns those of paths, clones in dest, that are not regular
// files with their entry's executable bit.
func misshapenClones(dest string, target tree, paths []string) ([]string, error) {
	var misshapen []string
	for _, p := range paths {
		fi, err := os.Lstat(filepath.Join(dest, filepath.FromSlash(p)))
		if err != nil {
			return nil, err
		}
		if !fi.Mode().IsRegular() || (fi.Mode().Perm()&0o100 != 0) != (target.entries[p].mode == "100755") {
			misshapen = append(misshapen, p)
		}
	}
	return misshapen, nil
}

func regular(mode string) bool { return mode == "100644" || mode == "100755" }

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// treeEntries reads the tree of HEAD in repo.
func treeEntries(ctx context.Context, repo string) (tree, error) {
	out, err := git(ctx, repo, nil, "ls-tree", "-r", "-z", "--full-tree", "HEAD")
	if err != nil {
		return tree{}, err
	}
	t := tree{entries: map[string]entry{}}
	for record := range bytes.SplitSeq(out, []byte{0}) {
		meta, p, ok := strings.Cut(string(record), "\t")
		if !ok {
			continue
		}
		fields := strings.Fields(meta)
		if len(fields) != 3 {
			return tree{}, fmt.Errorf("git ls-tree printed %q", record)
		}
		t.entries[p] = entry{mode: fields[0], oid: fields[2]}
		t.paths = append(t.paths, p)
	}
	return t, nil
}

// dirtyPaths returns the tracked paths that git status in repo lists.
func dirtyPaths(ctx context.Context, repo string) (map[string]bool, error) {
	out, err := git(ctx, repo, nil, "--no-optional-locks", "status", "--porcelain=v1", "-z", "--untracked-files=no")
	if err != nil {
		return nil, err
	}
	fields := strings.Split(string(out), "\x00")
	dirty := map[string]bool{}
	for i := 0; i < len(fields); i++ {
		field := fields[i]
		if len(field) < 4 {
			continue
		}
		dirty[field[3:]] = true
		// A rename or copy is followed by its source path.
		if strings.ContainsAny(field[:2], "RC") && i+1 < len(fields) {
			i++
			dirty[fields[i]] = true
		}
	}
	return dirty, nil
}

func diffFiles(ctx context.Context, worktree string) ([]string, error) {
	out, err := git(ctx, worktree, nil, "diff-files", "--name-only", "-z")
	if err != nil {
		return nil, err
	}
	var paths []string
	for p := range strings.SplitSeq(string(out), "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths, nil
}

// checkoutIndex writes paths from the index of worktree.
func checkoutIndex(ctx context.Context, worktree string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	_, err := git(ctx, worktree, []byte(strings.Join(paths, "\x00")+"\x00"), "checkout-index", "-u", "-f", "-z", "--stdin")
	return err
}

// configureGit makes cmd, a git that Add runs other than the hook's, stop as
// gitstore's commands stop, and start it in its own session with prompts off.
// A partial-clone base makes git fetch missing objects. Without a terminal, a
// prompt fails instead of stopping git.
func configureGit(cmd *exec.Cmd) {
	gitstore.ConfigureCancelableCommand(cmd)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "SSH_ASKPASS=", "SSH_ASKPASS_REQUIRE=never")
}

// git runs git -C dir with args and stdin, and returns its stdout.
func git(ctx context.Context, dir string, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	configureGit(cmd)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	stderr := gitstore.NewBoundedStderr()
	cmd.Stderr = stderr
	out, err := cmd.Output()
	if err != nil {
		name := args[0]
		if strings.HasPrefix(name, "-") && len(args) > 1 {
			name = args[1]
		}
		return nil, fmt.Errorf("git %s failed: %w: %s", name, err, auth.RedactString(strings.TrimSpace(stderr.String())))
	}
	return out, nil
}
