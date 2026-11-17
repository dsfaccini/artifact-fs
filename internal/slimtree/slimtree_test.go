package slimtree

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/daemon"
	"github.com/cloudflare/artifact-fs/internal/gitstore"
	"github.com/cloudflare/artifact-fs/internal/gittest"
	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/cloudflare/artifact-fs/internal/overlay"
	"github.com/cloudflare/artifact-fs/internal/slimtree/doctor"
)

func TestDefaultBackendByPlatform(t *testing.T) {
	t.Setenv("SLIMTREE_HOME", t.TempDir())
	for _, tc := range []struct {
		goos          string
		want          backend
		apfsCloneRuns bool
		usage         string
	}{
		{goos: "darwin", want: apfsClone, apfsCloneRuns: true, usage: "artifactfs (beta on macOS)"},
		{goos: "linux", want: artifactFS, usage: "apfsclone needs macOS"},
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
	t.Setenv("SLIMTREE_HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	if code := Main(t.Context(), []string{"slimtree", "--version"}, &stdout, &stderr); code != 0 {
		t.Fatalf("--version exit = %d, stderr=%q", code, stderr.String())
	}
	want := fmt.Sprintf("slimtree dev (untagged, unknown, %s, %s/%s)\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
	if stdout.String() != want {
		t.Fatalf("--version = %q, want %q", stdout.String(), want)
	}
}

// Main hands daemon and "afs <command>" to artifact-fs's CLI. A detached
// daemon that slimtree starts runs "slimtree daemon".
func TestMainForwardsArtifactFSCommands(t *testing.T) {
	t.Setenv("SLIMTREE_HOME", t.TempDir())
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
		if code != 0 || !strings.Contains(stdout.String(), tc.want) {
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
			work := hookedWorktree(t, hook)
			index, err := os.ReadFile(filepath.Join(work, ".git", "index"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(index, []byte(token)) {
				t.Fatalf("the index holds no token starting %q after git ran the %s", token, name)
			}
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
	if err := os.WriteFile(script, fmt.Appendf(nil, "#!/bin/sh\nexec '%s' fsmonitor-hook --name repo \"$@\"\n", exe), 0o700); err != nil {
		t.Fatal(err)
	}
	return link, script, fmt.Sprintf("artifact-fs:%s:%s:", repos[0].ID, repos[0].ConfigVersion)
}

// hookedWorktree makes a git work tree whose core.fsmonitor is hook, and runs
// git status there once.
func hookedWorktree(t *testing.T, hook string) string {
	t.Helper()
	work := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", work}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	git("init", "-q")
	if err := os.WriteFile(filepath.Join(work, "a"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "a")
	git("config", "core.fsmonitor", hook)
	git("update-index", "--fsmonitor")
	git("status", "--porcelain")
	return work
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
		Output: func(string, ...string) (string, error) { return "git version 2.47.0\n", nil },
		EUID:   1000,
	}
	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), "linux", probe, []string{"doctor", "--json"}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "doctor: a check failed") {
		t.Fatalf("doctor --json with an unwritable /dev/fuse: exit %d, stderr %q, want 1", code, stderr.String())
	}
	var report doctor.Report
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil || report.OK || len(report.Checks) != 4 || report.Checks[1].Fix != "sudo chmod 0666 /dev/fuse" {
		t.Fatalf("doctor --json = %s (%v), want a failed report with the chmod fix", stdout.String(), err)
	}
	stdout.Reset()
	if code := run(t.Context(), "linux", probe, []string{"doctor"}, &stdout, io.Discard); code != 1 || !strings.Contains(stdout.String(), "fail  dev-fuse") || !strings.Contains(stdout.String(), "      fix: sudo chmod 0666 /dev/fuse\n") {
		t.Fatalf("doctor: exit %d, stdout %q, want the failure and its fix", code, stdout.String())
	}
	probe.ReadWrite = func(string) error { return nil }
	stdout.Reset()
	if code := run(t.Context(), "linux", probe, []string{"doctor", "--json"}, &stdout, io.Discard); code != 0 {
		t.Fatalf("doctor --json on a healthy machine: exit %d, stdout %s", code, stdout.String())
	}
}
