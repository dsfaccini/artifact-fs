package apfsclone

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/execfile"
	"golang.org/x/sys/unix"
)

// run runs git -C dir with args and a committer identity, and returns its
// trimmed stdout.
func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return strings.TrimSpace(output(t, dir, args...))
}

// output runs git as run does, and returns its stdout whole.
func output(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return outputWithInput(t, dir, "", args...)
}

// runInput runs git as run does, with stdin as its input.
func runInput(t *testing.T, dir, stdin string, args ...string) string {
	t.Helper()
	return strings.TrimSpace(outputWithInput(t, dir, stdin, args...))
}

func outputWithInput(t *testing.T, dir, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, stderr.Bytes())
	}
	return string(out)
}

func write(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := execfile.Write(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// fixture is an upstream repository, and the base, a clone of it with local
// changes, whose remote-tracking upstream/main is a commit past the base's
// HEAD.
type fixture struct {
	upstream, base string
	// worktrees is the directory of the base and of each worktree a test
	// makes.
	worktrees string
}

// newFixture makes a fixture. beforeClone runs on upstream before the base
// clones it, and beforeAdvance before upstream main moves past the base.
// Either may be nil.
func newFixture(t *testing.T, beforeClone, beforeAdvance func(upstream string)) fixture {
	t.Helper()
	root := t.TempDir()
	f := fixture{upstream: filepath.Join(root, "upstream"), worktrees: filepath.Join(root, "repos")}
	f.base = filepath.Join(f.worktrees, "base")
	if err := os.Mkdir(f.upstream, 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, f.upstream, "init", "-q", "--initial-branch=main")
	write(t, filepath.Join(f.upstream, ".gitignore"), ".venv/\n", 0o644)
	write(t, filepath.Join(f.upstream, "Makefile"), "install:\n\ttrue\n", 0o644)
	write(t, filepath.Join(f.upstream, "README.md"), "readme\n", 0o644)
	write(t, filepath.Join(f.upstream, "bin/run.sh"), "#!/bin/sh\necho run\n", 0o755)
	if err := os.Symlink("README.md", filepath.Join(f.upstream, "link")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(f.upstream, "data/blob.bin"), strings.Repeat("blob\x00\xff", 8192), 0o644)
	write(t, filepath.Join(f.upstream, "changed.txt"), "base\n", 0o644)
	write(t, filepath.Join(f.upstream, "dirty.txt"), "committed\n", 0o644)
	write(t, filepath.Join(f.upstream, "hidden.txt"), "committed\n", 0o644)
	run(t, f.upstream, "add", "-A")
	run(t, f.upstream, "commit", "-qm", "base")
	if beforeClone != nil {
		beforeClone(f.upstream)
	}

	if err := os.Mkdir(f.worktrees, 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, root, "clone", "-q", "-o", "upstream", f.upstream, f.base)
	write(t, filepath.Join(f.base, "dirty.txt"), "local edit\n", 0o644)
	write(t, filepath.Join(f.base, "untracked.txt"), "untracked\n", 0o644)
	// git status in the base misses this edit, so Add clones a file that
	// does not match its blob, and must rewrite it from Git.
	run(t, f.base, "update-index", "--assume-unchanged", "hidden.txt")
	write(t, filepath.Join(f.base, "hidden.txt"), "hidden local edit\n", 0o644)

	if beforeAdvance != nil {
		beforeAdvance(f.upstream)
	}
	write(t, filepath.Join(f.upstream, "changed.txt"), "upstream\n", 0o644)
	run(t, f.upstream, "commit", "-qam", "upstream moves")
	run(t, f.base, "fetch", "-q", "upstream")
	return f
}

// commitUpstream commits files, by path, to upstream main. It adds only those
// paths, so the attributes of a .gitattributes among them convert no other
// file.
func (f fixture) commitUpstream(t *testing.T, files map[string]string) {
	t.Helper()
	add := []string{"add", "--"}
	for name, content := range files {
		write(t, filepath.Join(f.upstream, name), content, 0o644)
		add = append(add, name)
	}
	run(t, f.upstream, add...)
	run(t, f.upstream, "commit", "-qm", "upstream commits")
}

// addPlain makes the worktree name with git worktree add, as a checkout
// without clones.
func (f fixture) addPlain(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(f.worktrees, name)
	run(t, f.base, "worktree", "add", "-q", "-b", "feat/"+name, path, "upstream/main")
	return path
}

// add makes the worktree name with Add, cloning with clone.
func (f fixture) add(t *testing.T, name string, clone Clonefile) (string, Result) {
	t.Helper()
	path := filepath.Join(f.worktrees, name)
	var stderr bytes.Buffer
	result, err := Add(t.Context(), Options{Base: f.base, Path: path, Commit: "upstream/main",
		AddArgs: []string{"-q", "-b", "feat/" + name}, Clone: clone, Stderr: &stderr})
	if err != nil {
		t.Fatalf("Add: %v\n%s", err, stderr.Bytes())
	}
	return path, result
}

// addRefused runs Add for the worktree refused, on the new branch
// feat/refused, cloning with clone.
func (f fixture) addRefused(t *testing.T, clone Clonefile) error {
	t.Helper()
	_, err := Add(t.Context(), Options{Base: f.base, Path: filepath.Join(f.worktrees, "refused"), Commit: "upstream/main",
		AddArgs: []string{"-b", "feat/refused"}, Clone: clone})
	return err
}

// requireNothingCreated fails t when the worktree refused or the branch
// feat/refused exists, or the base's state is not before.
func (f fixture) requireNothingCreated(t *testing.T, before string) {
	t.Helper()
	path := filepath.Join(f.worktrees, "refused")
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Add left %s: %v", path, err)
	}
	if got := run(t, f.base, "branch", "--list", "feat/refused"); got != "" {
		t.Errorf("Add made the branch %q", got)
	}
	if got := f.state(t); got != before {
		t.Errorf("base state = %q, want %q", got, before)
	}
}

// state is what Add must leave unchanged in the base: its worktrees and its
// status.
func (f fixture) state(t *testing.T) string {
	t.Helper()
	return run(t, f.base, "worktree", "list", "--porcelain") + "\n--\n" + run(t, f.base, "status", "--porcelain")
}

// trackedTree describes each tracked path of worktree: a symlink's target, or
// a file's executable bit and content.
func trackedTree(t *testing.T, worktree string) map[string]string {
	t.Helper()
	tree := map[string]string{}
	for name := range strings.SplitSeq(output(t, worktree, "ls-files", "-z"), "\x00") {
		if name == "" {
			continue
		}
		path := filepath.Join(worktree, name)
		fi, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				t.Fatal(err)
			}
			tree[name] = "link " + target
		case fi.Mode().Perm()&0o111 != 0:
			tree[name] = "exec " + read(t, path)
		default:
			tree[name] = "file " + read(t, path)
		}
	}
	return tree
}

// copyClone clones as clonefile(2) with CLONE_NOFOLLOW does, by copying, on
// platforms without clones.
func copyClone(src, dst string) error {
	fi, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		return os.Symlink(target, dst)
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fi.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, fi.Mode().Perm())
}

// recorder records the paths of worktree that its clone made.
type recorder struct {
	clone    Clonefile
	worktree string
	cloned   []string
}

// newRecorder records the clones into worktree of the platform's clone:
// clonefile(2) on macOS, copyClone elsewhere.
func newRecorder(worktree string) *recorder {
	r := &recorder{clone: clonefile, worktree: worktree}
	if runtime.GOOS != "darwin" {
		r.clone = copyClone
	}
	return r
}

func (r *recorder) Clone(src, dst string) error {
	err := r.clone(src, dst)
	if rel, relErr := filepath.Rel(r.worktree, dst); err == nil && relErr == nil && !strings.HasPrefix(rel, "..") {
		r.cloned = append(r.cloned, filepath.ToSlash(rel))
	}
	return err
}

func TestAddMatchesGitWorktreeAdd(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil, nil)
	before := run(t, f.base, "status", "--porcelain")
	plain := f.addPlain(t, "plain")
	rec := newRecorder(filepath.Join(f.worktrees, "clone"))
	clone, result := f.add(t, "clone", rec.Clone)

	// dirty.txt is dirty in the base, changed.txt changed upstream, and link
	// is a symlink. hidden.txt is cloned, then rewritten.
	if want := []string{".gitignore", "Makefile", "README.md", "bin/run.sh", "data/blob.bin", "hidden.txt"}; !slices.Equal(rec.cloned, want) {
		t.Fatalf("cloned %q, want %q", rec.cloned, want)
	}
	if want := (Result{Cloned: 5, Written: 4}); result != want {
		t.Fatalf("Add returned %+v, want %+v", result, want)
	}
	if got := run(t, clone, "status", "--porcelain", "--untracked-files=no"); got != "" {
		t.Fatalf("git status in the clone lists %q", got)
	}
	if got, want := run(t, clone, "status", "--porcelain"), run(t, plain, "status", "--porcelain"); got != want {
		t.Fatalf("git status in the clone = %q, in the plain worktree %q", got, want)
	}
	head := run(t, f.upstream, "rev-parse", "main")
	if got, plainHead := run(t, clone, "rev-parse", "HEAD"), run(t, plain, "rev-parse", "HEAD"); got != head || plainHead != head {
		t.Fatalf("HEAD is %s in the clone and %s in the plain worktree, want %s", got, plainHead, head)
	}
	if got, want := run(t, f.base, "config", "branch.feat/clone.merge"), run(t, f.base, "config", "branch.feat/plain.merge"); got != want {
		t.Fatalf("feat/clone merges %q, feat/plain %q", got, want)
	}
	if got, want := trackedTree(t, clone), trackedTree(t, plain); !maps.Equal(got, want) {
		t.Fatalf("clone's tree = %q\nplain tree = %q", got, want)
	}
	for name, want := range map[string]string{"dirty.txt": "committed\n", "hidden.txt": "committed\n", "changed.txt": "upstream\n"} {
		if got := read(t, filepath.Join(clone, name)); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if target, err := os.Readlink(filepath.Join(clone, "link")); err != nil || target != "README.md" {
		t.Errorf("link points at %q (%v), want README.md", target, err)
	}
	if _, err := os.Lstat(filepath.Join(clone, "untracked.txt")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("untracked.txt in the clone: %v", err)
	}
	if got := run(t, f.base, "status", "--porcelain"); got != before {
		t.Errorf("git status in the base = %q, want %q", got, before)
	}
}

// TestAddWritesGitattributesFromGit covers a .gitattributes whose base copy
// git status misses. checkout-index reads the work tree's .gitattributes
// while it writes the other paths, and refresh hashes through them, so a
// clone of the stale copy would change what Git writes or what it calls
// clean.
func TestAddWritesGitattributesFromGit(t *testing.T) {
	t.Parallel()
	commitAttributes := func(path, content string) func(string) {
		return func(upstream string) {
			write(t, filepath.Join(upstream, path), content, 0o644)
			run(t, upstream, "add", ".")
			run(t, upstream, "commit", "-qm", "attributes")
		}
	}
	for _, tc := range []struct {
		name          string
		beforeClone   func(string)
		beforeAdvance func(string)
		// stale are the base's edits, which git status misses.
		stale map[string]string
		want  map[string]string
	}{{
		// The stale copy would make the clean filter turn README.md's CRLF
		// into LF when it hashes the file, so the index would call the CRLF
		// clone clean. A plain checkout writes the LF blob.
		name:        "hides a hidden edit",
		beforeClone: commitAttributes(".gitattributes", "*.bin -diff\n"),
		stale:       map[string]string{".gitattributes": "README.md text\n", "README.md": "readme\r\n"},
		want:        map[string]string{"README.md": "readme\n"},
	}, {
		// Beside the stale copy, Git would write changed.txt, which upstream
		// changed, with CRLF.
		name:        "changes a checkout",
		beforeClone: commitAttributes(".gitattributes", "*.bin -diff\n"),
		stale:       map[string]string{".gitattributes": "changed.txt eol=crlf\n"},
		want:        map[string]string{"changed.txt": "upstream\n", ".gitattributes": "*.bin -diff\n"},
	}, {
		name: "nested",
		beforeClone: func(upstream string) {
			write(t, filepath.Join(upstream, "sub/a.txt"), "base\n", 0o644)
			commitAttributes("sub/.gitattributes", "*.bin -diff\n")(upstream)
		},
		beforeAdvance: func(upstream string) { write(t, filepath.Join(upstream, "sub/a.txt"), "upstream\n", 0o644) },
		stale:         map[string]string{"sub/.gitattributes": "a.txt eol=crlf\n"},
		want:          map[string]string{"sub/a.txt": "upstream\n"},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, tc.beforeClone, tc.beforeAdvance)
			for name, content := range tc.stale {
				run(t, f.base, "update-index", "--assume-unchanged", name)
				write(t, filepath.Join(f.base, name), content, 0o644)
			}
			plain := f.addPlain(t, "plain")
			clone, _ := f.add(t, "clone", newRecorder(filepath.Join(f.worktrees, "clone")).Clone)
			for name, want := range tc.want {
				if got := read(t, filepath.Join(plain, name)); got != want {
					t.Fatalf("plain %s = %q, want %q", name, got, want)
				}
				if got := read(t, filepath.Join(clone, name)); got != want {
					t.Errorf("clone's %s = %q, want %q", name, got, want)
				}
			}
			if got, want := trackedTree(t, clone), trackedTree(t, plain); !maps.Equal(got, want) {
				t.Errorf("clone's tree = %q\nplain tree = %q", got, want)
			}
		})
	}
}

// TestAddRewritesACloneWithoutItsExecutableBit covers core.fileMode=false,
// under which git status in the base, and diff-files in the new worktree,
// ignore the executable bit that a plain checkout still writes.
func TestAddRewritesACloneWithoutItsExecutableBit(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil, nil)
	run(t, f.base, "config", "core.fileMode", "false")
	if err := os.Chmod(filepath.Join(f.base, "bin/run.sh"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := run(t, f.base, "status", "--porcelain", "--untracked-files=no"); got != "M dirty.txt" {
		t.Fatalf("git status in the base = %q, want only dirty.txt", got)
	}
	plain := f.addPlain(t, "plain")
	rec := newRecorder(filepath.Join(f.worktrees, "clone"))
	clone, _ := f.add(t, "clone", rec.Clone)
	if !slices.Contains(rec.cloned, "bin/run.sh") {
		t.Fatalf("Add did not clone bin/run.sh: cloned %q", rec.cloned)
	}
	fi, err := os.Stat(filepath.Join(clone, "bin/run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o100 == 0 {
		t.Errorf("clone's bin/run.sh has mode %v, want it executable", fi.Mode())
	}
	if got, want := trackedTree(t, clone), trackedTree(t, plain); !maps.Equal(got, want) {
		t.Errorf("clone's tree = %q\nplain tree = %q", got, want)
	}
}

// shellQuote quotes s for sh.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// TestAddRegistersHoldingALockItReleasesBeforePostCheckout probes the lock
// from inside git worktree add and read-tree, through a fake git first on
// PATH, and from the post-checkout hook. It is serial: it sets PATH.
func TestAddRegistersHoldingALockItReleasesBeforePostCheckout(t *testing.T) {
	f := newFixture(t, nil, nil)
	probe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "lock.log")
	write(t, filepath.Join(dir, "bin/git"), "#!/bin/sh\ncase \" $* \" in *\" worktree add \"*) "+shellQuote(probe)+" add ;; *\" read-tree HEAD \"*) "+shellQuote(probe)+" read-tree ;; esac\n"+
		"exec "+shellQuote(realGit)+" \"$@\"\n", 0o755)
	write(t, filepath.Join(f.base, ".git/hooks/post-checkout"), "#!/bin/sh\n"+shellQuote(probe)+" hook\n", 0o755)
	t.Setenv("PATH", filepath.Join(dir, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(lockProbeEnv, filepath.Join(f.base, ".git", RegistrationLock))
	t.Setenv(lockProbeLogEnv, log)

	f.add(t, "locked", newRecorder(filepath.Join(f.worktrees, "locked")).Clone)
	if got, want := strings.Split(strings.TrimSpace(read(t, log)), "\n"), []string{"add held", "read-tree held", "hook free"}; !slices.Equal(got, want) {
		t.Fatalf("lock probes = %q, want %q", got, want)
	}
}

func TestAddRunsPostCheckoutAsGitWorktreeAddDoes(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil, nil)
	log := filepath.Join(t.TempDir(), "post-checkout.log")
	write(t, filepath.Join(f.base, ".git/hooks/post-checkout"), "#!/bin/sh\necho \"$(pwd -P) $(ps -o pgid= -p $$ | tr -d ' ') GIT_DIR=${GIT_DIR-unset} $*\" >> "+shellQuote(log)+"\n", 0o755)
	plain := f.addPlain(t, "plain")
	clone, _ := f.add(t, "clone", newRecorder(filepath.Join(f.worktrees, "clone")).Clone)

	head := run(t, f.upstream, "rev-parse", "main")
	gitDir, err := filepath.EvalSymlinks(run(t, clone, "rev-parse", "--absolute-git-dir"))
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	// git worktree add unsets GIT_DIR for its hook. Add's git hook run exports
	// the new worktree's git dir, and Add keeps git hook run because it also
	// runs the hooks that config defines.
	for _, hook := range []struct{ worktree, gitDir string }{{plain, "unset"}, {clone, gitDir}} {
		resolved, err := filepath.EvalSymlinks(hook.worktree)
		if err != nil {
			t.Fatal(err)
		}
		// git worktree add runs the hook in its caller's process group, where
		// the hook can read the terminal.
		want = append(want, resolved+" "+strconv.Itoa(unix.Getpgrp())+" GIT_DIR="+hook.gitDir+" "+strings.Repeat("0", len(head))+" "+head+" 1")
	}
	if got := strings.Split(strings.TrimSpace(read(t, log)), "\n"); !slices.Equal(got, want) {
		t.Fatalf("post-checkout ran with %q, want %q", got, want)
	}
}

// TestPreflightRefusesWhatCannotClone injects the errors clonefile(2) returns
// on a filesystem without clones and across volumes.
func TestPreflightRefusesWhatCannotClone(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		errno syscall.Errno
		want  string
	}{
		{unix.ENOTSUP, "does not support clones; it is not APFS"},
		{unix.EXDEV, "is on another volume than"},
	} {
		t.Run(tc.errno.Error(), func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, nil, nil)
			before := f.state(t)
			calls := 0
			err := f.addRefused(t, func(src, dst string) error { calls++; return tc.errno })
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Add = %v, want an error saying %q", err, tc.want)
			}
			if calls != 1 {
				t.Errorf("Add cloned %d times, want the one probe", calls)
			}
			f.requireNothingCreated(t, before)
		})
	}
}

// TestAddFailsWhenARewriteStillDiffers edits each file that Add rewrites
// from Git right after the rewrite, as another writer could, so the file still
// differs from the index.
func TestAddFailsWhenARewriteStillDiffers(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil, nil)
	path := filepath.Join(f.worktrees, "clone")
	var rewritten []string
	_, err := Add(t.Context(), Options{Base: f.base, Path: path, Commit: "upstream/main",
		AddArgs: []string{"-b", "feat/clone"}, Clone: newRecorder(path).Clone,
		afterRewrite: func(paths []string) {
			rewritten = paths
			for _, p := range paths {
				write(t, filepath.Join(path, filepath.FromSlash(p)), "edited after the rewrite\n", 0o644)
			}
		}})
	// hidden.txt is the clone that does not match its blob.
	if !slices.Equal(rewritten, []string{"hidden.txt"}) {
		t.Fatalf("Add rewrote %q, want hidden.txt", rewritten)
	}
	var incomplete *IncompleteError
	if !errors.As(err, &incomplete) || incomplete.Path != path || !strings.Contains(err.Error(), "still differ from the index") {
		t.Fatalf("Add = %v, want an IncompleteError for %s saying files still differ", err, path)
	}
}

// TestAddRefusesAConvertingAttributeBeforeCreatingAnything gives only the
// target commit, upstream main, an attribute that converts changed.txt on
// checkout.
func TestAddRefusesAConvertingAttributeBeforeCreatingAnything(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ rule, reported string }{
		{"filter=lfs", "filter=lfs"},
		{"ident", "ident=set"},
		{"working-tree-encoding=UTF-16", "working-tree-encoding=UTF-16"},
		{"text", "text=set"},
		{"text=auto", "text=auto"},
		{"eol=lf", "eol=lf"},
		{"crlf", "crlf=set"},
	} {
		t.Run(tc.rule, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, nil, nil)
			f.commitUpstream(t, map[string]string{".gitattributes": "changed.txt " + tc.rule + "\n"})
			run(t, f.base, "fetch", "-q", "upstream")
			before := f.state(t)
			want := "changed.txt has the attribute " + tc.reported + ", which converts it on checkout"
			if err := f.addRefused(t, newRecorder(filepath.Join(f.worktrees, "refused")).Clone); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("Add = %v, want an error saying %q", err, want)
			}
			f.requireNothingCreated(t, before)
		})
	}
}

// TestAddAcceptsMinusText covers the binary macro, whose -text turns
// end-of-line conversion off, so Git writes the blobs unchanged.
func TestAddAcceptsMinusText(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil, nil)
	f.commitUpstream(t, map[string]string{".gitattributes": "*.txt binary\n", "changed.txt": "crlf\r\nlf\n"})
	run(t, f.base, "fetch", "-q", "upstream")
	plain := f.addPlain(t, "plain")
	clone, _ := f.add(t, "clone", newRecorder(filepath.Join(f.worktrees, "clone")).Clone)
	if got := read(t, filepath.Join(clone, "changed.txt")); got != "crlf\r\nlf\n" {
		t.Errorf("clone's changed.txt = %q, want the blob's bytes", got)
	}
	if got, want := trackedTree(t, clone), trackedTree(t, plain); !maps.Equal(got, want) {
		t.Errorf("clone's tree = %q\nplain tree = %q", got, want)
	}
	if got := run(t, clone, "status", "--porcelain", "--untracked-files=no"); got != "" {
		t.Errorf("git status in the clone lists %q", got)
	}
}

// TestAddRefusesAConvertingConfigBeforeCreatingAnything covers what makes a
// checkout convert files although no attribute that check-attr --cached
// reads converts them. It is serial: its GIT_ATTR_SOURCE case sets the
// environment.
func TestAddRefusesAConvertingConfigBeforeCreatingAnything(t *testing.T) {
	// attributesTree writes a tree whose .gitattributes converts every file.
	// A checkout reads the attributes of that tree in place of the index's,
	// and check-attr --cached does not.
	attributesTree := func(t *testing.T, f fixture) string {
		t.Helper()
		blob := runInput(t, f.base, "* text eol=crlf\n", "hash-object", "-w", "--stdin")
		return runInput(t, f.base, "100644 blob "+blob+"\t.gitattributes\n", "mktree")
	}
	setConfig := func(key, value string) func(*testing.T, fixture) string {
		return func(t *testing.T, f fixture) string {
			run(t, f.base, "config", key, value)
			return "the config sets " + key + "=" + value
		}
	}
	for _, tc := range []struct {
		name string
		// serial marks the case that sets the environment.
		serial bool
		// configure sets the case up and returns the error text Add must
		// return.
		configure func(t *testing.T, f fixture) string
	}{
		{name: "core.autocrlf=true", configure: setConfig("core.autocrlf", "true")},
		{name: "core.autocrlf=input", configure: setConfig("core.autocrlf", "input")},
		{name: "attr.tree", configure: func(t *testing.T, f fixture) string {
			return setConfig("attr.tree", attributesTree(t, f))(t, f)
		}},
		{name: "GIT_ATTR_SOURCE", serial: true, configure: func(t *testing.T, f fixture) string {
			tree := attributesTree(t, f)
			t.Setenv("GIT_ATTR_SOURCE", tree)
			return "the environment sets GIT_ATTR_SOURCE=" + tree
		}},
		{name: "includeIf", configure: func(t *testing.T, f fixture) string {
			include := filepath.Join(t.TempDir(), "autocrlf.inc")
			write(t, include, "[core]\n\tautocrlf = true\n", 0o644)
			run(t, f.base, "config", "includeIf.onbranch:feat/refused.path", include)
			// It applies only in the new worktree, so the base's config
			// shows no core.autocrlf.
			if got, err := config(t.Context(), f.base, "--get", "core.autocrlf"); err != nil || got != "" {
				t.Fatalf("core.autocrlf in the base = %q, %v, want it unset", got, err)
			}
			return "the config sets includeif.onbranch:feat/refused.path, a conditional include"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !tc.serial {
				t.Parallel()
			}
			f := newFixture(t, nil, nil)
			want := tc.configure(t, f)
			before := f.state(t)
			if err := f.addRefused(t, newRecorder(filepath.Join(f.worktrees, "refused")).Clone); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("Add = %v, want an error saying %q", err, want)
			}
			f.requireNothingCreated(t, before)
		})
	}
}

// TestAddRechecksTheAttributesOfACommitThatMoved fetches, between Add's
// checks and git worktree add, a commit of upstream main with an attribute
// that converts changed.txt on checkout.
func TestAddRechecksTheAttributesOfACommitThatMoved(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil, nil)
	f.commitUpstream(t, map[string]string{".gitattributes": "changed.txt eol=lf\n"})
	path := filepath.Join(f.worktrees, "clone")
	rec := newRecorder(path)
	_, err := Add(t.Context(), Options{Base: f.base, Path: path, Commit: "upstream/main",
		AddArgs: []string{"-b", "feat/clone"}, Clone: rec.Clone,
		beforeRegister: func() { run(t, f.base, "fetch", "-q", "upstream") }})
	if got, want := run(t, path, "rev-parse", "HEAD"), run(t, f.upstream, "rev-parse", "main"); got != want {
		t.Fatalf("the worktree's HEAD is %s, want the fetched %s", got, want)
	}
	var incomplete *IncompleteError
	want := "changed.txt has the attribute eol=lf, which converts it on checkout"
	if !errors.As(err, &incomplete) || incomplete.Path != path || !strings.Contains(err.Error(), want) {
		t.Fatalf("Add = %v, want an IncompleteError for %s saying %q", err, path, want)
	}
	if len(rec.cloned) > 0 {
		t.Errorf("Add cloned %q after the recheck refused", rec.cloned)
	}
}

// TestPreflightProbesUnderANameNoOtherAddUses leaves a file at the name a
// probe of this process could take, as a concurrent Add's probe would.
func TestPreflightProbesUnderANameNoOtherAddUses(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil, nil)
	other := filepath.Join(f.worktrees, ".apfsclone-probe-"+strconv.Itoa(os.Getpid()))
	write(t, other, "another probe\n", 0o644)
	f.add(t, "clone", newRecorder(filepath.Join(f.worktrees, "clone")).Clone)
	if got := read(t, other); got != "another probe\n" {
		t.Errorf("the other probe holds %q", got)
	}
	probes, err := filepath.Glob(filepath.Join(f.worktrees, ".apfsclone-probe-*"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(probes, []string{other}) {
		t.Errorf("probes left: %q, want only the other one", probes)
	}
}

// TestGitBoundsTheStderrItQuotes makes checkout-index report thousands of
// paths that are not in the index.
func TestGitBoundsTheStderrItQuotes(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	run(t, repo, "init", "-q")
	var paths strings.Builder
	for i := range 2000 {
		fmt.Fprintf(&paths, "missing-%04d.txt\x00", i)
	}
	_, err := git(t.Context(), repo, []byte(paths.String()), "checkout-index", "-z", "--stdin")
	if err == nil {
		t.Fatal("checkout-index of missing paths succeeded")
	}
	if !strings.Contains(err.Error(), "missing-0000.txt") || !strings.HasSuffix(err.Error(), "[git stderr truncated]") || len(err.Error()) > 17<<10 {
		t.Fatalf("error of %d bytes, want the first paths, a truncation label, and at most 17 KiB: %.200s", len(err.Error()), err)
	}
}
