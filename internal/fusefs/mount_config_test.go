//go:build !windows

package fusefs

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/dsfaccini/fuse"
	"github.com/dsfaccini/fuse/fuseops"
)

// mountRepoConfigs runs MountRepoWithGate with fuse.Mount replaced and returns
// each config it handed the library. Every attempt fails, so nothing mounts. An
// FSKit attempt fails as unavailable, which lets auto try the kernel extension.
func mountRepoConfigs(t *testing.T, repo model.RepoConfig, logger *slog.Logger) []fuse.MountConfig {
	t.Helper()
	t.Setenv(backendEnv, "")
	var cfgs []fuse.MountConfig
	saved := fuseMount
	fuseMount = func(_ string, _ fuse.Server, cfg *fuse.MountConfig) (*fuse.MountedFileSystem, error) {
		cfgs = append(cfgs, *cfg)
		if cfg.Options["backend"] == "fskit" {
			return nil, fuse.ErrFSKitUnavailable
		}
		return nil, errors.New("package tests do not mount")
	}
	t.Cleanup(func() { fuseMount = saved })
	if _, err := MountRepoWithGate(repo, nil, nil, logger); err == nil {
		t.Fatal("MountRepoWithGate succeeded without a mount")
	}
	if len(cfgs) == 0 {
		t.Fatal("MountRepoWithGate never reached fuse.Mount")
	}
	return cfgs
}

// The fuse library reports op errors and failed replies only to an
// ErrorLogger. An op error, which the library also logs for expected errnos,
// reaches the daemon log at Debug, and a failed reply as a warning. Each
// carries the repo, with its remote redacted.
func TestMountRepoLogsFuseErrorsRedacted(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	repo := model.RepoConfig{Name: "repo", MountPath: t.TempDir(), RemoteURL: "https://git.example.com/private/repo.git"}
	for _, cfg := range mountRepoConfigs(t, repo, logger) {
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
