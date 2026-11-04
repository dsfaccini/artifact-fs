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
// ErrorLogger. They reach the daemon log as warnings, with the remote redacted.
func TestMountRepoLogsFuseErrorsRedacted(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	repo := model.RepoConfig{Name: "repo", MountPath: t.TempDir(), RemoteURL: "https://git.example.com/private/repo.git"}
	for _, cfg := range mountRepoConfigs(t, repo, logger) {
		if cfg.ErrorLogger == nil {
			t.Fatal("mount config has no ErrorLogger")
		}
		buf.Reset()
		// The library's format for an op error.
		cfg.ErrorLogger.Printf("Op 0x%08x %T] -> Error: %q", 2, &fuseops.ReadFileOp{}, "fetch "+repo.RemoteURL+": exit status 128")
		var rec struct{ Level, Repo, Error string }
		if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
			t.Fatalf("log output %q is not one JSON record: %v", buf.String(), err)
		}
		if rec.Level != "WARN" || rec.Repo != repo.Name || !strings.Contains(rec.Error, "*fuseops.ReadFileOp") {
			t.Fatalf("log record = %+v, want a warning for repo %q that names the op", rec, repo.Name)
		}
		if strings.Contains(buf.String(), "git.example.com") || !strings.Contains(rec.Error, "REDACTED_REMOTE") {
			t.Fatalf("log output %q does not redact the remote", buf.String())
		}
	}
}
