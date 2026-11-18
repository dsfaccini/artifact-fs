package slimtree

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/cloudflare/artifact-fs/internal/overlay"
	"github.com/cloudflare/artifact-fs/internal/slimtree/mounts"
	"github.com/cloudflare/artifact-fs/internal/slimtree/state"
)

// stoppedArtifactFS makes, beside the main worktree that newRepo makes, the
// artifactfs worktree wt as slimtree new leaves it once stopped: git
// registers it locked on the new branch wt, its gitfile is in the record
// directory, its mount point is empty, and its state root holds an overlay
// that overlay.New made, with a file of each content in files, by its path.
// It returns the main worktree and the record.
func stoppedArtifactFS(t *testing.T, files map[string]string) (string, state.Record) {
	t.Helper()
	homeAfterFirstRun(t)
	main := newRepo(t)
	home, err := openHome()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(filepath.Dir(main), "wt")
	gitIn(t, main, "worktree", "add", "-q", "--no-checkout", "--lock", "--reason", artifactFSLockReason, "-b", "wt", path)
	gitIn(t, path, "read-tree", "HEAD")
	dir := home.RecordDir(path)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(path, ".git"), filepath.Join(dir, gitfileName)); err != nil {
		t.Fatal(err)
	}
	gitDir, err := state.Canonical(filepath.Join(main, ".git", "worktrees", "wt"))
	if err != nil {
		t.Fatal(err)
	}
	r := state.Record{Version: 1, Backend: "artifactfs", Path: path, CommonDir: filepath.Join(main, ".git"), Branch: "wt",
		StateDir: filepath.Join(dir, "state"), GitDir: gitDir}
	if err := home.Write(r); err != nil {
		t.Fatal(err)
	}
	overlayDir := filepath.Join(r.StateDir, "overlays", "wt")
	store, err := overlay.New(t.Context(), model.RepoPaths{OverlayDir: overlayDir, OverlayDBPath: filepath.Join(overlayDir, "meta.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	for p, data := range files {
		if _, err := store.CreateFile(t.Context(), p, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := store.WriteFile(t.Context(), p, 0, []byte(data)); err != nil {
			t.Fatal(err)
		}
		if err := store.FlushFile(t.Context(), p); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	return main, r
}

// servedArtifactFS starts a process that stands for r's daemon, and shows
// identify the mount id at r.Path while that process runs. The mount point
// then holds the worktree's gitfile and files, as the mount serves them. Once
// the process exits, identify finds nothing mounted and the mount point
// empty, as after the daemon's unmount. It records the process and id in r's
// record, and returns the process.
func servedArtifactFS(t *testing.T, r *state.Record, id mounts.Identity) *exec.Cmd {
	t.Helper()
	home, err := openHome()
	if err != nil {
		t.Fatal(err)
	}
	gitfile, err := os.ReadFile(filepath.Join(home.RecordDir(r.Path), gitfileName))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.Path, ".git"), gitfile, 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, r.Path, "checkout-index", "-a")
	d := exec.Command("sleep", "60")
	if err := d.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		d.Process.Kill()
		d.Wait()
	})
	token, err := mounts.StartToken(d.Process.Pid)
	if err != nil || token == "" {
		t.Fatalf("StartToken(%d) = %q, %v", d.Process.Pid, token, err)
	}
	r.DaemonPID, r.DaemonToken, r.MountIdentity = d.Process.Pid, token, string(id)
	if err := home.Write(*r); err != nil {
		t.Fatal(err)
	}
	path := r.Path
	identifyHook = func(string) (mounts.Identity, error) {
		if mounts.Signal(d.Process.Pid, token, 0) == nil {
			return id, nil
		}
		return "", emptyDir(path)
	}
	t.Cleanup(func() { identifyHook = nil })
	return d
}

// emptyDir removes what dir holds.
func emptyDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// archiveFiles returns the regular files in the state archive in dir, by
// name.
func archiveFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, stateArchiveName))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	files := map[string]string{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		files[h.Name] = string(data)
	}
}

// requireUnchanged fails t unless r is the only record, git registers r's
// worktree locked, its gitfile is in the record directory, and nothing was
// archived.
func requireUnchanged(t *testing.T, main string, r state.Record) {
	t.Helper()
	if got := records(t); !reflect.DeepEqual(got, []state.Record{r}) {
		t.Errorf("records = %+v, want %+v", got, r)
	}
	if list := gitIn(t, main, "worktree", "list", "--porcelain"); !strings.Contains(list, "worktree "+r.Path+"\n") || !strings.Contains(list, "locked "+artifactFSLockReason) {
		t.Errorf("git does not list %s locked:\n%s", r.Path, list)
	}
	home, err := openHome()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home.RecordDir(r.Path), gitfileName)); err != nil {
		t.Errorf("the record directory's gitfile: %v", err)
	}
	if refs := recoveryRefs(t, r.CommonDir); refs != "" {
		t.Errorf("recovery refs %q, want none", refs)
	}
	if entries, err := os.ReadDir(filepath.Join(home.Dir(), "archives")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("archives = %v, %v; want none", entries, err)
	}
}

// requireRemoved fails t unless r's worktree, its registration and its record
// are gone, the branch wt stays at head, and every recovery ref of r's
// worktree points at head.
func requireRemoved(t *testing.T, main string, r state.Record, head string) {
	t.Helper()
	requireNoWorktree(t, main, r.Path)
	if got := strings.TrimSpace(gitIn(t, main, "rev-parse", "wt")); got != head {
		t.Errorf("wt points at %s, want %s", got, head)
	}
	refs := strings.Fields(gitIn(t, main, "for-each-ref", "--format=%(objectname)", "refs/slimtree/archive/"+state.ID(r.Path)+"/"))
	if len(refs) == 0 || slices.ContainsFunc(refs, func(at string) bool { return at != head }) {
		t.Errorf("recovery refs point at %q, want %s", refs, head)
	}
}

// rm --force of an artifactfs worktree that no daemon serves archives its
// state root and its git dir, without the overlay files of the paths with a
// component that an --exclude glob matches and without a socket, writes its
// HEAD to the recovery ref, and removes the worktree, its registration and
// its record.
func TestRmForceArchivesAnArtifactFSWorktreesFiles(t *testing.T) {
	main, r := stoppedArtifactFS(t, map[string]string{
		"notes.txt":                 "kept\n",
		"src/node_modules.go":       "kept, since no component is node_modules\n",
		"node_modules/pkg/index.js": "left out\n",
		"web/node_modules/x.js":     "left out below the top\n",
		"build/out.o":               "left out by a glob\n",
	})
	// The worktree's HEAD differs from the main worktree's.
	head := strings.TrimSpace(gitIn(t, main, "commit-tree", "-p", "HEAD", "-m", "work", "HEAD^{tree}"))
	gitIn(t, main, "update-ref", "refs/heads/wt", head)
	home, err := openHome()
	if err != nil {
		t.Fatal(err)
	}
	// A daemon that crashed leaves its control socket, which tar cannot
	// hold. A relative name keeps the socket's path within sun_path.
	t.Chdir(r.StateDir)
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: "daemon.sock", Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	l.SetUnlinkOnClose(false)
	l.Close()
	t.Chdir(main)

	code, stdout, stderr := linux(t.Context(), "rm", "--force", "--exclude", "node_modules", "--exclude", "*.o", "wt")
	id := state.ID(r.Path)
	archives := filepath.Join(home.Dir(), "archives", id)
	m := regexp.MustCompile(`^removed ` + regexp.QuoteMeta(r.Path) + `; (refs/slimtree/archive/` + id + `/(\d+)/HEAD) keeps its HEAD ` + head +
		`, and ` + regexp.QuoteMeta(archives) + `/(\d+)/state\.tar\.gz its files` + "\n$").FindStringSubmatch(stdout)
	if code != 0 || m == nil || m[2] != m[3] {
		t.Fatalf("rm --force wt: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	requireRemoved(t, main, r, head)
	dir := filepath.Join(archives, m[2])
	data, err := os.ReadFile(filepath.Join(dir, "worktree.json"))
	if err != nil {
		t.Fatal(err)
	}
	var archived state.Record
	if err := json.Unmarshal(data, &archived); err != nil {
		t.Fatal(err)
	}
	if want := r; archived.Head != head || archived.Path != want.Path || archived.CommonDir != want.CommonDir || archived.StateDir != want.StateDir {
		t.Errorf("archived record = %+v, want %+v with head %s", archived, want, head)
	}
	files := archiveFiles(t, dir)
	var kept []string
	for name, data := range files {
		if strings.HasPrefix(name, "state/overlays/wt/upper/") {
			kept = append(kept, data)
		}
	}
	slices.Sort(kept)
	if want := []string{"kept\n", "kept, since no component is node_modules\n"}; !slices.Equal(kept, want) {
		t.Errorf("the archive's overlay files = %q, want %q", kept, want)
	}
	if _, ok := files["state/overlays/wt/meta.sqlite"]; !ok {
		t.Errorf("the archive holds no overlay database: %v", slices.Sorted(maps.Keys(files)))
	}
	if got := files["gitdir/HEAD"]; got != "ref: refs/heads/wt\n" {
		t.Errorf("the archive's gitdir/HEAD = %q, want the worktree's", got)
	}
}

// Without --force, rm refuses an artifactfs worktree that its daemon does
// not serve, since only the mount lets git status check it, and one whose
// directory is gone. rm refuses an --exclude that is no glob of one path
// component, and --exclude for an apfsclone worktree. rm --check refuses
// the same, and each refusal changes nothing.
func TestRmRefusesAnArtifactFSWorktreeItCannotCheck(t *testing.T) {
	main, r := stoppedArtifactFS(t, map[string]string{"notes.txt": "kept\n"})
	notServed := "the daemon of " + r.Path + " does not serve it, so git status cannot check it for changes"
	gone := r.Path + " is gone; if its volume is not mounted, mount it and rerun"
	for _, tc := range []struct {
		args []string
		want string
	}{
		{args: []string{"rm", "wt"}, want: notServed},
		{args: []string{"rm", "--check", "wt"}, want: notServed},
		{args: []string{"rm", "--force", "--exclude", "a/b", "wt"}, want: "--exclude a/b: a pattern matches one path component, so it holds no slash"},
		{args: []string{"rm", "--force", "--exclude", "[", "wt"}, want: "--exclude [: syntax error in pattern"},
	} {
		if code, stdout, stderr := linux(t.Context(), tc.args...); code != 1 || stdout != "" || !strings.Contains(stderr, tc.want) {
			t.Errorf("%q: exit %d, stdout %q, stderr %q; want exit 1 and %q", tc.args, code, stdout, stderr, tc.want)
		}
	}
	requireUnchanged(t, main, r)
	if err := os.Remove(r.Path); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"rm", "wt"}, {"rm", "--check", "wt"}} {
		if code, stdout, stderr := linux(t.Context(), args...); code != 1 || stdout != "" || !strings.Contains(stderr, gone) {
			t.Errorf("%q of a gone worktree: exit %d, stdout %q, stderr %q; want exit 1 and %q", args, code, stdout, stderr, gone)
		}
	}
	requireUnchanged(t, main, r)

	cloned := filepath.Join(filepath.Dir(main), "cloned")
	if err := os.Mkdir(cloned, 0o755); err != nil {
		t.Fatal(err)
	}
	home, err := openHome()
	if err != nil {
		t.Fatal(err)
	}
	if err := home.Write(state.Record{Version: 1, Backend: "apfsclone", Path: cloned, CommonDir: r.CommonDir, Branch: "cloned"}); err != nil {
		t.Fatal(err)
	}
	want := "--exclude leaves files out of an artifactfs worktree's archive, and rm archives no files of the apfsclone worktree " + cloned
	if code, _, stderr := linux(t.Context(), "rm", "--exclude", "x", "cloned"); code != 1 || !strings.Contains(stderr, want) {
		t.Errorf("rm --exclude of an apfsclone worktree: exit %d, stderr %q; want %q", code, stderr, want)
	}
}

// While its daemon serves an artifactfs worktree, rm refuses the worktree
// when git status lists a change in it, or a process other than the daemon
// uses its mount, also with --force. It refuses another mount than the
// recorded one at its path. rm --check runs the same checks, and passes a clean worktree
// that no process holds. Each refusal and each check changes nothing.
func TestRmRefusesAServedArtifactFSWorktreeWithChangesOrHolders(t *testing.T) {
	main, r := stoppedArtifactFS(t, map[string]string{"notes.txt": "kept\n"})
	d := servedArtifactFS(t, &r, "mounted")
	alive := func() {
		t.Helper()
		if err := mounts.Signal(d.Process.Pid, r.DaemonToken, 0); err != nil {
			t.Fatalf("the daemon after a refusal: %v", err)
		}
		requireUnchanged(t, main, r)
	}
	if code, stdout, stderr := linux(t.Context(), "rm", "--check", "wt"); code != 0 || stdout != r.Path+" can be removed, unless files lie below its mount, which rm sees only once it unmounted it\n" {
		t.Fatalf("rm --check of a clean worktree: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	alive()

	if err := os.WriteFile(filepath.Join(r.Path, "scratch.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dirty := "git status lists changes in " + r.Path + "; commit them, or discard them with slimtree rm --force, which archives its files"
	for _, args := range [][]string{{"rm", "wt"}, {"rm", "--check", "wt"}} {
		if code, stdout, stderr := linux(t.Context(), args...); code != 1 || stdout != "" || !strings.Contains(stderr, dirty) {
			t.Errorf("%q with an untracked file: exit %d, stdout %q, stderr %q; want exit 1 and %q", args, code, stdout, stderr, dirty)
		}
	}
	alive()

	holder := exec.Command("sleep", "60")
	holder.Dir = r.Path
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		holder.Process.Kill()
		holder.Wait()
	})
	held := regexp.MustCompile(`processes use ` + regexp.QuoteMeta(r.Path) + `, so slimtree left it mounted: ` + strconv.Itoa(holder.Process.Pid) + ` sleep \(parent \d+ `)
	for _, args := range [][]string{{"rm", "--force", "wt"}, {"rm", "--check", "--force", "wt"}} {
		if code, stdout, stderr := linux(t.Context(), args...); code != 1 || stdout != "" || !held.MatchString(stderr) {
			t.Errorf("%q while pid %d works in the mount: exit %d, stdout %q, stderr %q; want exit 1 and the holder", args, holder.Process.Pid, code, stdout, stderr)
		}
	}
	alive()
	holder.Process.Kill()
	holder.Wait()

	// Once the recorded daemon is gone, no remount by it explains another
	// mount.
	if err := d.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	d.Wait()
	identifyHook = func(string) (mounts.Identity, error) { return "another", nil }
	other := r.Path + " holds another mount than the one slimtree recorded; slimtree left it"
	if code, _, stderr := linux(t.Context(), "rm", "--force", "wt"); code != 1 || !strings.Contains(stderr, other) {
		t.Errorf("rm --force over another mount: exit %d, stderr %q; want %q", code, stderr, other)
	}
	requireUnchanged(t, main, r)
}

// rm of a clean artifactfs worktree that its daemon serves stops the daemon
// with SIGTERM, matched by its start token, and removes the worktree once
// the daemon unmounted it. rm --force of a worktree whose daemon died
// unmounts the mount it left, by its recorded identity.
func TestRmStopsTheDaemonOfAnArtifactFSWorktree(t *testing.T) {
	t.Run("served", func(t *testing.T) {
		main, r := stoppedArtifactFS(t, map[string]string{"notes.txt": "kept\n"})
		head := strings.TrimSpace(gitIn(t, main, "rev-parse", "wt"))
		d := servedArtifactFS(t, &r, "mounted")
		if code, stdout, stderr := linux(t.Context(), "rm", "wt"); code != 0 || !strings.HasPrefix(stdout, "removed "+r.Path+"; ") {
			t.Fatalf("rm wt: exit %d, stdout %q, stderr %q", code, stdout, stderr)
		}
		if err := d.Wait(); err == nil || err.Error() != "signal: terminated" {
			t.Errorf("the daemon's wait = %v, want SIGTERM", err)
		}
		requireRemoved(t, main, r, head)
	})
	t.Run("died", func(t *testing.T) {
		main, r := stoppedArtifactFS(t, map[string]string{"notes.txt": "kept\n"})
		head := strings.TrimSpace(gitIn(t, main, "rev-parse", "wt"))
		d := servedArtifactFS(t, &r, "left")
		if err := d.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		d.Wait()
		mounted := true
		var unmounted []mounts.Identity
		identifyHook = func(string) (mounts.Identity, error) {
			if mounted {
				return "left", nil
			}
			return "", nil
		}
		unmountHook = func(_ context.Context, path string, id mounts.Identity) error {
			if path != r.Path {
				t.Errorf("unmount %s, want %s", path, r.Path)
			}
			unmounted = append(unmounted, id)
			mounted = false
			return emptyDir(r.Path)
		}
		t.Cleanup(func() { unmountHook = nil })
		if code, _, stderr := linux(t.Context(), "rm", "wt"); code != 1 || !strings.Contains(stderr, "does not serve it") {
			t.Fatalf("rm of a worktree whose daemon died: exit %d, stderr %q; want the refusal", code, stderr)
		}
		if code, stdout, stderr := linux(t.Context(), "rm", "--force", "wt"); code != 0 || !strings.HasPrefix(stdout, "removed "+r.Path+"; ") {
			t.Fatalf("rm --force wt: exit %d, stdout %q, stderr %q", code, stdout, stderr)
		}
		if !slices.Equal(unmounted, []mounts.Identity{"left"}) {
			t.Errorf("unmounts = %q, want one of the recorded mount", unmounted)
		}
		requireRemoved(t, main, r, head)
	})
}

// A rm --force that fails after it stopped the daemon leaves a worktree that
// the next rm --force removes: it says so, writes the recovery ref only
// after the archive, and keeps the record until the rest is gone.
func TestRmForceFinishesAnArtifactFSRemovalThatFailed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the permission bits that fail some steps")
	}
	for _, tc := range []struct {
		name string
		// fail makes the first rm fail, and returns what undoes that.
		fail func(t *testing.T, main string, r state.Record) func()
		// failed checks the state the failed rm left.
		failed func(t *testing.T, main string, r state.Record, archives []string)
	}{{
		name: "archive",
		fail: func(t *testing.T, _ string, r state.Record) func() {
			unreadable := filepath.Join(r.StateDir, "unreadable")
			if err := os.WriteFile(unreadable, nil, 0o000); err != nil {
				t.Fatal(err)
			}
			return func() { os.Chmod(unreadable, 0o600) }
		},
		failed: func(t *testing.T, main string, r state.Record, archives []string) {
			if len(archives) != 1 {
				t.Fatalf("archives = %q, want one", archives)
			}
			if entries, err := os.ReadDir(archives[0]); err != nil || len(entries) != 1 || entries[0].Name() != "worktree.json" {
				t.Errorf("the archive holds %v (%v), want its record alone", entries, err)
			}
			if refs := recoveryRefs(t, r.CommonDir); refs != "" {
				t.Errorf("recovery refs %q after a failed archive, want none", refs)
			}
			requireStopped(t, main, r)
		},
	}, {
		name: "recovery ref",
		fail: func(t *testing.T, main string, r state.Record) func() {
			// A ref at the archive's ID keeps git from writing a ref below it.
			blocking := "refs/slimtree/archive/" + state.ID(r.Path)
			gitIn(t, main, "update-ref", blocking, "HEAD")
			return func() { gitIn(t, main, "update-ref", "-d", blocking) }
		},
		failed: func(t *testing.T, main string, r state.Record, archives []string) {
			if len(archives) != 1 {
				t.Fatalf("archives = %q, want one", archives)
			}
			if _, err := os.Stat(filepath.Join(archives[0], stateArchiveName)); err != nil {
				t.Errorf("the archive before a failed recovery ref: %v, want its state", err)
			}
			requireStopped(t, main, r)
		},
	}, {
		name: "gitfile",
		fail: func(t *testing.T, _ string, _ state.Record) func() {
			renameHook = func(src, dst string) error {
				return &os.LinkError{Op: "rename", Old: src, New: dst, Err: syscall.EIO}
			}
			return func() { renameHook = nil }
		},
		failed: func(t *testing.T, main string, r state.Record, _ []string) {
			if refs := recoveryRefs(t, r.CommonDir); refs == "" {
				t.Error("no recovery ref after the archive")
			}
			requireStopped(t, main, r)
		},
	}, {
		name: "gitfile copy",
		fail: func(t *testing.T, _ string, r state.Record) func() {
			// The move across file systems copies the gitfile, then cannot
			// remove the saved one from the record directory.
			renameHook = func(src, dst string) error {
				return &os.LinkError{Op: "rename", Old: src, New: dst, Err: syscall.EXDEV}
			}
			home, err := openHome()
			if err != nil {
				t.Fatal(err)
			}
			dir := home.RecordDir(r.Path)
			if err := os.Chmod(dir, 0o500); err != nil {
				t.Fatal(err)
			}
			restore := func() {
				renameHook = nil
				os.Chmod(dir, 0o700)
			}
			t.Cleanup(restore)
			return restore
		},
		failed: func(t *testing.T, _ string, r state.Record, _ []string) {
			home, err := openHome()
			if err != nil {
				t.Fatal(err)
			}
			saved, err := os.ReadFile(filepath.Join(home.RecordDir(r.Path), gitfileName))
			if err != nil {
				t.Fatal(err)
			}
			if back, err := os.ReadFile(filepath.Join(r.Path, ".git")); err != nil || !bytes.Equal(back, saved) {
				t.Errorf("the copied .git = %q, %v; want the saved gitfile %q", back, err, saved)
			}
		},
	}, {
		name: "worktree remove",
		fail: func(t *testing.T, main string, _ state.Record) func() {
			// git cannot remove the worktree's directory, and then removes
			// its registration.
			parent := filepath.Dir(main)
			if err := os.Chmod(parent, 0o555); err != nil {
				t.Fatal(err)
			}
			restore := func() { os.Chmod(parent, 0o755) }
			t.Cleanup(restore)
			return restore
		},
		failed: func(t *testing.T, main string, r state.Record, _ []string) {
			if list := gitIn(t, main, "worktree", "list", "--porcelain"); strings.Contains(list, "worktree "+r.Path+"\n") {
				t.Errorf("git lists %s after git worktree remove failed to remove its directory:\n%s", r.Path, list)
			}
			if _, err := os.Stat(r.Path); err != nil {
				t.Errorf("%s: %v, want it left", r.Path, err)
			}
		},
	}, {
		name: "record",
		fail: func(t *testing.T, _ string, r state.Record) func() {
			stuck := filepath.Join(r.StateDir, "stuck")
			if err := os.Mkdir(stuck, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(stuck, "file"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(stuck, 0o555); err != nil {
				t.Fatal(err)
			}
			restore := func() { os.Chmod(stuck, 0o755) }
			t.Cleanup(restore)
			return restore
		},
		failed: func(t *testing.T, main string, r state.Record, _ []string) {
			if _, err := os.Lstat(r.Path); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("%s: %v, want it removed", r.Path, err)
			}
			if got := records(t); !reflect.DeepEqual(got, []state.Record{r}) {
				t.Errorf("records = %+v, want %+v kept", got, r)
			}
		},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			main, r := stoppedArtifactFS(t, map[string]string{"notes.txt": "kept\n"})
			head := strings.TrimSpace(gitIn(t, main, "rev-parse", "wt"))
			home, err := openHome()
			if err != nil {
				t.Fatal(err)
			}
			undo := tc.fail(t, main, r)
			code, stdout, stderr := linux(t.Context(), "rm", "--force", "wt")
			if want := "; slimtree stopped " + r.Path + ", and slimtree rm --force " + r.Path + " removes it\n"; code != 1 || stdout != "" || !strings.HasSuffix(stderr, want) {
				t.Fatalf("rm --force that fails at the %s: exit %d, stdout %q, stderr %q; want exit 1 and %q", tc.name, code, stdout, stderr, want)
			}
			archives, err := filepath.Glob(filepath.Join(home.Dir(), "archives", state.ID(r.Path), "*"))
			if err != nil {
				t.Fatal(err)
			}
			tc.failed(t, main, r, archives)
			undo()
			if code, stdout, stderr := linux(t.Context(), "rm", "--force", "wt"); code != 0 || !strings.HasPrefix(stdout, "removed "+r.Path+"; ") {
				t.Fatalf("rm --force after the failure: exit %d, stdout %q, stderr %q", code, stdout, stderr)
			}
			requireRemoved(t, main, r, head)
		})
	}
}

// requireStopped fails t unless git registers r's worktree, its gitfile is
// in the record directory, its mount point is empty, and its record stays.
func requireStopped(t *testing.T, main string, r state.Record) {
	t.Helper()
	if list := gitIn(t, main, "worktree", "list", "--porcelain"); !strings.Contains(list, "worktree "+r.Path+"\n") {
		t.Errorf("git does not list %s:\n%s", r.Path, list)
	}
	home, err := openHome()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home.RecordDir(r.Path), gitfileName)); err != nil {
		t.Errorf("the record directory's gitfile: %v", err)
	}
	if entries, err := os.ReadDir(r.Path); err != nil || len(entries) != 0 {
		t.Errorf("the mount point holds %v (%v), want nothing", entries, err)
	}
	if got := records(t); !reflect.DeepEqual(got, []state.Record{r}) {
		t.Errorf("records = %+v, want %+v", got, r)
	}
}

// rm --force removes an artifactfs worktree in each state that a stop, an
// earlier rm or the user can leave: its directory gone, its registration
// unlocked, its gitfile put back, or its gitfile lost.
func TestRmForceRemovesAnArtifactFSWorktreeInEachStateAStopLeaves(t *testing.T) {
	for _, tc := range []struct {
		name  string
		leave func(t *testing.T, main string, r state.Record, gitfile string)
	}{
		{name: "gone", leave: func(t *testing.T, _ string, r state.Record, _ string) {
			if err := os.Remove(r.Path); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "unlocked", leave: func(t *testing.T, main string, r state.Record, _ string) {
			gitIn(t, main, "worktree", "unlock", r.Path)
		}},
		{name: "gitfile put back", leave: func(t *testing.T, _ string, r state.Record, gitfile string) {
			if err := os.Rename(gitfile, filepath.Join(r.Path, ".git")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "gitfile lost", leave: func(t *testing.T, _ string, _ state.Record, gitfile string) {
			if err := os.Remove(gitfile); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			main, r := stoppedArtifactFS(t, map[string]string{"notes.txt": "kept\n"})
			head := strings.TrimSpace(gitIn(t, main, "rev-parse", "wt"))
			home, err := openHome()
			if err != nil {
				t.Fatal(err)
			}
			tc.leave(t, main, r, filepath.Join(home.RecordDir(r.Path), gitfileName))
			if code, stdout, stderr := linux(t.Context(), "rm", "--force", "wt"); code != 0 || !strings.HasPrefix(stdout, "removed "+r.Path+"; refs/slimtree/archive/") {
				t.Fatalf("rm --force wt: exit %d, stdout %q, stderr %q", code, stdout, stderr)
			}
			requireRemoved(t, main, r, head)
		})
	}
}

// rm --force refuses a mount point where nothing is mounted that holds
// files, which no mount serves and git worktree remove would delete, and a
// state root that a daemon slimtree did not record serves. rm --check
// refuses both. No refusal archives anything.
func TestRmForceRefusesWhatWouldLoseFiles(t *testing.T) {
	main, r := stoppedArtifactFS(t, map[string]string{"notes.txt": "kept\n"})
	stray := filepath.Join(r.Path, "stray")
	if err := os.WriteFile(stray, []byte("written while unmounted\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	want := r.Path + " holds stray, which no mount serves and git worktree remove would delete; move them away"
	for _, args := range [][]string{{"rm", "--force", "wt"}, {"rm", "--check", "--force", "wt"}} {
		if code, _, stderr := linux(t.Context(), args...); code != 1 || !strings.Contains(stderr, want) {
			t.Fatalf("%q over a stray file: exit %d, stderr %q; want %q", args, code, stderr, want)
		}
	}
	if data, err := os.ReadFile(stray); err != nil || string(data) != "written while unmounted\n" {
		t.Errorf("the stray file = %q, %v; want it kept", data, err)
	}
	if err := os.Remove(stray); err != nil {
		t.Fatal(err)
	}
	requireUnchanged(t, main, r)

	// A .git that is not the saved gitfile, while the gitfile is saved.
	home, err := openHome()
	if err != nil {
		t.Fatal(err)
	}
	strayGit := filepath.Join(r.Path, ".git")
	if err := os.WriteFile(strayGit, []byte("gitdir: elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	want = r.Path + " holds a .git that is not the gitfile slimtree keeps in " + filepath.Join(home.RecordDir(r.Path), gitfileName)
	if code, _, stderr := linux(t.Context(), "rm", "--force", "wt"); code != 1 || !strings.Contains(stderr, want) {
		t.Fatalf("rm --force over a stray .git: exit %d, stderr %q; want %q", code, stderr, want)
	}
	if data, err := os.ReadFile(strayGit); err != nil || string(data) != "gitdir: elsewhere\n" {
		t.Errorf("the stray .git = %q, %v; want it kept", data, err)
	}
	if err := os.Remove(strayGit); err != nil {
		t.Fatal(err)
	}
	requireUnchanged(t, main, r)

	lockPath := filepath.Join(r.StateDir, "config", "daemon.lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, []byte("4242\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := os.Open(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		unix.Flock(int(lock.Fd()), unix.LOCK_UN)
		lock.Close()
	})
	want = "pid 4242, a daemon slimtree did not record, serves " + r.StateDir + "; stop it, then rm again"
	for _, args := range [][]string{{"rm", "--force", "wt"}, {"rm", "--check", "--force", "wt"}} {
		if code, _, stderr := linux(t.Context(), args...); code != 1 || !strings.Contains(stderr, want) {
			t.Fatalf("%q while another daemon serves the state root: exit %d, stderr %q; want %q", args, code, stderr, want)
		}
	}
	requireUnchanged(t, main, r)
}

// rm --force of an artifactfs worktree on an unborn branch, as after git
// switch --orphan, archives its files and removes it, and writes no recovery
// ref: git worktree list gives its HEAD as the null object ID, which names
// no commit.
func TestRmForceRemovesAnArtifactFSWorktreeOnAnUnbornBranch(t *testing.T) {
	main, r := stoppedArtifactFS(t, map[string]string{"notes.txt": "kept\n"})
	gitIn(t, main, "--git-dir="+r.GitDir, "symbolic-ref", "HEAD", "refs/heads/orphan")
	home, err := openHome()
	if err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := linux(t.Context(), "rm", "--force", "wt")
	archives := filepath.Join(home.Dir(), "archives", state.ID(r.Path))
	m := regexp.MustCompile(`^removed ` + regexp.QuoteMeta(r.Path) + `; ` + regexp.QuoteMeta(archives) + `/(\d+)/state\.tar\.gz keeps its files` + "\n$").FindStringSubmatch(stdout)
	if code != 0 || m == nil {
		t.Fatalf("rm --force of a worktree on an unborn branch: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	requireNoWorktree(t, main, r.Path)
	if refs := recoveryRefs(t, r.CommonDir); refs != "" {
		t.Errorf("recovery refs %q, want none", refs)
	}
	archived, err := state.Archived{Dir: filepath.Join(archives, m[1])}.Record()
	if err != nil || archived.Head != "" || archived.Path != r.Path {
		t.Errorf("archived record = %+v, %v; want the worktree's, without head", archived, err)
	}
	if files := archiveFiles(t, filepath.Join(archives, m[1])); files["gitdir/HEAD"] != "ref: refs/heads/orphan\n" {
		t.Errorf("the archive's gitdir/HEAD = %q, want the unborn branch", files["gitdir/HEAD"])
	}
}

// rm refuses a worktree whose repository is missing, as on a volume that is
// not mounted, also with --force once and with --check, and changes nothing.
// Without the repository, rm cannot remove git's registration, which keeps
// the branch checked out. --force twice removes the worktree's record and
// archives its files, and leaves the registration, as the repository shows
// once it is back. The same holds for an apfsclone worktree.
func TestRmRefusesAWorktreeWhoseRepositoryIsMissing(t *testing.T) {
	main, r := stoppedArtifactFS(t, map[string]string{"notes.txt": "kept\n"})
	home, err := openHome()
	if err != nil {
		t.Fatal(err)
	}
	cloned := state.Record{Version: 1, Backend: "apfsclone", Path: filepath.Join(filepath.Dir(main), "cloned"), CommonDir: r.CommonDir, Branch: "cloned"}
	if err := home.Write(cloned); err != nil {
		t.Fatal(err)
	}
	away := main + "-away"
	t.Chdir(filepath.Dir(main))
	if err := os.Rename(main, away); err != nil {
		t.Fatal(err)
	}
	want := "the repository " + r.CommonDir + " is missing; reconnect its volume and run slimtree rm again, or if the repository is gone for good, pass --force twice"
	for _, args := range [][]string{{"rm", "wt"}, {"rm", "--force", "wt"}, {"rm", "--check", "--force", "wt"}, {"rm", "--force", "cloned"}} {
		if code, stdout, stderr := linux(t.Context(), args...); code != 1 || stdout != "" || !strings.Contains(stderr, want) {
			t.Errorf("%q while the repository is missing: exit %d, stdout %q, stderr %q; want exit 1 and %q", args, code, stdout, stderr, want)
		}
	}
	if got := records(t); !reflect.DeepEqual(got, []state.Record{cloned, r}) {
		t.Fatalf("records after the refusals = %+v, want %+v", got, []state.Record{cloned, r})
	}
	if _, err := os.Stat(r.StateDir); err != nil {
		t.Fatalf("the state root after the refusals: %v", err)
	}

	for _, name := range []string{"wt", "cloned"} {
		if code, stdout, stderr := linux(t.Context(), "rm", "--force", "--force", name); code != 0 || !strings.HasPrefix(stdout, "removed "+filepath.Join(filepath.Dir(main), name)) {
			t.Fatalf("rm --force --force %s: exit %d, stdout %q, stderr %q", name, code, stdout, stderr)
		}
	}
	if got := records(t); len(got) != 0 {
		t.Errorf("records = %+v, want none", got)
	}
	archives, err := filepath.Glob(filepath.Join(home.Dir(), "archives", state.ID(r.Path), "*", stateArchiveName))
	if err != nil || len(archives) != 1 {
		t.Errorf("the state archives of %s = %q, %v; want one", r.Path, archives, err)
	}
	if err := os.Rename(away, main); err != nil {
		t.Fatal(err)
	}
	if list := gitIn(t, main, "worktree", "list", "--porcelain"); !strings.Contains(list, "worktree "+r.Path+"\n") {
		t.Errorf("git no longer lists %s, which rm could not unregister:\n%s", r.Path, list)
	}
}

// stop, resume and rm leave the mount a dead daemon left while a daemon
// slimtree did not record holds the state root's daemon lock. Before Linux
// 6.8 that daemon's mount can carry the recorded identity.
func TestDeadDaemonsMountStaysWhileAnUnrecordedDaemonServes(t *testing.T) {
	_, r := stoppedArtifactFS(t, map[string]string{"notes.txt": "kept\n"})
	dead := exec.Command("sleep", "60")
	if err := dead.Start(); err != nil {
		t.Fatal(err)
	}
	token, err := mounts.StartToken(dead.Process.Pid)
	if err != nil || token == "" {
		t.Fatalf("StartToken(%d) = %q, %v", dead.Process.Pid, token, err)
	}
	dead.Process.Kill()
	dead.Wait()
	r.DaemonPID, r.DaemonToken, r.MountIdentity = dead.Process.Pid, token, "left"
	home, err := openHome()
	if err != nil {
		t.Fatal(err)
	}
	if err := home.Write(r); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(r.StateDir, "config", "daemon.lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, []byte("4242\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := os.Open(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		unix.Flock(int(lock.Fd()), unix.LOCK_UN)
		lock.Close()
	})
	identifyHook = func(string) (mounts.Identity, error) { return "left", nil }
	var unmounted []string
	unmountHook = func(_ context.Context, path string, _ mounts.Identity) error {
		unmounted = append(unmounted, path)
		return nil
	}
	t.Cleanup(func() { identifyHook, unmountHook = nil, nil })
	for _, args := range [][]string{{"stop", "wt"}, {"resume", "wt"}, {"rm", "--force", "wt"}} {
		if code, _, stderr := linux(t.Context(), args...); code != 1 || !strings.Contains(stderr, "pid 4242, a daemon slimtree did not record, serves "+r.StateDir) {
			t.Errorf("%q while pid 4242 holds the daemon lock: exit %d, stderr %q; want the refusal", args, code, stderr)
		}
	}
	if len(unmounted) != 0 {
		t.Errorf("unmounted %q while an unrecorded daemon served the state root", unmounted)
	}
	if got := records(t); !reflect.DeepEqual(got, []state.Record{r}) {
		t.Errorf("records = %+v, want %+v", got, r)
	}
}
