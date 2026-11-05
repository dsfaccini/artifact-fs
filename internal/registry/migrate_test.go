package registry

import (
	"context"
	"io"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/metatest"
	"github.com/cloudflare/artifact-fs/internal/model"
)

// legacyRegistries are databases that builds before migrations created, named
// for the commit that created them.
var legacyRegistries = map[string]struct {
	stmts []string
	want  model.RepoConfig
}{
	// The first schema: no remote_url and none of the columns added since.
	"01dd4de": {
		stmts: []string{
			`CREATE TABLE IF NOT EXISTS repos (
			  repo_id TEXT PRIMARY KEY,
			  name TEXT NOT NULL UNIQUE,
			  mount_root TEXT NOT NULL,
			  mount_path TEXT NOT NULL,
			  remote_url_redacted TEXT NOT NULL,
			  remote_url_secret_ref TEXT,
			  branch TEXT NOT NULL,
			  refresh_interval_seconds INTEGER NOT NULL,
			  git_dir TEXT NOT NULL,
			  overlay_dir TEXT NOT NULL,
			  blob_cache_dir TEXT NOT NULL,
			  meta_db_path TEXT NOT NULL,
			  overlay_db_path TEXT NOT NULL,
			  enabled INTEGER NOT NULL DEFAULT 1,
			  created_at_ns INTEGER NOT NULL,
			  updated_at_ns INTEGER NOT NULL
			);`,
			`INSERT INTO repos (repo_id, name, mount_root, mount_path, remote_url_redacted, remote_url_secret_ref, branch, refresh_interval_seconds, git_dir, overlay_dir, blob_cache_dir, meta_db_path, overlay_db_path, enabled, created_at_ns, updated_at_ns)
			VALUES ('repo', 'repo', '/mnt', '/mnt/repo', 'https://example.com/repo.git', 'secret-ref', 'main', 45, '/git/repo', '/overlay/repo', '/cache/repo', '/meta/repo.sqlite', '/overlay/repo/meta.sqlite', 1, 1, 1)`,
		},
		want: model.RepoConfig{ID: "repo", Name: "repo", MountRoot: "/mnt", MountPath: "/mnt/repo", RemoteURLRedacted: "https://example.com/repo.git", Branch: "main", RefreshInterval: 45 * time.Second, GitDir: "/git/repo", OverlayDir: "/overlay/repo", BlobCacheDir: "/cache/repo", MetaDBPath: "/meta/repo.sqlite", OverlayDBPath: "/overlay/repo/meta.sqlite", Enabled: true},
	},
	// main before migrations.
	"25971f7": {
		stmts: []string{
			reposTableV1,
			`INSERT INTO repos (repo_id, name, mount_root, mount_path, remote_url, remote_url_redacted, branch, refresh_interval_seconds, refresh_interval_ns, git_dir, overlay_dir, blob_cache_dir, meta_db_path, overlay_db_path, enabled, prepared_gitdir, adopt_gitdir, fetch_ref, prepare_state, prepare_error, mount_error, required_commit, history_depth, refresh_disabled, acquired_ref, acquired_commit, acquired_at_ns, config_version, created_at_ns, updated_at_ns)
			VALUES ('repo', 'repo', '/mnt', '/mnt/repo', 'https://example.com/repo.git', 'https://example.com/repo.git', 'main', 0, 1500000000, '/git/repo', '/overlay/repo', '/cache/repo', '/meta/repo.sqlite', '/overlay/repo/meta.sqlite', 1, 1, 1, 'main', 'failed', 'clone failed', 'mount failed', '0123456789012345678901234567890123456789', 1, 1, 'refs/heads/main', '0123456789012345678901234567890123456789', 7, 'version-1', 1, 1)`,
		},
		want: model.RepoConfig{ID: "repo", Name: "repo", MountRoot: "/mnt", MountPath: "/mnt/repo", RemoteURL: "https://example.com/repo.git", RemoteURLRedacted: "https://example.com/repo.git", Branch: "main", RefreshInterval: 1500 * time.Millisecond, GitDir: "/git/repo", OverlayDir: "/overlay/repo", BlobCacheDir: "/cache/repo", MetaDBPath: "/meta/repo.sqlite", OverlayDBPath: "/overlay/repo/meta.sqlite", Enabled: true, PreparedGitDir: true, AdoptGitDir: true, FetchRef: "main", PrepareState: "failed", PrepareError: "clone failed", MountError: "mount failed", RequiredCommit: "0123456789012345678901234567890123456789", HistoryDepth: 1, RemoteRefreshDisabled: true, AcquiredRef: "refs/heads/main", AcquiredCommit: "0123456789012345678901234567890123456789", AcquiredAt: time.Unix(0, 7), ConfigVersion: "version-1"},
	},
}

func openRegistry(path string) func() (io.Closer, error) {
	return func() (io.Closer, error) { return New(context.Background(), path) }
}

// createRegistry creates a registry at a new path and returns the path.
func createRegistry(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "repos.sqlite")
	store, err := New(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	return path
}

func TestNewMigratesEarlierSchemas(t *testing.T) {
	want := metatest.Schema(t, createRegistry(t))
	for name, legacy := range legacyRegistries {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "repos.sqlite")
			metatest.Exec(t, path, legacy.stmts...)
			store, err := New(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if got := metatest.Schema(t, path); !slices.Equal(got, want) {
				t.Fatalf("schema = %q, want %q", got, want)
			}
			if got := metatest.UserVersion(t, path); got != len(migrations) {
				t.Fatalf("user_version = %d, want %d", got, len(migrations))
			}
			got, err := store.GetRepo(ctx, "repo")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, legacy.want) {
				t.Fatalf("repo = %+v, want %+v", got, legacy.want)
			}
		})
	}
}

func TestNewWaitsOutAnotherConnectionsWriteLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "repos.sqlite")
	metatest.Exec(t, path, legacyRegistries["25971f7"].stmts...)
	metatest.OpenWhileLocked(t, path, openRegistry(path))
}

func TestConcurrentFirstOpensApplyEachMigrationOnce(t *testing.T) {
	want := metatest.Schema(t, createRegistry(t))
	for range 20 {
		path := filepath.Join(t.TempDir(), "repos.sqlite")
		metatest.OpenConcurrently(t, 16, openRegistry(path))
		if got := metatest.Schema(t, path); !slices.Equal(got, want) {
			t.Fatalf("schema = %q, want %q", got, want)
		}
		if got := metatest.UserVersion(t, path); got != len(migrations) {
			t.Fatalf("user_version = %d, want %d", got, len(migrations))
		}
	}
}

// A database a later build migrated may hold a schema this build cannot read
// or write correctly.
func TestNewRefusesANewerSchema(t *testing.T) {
	path := createRegistry(t)
	metatest.Exec(t, path, `PRAGMA user_version = 99`)
	store, err := New(context.Background(), path)
	if err == nil {
		store.Close()
		t.Fatal("New on a newer schema succeeded")
	}
	if !strings.Contains(err.Error(), "newer") {
		t.Fatalf("New on a newer schema: %v", err)
	}
}
