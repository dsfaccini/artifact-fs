package registry

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/model"
)

func TestRepoPrepareFieldsRoundTrip(t *testing.T) {
	ctx := context.Background()
	store, err := New(ctx, filepath.Join(t.TempDir(), "repos.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	cfg := model.RepoConfig{
		RepoPaths: model.RepoPaths{
			ID:            "repo",
			Name:          "repo",
			MountRoot:     "/mnt",
			MountPath:     "/mnt/repo",
			GitDir:        "/git/repo",
			OverlayDir:    "/overlay/repo",
			BlobCacheDir:  "/cache/repo",
			MetaDBPath:    "/meta/repo.sqlite",
			OverlayDBPath: "/overlay/repo/meta.sqlite",
		},
		RepoSpec: model.RepoSpec{
			RemoteURL:         "https://github.com/example/repo.git",
			RemoteURLRedacted: "https://github.com/example/repo.git",
			Branch:            "master",
			PreparedGitDir:    true,
			AdoptGitDir:       true,
			FetchRef:          "master",
			RequiredCommit:    "0123456789012345678901234567890123456789",
			HistoryDepth:      1,
		},
		RefreshPolicy: model.RefreshPolicy{
			RefreshInterval:       time.Minute,
			RemoteRefreshDisabled: true,
		},
		Enabled:       true,
		PrepareState:  model.PrepareStatePreparing,
		ConfigVersion: "version-1",
	}
	if err := store.AddRepo(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdatePrepareStateForRequest(ctx, cfg, model.PrepareStateFailed, "clone failed"); err != nil {
		t.Fatal(err)
	}

	got, err := store.GetRepo(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if !got.AdoptGitDir {
		t.Fatal("AdoptGitDir = false, want true")
	}
	if !got.PreparedGitDir {
		t.Fatal("PreparedGitDir = false, want true")
	}
	if got.FetchRef != "master" {
		t.Fatalf("FetchRef = %q, want master", got.FetchRef)
	}
	if got.PrepareState != model.PrepareStateFailed {
		t.Fatalf("PrepareState = %q, want failed", got.PrepareState)
	}
	if got.PrepareError != "clone failed" {
		t.Fatalf("PrepareError = %q, want clone failed", got.PrepareError)
	}
	if got.RequiredCommit != cfg.RequiredCommit || got.HistoryDepth != 1 || !got.RemoteRefreshDisabled {
		t.Fatalf("source policy = %+v", got)
	}
	if err := store.RecordAcquisition(ctx, cfg, model.PreparedSource{Ref: cfg.Branch, Commit: cfg.RequiredCommit, Verified: true}); err != nil {
		t.Fatal(err)
	}
	got, err = store.GetRepo(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if got.AcquiredRef != cfg.Branch || got.AcquiredCommit != cfg.RequiredCommit || got.AcquiredAt.IsZero() {
		t.Fatalf("acquisition receipt = %+v", got)
	}
	if got.ConfigVersion != "version-1" {
		t.Fatalf("ConfigVersion = %q, want version-1", got.ConfigVersion)
	}
}

// A preparation's outcome lands only while its config and its prepare request
// are both still the registry's.
func TestPrepareStateUpdateRejectsOlderConfigOrRequest(t *testing.T) {
	ctx := context.Background()
	store, err := New(ctx, filepath.Join(t.TempDir(), "repos.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{ID: "repo", Name: "repo"}, RepoSpec: model.RepoSpec{Branch: "refs/heads/main"}, ConfigVersion: "version-1", PrepareRequest: "request-1"}
	if err := store.AddRepo(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	staleConfig := cfg
	cfg.ConfigVersion = "version-2"
	cfg.PrepareState = model.PrepareStatePreparing
	if err := store.AddRepo(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdatePrepareStateForRequest(ctx, staleConfig, "", "old failure"); err != ErrRepoChanged {
		t.Fatalf("stale config update error = %v, want ErrRepoChanged", err)
	}
	staleRequest := cfg
	if err := store.BeginPrepare(ctx, cfg, model.PrepareStatePreparing, "request-2"); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdatePrepareStateForRequest(ctx, staleRequest, model.PrepareStateFailed, "old failure"); err != ErrRepoChanged {
		t.Fatalf("stale request update error = %v, want ErrRepoChanged", err)
	}
	got, err := store.GetRepo(ctx, cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	if got.ConfigVersion != "version-2" || got.PrepareRequest != "request-2" || got.PrepareState != model.PrepareStatePreparing || got.PrepareError != "" {
		t.Fatalf("new config changed: version/request/state/error = %q/%q/%q/%q", got.ConfigVersion, got.PrepareRequest, got.PrepareState, got.PrepareError)
	}
	cfg.PrepareRequest = "request-2"
	if err := store.UpdatePrepareStateForRequest(ctx, cfg, model.PrepareStateReady, ""); err != nil {
		t.Fatal(err)
	}
}

// A prepare request applies only to the config it was made for, and leaves
// that config's version as it is.
func TestBeginPrepareRecordsRequestWithCompareAndSwap(t *testing.T) {
	ctx := context.Background()
	store, err := New(ctx, filepath.Join(t.TempDir(), "repos.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{ID: "repo", Name: "repo"}, RepoSpec: model.RepoSpec{Branch: "refs/heads/main"}, ConfigVersion: "version-1", PrepareRequest: "request-1"}
	if err := store.AddRepo(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginPrepare(ctx, cfg, model.PrepareStateSyncPreparing, "request-2"); err != nil {
		t.Fatal(err)
	}
	stale := cfg
	stale.ConfigVersion = "version-0"
	if err := store.BeginPrepare(ctx, stale, model.PrepareStateSyncPreparing, "request-3"); err != ErrRepoChanged {
		t.Fatalf("stale begin error = %v, want ErrRepoChanged", err)
	}
	got, err := store.GetRepo(ctx, cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	if got.ConfigVersion != "version-1" || got.PrepareRequest != "request-2" || got.PrepareState != model.PrepareStateSyncPreparing || got.PrepareError != "" {
		t.Fatalf("prepared config = %+v", got)
	}
}

func TestSubsecondRefreshRoundTripsWithoutChangingPrepareState(t *testing.T) {
	ctx := context.Background()
	store, err := New(ctx, filepath.Join(t.TempDir(), "repos.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := model.RepoConfig{RepoPaths: model.RepoPaths{ID: "repo", Name: "repo"}, RepoSpec: model.RepoSpec{Branch: "main"}, RefreshPolicy: model.RefreshPolicy{RefreshInterval: time.Second}, PrepareState: model.PrepareStatePreparing}
	if err := store.AddRepo(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateRefresh(ctx, cfg.ID, model.RefreshPolicy{RefreshInterval: 250 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetRepo(ctx, cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	if got.RefreshInterval != 250*time.Millisecond {
		t.Fatalf("refresh interval = %v, want 250ms", got.RefreshInterval)
	}
	if got.PrepareState != model.PrepareStatePreparing {
		t.Fatalf("prepare state = %q, want preparing", got.PrepareState)
	}
}
