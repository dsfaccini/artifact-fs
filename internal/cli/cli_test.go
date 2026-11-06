package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/daemon"
	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/cloudflare/artifact-fs/internal/overlay"
	"github.com/cloudflare/artifact-fs/internal/registry"
)

func TestFormatStatusLineUsesNeverForUnsetFetch(t *testing.T) {
	st := model.RepoRuntimeState{
		RepoID:            "workerd",
		State:             "mounted",
		CurrentHEADOID:    "abc123",
		CurrentHEADRef:    "main",
		LastFetchResult:   "never",
		HydratedBlobCount: 3,
		HydratedBlobBytes: 42,
	}

	got := formatStatusLine(st)
	for _, want := range []string{
		" head=abc123 ",
		" ref=main ",
		"last_fetch=never",
		"result=never",
		"required_commit=none",
		"acquisition=not_required",
		"base_commit=abc123",
		"remote_refresh=enabled",
		"prepare_error=none",
		"hydrated_blobs=3",
		"hydrated_bytes=42",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("status line %q missing %q", got, want)
		}
	}
	if strings.Contains(got, "0001-01-01T00:00:00Z") {
		t.Fatalf("status line leaked zero time: %q", got)
	}
}

func TestFormatStatusLineFormatsFetchTimestamp(t *testing.T) {
	at := time.Date(2026, time.March, 31, 12, 34, 56, 0, time.UTC)
	st := model.RepoRuntimeState{LastFetchAt: at, LastFetchResult: "ok"}

	got := formatStatusLine(st)
	if !strings.Contains(got, "last_fetch=2026-03-31T12:34:56Z") {
		t.Fatalf("status line %q missing formatted timestamp", got)
	}
}

func TestFormatStatusLineKeepsPrepareErrorSingleLine(t *testing.T) {
	st := model.RepoRuntimeState{PrepareError: "fatal: clone failed\ntry again\tlater"}

	got := formatStatusLine(st)
	if strings.ContainsAny(got, "\n\t") {
		t.Fatalf("status line contains raw whitespace: %q", got)
	}
	if !strings.Contains(got, "prepare_error=fatal:_clone_failed_try_again_later") {
		t.Fatalf("status line %q missing normalized prepare error", got)
	}
}

func TestFormatStatusLineRedactsPrepareError(t *testing.T) {
	st := model.RepoRuntimeState{PrepareError: "clone https://token@example.com/org/repo.git?access_token=secret failed"}

	got := formatStatusLine(st)
	if strings.Contains(got, "token@example.com") || strings.Contains(got, "secret") {
		t.Fatalf("status line leaked prepare error credential: %q", got)
	}
	if !strings.Contains(got, "REDACTED") {
		t.Fatalf("status line %q missing redaction marker", got)
	}
}

func TestAddRepoAsyncCLIRegistersWithoutClone(t *testing.T) {
	t.Setenv("ARTIFACT_FS_ROOT", t.TempDir())
	var stdout, stderr bytes.Buffer

	code := Run(context.Background(), []string{
		"add-repo",
		"--name", "repo",
		"--remote", "https://github.com/example/repo.git",
		"--ref", "refs/heads/main",
		"--require-commit", "0123456789012345678901234567890123456789",
		"--depth", "1",
		"--refresh", "never",
		"--async",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("Run exit = %d, stderr=%q", code, stderr.String())
	}
	if got := stdout.String(); got != "queued repo\n" {
		t.Fatalf("stdout = %q, want queued repo", got)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run(context.Background(), []string{"list-repos"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("list-repos exit = %d, stderr=%q", code, stderr.String())
	}
	if got := stdout.String(); !strings.HasPrefix(got, "repo\tmain\t") {
		t.Fatalf("list-repos output = %q, want legacy short branch column", got)
	}
}

func TestAddRepoAsyncCLIFlagValidation(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "prepared_gitdir_requires_async",
			args: []string{"add-repo", "--name", "repo", "--prepared-gitdir", "--git-dir", "/tmp/repo.git"},
			want: "--prepared-gitdir requires --async",
		},
		{
			name: "prepared_gitdir_requires_git_dir",
			args: []string{"add-repo", "--name", "repo", "--async", "--prepared-gitdir"},
			want: "--git-dir is required with --prepared-gitdir",
		},
		{
			name: "async_clone_requires_remote",
			args: []string{"add-repo", "--name", "repo", "--async"},
			want: "--remote is required",
		},
		{
			name: "async_rejects_inline_credentials",
			args: []string{"add-repo", "--name", "repo", "--async", "--remote", "https://token@example.com/org/repo.git"},
			want: "async repositories must use ambient credentials",
		},
		{
			name: "async_rejects_password_url_parse_reads_as_host_and_path",
			args: []string{"add-repo", "--name", "repo", "--async", "--remote", "ssh://user:1234/secret@host/repo"},
			want: "async repositories must use ambient credentials",
		},
		{
			name: "async_rejects_password_left_after_ssh_username",
			args: []string{"add-repo", "--name", "repo", "--async", "--remote", "ssh://git@host.example.com/a://u2:pw2zq@h2zz/r"},
			want: "async repositories must use ambient credentials",
		},
		{
			name: "required_commit_requires_explicit_ref",
			args: []string{"add-repo", "--name", "repo", "--async", "--remote", "https://github.com/example/repo.git", "--require-commit", "0123456789012345678901234567890123456789"},
			want: "--require-commit requires an explicit --ref",
		},
		{
			name: "rejects_invalid_required_commit",
			args: []string{"add-repo", "--name", "repo", "--async", "--remote", "https://github.com/example/repo.git", "--ref", "refs/heads/main", "--require-commit", "not-an-oid"},
			want: "--require-commit must be a full",
		},
		{
			name: "rejects_negative_depth",
			args: []string{"add-repo", "--name", "repo", "--async", "--remote", "https://github.com/example/repo.git", "--depth", "-1"},
			want: "--depth must not be negative",
		},
		{
			name: "ref_conflicts_with_branch",
			args: []string{"add-repo", "--name", "repo", "--async", "--remote", "https://github.com/example/repo.git", "--ref", "refs/heads/main", "--branch", "main"},
			want: "--ref and --branch cannot be used together",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ARTIFACT_FS_ROOT", t.TempDir())
			var stdout, stderr bytes.Buffer
			code := Run(context.Background(), tt.args, &stdout, &stderr)
			if code == 0 {
				t.Fatalf("Run unexpectedly succeeded, stdout=%q", stdout.String())
			}
			if !strings.Contains(stderr.String(), tt.want) {
				t.Fatalf("stderr = %q, want substring %q", stderr.String(), tt.want)
			}
		})
	}
}

func TestAddRepoCLIRejectsOptionRemoteBeforeGit(t *testing.T) {
	for _, tt := range []struct {
		name  string
		extra []string
	}{{"sync", nil}, {"async", []string{"--async"}}} {
		t.Run(tt.name, func(t *testing.T) {
			tmp := t.TempDir()
			bin := filepath.Join(tmp, "bin")
			if err := os.Mkdir(bin, 0o755); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(tmp, "git-invoked")
			if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\n: > \"$GIT_INVOKED_MARKER\"\nexit 1\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("GIT_INVOKED_MARKER", marker)
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("ARTIFACT_FS_ROOT", filepath.Join(tmp, "state"))
			var stdout, stderr bytes.Buffer

			args := append([]string{"add-repo", "--name", "repo", "--remote", "--upload-pack=a@b:c", "--ref", "refs/tags/v1"}, tt.extra...)
			if code := Run(context.Background(), args, &stdout, &stderr); code == 0 {
				t.Fatalf("add-repo unexpectedly succeeded, stdout=%q", stdout.String())
			}
			if !strings.Contains(stderr.String(), "remote URL must not start with '-'") {
				t.Fatalf("stderr = %q, want leading-dash rejection", stderr.String())
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("git was invoked before rejecting the remote")
			}
			stdout.Reset()
			if code := Run(context.Background(), []string{"list-repos"}, &stdout, &stderr); code != 0 {
				t.Fatalf("list-repos exit = %d, stderr=%q", code, stderr.String())
			}
			if stdout.Len() != 0 {
				t.Fatalf("list-repos = %q, want no registered repo", stdout.String())
			}
		})
	}
}

// The repo name becomes part of the mount's fsname option, so add-repo must
// refuse it before registration rather than leave the daemon a mount that fails.
func TestAddRepoCLIRejectsMountOptionSeparatorsInName(t *testing.T) {
	for _, name := range []string{"repo,name", "repo=name"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("ARTIFACT_FS_ROOT", t.TempDir())
			var stdout, stderr bytes.Buffer
			args := []string{"add-repo", "--name", name, "--async", "--remote", "https://github.com/example/repo.git"}
			if code := Run(context.Background(), args, &stdout, &stderr); code == 0 {
				t.Fatalf("add-repo unexpectedly succeeded, stdout=%q", stdout.String())
			}
			if !strings.Contains(stderr.String(), "must not contain ',' or '='") {
				t.Fatalf("stderr = %q, want the mount option separator rejection", stderr.String())
			}
			stdout.Reset()
			if code := Run(context.Background(), []string{"list-repos"}, &stdout, &stderr); code != 0 {
				t.Fatalf("list-repos exit = %d, stderr=%q", code, stderr.String())
			}
			if stdout.Len() != 0 {
				t.Fatalf("list-repos = %q, want no registered repo", stdout.String())
			}
		})
	}
}

func TestPrepareCLIReportsQueuedPrepare(t *testing.T) {
	t.Setenv("ARTIFACT_FS_ROOT", t.TempDir())
	var stdout, stderr bytes.Buffer
	ctx := context.Background()
	code := Run(ctx, []string{
		"add-repo",
		"--name", "repo",
		"--remote", "https://github.com/example/repo.git",
		"--async",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("add-repo exit = %d, stderr=%q", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run(ctx, []string{"prepare", "--name", "repo"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("prepare exit = %d, stderr=%q", code, stderr.String())
	}
	if got := stdout.String(); got != "preparing repo\n" {
		t.Fatalf("stdout = %q, want preparing repo", got)
	}
}

// unmount and remount are requests to the running daemon, so without one
// they must fail rather than report a change nothing made.
func TestMountCommandsFailWithoutDaemon(t *testing.T) {
	for _, command := range []string{"unmount", "remount"} {
		t.Run(command, func(t *testing.T) {
			t.Setenv("ARTIFACT_FS_ROOT", t.TempDir())
			ctx := context.Background()
			var stdout, stderr bytes.Buffer
			if code := Run(ctx, []string{"add-repo", "--name", "repo", "--remote", "https://github.com/example/repo.git", "--async"}, &stdout, &stderr); code != 0 {
				t.Fatalf("add-repo exit = %d, stderr=%q", code, stderr.String())
			}
			stdout.Reset()
			stderr.Reset()
			if code := Run(ctx, []string{command, "--name", "repo"}, &stdout, &stderr); code == 0 {
				t.Fatalf("%s exit = 0, stdout=%q", command, stdout.String())
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want nothing", stdout.String())
			}
			if !strings.Contains(stderr.String(), daemon.ErrNoDaemon.Error()) {
				t.Fatalf("stderr = %q, want %q", stderr.String(), daemon.ErrNoDaemon)
			}
		})
	}
}

func TestStatusCLIPrintsDaemonMountError(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ARTIFACT_FS_ROOT", root)
	ctx := context.Background()
	var stdout, stderr bytes.Buffer
	if code := Run(ctx, []string{"add-repo", "--name", "repo", "--remote", "https://github.com/example/repo.git", "--async"}, &stdout, &stderr); code != 0 {
		t.Fatalf("add-repo exit = %d, stderr=%q", code, stderr.String())
	}
	// The daemon records mount errors in the registry that status reads.
	reg, err := registry.New(ctx, filepath.Join(root, "config", "repos.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	err = reg.PutRuntime(ctx, model.RepoRuntimeRecord{RepoID: "repo", LastError: "fskit: extension requires approval", LastErrorPhase: "mount"})
	if closeErr := reg.Close(); err != nil || closeErr != nil {
		t.Fatalf("record mount error: %v, close: %v", err, closeErr)
	}

	stdout.Reset()
	if code := Run(ctx, []string{"status", "--name", "repo"}, &stdout, &stderr); code != 0 {
		t.Fatalf("status exit = %d, stderr=%q", code, stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, " mount_error=fskit:_extension_requires_approval ") {
		t.Fatalf("status output %q missing mount error", got)
	}
}

func TestFSMonitorHookCLIAnswersGitToken(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ARTIFACT_FS_ROOT", root)
	ctx := context.Background()
	svc, err := daemon.NewController(ctx, root, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	// "/" is always in the mount table, so it stands in for a live mount.
	cfg := model.RepoConfig{Name: "repo", RemoteURL: "https://github.com/example/repo.git", MountPath: "/", Enabled: true}
	if err := svc.AddRepoWithOptions(ctx, cfg, daemon.AddRepoOptions{Async: true}); err != nil {
		t.Fatal(err)
	}
	repos, err := svc.ListRepos(ctx)
	if closeErr := svc.Close(); err != nil || closeErr != nil || len(repos) != 1 {
		t.Fatalf("list repos: %v, %v, %d", err, closeErr, len(repos))
	}
	ov, err := overlay.New(ctx, repos[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := ov.Close(); err != nil {
		t.Fatal(err)
	}
	hook := func(since string) []string {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if code := Run(ctx, []string{"fsmonitor-hook", "--name", "repo", "2", since}, &stdout, &stderr); code != 0 {
			t.Fatalf("fsmonitor-hook exit = %d, stderr=%q", code, stderr.String())
		}
		return strings.Split(stdout.String(), "\x00")
	}
	fields := hook("other-hook:1")
	if len(fields) != 3 || !strings.HasPrefix(fields[0], "artifact-fs:repo:") || fields[1] != "/" {
		t.Fatalf("fsmonitor-hook output for a foreign token = %q, want a token and a rescan", fields)
	}
	// A new token and no paths: the empty overlay answers the current token.
	if fields := hook(fields[0]); len(fields) != 2 || !strings.HasPrefix(fields[0], "artifact-fs:repo:") || fields[1] != "" {
		t.Fatalf("fsmonitor-hook output for its own token = %q", fields)
	}
}
