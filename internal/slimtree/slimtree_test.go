package slimtree

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/daemon"
	"github.com/cloudflare/artifact-fs/internal/gitstore"
	"github.com/cloudflare/artifact-fs/internal/gittest"
	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/cloudflare/artifact-fs/internal/overlay"
)

func TestDefaultBackendByPlatform(t *testing.T) {
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
			var stdout, stderr bytes.Buffer
			code := run(t.Context(), tc.goos, []string{"--backend", "apfsclone"}, &stdout, &stderr)
			if tc.apfsCloneRuns != (code == 0) {
				t.Fatalf("--backend apfsclone on %s: exit %d, stderr %q", tc.goos, code, stderr.String())
			}
			if !tc.apfsCloneRuns && !strings.Contains(stderr.String(), "apfsclone needs macOS, which clones files on APFS; use artifactfs on "+tc.goos) {
				t.Fatalf("--backend apfsclone on %s: stderr %q, want the refusal", tc.goos, stderr.String())
			}
			stdout.Reset()
			if code := run(t.Context(), tc.goos, []string{"--help"}, &stdout, io.Discard); code != 0 || !strings.Contains(stdout.String(), tc.usage) || !strings.Contains(stdout.String(), fmt.Sprintf("(default: %q)", tc.want)) {
				t.Fatalf("--help on %s: exit %d, stdout %q, want %q and the default", tc.goos, code, stdout.String(), tc.usage)
			}
		})
	}
}

func TestVersionFlagPrintsSlimtree(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Main(t.Context(), []string{"slimtree", "--version"}, &stdout, &stderr); code != 0 {
		t.Fatalf("--version exit = %d, stderr=%q", code, stderr.String())
	}
	want := fmt.Sprintf("slimtree dev (untagged, unknown, %s, %s/%s)\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
	if stdout.String() != want {
		t.Fatalf("--version = %q, want %q", stdout.String(), want)
	}
}

// The daemon links the FSMonitor hook to its own binary, which is slimtree's
// for a slimtree worktree. Git runs this test binary, whose TestMain runs
// Main, through the hook link and through a hook script, and stores the
// token each answer starts with in the index.
func TestSlimtreeRunsTheFSMonitorHookLink(t *testing.T) {
	root := gittest.StateRoot(t)
	t.Setenv("ARTIFACT_FS_ROOT", root)
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
	link := gitstore.FSMonitorHookLink(resolved, repos[0].ID)
	if err := os.Symlink(exe, link); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "fsmonitor-hook")
	if err := os.WriteFile(script, fmt.Appendf(nil, "#!/bin/sh\nexec '%s' fsmonitor-hook --name repo \"$@\"\n", exe), 0o700); err != nil {
		t.Fatal(err)
	}
	token := fmt.Sprintf("artifact-fs:%s:%s:", repos[0].ID, repos[0].ConfigVersion)
	for name, hook := range map[string]string{"hook link": link, "hook script": script} {
		t.Run(name, func(t *testing.T) {
			work := t.TempDir()
			git := func(args ...string) {
				t.Helper()
				cmd := exec.CommandContext(ctx, "git", append([]string{"-C", work}, args...)...)
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
