package slimtree

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/execfile"
	"github.com/cloudflare/artifact-fs/internal/slimtree/doctor"
	"github.com/cloudflare/artifact-fs/internal/slimtree/state"
	"github.com/cloudflare/artifact-fs/internal/version"
)

// healthyDarwin is a Mac where every doctor check that new and rm run passes.
func healthyDarwin() doctor.Probe {
	return doctor.Probe{
		LookPath: func(file string) (string, error) { return "/usr/bin/" + file, nil },
		Output: func(name string, _ ...string) (string, error) {
			return map[string]string{"xcode-select": "/Library/Developer/CommandLineTools\n", "git": "git version 2.54.0\n"}[name], nil
		},
	}
}

// slimtree runs slimtree's commands with args as on a Mac, with ctx.
func slimtree(ctx context.Context, args ...string) (code int, stdout, stderr string) {
	var out, errOut bytes.Buffer
	code = run(ctx, "darwin", healthyDarwin(), args, &out, &errOut)
	return code, out.String(), errOut.String()
}

// cloneWith makes new clone with clone, or, when clone is nil, with
// clonefile(2) on macOS and by copying elsewhere.
func cloneWith(t *testing.T, clone func(src, dst string) error) {
	t.Helper()
	if clone == nil && runtime.GOOS != "darwin" {
		clone = copyClone
	}
	cloneFile = clone
	t.Cleanup(func() { cloneFile = nil })
}

// copyClone clones as clonefile(2) with CLONE_NOFOLLOW does, by copying.
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
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.WriteFile(dst, data, fi.Mode().Perm()); err != nil {
		return err
	}
	return os.Chmod(dst, fi.Mode().Perm())
}

// newRepo makes a repository whose main worktree, main, holds a file, an
// executable, a symlink and a file in a directory, and has a local edit to
// one of them, and the branch topic a commit past main. It makes main the
// current directory, and returns main.
func newRepo(t *testing.T) string {
	t.Helper()
	root, err := state.Canonical(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	main := filepath.Join(root, "main")
	gitIn(t, root, "init", "-q", "-b", "main", main)
	for name, mode := range map[string]os.FileMode{"README.md": 0o644, "bin/run.sh": 0o755, "src/lib.go": 0o644} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(main, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(main, name), []byte(name+"\n"), mode); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("README.md", filepath.Join(main, "link")); err != nil {
		t.Fatal(err)
	}
	gitIn(t, main, "add", "-A")
	gitIn(t, main, "commit", "-qm", "base")
	gitIn(t, main, "switch", "-qc", "topic")
	if err := os.WriteFile(filepath.Join(main, "src/lib.go"), []byte("topic\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, main, "commit", "-qam", "topic")
	gitIn(t, main, "switch", "-q", "main")
	if err := os.WriteFile(filepath.Join(main, "README.md"), []byte("local edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(main)
	return main
}

// trackedTree describes each tracked path of worktree: a symlink's target, or
// a file's executable bit and content.
func trackedTree(t *testing.T, worktree string) map[string]string {
	t.Helper()
	tree := map[string]string{}
	for name := range strings.SplitSeq(gitIn(t, worktree, "ls-files", "-z"), "\x00") {
		if name == "" {
			continue
		}
		path := filepath.Join(worktree, name)
		fi, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				t.Fatal(err)
			}
			tree[name] = "link " + target
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		// Git tracks only the executable bit. A clone keeps the base file's
		// other bits, and a checkout takes them from the umask.
		kind := "file "
		if fi.Mode().Perm()&0o111 != 0 {
			kind = "exec "
		}
		tree[name] = kind + string(data)
	}
	return tree
}

// records returns the records of the home SLIMTREE_HOME names.
func records(t *testing.T) []state.Record {
	t.Helper()
	home, err := openHome()
	if err != nil {
		t.Fatal(err)
	}
	r, err := home.Records(func(err error) { t.Error(err) })
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// gitFindsNothing runs git in dir and fails t unless git exits 1, as a
// lookup that finds nothing does.
func gitFindsNothing(t *testing.T, dir string, args ...string) {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Errorf("git %s = %v, %q; want exit 1 and nothing found", strings.Join(args, " "), err, out)
	}
}

// requireNoWorktree fails t when path exists, git lists it as a worktree of
// the repository whose worktree main is, or slimtree has a record of it.
func requireNoWorktree(t *testing.T, main, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s exists: %v", path, err)
	}
	if list := gitIn(t, main, "worktree", "list", "--porcelain"); strings.Contains(list, "worktree "+path+"\n") {
		t.Errorf("git lists %s:\n%s", path, list)
	}
	for _, r := range records(t) {
		if r.Path == path {
			t.Errorf("slimtree has a record of %s: %+v", path, r)
		}
	}
	home, err := openHome()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(home.Dir(), "w", state.ID(path))); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the record directory of %s exists: %v", path, err)
	}
}

// new makes a worktree whose files match a plain git worktree add's, and the
// record of it, and prints its path.
func TestNewMatchesGitWorktreeAdd(t *testing.T) {
	homeAfterFirstRun(t)
	cloneWith(t, nil)
	main := newRepo(t)
	path := filepath.Join(filepath.Dir(main), "main-feat")
	code, stdout, stderr := slimtree(t.Context(), "new", "feat", "--from", "topic")
	if code != 0 || stdout != path+"\n" {
		t.Fatalf("new feat --from topic: exit %d, stdout %q, stderr %q; want 0 and %s", code, stdout, stderr, path)
	}
	plain := filepath.Join(filepath.Dir(main), "plain")
	gitIn(t, main, "worktree", "add", "-q", "-b", "plain", plain, "topic")

	if got, want := trackedTree(t, path), trackedTree(t, plain); !maps.Equal(got, want) {
		t.Fatalf("new's tree = %q\nplain tree = %q", got, want)
	}
	if got := gitIn(t, path, "status", "--porcelain"); got != "" {
		t.Fatalf("git status in the new worktree lists %q", got)
	}
	if got, want := gitIn(t, path, "rev-parse", "HEAD"), gitIn(t, main, "rev-parse", "topic"); got != want {
		t.Fatalf("HEAD = %s, want topic's %s", got, want)
	}
	if got := gitIn(t, path, "symbolic-ref", "HEAD"); got != "refs/heads/feat\n" {
		t.Fatalf("HEAD names %q, want refs/heads/feat", got)
	}
	gitFindsNothing(t, main, "config", "branch.feat.merge")

	got := records(t)
	if len(got) != 1 || got[0].Created.IsZero() {
		t.Fatalf("records = %+v, want one, with its creation time", got)
	}
	got[0].Created = got[0].Created.UTC().Truncate(0)
	want := state.Record{Version: 1, Backend: "apfsclone", Path: path, CommonDir: filepath.Join(main, ".git"), Branch: "feat",
		Created: got[0].Created, SlimtreeVersion: version.String()}
	if got[0] != want {
		t.Fatalf("record = %+v, want %+v", got[0], want)
	}
}

// new checks out a branch that exists, and starts a new branch where git
// worktree add would from the current directory: at HEAD, or at --from, which
// --track tracks.
func TestNewStartsWhereGitWorktreeAddStarts(t *testing.T) {
	homeAfterFirstRun(t)
	cloneWith(t, nil)
	main := newRepo(t)
	topic := gitIn(t, main, "rev-parse", "topic")
	// The current directory is a worktree whose HEAD and upstream differ from
	// the base's.
	here := filepath.Join(filepath.Dir(main), "here")
	gitIn(t, main, "worktree", "add", "-q", "-b", "here", here, "topic")
	gitIn(t, here, "branch", "-q", "--set-upstream-to=topic")
	t.Chdir(here)

	for _, tc := range []struct {
		name  string
		args  []string
		merge string
	}{
		{name: "topic", args: []string{"new", "topic"}},
		{name: "at-head", args: []string{"new", "at-head"}},
		{name: "tracking", args: []string{"new", "tracking", "--from", "@{upstream}", "--track"}, merge: "refs/heads/topic\n"},
	} {
		path := filepath.Join(filepath.Dir(main), "main-"+tc.name)
		if code, stdout, stderr := slimtree(t.Context(), tc.args...); code != 0 || stdout != path+"\n" {
			t.Fatalf("%s: exit %d, stdout %q, stderr %q", strings.Join(tc.args, " "), code, stdout, stderr)
		}
		if got := gitIn(t, path, "rev-parse", "HEAD"); got != topic {
			t.Errorf("%s: HEAD = %s, want topic's %s", tc.name, got, topic)
		}
		if got := gitIn(t, path, "symbolic-ref", "HEAD"); got != "refs/heads/"+tc.name+"\n" {
			t.Errorf("%s: HEAD names %q", tc.name, got)
		}
		if tc.merge == "" {
			gitFindsNothing(t, main, "config", "branch."+tc.name+".merge")
		} else if got := gitIn(t, main, "config", "branch."+tc.name+".merge"); got != tc.merge {
			t.Errorf("%s merges %q, want %q", tc.name, got, tc.merge)
		}
	}

	// In a detached worktree, HEAD names no branch to track. git worktree
	// add, which runs in the base, would track the base's branch.
	detached := filepath.Join(filepath.Dir(main), "detached")
	gitIn(t, main, "worktree", "add", "-q", "--detach", detached, "topic")
	for _, tc := range []struct {
		dir  string
		args []string
		want string
	}{
		{here, []string{"new", "topic", "--from", "main"}, "the branch topic exists; drop --from to check it out\n"},
		{here, []string{"new", "lone", "--track"}, "--track needs --from, the branch to track\n"},
		{detached, []string{"new", "lone", "--from", "HEAD", "--track"}, "--track needs --from to name a branch, and HEAD names none\n"},
		{detached, []string{"new", "lone", "--from", "@", "--track"}, "--track needs --from to name a branch, and @ names none\n"},
	} {
		t.Chdir(tc.dir)
		if code, stdout, stderr := slimtree(t.Context(), tc.args...); code != 1 || stdout != "" || stderr != tc.want {
			t.Errorf("%s: exit %d, stdout %q, stderr %q; want 1 and %q", strings.Join(tc.args, " "), code, stdout, stderr, tc.want)
		}
	}
	gitFindsNothing(t, main, "rev-parse", "--verify", "--quiet", "refs/heads/lone")
}

// A new that fails removes the worktree and the branch it made, and keeps a
// branch that existed and the directory git refused to fill.
func TestNewRollsBackWhatItMade(t *testing.T) {
	homeAfterFirstRun(t)
	main := newRepo(t)
	topic := gitIn(t, main, "rev-parse", "topic")
	full := filepath.Join(filepath.Dir(main), "full")
	if err := os.MkdirAll(full, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(full, "keep"), []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Ctrl-C while new fills the worktree: the next git fails, after git
	// worktree add made the branch, the directories above the worktree, and
	// the worktree.
	fresh := filepath.Join(filepath.Dir(main), "fresh")
	path := filepath.Join(fresh, "deeper", "feat")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cloneWith(t, func(src, dst string) error {
		if strings.HasPrefix(dst, path+string(filepath.Separator)) {
			cancel()
		}
		return copyClone(src, dst)
	})
	if code, stdout, stderr := slimtree(ctx, "new", "feat", "--from", "topic", "--track", "--path", path); code != 1 || stdout != "" || !strings.Contains(stderr, "context canceled") {
		t.Fatalf("an interrupted new: exit %d, stdout %q, stderr %q; want 1 and context canceled", code, stdout, stderr)
	}
	requireNoWorktree(t, main, path)
	if _, err := os.Lstat(fresh); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s, which git worktree add made, exists: %v", fresh, err)
	}
	gitFindsNothing(t, main, "rev-parse", "--verify", "--quiet", "refs/heads/feat")
	gitFindsNothing(t, main, "config", "--get-regexp", `^branch\.feat\.`)

	// git worktree add makes the branch, then refuses a directory that holds
	// files. The rollback keeps the config new did not write.
	cloneWith(t, nil)
	gitIn(t, main, "config", "branch.made.pushRemote", "origin")
	gitIn(t, main, "config", "branch.autoSetupRebase", "always")
	for _, tc := range []struct {
		args []string
		// kept is the branch that must still point at topic.
		kept, gone string
	}{
		{args: []string{"new", "made", "--from", "topic", "--track", "--path", full}, gone: "made"},
		{args: []string{"new", "topic", "--path", full}, kept: "topic"},
	} {
		if code, stdout, stderr := slimtree(t.Context(), tc.args...); code != 1 || stdout != "" || !strings.Contains(stderr, "already exists") {
			t.Fatalf("%s: exit %d, stdout %q, stderr %q; want 1 and git's refusal", strings.Join(tc.args, " "), code, stdout, stderr)
		}
		if got, err := os.ReadFile(filepath.Join(full, "keep")); err != nil || string(got) != "keep\n" {
			t.Fatalf("%s left %s/keep = %q, %v", strings.Join(tc.args, " "), full, got, err)
		}
		if entries, err := os.ReadDir(full); err != nil || len(entries) != 1 {
			t.Fatalf("%s left %s with %d entries, %v; want keep alone", strings.Join(tc.args, " "), full, len(entries), err)
		}
		if tc.gone != "" {
			gitFindsNothing(t, main, "rev-parse", "--verify", "--quiet", "refs/heads/"+tc.gone)
			if got := gitIn(t, main, "config", "--get-regexp", `^branch\.`+tc.gone+`\.`); got != "branch."+tc.gone+".pushremote origin\n" {
				t.Fatalf("branch.%s config = %q, want pushremote alone", tc.gone, got)
			}
		}
		if tc.kept != "" {
			if got := gitIn(t, main, "rev-parse", tc.kept); got != topic {
				t.Fatalf("%s points at %s, want %s", tc.kept, got, topic)
			}
		}
	}
	if got := records(t); len(got) != 0 {
		t.Fatalf("records = %+v, want none", got)
	}
}

// A failed post-checkout hook leaves a complete worktree, as git worktree add
// does, and new records it.
func TestNewKeepsAWorktreeWhosePostCheckoutHookFailed(t *testing.T) {
	homeAfterFirstRun(t)
	cloneWith(t, nil)
	main := newRepo(t)
	if err := execfile.Write(filepath.Join(main, ".git", "hooks", "post-checkout"), []byte("#!/bin/sh\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(filepath.Dir(main), "main-feat")
	code, stdout, stderr := slimtree(t.Context(), "new", "feat")
	if code != 1 || stdout != path+"\n" || !strings.HasSuffix(stderr, "the post-checkout hook in "+path+" failed: exit status 3\n") {
		t.Fatalf("new with a failing hook: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if got := gitIn(t, path, "status", "--porcelain"); got != "" {
		t.Fatalf("git status in the worktree lists %q", got)
	}
	if got := records(t); len(got) != 1 || got[0].Path != path {
		t.Fatalf("records = %+v, want the one of %s", got, path)
	}
}

// rm removes the worktree, git's registration and the record, keeps the
// branch, and keeps HEAD in a recovery ref beside an archived copy of the
// record.
func TestRmRemovesTheWorktreeAndItsRecord(t *testing.T) {
	homeDir := homeAfterFirstRun(t)
	cloneWith(t, nil)
	main := newRepo(t)
	path := filepath.Join(filepath.Dir(main), "main-feat")
	if code, _, stderr := slimtree(t.Context(), "new", "feat"); code != 0 {
		t.Fatalf("new feat: exit %d, stderr %q", code, stderr)
	}
	gitIn(t, path, "commit", "-q", "--allow-empty", "-m", "work")
	head := strings.TrimSpace(gitIn(t, path, "rev-parse", "HEAD"))
	if got := records(t); len(got) != 1 || got[0].Path != path {
		t.Fatalf("records after new = %+v, want the one of %s", got, path)
	}
	if list := gitIn(t, main, "worktree", "list", "--porcelain"); !strings.Contains(list, "worktree "+path+"\n") {
		t.Fatalf("git does not list %s after new:\n%s", path, list)
	}

	code, stdout, stderr := slimtree(t.Context(), "rm", "main-feat")
	id := state.ID(path)
	m := regexp.MustCompile(`^removed ` + regexp.QuoteMeta(path) + `; (refs/slimtree/archive/` + id + `/(\d+)/HEAD) keeps its HEAD ` + head + "\n$").FindStringSubmatch(stdout)
	if code != 0 || m == nil {
		t.Fatalf("rm main-feat: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	requireNoWorktree(t, main, path)
	if got := strings.TrimSpace(gitIn(t, main, "rev-parse", "feat")); got != head {
		t.Fatalf("feat points at %s, want %s", got, head)
	}
	if got := gitIn(t, main, "for-each-ref", "--format=%(refname) %(objectname)", "refs/slimtree/"); got != m[1]+" "+head+"\n" {
		t.Fatalf("recovery refs = %q, want %s at %s", got, m[1], head)
	}
	data, err := os.ReadFile(filepath.Join(homeDir, "archives", id, m[2], "worktree.json"))
	if err != nil || !strings.Contains(string(data), `"common_dir": "`+filepath.Join(main, ".git")+`"`) {
		t.Fatalf("archived record = %s, %v; want one that names the repository", data, err)
	}
}

// This release makes and removes only apfsclone worktrees. new refuses an
// artifactfs worktree, Linux's default, and rm an artifactfs record, each with
// exit 1, and changes nothing.
func TestArtifactFSWorktreesAreNotInThisReleaseYet(t *testing.T) {
	homeAfterFirstRun(t)
	main := newRepo(t)
	const notYet = ": artifactfs worktrees are not in this release yet; this release makes and removes only apfsclone worktrees, on macOS\n"
	for _, tc := range []struct {
		goos  string
		probe doctor.Probe
		args  []string
	}{
		{goos: "linux", probe: healthyLinux(), args: []string{"new", "feat"}},
		{goos: "linux", probe: healthyLinux(), args: []string{"--backend", "artifactfs", "new", "feat"}},
		{goos: "darwin", probe: healthyDarwin(), args: []string{"--backend", "artifactfs", "new", "feat"}},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(t.Context(), tc.goos, tc.probe, tc.args, &stdout, &stderr); code != 1 || stdout.Len() != 0 || stderr.String() != "slimtree new"+notYet {
			t.Fatalf("%s on %s: exit %d, stdout %q, stderr %q; want exit 1 and the refusal", tc.args, tc.goos, code, stdout.String(), stderr.String())
		}
		requireNoWorktree(t, main, filepath.Join(filepath.Dir(main), "main-feat"))
		gitFindsNothing(t, main, "rev-parse", "--verify", "--quiet", "refs/heads/feat")
	}

	served := filepath.Join(filepath.Dir(main), "served")
	if err := os.Mkdir(served, 0o755); err != nil {
		t.Fatal(err)
	}
	home, err := openHome()
	if err != nil {
		t.Fatal(err)
	}
	record := state.Record{Version: 1, Backend: "artifactfs", Path: served, CommonDir: filepath.Join(main, ".git"), Branch: "served"}
	if err := home.Write(record); err != nil {
		t.Fatal(err)
	}
	for goos, probe := range map[string]doctor.Probe{"linux": healthyLinux(), "darwin": healthyDarwin()} {
		var stdout, stderr bytes.Buffer
		if code := run(t.Context(), goos, probe, []string{"rm", "served"}, &stdout, &stderr); code != 1 || stdout.Len() != 0 || stderr.String() != "slimtree rm "+served+notYet {
			t.Fatalf("rm served on %s: exit %d, stdout %q, stderr %q; want exit 1 and the refusal", goos, code, stdout.String(), stderr.String())
		}
		if got := records(t); !reflect.DeepEqual(got, []state.Record{record}) {
			t.Fatalf("records after rm on %s = %+v, want %+v", goos, got, record)
		}
		if _, err := os.Stat(served); err != nil {
			t.Fatalf("rm on %s removed %s: %v", goos, served, err)
		}
	}
}

// rm refuses a worktree in which git status lists anything, and changes
// nothing, unless --force.
func TestRmRefusesAWorktreeWithChanges(t *testing.T) {
	homeAfterFirstRun(t)
	cloneWith(t, nil)
	main := newRepo(t)
	path := filepath.Join(filepath.Dir(main), "main-feat")
	if code, _, stderr := slimtree(t.Context(), "new", "feat"); code != 0 {
		t.Fatalf("new feat: exit %d, stderr %q", code, stderr)
	}
	if err := os.WriteFile(filepath.Join(path, "notes.txt"), []byte("notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// git worktree remove would hide the file too, then delete it.
	gitIn(t, main, "config", "status.showUntrackedFiles", "no")
	want := "git status lists changes in " + path + "; commit them, or discard them with slimtree rm --force\n"
	if code, stdout, stderr := slimtree(t.Context(), "rm", path); code != 1 || stdout != "" || stderr != want {
		t.Fatalf("rm of a worktree with changes: exit %d, stdout %q, stderr %q; want 1 and %q", code, stdout, stderr, want)
	}
	if _, err := os.Lstat(filepath.Join(path, "notes.txt")); err != nil {
		t.Fatal(err)
	}
	if got := records(t); len(got) != 1 {
		t.Fatalf("records = %+v, want the one of %s", got, path)
	}
	if got := gitIn(t, main, "for-each-ref", "refs/slimtree/"); got != "" {
		t.Fatalf("a refused rm wrote recovery refs %q", got)
	}

	if code, _, stderr := slimtree(t.Context(), "rm", "--force", path); code != 0 {
		t.Fatalf("rm --force: exit %d, stderr %q", code, stderr)
	}
	requireNoWorktree(t, main, path)
}

// rm refuses a name that also names another directory here, so it never
// removes a worktree the user did not mean.
func TestRmRefusesANameThatAlsoNamesADirectoryHere(t *testing.T) {
	homeAfterFirstRun(t)
	cloneWith(t, nil)
	main := newRepo(t)
	path := filepath.Join(filepath.Dir(main), "elsewhere", "feat")
	if code, _, stderr := slimtree(t.Context(), "new", "feat", "--path", path); code != 0 {
		t.Fatalf("new feat: exit %d, stderr %q", code, stderr)
	}
	if err := os.Mkdir(filepath.Join(main, "feat"), 0o755); err != nil {
		t.Fatal(err)
	}
	want := "feat names the worktree " + path + " and also ./feat; name the one you mean by its path\n"
	if code, stdout, stderr := slimtree(t.Context(), "rm", "feat"); code != 1 || stdout != "" || stderr != want {
		t.Fatalf("rm feat: exit %d, stdout %q, stderr %q; want 1 and %q", code, stdout, stderr, want)
	}
	if got := gitIn(t, path, "status", "--porcelain"); got != "" {
		t.Fatalf("git status in %s lists %q", path, got)
	}

	// ./feat that is a symlink to the worktree names the worktree.
	if err := os.Remove(filepath.Join(main, "feat")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path, filepath.Join(main, "feat")); err != nil {
		t.Fatal(err)
	}
	if code, stdout, stderr := slimtree(t.Context(), "rm", "feat"); code != 0 {
		t.Fatalf("rm feat through a symlink to it: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	requireNoWorktree(t, main, path)
}

// rm refuses a worktree whose directory is gone, which can be on a volume
// that is not mounted. rm --force removes git's registration and the record,
// and keeps HEAD in a recovery ref.
func TestRmForceDropsAGoneWorktree(t *testing.T) {
	homeAfterFirstRun(t)
	cloneWith(t, nil)
	main := newRepo(t)
	path := filepath.Join(filepath.Dir(main), "main-feat")
	if code, _, stderr := slimtree(t.Context(), "new", "feat"); code != 0 {
		t.Fatalf("new feat: exit %d, stderr %q", code, stderr)
	}
	head := strings.TrimSpace(gitIn(t, path, "rev-parse", "HEAD"))
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	want := path + " is gone; if its volume is not mounted, mount it and rerun. slimtree rm --force removes git's registration of it and its record\n"
	if code, stdout, stderr := slimtree(t.Context(), "rm", path); code != 1 || stdout != "" || stderr != want {
		t.Fatalf("rm of a gone worktree: exit %d, stdout %q, stderr %q; want 1 and %q", code, stdout, stderr, want)
	}
	if list := gitIn(t, main, "worktree", "list", "--porcelain"); !strings.Contains(list, "worktree "+path+"\n") {
		t.Fatalf("a refused rm removed git's registration of %s:\n%s", path, list)
	}
	// A Ctrl-C once rm listed the worktrees fails the command, which changes
	// nothing.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	goneListed = cancel
	code, stdout, stderr := slimtree(ctx, "rm", "--force", path)
	goneListed = nil
	if code != 1 || stdout != "" || stderr != "context canceled\n" {
		t.Fatalf("a canceled rm --force: exit %d, stdout %q, stderr %q; want 1 and context canceled", code, stdout, stderr)
	}
	if list := gitIn(t, main, "worktree", "list", "--porcelain"); !strings.Contains(list, "worktree "+path+"\n") || len(records(t)) != 1 {
		t.Fatalf("a canceled rm --force removed the registration or the record:\n%s", list)
	}
	if code, stdout, stderr := slimtree(t.Context(), "rm", "--force", path); code != 0 || !strings.HasSuffix(stdout, " keeps its HEAD "+head+"\n") {
		t.Fatalf("rm --force of a gone worktree: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	requireNoWorktree(t, main, path)
	if got := gitIn(t, main, "for-each-ref", "--format=%(objectname)", "refs/slimtree/"); got != head+"\n" {
		t.Fatalf("recovery refs point at %q, want %s", got, head)
	}
}

// A rollback keeps a branch that moved after new made it, and says so.
func TestNewRollbackKeepsABranchThatMoved(t *testing.T) {
	homeAfterFirstRun(t)
	main := newRepo(t)
	topic, moved := gitIn(t, main, "rev-parse", "topic"), gitIn(t, main, "rev-parse", "main")
	path := filepath.Join(filepath.Dir(main), "main-feat")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cloneWith(t, func(src, dst string) error {
		if strings.HasPrefix(dst, path+string(filepath.Separator)) && ctx.Err() == nil {
			gitIn(t, main, "update-ref", "refs/heads/feat", strings.TrimSpace(moved))
			cancel()
		}
		return copyClone(src, dst)
	})
	want := "slimtree kept the branch feat, which it made at " + strings.TrimSpace(topic) + " and which now points at " + strings.TrimSpace(moved) + "\n"
	if code, stdout, stderr := slimtree(ctx, "new", "feat", "--from", "topic"); code != 1 || stdout != "" || !strings.HasSuffix(stderr, want) {
		t.Fatalf("new: exit %d, stdout %q, stderr %q; want 1 and %q", code, stdout, stderr, want)
	}
	requireNoWorktree(t, main, path)
	if got := gitIn(t, main, "rev-parse", "feat"); got != moved {
		t.Fatalf("feat points at %s, want %s", got, moved)
	}
}

// new refuses a base that is not a worktree of the current repository, or
// that slimtree.base names by a relative path, and creates nothing.
func TestNewRefusesABaseOutsideTheRepository(t *testing.T) {
	homeAfterFirstRun(t)
	cloneWith(t, nil)
	main := newRepo(t)
	other := filepath.Join(filepath.Dir(main), "other")
	gitIn(t, filepath.Dir(main), "init", "-q", "-b", "main", other)
	gitIn(t, other, "commit", "-q", "--allow-empty", "-m", "other")
	otherWorktrees := gitIn(t, other, "worktree", "list", "--porcelain")
	for _, tc := range []struct{ base, want string }{
		{"../other", "slimtree.base ../other is not an absolute path\n"},
		{other, "the base " + other + " is not the top directory of a worktree of " + filepath.Join(main, ".git") + "; set git config slimtree.base to one\n"},
	} {
		gitIn(t, main, "config", "slimtree.base", tc.base)
		if code, stdout, stderr := slimtree(t.Context(), "new", "feat"); code != 1 || stdout != "" || stderr != tc.want {
			t.Errorf("new with slimtree.base %s: exit %d, stdout %q, stderr %q; want 1 and %q", tc.base, code, stdout, stderr, tc.want)
		}
	}
	gitFindsNothing(t, main, "rev-parse", "--verify", "--quiet", "refs/heads/feat")
	gitFindsNothing(t, other, "rev-parse", "--verify", "--quiet", "refs/heads/feat")
	if got := gitIn(t, other, "worktree", "list", "--porcelain"); got != otherWorktrees {
		t.Fatalf("other's worktrees = %q, want %q", got, otherWorktrees)
	}
	if got := records(t); len(got) != 0 {
		t.Fatalf("records = %+v, want none", got)
	}
}

// new runs the post-checkout hook holding no lock, so a hook can run
// slimtree, and a long hook stalls no other command.
func TestNewRunsPostCheckoutHoldingNoLock(t *testing.T) {
	homeDir := homeAfterFirstRun(t)
	cloneWith(t, nil)
	main := newRepo(t)
	probe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := execfile.Write(filepath.Join(main, ".git", "hooks", "post-checkout"), []byte("#!/bin/sh\nexec '"+probe+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(lockProbeEnv, filepath.Join(homeDir, "lock"))
	if code, _, stderr := slimtree(t.Context(), "new", "feat"); code != 0 || !strings.Contains(stderr, "\nlock free\n") {
		t.Fatalf("new: exit %d, stderr %q; want 0 and the hook's lock free", code, stderr)
	}
}

// A Ctrl-C can land after git worktree add registered the worktree, before
// Go reaped git, which then reports a failure. The rollback still removes
// the worktree, also when a git worktree add that was killed left it locked
// as initializing. A fake git runs the real git worktree add, locks the
// worktree, then exits as a git that SIGINT stopped.
func TestNewRollsBackAWorktreeGitRegisteredBeforeItFailed(t *testing.T) {
	homeAfterFirstRun(t)
	cloneWith(t, nil)
	main := newRepo(t)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := execfile.Write(filepath.Join(bin, "git"), []byte("#!/bin/sh\nif [ \"$3 $4\" = \"worktree add\" ]; then\n"+
		"  '"+realGit+"' \"$@\" || exit\n"+
		"  eval \"dest=\\${$(($# - 1))}\"\n"+
		"  '"+realGit+"' -C \"$2\" worktree lock --reason initializing \"$dest\" || exit\n"+
		"  exit 130\nfi\nexec '"+realGit+"' \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	path := filepath.Join(filepath.Dir(main), "main-feat")
	if code, stdout, stderr := slimtree(t.Context(), "new", "feat", "--from", "topic"); code != 1 || stdout != "" || !strings.Contains(stderr, "exit status 130") {
		t.Fatalf("new: exit %d, stdout %q, stderr %q; want 1 and git's exit status 130", code, stdout, stderr)
	}
	requireNoWorktree(t, main, path)
	gitFindsNothing(t, main, "rev-parse", "--verify", "--quiet", "refs/heads/feat")
}

// new refuses a path at which git registers a worktree, which a rollback
// would otherwise remove.
func TestNewRefusesAPathGitRegisters(t *testing.T) {
	homeAfterFirstRun(t)
	cloneWith(t, nil)
	main := newRepo(t)
	path := filepath.Join(filepath.Dir(main), "main-feat")
	gitIn(t, main, "worktree", "add", "-q", "-b", "other", path)
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	want := "git already registers a worktree at " + path + "\n"
	if code, stdout, stderr := slimtree(t.Context(), "new", "feat", "--from", "topic"); code != 1 || stdout != "" || stderr != want {
		t.Fatalf("new: exit %d, stdout %q, stderr %q; want 1 and %q", code, stdout, stderr, want)
	}
	if list := gitIn(t, main, "worktree", "list", "--porcelain"); !strings.Contains(list, "worktree "+path+"\n") {
		t.Fatalf("git no longer registers %s:\n%s", path, list)
	}
	gitFindsNothing(t, main, "rev-parse", "--verify", "--quiet", "refs/heads/feat")
}

// On a case-insensitive volume, the Mac's default, new and rm accept a path
// spelled in another case than the directory's: in slimtree.base, the
// current directory, --path and rm's argument.
func TestNewAndRmAcceptAnotherSpelling(t *testing.T) {
	homeAfterFirstRun(t)
	cloneWith(t, nil)
	main := newRepo(t)
	upper := filepath.Join(filepath.Dir(main), "MAIN")
	if _, err := os.Stat(upper); err != nil {
		t.Skipf("the volume is case-sensitive: %v", err)
	}
	work := filepath.Join(filepath.Dir(main), "work")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, main, "config", "slimtree.base", upper)
	t.Chdir(upper)
	path := filepath.Join(filepath.Dir(main), "WORK", "feat")
	if code, stdout, stderr := slimtree(t.Context(), "new", "feat", "--path", path); code != 0 || stdout != path+"\n" {
		t.Fatalf("new: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if code, stdout, stderr := slimtree(t.Context(), "rm", filepath.Join(work, "feat")); code != 0 {
		t.Fatalf("rm: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	requireNoWorktree(t, main, path)
}

// Another process can register the worktree's path after new checked it,
// outside slimtree's lock, as a plain git worktree add or another new's
// post-checkout hook can. new's git worktree add then fails, and the rollback
// keeps that worktree and its files: on another branch, and on a branch that
// existed, since git refused to check it out for new too.
func TestNewRollbackKeepsAWorktreeAnotherProcessRegistered(t *testing.T) {
	for _, tc := range []struct {
		name, branch, foreign string
		args                  []string
		want                  string
	}{
		{name: "new branch", branch: "feat", foreign: "foreign", args: []string{"new", "feat", "--from", "topic"}, want: "already exists"},
		{name: "existing branch", branch: "topic", foreign: "topic", args: []string{"new", "topic"}, want: "slimtree kept the worktree "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			homeAfterFirstRun(t)
			main := newRepo(t)
			path := filepath.Join(filepath.Dir(main), "main-"+tc.branch)
			registered := false
			// Preflight's probe is the first clone, before new's git
			// worktree add.
			cloneWith(t, func(src, dst string) error {
				if !registered {
					registered = true
					args := []string{"worktree", "add", "-q", path, tc.foreign}
					if tc.foreign != tc.branch {
						args = []string{"worktree", "add", "-q", "-b", tc.foreign, path}
					}
					gitIn(t, main, args...)
					if err := os.WriteFile(filepath.Join(path, "precious.txt"), []byte("precious\n"), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				return copyClone(src, dst)
			})
			if code, stdout, stderr := slimtree(t.Context(), tc.args...); code != 1 || stdout != "" || !strings.Contains(stderr, tc.want) {
				t.Fatalf("%s: exit %d, stdout %q, stderr %q; want 1 and %q", strings.Join(tc.args, " "), code, stdout, stderr, tc.want)
			}
			if got, err := os.ReadFile(filepath.Join(path, "precious.txt")); err != nil || string(got) != "precious\n" {
				t.Fatalf("precious.txt = %q, %v", got, err)
			}
			if got := gitIn(t, path, "symbolic-ref", "HEAD"); got != "refs/heads/"+tc.foreign+"\n" {
				t.Fatalf("the worktree at %s is on %q, want %s", path, got, tc.foreign)
			}
			if tc.foreign != tc.branch {
				gitFindsNothing(t, main, "rev-parse", "--verify", "--quiet", "refs/heads/"+tc.branch)
			}
			if got := records(t); len(got) != 0 {
				t.Fatalf("records = %+v, want none", got)
			}
		})
	}
}

// A symlink at a recorded worktree's path never stands for the worktree it
// points at, so rm removes nothing through it.
func TestRmRefusesASymlinkToAnotherWorktree(t *testing.T) {
	homeAfterFirstRun(t)
	cloneWith(t, nil)
	main := newRepo(t)
	path := filepath.Join(filepath.Dir(main), "main-feat")
	if code, _, stderr := slimtree(t.Context(), "new", "feat"); code != 0 {
		t.Fatalf("new feat: exit %d, stderr %q", code, stderr)
	}
	other := filepath.Join(filepath.Dir(main), "other")
	gitIn(t, main, "worktree", "add", "-q", "-b", "other", other)
	gitIn(t, main, "worktree", "remove", "--force", path)
	if err := os.Symlink(other, path); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ arg, want string }{
		{path, "slimtree made no worktree " + path + "\n"},
		{"main-feat", "slimtree removes nothing at " + path + ", which is not a worktree of " + filepath.Join(main, ".git") + "\n"},
	} {
		if code, stdout, stderr := slimtree(t.Context(), "rm", tc.arg); code != 1 || stdout != "" || stderr != tc.want {
			t.Errorf("rm %s: exit %d, stdout %q, stderr %q; want 1 and %q", tc.arg, code, stdout, stderr, tc.want)
		}
	}
	if got := gitIn(t, other, "symbolic-ref", "HEAD"); got != "refs/heads/other\n" {
		t.Fatalf("the worktree other is on %q", got)
	}
	if list := gitIn(t, main, "worktree", "list", "--porcelain"); !strings.Contains(list, "worktree "+other+"\n") {
		t.Fatalf("git no longer registers %s:\n%s", other, list)
	}
}
