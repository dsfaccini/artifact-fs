package daemon

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/cloudflare/artifact-fs/internal/overlay"
	"github.com/cloudflare/artifact-fs/internal/snapshot"
)

func (s *Service) bootstrapCleanStatusIndex(ctx context.Context, cfg model.RepoConfig, headOID string, gen int64, snap *snapshot.Store, ov *overlay.Store) error {
	storedOID, _, storedGen, err := snap.ReadState(ctx)
	if err != nil {
		return err
	}
	if storedOID != headOID || storedGen != gen {
		return nil
	}
	entries, err := ov.ListAll(ctx)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return nil
	}
	if err := s.git.ConfigureFSMonitorHook(ctx, cfg, s.root); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.logger.Warn("git status clean baseline setup skipped", "repo", cfg.Name, "error", err)
		return nil
	}
	bootstrapped, err := s.git.BootstrapCleanIndexForFSMonitor(ctx, cfg, headOID, func() (bool, error) {
		currentOID, _, currentGen, err := snap.ReadState(ctx)
		if err != nil {
			return false, err
		}
		if currentOID != headOID || currentGen != gen {
			return false, nil
		}
		currentEntries, err := ov.ListAll(ctx)
		if err != nil {
			return false, err
		}
		return len(currentEntries) == 0, nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.logger.Warn("git status clean baseline bootstrap skipped", "repo", cfg.Name, "error", err)
		return nil
	}
	if bootstrapped {
		s.logger.Debug("git status clean baseline bootstrapped", "repo", cfg.Name)
	}
	return nil
}

func (s *Service) configureStatusOptimization(ctx context.Context, cfg model.RepoConfig) {
	if err := s.git.ConfigureStatusOptimization(ctx, cfg, s.root); err != nil {
		if ctx.Err() != nil {
			return
		}
		s.logger.Warn("git status optimization setup failed", "repo", cfg.Name, "error", err)
	}
}

// FSMonitorHook answers Git's fsmonitor hook protocol version 2 from the
// overlay, which records changes made through the mount. It trusts only tokens
// issued for this registration, and only while the worktree is mounted.
// Otherwise native edits may have bypassed the overlay, and "/" makes Git
// rescan the worktree.
func (s *Service) FSMonitorHook(ctx context.Context, name string, since string, w io.Writer) error {
	cfg, err := s.registry.GetRepo(ctx, name)
	if err != nil {
		return err
	}
	s.fillPaths(&cfg)
	prefix := fmt.Sprintf("artifact-fs:%s:%s:", cfg.ID, cfg.ConfigVersion)
	paths := []string{"/"}
	// The mount table check sends no request to the mount, which stays gated
	// while the daemon runs Git during preparation.
	if strings.HasPrefix(since, prefix) && isMounted(cfg.MountPath) {
		ov, err := overlay.New(ctx, cfg)
		if err != nil {
			return err
		}
		defer ov.Close()
		entries, err := ov.ListAll(ctx)
		if err != nil {
			return err
		}
		paths = fsMonitorDirtyPaths(entries)
	}
	token := fmt.Sprintf("%s%d", prefix, time.Now().UnixNano())
	if err := writeHookField(w, token); err != nil {
		return err
	}
	for _, p := range paths {
		if err := writeHookField(w, p); err != nil {
			return err
		}
	}
	return nil
}

func writeHookField(w io.Writer, value string) error {
	n, err := io.WriteString(w, value)
	if err != nil {
		return err
	}
	if n != len(value) {
		return io.ErrShortWrite
	}
	n, err = w.Write([]byte{0})
	if err != nil {
		return err
	}
	if n != 1 {
		return io.ErrShortWrite
	}
	return nil
}

func fsMonitorDirtyPaths(entries []model.OverlayEntry) []string {
	set := map[string]struct{}{}
	add := func(path string) {
		path = model.CleanPath(path)
		if path == "." {
			return
		}
		set[path] = struct{}{}
	}
	for _, e := range entries {
		add(e.Path)
		if e.Kind == model.OverlayKindRename && e.TargetPath != "" {
			add(e.TargetPath)
		}
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
