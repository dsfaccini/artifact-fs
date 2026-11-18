package slimtree

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/daemon"
	"github.com/cloudflare/artifact-fs/internal/execfile"
	"github.com/cloudflare/artifact-fs/internal/gitstore"
	"github.com/cloudflare/artifact-fs/internal/gittest"
	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/cloudflare/artifact-fs/internal/overlay"
	"github.com/cloudflare/artifact-fs/internal/slimtree/doctor"
	"github.com/cloudflare/artifact-fs/internal/slimtree/state"
)

func TestDefaultBackendByPlatform(t *testing.T) {
	homeAfterFirstRun(t)
	for _, tc := range []struct {
		goos          string
		want          backend
		apfsCloneRuns bool
		usage         string
	}{
		{goos: "darwin", want: apfsClone, apfsCloneRuns: true, usage: "apfsclone (artifactfs worktrees are not supported yet; Linux support is coming)"},
		{goos: "linux", want: artifactFS, usage: "artifactfs, whose worktrees are not supported yet; Linux support is coming (apfsclone needs macOS)"},
	} {
		t.Run(tc.goos, func(t *testing.T) {
			if got := defaultBackend(tc.goos); got != tc.want {
				t.Fatalf("defaultBackend(%q) = %q, want %q", tc.goos, got, tc.want)
			}
			for _, c := range []struct {
				backend string
				runs    bool
				refusal string
			}{
				{backend: "apfsclone", runs: tc.apfsCloneRuns, refusal: "apfsclone needs macOS, which clones files on APFS; use artifactfs on " + tc.goos},
				{backend: "artifactfs", runs: true},
				{backend: "nope", refusal: `unknown backend "nope": use apfsclone or artifactfs`},
			} {
				var stderr bytes.Buffer
				code := run(t.Context(), tc.goos, doctor.Probe{}, []string{"--backend", c.backend}, io.Discard, &stderr)
				if c.runs != (code == 0) || !c.runs && !strings.Contains(stderr.String(), c.refusal) {
					t.Fatalf("--backend %s on %s: exit %d, stderr %q, want runs %v", c.backend, tc.goos, code, stderr.String(), c.runs)
				}
			}
			var stdout bytes.Buffer
			if code := run(t.Context(), tc.goos, doctor.Probe{}, []string{"--help"}, &stdout, io.Discard); code != 0 || !strings.Contains(stdout.String(), tc.usage) || !strings.Contains(stdout.String(), fmt.Sprintf("(default: %q)", tc.want)) {
				t.Fatalf("--help on %s: exit %d, stdout %q, want %q and the default", tc.goos, code, stdout.String(), tc.usage)
			}
		})
	}
}

func TestVersionFlagPrintsSlimtree(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SLIMTREE_HOME", home)
	var stdout, stderr bytes.Buffer
	if code := Main(t.Context(), []string{"slimtree", "--version"}, &stdout, &stderr); code != 0 || stderr.Len() != 0 {
		t.Fatalf("--version exit = %d, stderr=%q, want 0 and no first-run summary", code, stderr.String())
	}
	requireNoFirstRunMarker(t, home)
	want := fmt.Sprintf("slimtree dev (untagged, unknown, %s, %s/%s)\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
	if stdout.String() != want {
		t.Fatalf("--version = %q, want %q", stdout.String(), want)
	}
}

// Main hands daemon and "afs <command>" to artifact-fs's CLI. A detached
// daemon that slimtree starts runs "slimtree daemon".
func TestMainForwardsArtifactFSCommands(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SLIMTREE_HOME", home)
	defer requireNoFirstRunMarker(t, home)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{args: []string{"slimtree", "daemon", "--help"}, want: "start the artifact-fs daemon"},
		// artifact-fs answers --version before it looks up a command, so only a
		// command shows that Main drops "afs".
		{args: []string{"slimtree", "afs", "add-repo", "--help"}, want: "register a repository and prepare it for the daemon to mount"},
		{args: []string{"slimtree", "afs", "--version"}, want: fmt.Sprintf("artifact-fs dev (untagged, unknown, %s, %s/%s)\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)},
	} {
		var stdout, stderr bytes.Buffer
		code := Main(t.Context(), tc.args, &stdout, &stderr)
		if code != 0 || !strings.Contains(stdout.String(), tc.want) || strings.Contains(stderr.String(), "first run") {
			t.Errorf("%q: exit %d, stdout %q, stderr %q, want %q", tc.args, code, stdout.String(), stderr.String(), tc.want)
		}
	}
}

// The daemon links the FSMonitor hook to its own binary, which is slimtree's
// for a slimtree worktree. Git runs this test binary, whose TestMain runs
// Main, through the hook link and through a hook script, and stores the
// token each answer starts with in the index.
func TestSlimtreeRunsTheFSMonitorHookLink(t *testing.T) {
	link, script, token := registerHookedRepo(t)
	for name, hook := range map[string]string{"hook link": link, "hook script": script} {
		t.Run(name, func(t *testing.T) {
			work, status := hookedWorktree(t, hook)
			if strings.Contains(status, "first run") {
				t.Errorf("git status through the %s printed the first-run summary:\n%s", name, status)
			}
			index, err := os.ReadFile(filepath.Join(work, ".git", "index"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(index, []byte(token)) {
				t.Fatalf("the index holds no token starting %q after git ran the %s", token, name)
			}
			requireNoFirstRunMarker(t, os.Getenv("SLIMTREE_HOME"))
		})
	}
}

// registerHookedRepo registers a repo under a new state root. It returns the
// repo's hook link, a hook script that runs fsmonitor-hook, both running this
// test binary, and the prefix of the tokens the hook answers.
func registerHookedRepo(t *testing.T) (link, script, token string) {
	t.Helper()
	root := gittest.StateRoot(t)
	t.Setenv("ARTIFACT_FS_ROOT", root)
	t.Setenv("SLIMTREE_HOME", t.TempDir())
	ctx := t.Context()
	svc, err := daemon.NewController(ctx, root, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	// "/" is always in the mount table, so it stands in for a live mount.
	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{Name: "repo", MountPath: "/"}, RepoSpec: model.RepoSpec{RemoteURL: "https://github.com/example/repo.git"}, Enabled: true}
	if err := svc.AddRepoWithOptions(ctx, cfg, daemon.AddRepoOptions{Async: true}); err != nil {
		t.Fatal(err)
	}
	repos, err := svc.ListRepos(ctx)
	if closeErr := svc.Close(); err != nil || closeErr != nil || len(repos) != 1 {
		t.Fatalf("list repos: %v, %v, %d", err, closeErr, len(repos))
	}
	ov, err := overlay.New(ctx, repos[0].RepoPaths)
	if err != nil {
		t.Fatal(err)
	}
	if err := ov.Close(); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	link = gitstore.FSMonitorHookLink(resolved, repos[0].ID)
	if err := os.Symlink(exe, link); err != nil {
		t.Fatal(err)
	}
	script = filepath.Join(t.TempDir(), "fsmonitor-hook")
	if err := execfile.Write(script, fmt.Appendf(nil, "#!/bin/sh\nexec '%s' fsmonitor-hook --name repo \"$@\"\n", exe), 0o700); err != nil {
		t.Fatal(err)
	}
	return link, script, fmt.Sprintf("artifact-fs:%s:%s:", repos[0].ID, repos[0].ConfigVersion)
}

// hookedWorktree makes a git work tree whose core.fsmonitor is hook, runs
// git status there once, and returns the work tree and git status's output,
// stderr included.
func hookedWorktree(t *testing.T, hook string) (string, string) {
	t.Helper()
	work := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", work}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	git("init", "-q")
	if err := os.WriteFile(filepath.Join(work, "a"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "a")
	git("config", "core.fsmonitor", hook)
	git("update-index", "--fsmonitor")
	return work, git("status", "--porcelain")
}

func TestHomeResolvesItsEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name, slimtreeHome, xdgDataHome, want string
	}{
		{name: "SLIMTREE_HOME wins", slimtreeHome: "/srv/slimtree/", xdgDataHome: "/data", want: "/srv/slimtree"},
		{name: "XDG_DATA_HOME", xdgDataHome: "/data", want: "/data/slimtree"},
		{name: "relative XDG_DATA_HOME is ignored", xdgDataHome: "data", want: "/home/u/.local/share/slimtree"},
		{name: "neither", want: "/home/u/.local/share/slimtree"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", "/home/u")
			t.Setenv("SLIMTREE_HOME", tc.slimtreeHome)
			t.Setenv("XDG_DATA_HOME", tc.xdgDataHome)
			if got, err := Home(); err != nil || got != tc.want {
				t.Fatalf("Home() = %q, %v, want %q", got, err, tc.want)
			}
		})
	}
	t.Setenv("SLIMTREE_HOME", "slimtree")
	if got, err := Home(); err == nil {
		t.Fatalf("Home() with a relative SLIMTREE_HOME = %q, want an error", got)
	}
}

type modeInfo struct {
	fs.FileInfo
	mode fs.FileMode
}

func (m modeInfo) Mode() fs.FileMode { return m.mode }
func (m modeInfo) Sys() any          { return &syscall.Stat_t{} }

// doctor prints its report, as JSON with --json, and exits 1 when a check
// fails.
func TestDoctorExitsOneOnAFailedCheck(t *testing.T) {
	t.Setenv("SLIMTREE_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	probe := doctor.Probe{
		LookPath: func(file string) (string, error) { return "/usr/bin/" + file, nil },
		Stat: func(path string) (fs.FileInfo, error) {
			if path == "/dev/fuse" {
				return modeInfo{mode: fs.ModeDevice | fs.ModeCharDevice}, nil
			}
			return modeInfo{mode: fs.ModeSetuid | 0o755}, nil
		},
		ReadWrite: func(string) error { return errors.New("permission denied") },
		ReadFile: func(path string) ([]byte, error) {
			if path == "/proc/self/status" {
				return []byte("CapBnd:\t000001ffffffffff\n"), nil
			}
			return []byte("ID=debian\n"), nil
		},
		Output:   func(string, ...string) (string, error) { return "git version 2.47.0\n", nil },
		EUID:     1000,
		MountIDs: func() (bool, error) { return true, nil },
	}
	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), "linux", probe, []string{"doctor", "--json"}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "doctor: a check failed") {
		t.Fatalf("doctor --json with an unwritable /dev/fuse: exit %d, stderr %q, want 1", code, stderr.String())
	}
	var report doctor.Report
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil || report.OK || len(report.Checks) != 7 || report.Checks[2].Fix != "sudo chmod 0666 /dev/fuse" {
		t.Fatalf("doctor --json = %s (%v), want a failed report with the chmod fix", stdout.String(), err)
	}
	stdout.Reset()
	if code := run(t.Context(), "linux", probe, []string{"doctor"}, &stdout, io.Discard); code != 1 || !strings.Contains(stdout.String(), "fail  dev-fuse") || !strings.Contains(stdout.String(), "      fix: sudo chmod 0666 /dev/fuse\n") {
		t.Fatalf("doctor: exit %d, stdout %q, want the failure and its fix", code, stdout.String())
	}
	// This release makes no artifactfs worktrees, so a healthy Linux machine
	// fails only the backend check.
	probe.ReadWrite = func(string) error { return nil }
	stdout.Reset()
	if code := run(t.Context(), "linux", probe, []string{"doctor", "--json"}, &stdout, io.Discard); code != 1 {
		t.Fatalf("doctor --json on a healthy machine: exit %d, stdout %s", code, stdout.String())
	}
	report = doctor.Report{}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	want := doctor.Result{ID: "backend", Status: doctor.StatusFail, Detail: "artifactfs worktrees are not supported yet; Linux support is coming", Fix: "upgrade slimtree once a release makes artifactfs worktrees"}
	for i, c := range report.Checks {
		if i == 0 && c != want || i > 0 && c.Status == doctor.StatusFail {
			t.Fatalf("doctor --json on a healthy machine = %+v, want only %+v to fail", report.Checks, want)
		}
	}
}

// --backend artifactfs doctor on macOS fails the same check, whose fix is
// the apfsclone backend.
func TestDoctorSaysArtifactFSIsNotInThisRelease(t *testing.T) {
	homeAfterFirstRun(t)
	probe := healthyDarwin()
	probe.OSVersion = func() (string, error) { return "26.0", nil }
	probe.FSKitAvailable = func() error { return nil }
	var stdout bytes.Buffer
	if code := run(t.Context(), "darwin", probe, []string{"--backend", "artifactfs", "doctor"}, &stdout, io.Discard); code != 1 ||
		!strings.HasPrefix(stdout.String(), "fail  backend        artifactfs worktrees are not supported yet; Linux support is coming\n      fix: use --backend apfsclone, the default on macOS\n") {
		t.Fatalf("--backend artifactfs doctor on macOS: exit %d, stdout %q; want exit 1 and the backend failure first", code, stdout.String())
	}
}

// homeAfterFirstRun points SLIMTREE_HOME at a home whose first-run summary
// has been shown.
func homeAfterFirstRun(t *testing.T) string {
	t.Helper()
	home := gittest.StateRoot(t)
	if err := os.WriteFile(filepath.Join(home, firstRunMarker), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SLIMTREE_HOME", home)
	return home
}

func requireNoFirstRunMarker(t *testing.T, home string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(home, firstRunMarker)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s holds a first-run marker (%v), want none", home, err)
	}
}

// healthyLinux is a Linux machine where every doctor check passes.
func healthyLinux() doctor.Probe {
	return doctor.Probe{
		LookPath: func(file string) (string, error) { return "/usr/bin/" + file, nil },
		Stat: func(path string) (fs.FileInfo, error) {
			if path == "/dev/fuse" {
				return modeInfo{mode: fs.ModeDevice | fs.ModeCharDevice}, nil
			}
			return modeInfo{mode: fs.ModeSetuid | 0o755}, nil
		},
		ReadWrite: func(string) error { return nil },
		ReadFile: func(path string) ([]byte, error) {
			switch path {
			case "/proc/self/status":
				return []byte("CapEff:\t0000000000000000\nCapBnd:\t000001ffffffffff\nNoNewPrivs:\t0\n"), nil
			case "/etc/os-release":
				return []byte("ID=debian\n"), nil
			}
			return nil, os.ErrNotExist
		},
		Output:   func(string, ...string) (string, error) { return "git version 2.47.0\n", nil },
		EUID:     1000,
		MountIDs: func() (bool, error) { return true, nil },
	}
}

// The first run prints the doctor summary to stderr, and no later run does.
// The summary probes no directory, so it never drops the APFS probe file into
// the current directory.
func TestFirstRunSummaryPrintsOnce(t *testing.T) {
	darwin := doctor.Probe{
		LookPath: func(file string) (string, error) { return "/usr/bin/" + file, nil },
		Output: func(name string, _ ...string) (string, error) {
			return map[string]string{"xcode-select": "/Library/Developer/CommandLineTools\n", "git": "git version 2.54.0\n"}[name], nil
		},
		APFS: func(dir string) error {
			t.Errorf("the first-run summary probed %q", dir)
			return nil
		},
	}
	for _, tc := range []struct {
		goos  string
		probe doctor.Probe
		want  string
	}{
		{goos: "linux", probe: healthyLinux(), want: "slimtree's first run: what --backend artifactfs needs here (slimtree doctor checks again)\n" +
			"fail  backend        artifactfs worktrees are not supported yet; Linux support is coming\n" +
			"      fix: upgrade slimtree once a release makes artifactfs worktrees\n" +
			"ok    fusermount     /usr/bin/fusermount3\n" +
			"ok    dev-fuse       /dev/fuse\n" +
			"ok    cap-sys-admin  CapBnd holds CAP_SYS_ADMIN\n" +
			"ok    apparmor       no AppArmor profile\n" +
			"ok    mount-id       unique mount IDs\n" +
			"ok    git            git version 2.47.0\n"},
		{goos: "darwin", probe: darwin, want: "slimtree's first run: what --backend apfsclone needs here (slimtree doctor checks again)\n" +
			"ok    xcode-select   /Library/Developer/CommandLineTools\n" +
			"ok    git            git version 2.54.0\n" +
			"skip  apfs           no directory to probe; run slimtree doctor where the worktree will go\n"},
	} {
		t.Run(tc.goos, func(t *testing.T) {
			home := filepath.Join(t.TempDir(), "home")
			t.Setenv("SLIMTREE_HOME", home)
			var first, firstErr bytes.Buffer
			if code := run(t.Context(), tc.goos, tc.probe, nil, &first, &firstErr); code != 0 || firstErr.String() != tc.want {
				t.Fatalf("first run: exit %d, stderr\n%s\nwant\n%s", code, firstErr.String(), tc.want)
			}
			info, err := os.Stat(filepath.Join(home, firstRunMarker))
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("first-run marker: %v, %v; want mode 0600", info, err)
			}
			marker, err := os.ReadFile(filepath.Join(home, firstRunMarker))
			if err != nil || !strings.HasPrefix(string(marker), "slimtree dev (untagged, ") {
				t.Fatalf("first-run marker holds %q (%v), want the version and time", marker, err)
			}
			var second, secondErr bytes.Buffer
			if code := run(t.Context(), tc.goos, tc.probe, nil, &second, &secondErr); code != 0 || secondErr.Len() != 0 {
				t.Fatalf("second run: exit %d, stderr %q, want no summary", code, secondErr.String())
			}
			if second.String() != first.String() {
				t.Fatalf("stdout with the summary =\n%s\nwithout =\n%s\nwant the summary on stderr only", first.String(), second.String())
			}
		})
	}
}

// --help, --version, help and doctor never print the summary or write the
// marker, and doctor --json's stdout stays one JSON report.
func TestFirstRunSkipsHelpVersionAndDoctor(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SLIMTREE_HOME", home)
	for _, args := range [][]string{{"--help"}, {"--version"}, {"help"}, {"h"}, {"doctor", "--help"}, {"doctor", "--json"}} {
		// This release makes no artifactfs worktrees, so doctor fails on Linux.
		wantCode, wantErr := 0, ""
		if args[len(args)-1] == "--json" {
			wantCode, wantErr = 1, "doctor: a check failed\n"
		}
		var stdout, stderr bytes.Buffer
		if code := run(t.Context(), "linux", healthyLinux(), args, &stdout, &stderr); code != wantCode || stderr.String() != wantErr {
			t.Errorf("%q: exit %d, stderr %q, want %d, %q and no summary", args, code, stderr.String(), wantCode, wantErr)
		}
		if args[len(args)-1] == "--json" {
			var report doctor.Report
			if err := json.Unmarshal(stdout.Bytes(), &report); err != nil || len(report.Checks) != 7 {
				t.Errorf("doctor --json stdout = %s (%v), want one report", stdout.String(), err)
			}
		}
	}
	requireNoFirstRunMarker(t, home)
}

// A home slimtree cannot create never fails the command. The summary says so,
// and the next run prints it again.
func TestFirstRunSurvivesAnUnwritableHome(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SLIMTREE_HOME", filepath.Join(file, "home"))
	for range 2 {
		var stderr bytes.Buffer
		if code := run(t.Context(), "linux", healthyLinux(), nil, io.Discard, &stderr); code != 0 || !strings.HasPrefix(stderr.String(), "slimtree's first run: ") || !strings.Contains(stderr.String(), "\nslimtree: cannot record the first run, so the next run prints this summary again: lstat "+file+"/home: not a directory\n") {
			t.Fatalf("run with an unwritable home: exit %d, stderr %q, want 0, the summary and why no marker was written", code, stderr.String())
		}
	}
}

// A write that fails after the marker's create, as on a full disk, leaves no
// marker, so the next run prints the summary again, as the failure says.
func TestFirstRunMarkerWriteFailureLeavesNoMarker(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	t.Setenv("SLIMTREE_HOME", home)
	markerCreated = func(f *os.File) {
		if err := f.Close(); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { markerCreated = nil })
	var stderr bytes.Buffer
	if code := run(t.Context(), "linux", healthyLinux(), nil, io.Discard, &stderr); code != 0 || !strings.Contains(stderr.String(), "\nslimtree: cannot record the first run, so the next run prints this summary again: ") {
		t.Fatalf("run whose marker write fails: exit %d, stderr %q, want 0 and why no marker was written", code, stderr.String())
	}
	if entries, err := os.ReadDir(home); err != nil || len(entries) != 1 || entries[0].Name() != "w" {
		t.Fatalf("after the failed write, %s holds %v (%v), want only the records directory", home, entries, err)
	}
	markerCreated = nil
	stderr.Reset()
	if code := run(t.Context(), "linux", healthyLinux(), nil, io.Discard, &stderr); code != 0 || !strings.HasPrefix(stderr.String(), "slimtree's first run: ") {
		t.Fatalf("next run: exit %d, stderr %q, want the summary again", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(home, firstRunMarker)); err != nil {
		t.Fatalf("next run wrote no marker: %v", err)
	}
}

// gitIn runs git in dir and returns its output.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// repoWithWorktrees makes a repository with a commit and a linked worktree
// on a new branch for each name, and returns its canonical common dir and
// worktree paths.
func repoWithWorktrees(t *testing.T, names ...string) (string, []string) {
	t.Helper()
	base, err := state.Canonical(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	main := filepath.Join(base, "main")
	gitIn(t, base, "init", "-q", "-b", "main", main)
	gitIn(t, main, "commit", "-q", "--allow-empty", "-m", "base")
	paths := make([]string, len(names))
	for i, name := range names {
		paths[i] = filepath.Join(base, name)
		gitIn(t, main, "worktree", "add", "-q", "-b", name, paths[i])
	}
	return filepath.Join(main, ".git"), paths
}

// list reports each record's state from git's worktree list: ok for an
// apfsclone worktree git lists, stopped for an artifactfs one, and missing
// for one whose directory or repository is gone.
func TestListReportsEachRecordsState(t *testing.T) {
	homeDir := homeAfterFirstRun(t)
	home, err := state.Open(homeDir)
	if err != nil {
		t.Fatal(err)
	}
	repo, paths := repoWithWorktrees(t, "kept", "gone", "served")
	other, otherPaths := repoWithWorktrees(t, "elsewhere")
	if err := os.RemoveAll(paths[1]); err != nil {
		t.Fatal(err)
	}
	created := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	vanishedRepo := filepath.Join(filepath.Dir(other), "vanished", ".git")
	for _, r := range []state.Record{
		{Backend: "apfsclone", Path: paths[0], CommonDir: repo, Branch: "kept", Created: created},
		{Backend: "apfsclone", Path: paths[1], CommonDir: repo, Branch: "gone", Created: created},
		{Backend: "artifactfs", Path: paths[2], CommonDir: repo, Branch: "served", Created: created},
		{Backend: "apfsclone", Path: otherPaths[0], CommonDir: other, Branch: "elsewhere", Created: created},
		{Backend: "apfsclone", Path: filepath.Join(filepath.Dir(vanishedRepo), "wt"), CommonDir: vanishedRepo, Branch: "wt", Created: created},
	} {
		if err := home.Write(r); err != nil {
			t.Fatal(err)
		}
	}
	// The worktree moved to another branch after it was made.
	gitIn(t, paths[0], "switch", "-q", "-c", "moved")
	stray := filepath.Join(home.Dir(), "w", "stray")
	if err := os.WriteFile(stray, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), "linux", doctor.Probe{}, []string{"list", "--json"}, &stdout, &stderr); code != 0 || stderr.String() != "slimtree: skipping "+stray+", which holds no readable record: open "+stray+"/worktree.json: not a directory\n" {
		t.Fatalf("list --json: exit %d, stderr %q, want 0 and the stray entry skipped", code, stderr.String())
	}
	var got struct {
		Version   int
		Worktrees []struct{ Name, Path, Backend, State, Branch, Repo string }
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("list --json = %s: %v", stdout.String(), err)
	}
	type row struct{ Name, Path, Backend, State, Branch, Repo string }
	want := []row{
		{"kept", paths[0], "apfsclone", "ok", "moved", repo},
		// git still reads the branch from the worktree's admin directory.
		{"gone", paths[1], "apfsclone", "missing", "gone", repo},
		{"served", paths[2], "artifactfs", "stopped", "served", repo},
		{"elsewhere", otherPaths[0], "apfsclone", "ok", "elsewhere", other},
		{"wt", filepath.Join(filepath.Dir(vanishedRepo), "wt"), "apfsclone", "missing", "", vanishedRepo},
	}
	var rows []row
	for _, w := range got.Worktrees {
		rows = append(rows, row(w))
	}
	slices.SortFunc(want, func(a, b row) int { return strings.Compare(a.Path, b.Path) })
	if got.Version != 1 || !reflect.DeepEqual(rows, want) {
		t.Fatalf("list --json = version %d, %+v\nwant version 1, %+v", got.Version, rows, want)
	}

	// --repo keeps the worktrees of the repository that holds the current
	// directory.
	t.Chdir(paths[2])
	stdout.Reset()
	if code := run(t.Context(), "linux", doctor.Probe{}, []string{"list", "--repo"}, &stdout, &stderr); code != 0 {
		t.Fatalf("list --repo: exit %d, stderr %q", code, stderr.String())
	}
	wantText := "NAME    STATE    BACKEND     BRANCH  PATH\n" +
		"gone    missing  apfsclone   gone    " + paths[1] + "\n" +
		"kept    ok       apfsclone   moved   " + paths[0] + "\n" +
		"served  stopped  artifactfs  served  " + paths[2] + "\n"
	if stdout.String() != wantText {
		t.Fatalf("list --repo =\n%s\nwant\n%s", stdout.String(), wantText)
	}
}

// A usage error writes the error once to stderr and nothing to stdout, where
// --json output goes.
func TestUsageErrorLeavesStdoutEmpty(t *testing.T) {
	homeAfterFirstRun(t)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"list", "--json", "--bogus"}, "flag provided but not defined: -bogus\n"},
		{[]string{"--backend", "nope", "list", "--json"}, `invalid value "nope" for flag -backend: unknown backend "nope": use apfsclone or artifactfs` + "\n"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(t.Context(), "linux", doctor.Probe{}, tc.args, &stdout, &stderr); code != 1 || stdout.Len() != 0 || stderr.String() != tc.want {
			t.Errorf("%q: exit %d, stdout %q, stderr %q; want 1, no stdout, stderr %q", tc.args, code, stdout.String(), stderr.String(), tc.want)
		}
	}
}

// A process that holds git worktree list's stdout and stderr after git exits
// cannot fail list. It is serial: it sets PATH.
func TestListSucceedsWhileAnotherProcessHoldsGitsOutput(t *testing.T) {
	homeDir := homeAfterFirstRun(t)
	home, err := state.Open(homeDir)
	if err != nil {
		t.Fatal(err)
	}
	repo, paths := repoWithWorktrees(t, "kept")
	if err := home.Write(state.Record{Backend: "apfsclone", Path: paths[0], CommonDir: repo, Branch: "kept"}); err != nil {
		t.Fatal(err)
	}
	_, holderExited := gittest.HoldGitOutput(t)
	var stdout, stderr bytes.Buffer
	code := run(t.Context(), "linux", doctor.Probe{}, []string{"list", "--json"}, &stdout, &stderr)
	if _, err := os.Stat(holderExited); err == nil {
		t.Error("list returned only after a holder of git's output exited")
	}
	var got struct {
		Worktrees []struct{ Name, State, Branch string }
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); code != 0 || stderr.Len() != 0 || err != nil || len(got.Worktrees) != 1 || got.Worktrees[0] != (struct{ Name, State, Branch string }{"kept", "ok", "kept"}) {
		t.Fatalf("list --json: exit %d, stdout %s, stderr %q; want 0 and kept ok on branch kept", code, stdout.String(), stderr.String())
	}
}

// git stats each unlocked worktree's .git, so a hung mount can hang git
// worktree list. list stops git once worktreeListTimeout passes, and reports
// the repository's worktrees as unknown.
func TestListReportsUnknownWhenGitHangs(t *testing.T) {
	homeDir := homeAfterFirstRun(t)
	home, err := state.Open(homeDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := home.Write(state.Record{Backend: "artifactfs", Path: "/work/hung", CommonDir: "/src/repo/.git"}); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	marker := filepath.Join(t.TempDir(), "listing")
	if err := execfile.Write(filepath.Join(bin, "git"), fmt.Appendf(nil, "#!/bin/sh\n: > '%s'\nexec sleep 60\n", marker), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := gittest.ExpireOnFile(t.Context(), marker)
	defer cancel()
	var stdout, stderr bytes.Buffer
	if code := run(ctx, "linux", doctor.Probe{}, []string{"list", "--json"}, &stdout, &stderr); code != 0 || stderr.String() != "slimtree: git worktree list did not finish within 10s in /src/repo/.git, so its worktrees are unknown\n" {
		t.Fatalf("list with a hung git: exit %d, stderr %q", code, stderr.String())
	}
	var got struct {
		Worktrees []struct{ State, Reason string }
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil || len(got.Worktrees) != 1 || got.Worktrees[0].State != "unknown" || got.Worktrees[0].Reason != "git worktree list did not finish within 10s" {
		t.Fatalf("list --json with a hung git = %s (%v), want the worktree unknown", stdout.String(), err)
	}
}

// A canceled list, as on Ctrl-C, fails with the context's error instead of
// reporting the worktrees missing.
func TestListFailsWhenCanceled(t *testing.T) {
	homeDir := homeAfterFirstRun(t)
	home, err := state.Open(homeDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := home.Write(state.Record{Backend: "apfsclone", Path: "/work/a", CommonDir: "/src/repo/.git"}); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	marker := filepath.Join(t.TempDir(), "listing")
	if err := execfile.Write(filepath.Join(bin, "git"), fmt.Appendf(nil, "#!/bin/sh\n: > '%s'\nexec sleep 60\n", marker), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		for {
			if _, err := os.Stat(marker); err == nil {
				cancel()
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}()
	var stdout, stderr bytes.Buffer
	if code := run(ctx, "linux", doctor.Probe{}, []string{"list", "--json"}, &stdout, &stderr); code != 1 || stdout.Len() != 0 || stderr.String() != "context canceled\n" {
		t.Fatalf("canceled list: exit %d, stdout %q, stderr %q; want 1 and the context's error", code, stdout.String(), stderr.String())
	}
}
