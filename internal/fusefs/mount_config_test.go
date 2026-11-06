//go:build !windows

package fusefs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"syscall"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/dsfaccini/fuse"
	"github.com/dsfaccini/fuse/fuseops"
)

// mountAttempt is one fuse.Mount call.
type mountAttempt struct {
	cfg    fuse.MountConfig
	server fuse.Server
}

// mountRepoAttempts runs MountRepoWithGate with fuse.Mount replaced and returns
// each call it made. Every attempt fails, so nothing mounts. An FSKit attempt
// fails as unavailable, which lets auto try the kernel extension.
func mountRepoAttempts(t *testing.T, repo model.RepoConfig, logger *slog.Logger) []mountAttempt {
	t.Helper()
	t.Setenv(backendEnv, "")
	var attempts []mountAttempt
	saved := fuseMount
	fuseMount = func(_ context.Context, _ string, server fuse.Server, cfg *fuse.MountConfig) (*fuse.MountedFileSystem, error) {
		attempts = append(attempts, mountAttempt{cfg: *cfg, server: server})
		if cfg.Backend == fuse.BackendFSKit {
			return nil, fuse.ErrFSKitUnavailable
		}
		return nil, errors.New("package tests do not mount")
	}
	t.Cleanup(func() { fuseMount = saved })
	if _, err := MountRepoWithGate(context.Background(), context.Background(), repo, nil, nil, logger); err == nil {
		t.Fatal("MountRepoWithGate succeeded without a mount")
	}
	if len(attempts) == 0 {
		t.Fatal("MountRepoWithGate never reached fuse.Mount")
	}
	return attempts
}

// The fuse library reports op errors and failed replies only to an
// ErrorLogger. A bug it answered with EIO reaches the daemon log as an error,
// and a short FSKit read it answered with EIO as a warning. Any other op error,
// which the library also logs for expected errnos, goes out at Debug, and a
// failed reply as a warning. Each carries the repo, with its remote redacted.
func TestMountRepoLogsFuseErrorsRedacted(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	repo := model.RepoConfig{Name: "repo", MountPath: t.TempDir(), RemoteURL: "https://git.example.com/private/repo.git"}
	for _, attempt := range mountRepoAttempts(t, repo, logger) {
		cfg := attempt.cfg
		if cfg.ErrorLogger == nil {
			t.Fatal("mount config has no ErrorLogger")
		}
		// The library's formats for an op error and for a failed reply.
		for _, line := range []struct {
			log       func()
			wantLevel string
			wantText  string
			remote    bool
		}{
			{
				log: func() {
					cfg.ErrorLogger.Printf("Op 0x%08x %T] -> Error: %q", 2, &fuseops.ReadFileOp{}, "fetch "+repo.RemoteURL+": exit status 128")
				},
				wantLevel: "DEBUG",
				wantText:  "*fuseops.ReadFileOp",
				remote:    true,
			},
			{
				log: func() {
					cfg.ErrorLogger.Printf("Op 0x%08x %T] -> %s: panic: %v\n%s", 3, &fuseops.LookUpInodeOp{}, "Error", "fetch "+repo.RemoteURL+": nil map", "goroutine 7 [running]:\nruntime/debug.Stack()")
				},
				wantLevel: "ERROR",
				wantText:  "panic: fetch",
				remote:    true,
			},
			{
				log: func() {
					cfg.ErrorLogger.Printf("Op 0x%08x %T] -> %s: panic: %v\n%s", 4, &fuseops.WriteFileOp{}, "Callback", "callback for "+repo.RemoteURL, "goroutine 8 [running]:")
				},
				wantLevel: "ERROR",
				wantText:  "Callback: panic",
				remote:    true,
			},
			{
				log: func() {
					cfg.ErrorLogger.Printf("Op 0x%08x %T] -> %s: %q", 5, &fuseops.OpenFileOp{}, "Error", fmt.Errorf("fuseutil: the method exited without returning: %w", syscall.EIO))
				},
				wantLevel: "ERROR",
				wantText:  "exited without returning",
			},
			{
				log: func() {
					cfg.ErrorLogger.Printf("Op 0x%08x %T] -> %s: %q", 6, &fuseops.ReadDirOp{}, "Error", fmt.Errorf("%T.BytesRead is %d, outside its %d-byte buffer: %w", &fuseops.ReadDirOp{}, 5000, 4096, syscall.EIO))
				},
				wantLevel: "ERROR",
				wantText:  "outside its 4096-byte buffer",
			},
			{
				log: func() {
					cfg.ErrorLogger.Printf("Op 0x%08x %T] -> %s: %q", 7, &fuseops.ReadFileOp{}, "Error", fmt.Errorf("read of inode %d at offset %d returned %d bytes, want %d for size %d: %w", 5, 0, 3, 10, 10, syscall.EIO))
				},
				wantLevel: "WARN",
				wantText:  "returned 3 bytes, want 10",
			},
			{
				log: func() {
					cfg.ErrorLogger.Print("writeMessage: no such device [16 0 0 0 254 255 255 255 2 0 0 0 0 0 0 0]")
				},
				wantLevel: "WARN",
				wantText:  "writeMessage",
			},
		} {
			buf.Reset()
			line.log()
			var rec struct{ Level, Repo, Error string }
			if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
				t.Fatalf("log output %q is not one JSON record: %v", buf.String(), err)
			}
			if rec.Level != line.wantLevel || rec.Repo != repo.Name || !strings.Contains(rec.Error, line.wantText) {
				t.Fatalf("log record = %+v, want level %s for repo %q containing %q", rec, line.wantLevel, repo.Name, line.wantText)
			}
			if strings.Contains(buf.String(), "git.example.com") || line.remote && !strings.Contains(rec.Error, "REDACTED_REMOTE") {
				t.Fatalf("log output %q does not redact the remote", buf.String())
			}
		}
	}
}

// The mount waits only as long as ctx, and every op's context derives from
// opCtx, so the daemon cancels a stopped runtime's ops in flight.
func TestMountRepoBoundsTheMountAndParentsOpsOnOpContext(t *testing.T) {
	t.Setenv(backendEnv, BackendFSKit)
	type key struct{}
	ctx := context.WithValue(context.Background(), key{}, "mount")
	opCtx := context.WithValue(context.Background(), key{}, "ops")
	saved := fuseMount
	t.Cleanup(func() { fuseMount = saved })
	var mountCtx, gotOpCtx context.Context
	fuseMount = func(c context.Context, _ string, _ fuse.Server, cfg *fuse.MountConfig) (*fuse.MountedFileSystem, error) {
		mountCtx, gotOpCtx = c, cfg.OpContext
		return nil, errors.New("package tests do not mount")
	}
	repo := model.RepoConfig{Name: "repo", MountPath: t.TempDir()}
	if _, err := MountRepoWithGate(ctx, opCtx, repo, nil, nil, slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("MountRepoWithGate succeeded without a mount")
	}
	if mountCtx == nil || mountCtx.Value(key{}) != "mount" || gotOpCtx == nil || gotOpCtx.Value(key{}) != "ops" {
		t.Fatalf("mount context %v and op context %v, want the mount bounded by ctx and ops parented on opCtx", mountCtx, gotOpCtx)
	}
}
