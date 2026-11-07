package gitstore

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/model"
)

func TestAdoptedWorktreeConfigMigration(t *testing.T) {
	for _, mode := range []string{"bare-primary", "existing-primary-override", "missing-index", "primary-gitdir", "wrong-mount-path", "primary-path-whitespace", "reftable"} {
		t.Run(mode, func(t *testing.T) {
			dir, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			source, worktree := filepath.Join(dir, "source"), filepath.Join(dir, "worktree")
			if mode == "primary-path-whitespace" {
				source += " "
			}
			initArgs := []string{"init", "--initial-branch=main"}
			if mode == "reftable" {
				initArgs = append(initArgs, "--ref-format=reftable")
			}
			run(t, "git", append(initArgs, source)...)
			if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("base\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			run(t, "git", "-C", source, "add", "README.md")
			run(t, "git", "-C", source, "-c", "user.name=test", "-c", "user.email=test@invalid", "commit", "-m", "base")
			run(t, "git", "-C", source, "worktree", "add", "-b", "topic", worktree)
			gitDir := strings.TrimSpace(runOutput(t, "git", "-C", worktree, "rev-parse", "--path-format=absolute", "--git-dir"))
			common := filepath.Join(source, ".git")
			wantBare := "false"
			switch mode {
			case "primary-path-whitespace":
				run(t, "git", "--git-dir", common, "config", "core.worktree", source)
			case "bare-primary":
				run(t, "git", "--git-dir", common, "config", "core.bare", "true")
				wantBare = "true"
			case "existing-primary-override":
				run(t, "git", "--git-dir", common, "config", "extensions.worktreeConfig", "true")
				run(t, "git", "--git-dir", common, "config", "--worktree", "core.worktree", source)
				run(t, "git", "--git-dir", common, "config", "--local", "core.worktree", filepath.Join(dir, "unused-shared-path"))
			case "missing-index":
				if err := os.Remove(filepath.Join(gitDir, "index")); err != nil {
					t.Fatal(err)
				}
			}
			store := New(nil)
			defer store.Close()
			cfg := model.RepoConfig{RepoPaths: model.RepoPaths{GitDir: gitDir, MountPath: worktree}, RepoSpec: model.RepoSpec{AdoptGitDir: true}}
			beforeConfig, err := os.ReadFile(filepath.Join(common, "config"))
			if err != nil {
				t.Fatal(err)
			}
			if mode == "primary-gitdir" {
				cfg.GitDir = common
			}
			if mode == "wrong-mount-path" {
				cfg.MountPath = filepath.Join(dir, "wrong-mount")
				if err := os.Mkdir(cfg.MountPath, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			err = store.ConfigureAdoptedWorktree(t.Context(), cfg)
			if mode == "primary-gitdir" || mode == "wrong-mount-path" || mode == "reftable" {
				if err == nil {
					t.Fatal("adoption accepted unregistered, relocated or reftable Git metadata")
				}
				if mode == "reftable" && !strings.Contains(err.Error(), "reftable") {
					t.Fatalf("reftable rejection reason: %v", err)
				}
				afterConfig, readErr := os.ReadFile(filepath.Join(common, "config"))
				if readErr != nil || string(beforeConfig) != string(afterConfig) {
					t.Fatalf("invalid adoption changed native config: %v", readErr)
				}
				if _, statErr := os.Stat(filepath.Join(common, "config.worktree")); !os.IsNotExist(statErr) {
					t.Fatalf("invalid adoption created primary overrides: %v", statErr)
				}
				return
			}
			if mode == "missing-index" {
				if err == nil {
					t.Fatal("adoption must not reconstruct a missing staged index")
				}
				if _, statErr := os.Stat(filepath.Join(gitDir, "index")); !os.IsNotExist(statErr) {
					t.Fatalf("adoption recreated index: %v", statErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := store.ConfigureAdoptedWorktree(t.Context(), cfg); err != nil {
				t.Fatalf("repeated adoption: %v", err)
			}
			if got := strings.TrimSpace(runOutput(t, "git", "--git-dir", common, "rev-parse", "--is-bare-repository")); got != wantBare {
				t.Fatalf("primary bare mode changed to %q, want %q", got, wantBare)
			}
			if got := strings.TrimSpace(runOutput(t, "git", "-C", worktree, "rev-parse", "--show-toplevel")); got != worktree {
				t.Fatalf("adopted worktree resolved to %q", got)
			}
			if mode == "existing-primary-override" || mode == "primary-path-whitespace" {
				if got := strings.TrimSuffix(runOutput(t, "git", "-C", source, "rev-parse", "--show-toplevel"), "\n"); got != source {
					t.Fatalf("existing primary override changed to %q", got)
				}
			}
		})
	}
}

func TestAdoptedWorktreeConfigMigrationFailureKeepsWorktreePaths(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	for _, failingWrite := range []string{"--unset-all core.worktree", "extensions.worktreeConfig true"} {
		t.Run(failingWrite, func(t *testing.T) {
			dir, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			source, worktree, side := filepath.Join(dir, "source"), filepath.Join(dir, "worktree"), filepath.Join(dir, "side")
			run(t, realGit, "init", "--initial-branch=main", source)
			if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("base\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			run(t, realGit, "-C", source, "add", "README.md")
			run(t, realGit, "-C", source, "-c", "user.name=test", "-c", "user.email=test@invalid", "commit", "-m", "base")
			run(t, realGit, "-C", source, "config", "core.worktree", source)
			run(t, realGit, "-C", source, "worktree", "add", "-b", "topic", worktree)
			run(t, realGit, "-C", source, "worktree", "add", "-b", "side", side)
			gitDir := strings.TrimSpace(runOutput(t, realGit, "-C", worktree, "rev-parse", "--path-format=absolute", "--git-dir"))
			// Fail one shared config write, as a held config.lock would.
			bin := filepath.Join(dir, "bin")
			if err := os.Mkdir(bin, 0o755); err != nil {
				t.Fatal(err)
			}
			wrapper := fmt.Sprintf("#!/bin/sh\ncase \" $* \" in *\" %s \"*) exit 1 ;; esac\nexec '%s' \"$@\"\n", failingWrite, realGit)
			if err := os.WriteFile(filepath.Join(bin, "git"), []byte(wrapper), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			store := New(nil)
			defer store.Close()
			if err := store.ConfigureAdoptedWorktree(t.Context(), model.RepoConfig{RepoPaths: model.RepoPaths{GitDir: gitDir, MountPath: worktree}, RepoSpec: model.RepoSpec{AdoptGitDir: true}}); err == nil {
				t.Fatal("adoption ignored a failed config write")
			}
			for _, checkout := range []string{source, worktree, side} {
				if got := strings.TrimSpace(runOutput(t, realGit, "-C", checkout, "rev-parse", "--show-toplevel")); got != checkout {
					t.Fatalf("interrupted migration resolved %s to %q", checkout, got)
				}
			}
		})
	}
}
