package registry

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/metatest"
	"github.com/cloudflare/artifact-fs/internal/model"
)

// fullRepoRow sets every column the 25971f7 schema has but
// remote_url_secret_ref, and fullRepo is the config it reads back as, with
// the warning threshold migration 6 gives an existing row.
const fullRepoRow = `INSERT INTO repos (repo_id, name, mount_root, mount_path, remote_url, remote_url_redacted, branch, refresh_interval_seconds, refresh_interval_ns, git_dir, overlay_dir, blob_cache_dir, meta_db_path, overlay_db_path, enabled, prepared_gitdir, adopt_gitdir, fetch_ref, prepare_state, prepare_error, mount_error, required_commit, history_depth, refresh_disabled, acquired_ref, acquired_commit, acquired_at_ns, config_version, created_at_ns, updated_at_ns)
	VALUES ('repo', 'repo', '/mnt', '/mnt/repo', 'https://example.com/repo.git', 'https://example.com/repo.git', 'main', 0, 1500000000, '/git/repo', '/overlay/repo', '/cache/repo', '/meta/repo.sqlite', '/overlay/repo/meta.sqlite', 1, 1, 1, 'main', 'failed', 'clone failed', 'mount failed', '0123456789012345678901234567890123456789', 1, 1, 'refs/heads/main', '0123456789012345678901234567890123456789', 7, 'version-1', 1, 1)`

var fullRepo = model.RepoConfig{RepoPaths: model.RepoPaths{ID: "repo", Name: "repo", MountRoot: "/mnt", MountPath: "/mnt/repo", GitDir: "/git/repo", OverlayDir: "/overlay/repo", BlobCacheDir: "/cache/repo", MetaDBPath: "/meta/repo.sqlite", OverlayDBPath: "/overlay/repo/meta.sqlite"}, RepoSpec: model.RepoSpec{RemoteURL: "https://example.com/repo.git", RemoteURLRedacted: "https://example.com/repo.git", Branch: "main", PreparedGitDir: true, AdoptGitDir: true, FetchRef: "main", RequiredCommit: "0123456789012345678901234567890123456789", HistoryDepth: 1}, RefreshPolicy: model.RefreshPolicy{RefreshInterval: 1500 * time.Millisecond, RemoteRefreshDisabled: true}, Enabled: true, PrepareState: "failed", PrepareError: "clone failed", AcquiredRef: "refs/heads/main", AcquiredCommit: "0123456789012345678901234567890123456789", AcquiredAt: time.Unix(0, 7), ConfigVersion: "version-1", WarnKeptGenerations: 50}

// fullRepoWithRequest is fullRepo with the mount request a migrated row can hold.
var fullRepoWithRequest = func() model.RepoConfig { cfg := fullRepo; cfg.MountRequest = "request-1"; return cfg }()

// fullRepoWithRequests is fullRepoWithRequest with the prepare request a
// migrated row can hold.
var fullRepoWithRequests = func() model.RepoConfig { cfg := fullRepoWithRequest; cfg.PrepareRequest = "prepare-1"; return cfg }()

// reposTableV3 is the repos table after migration 3.
const reposTableV3 = `CREATE TABLE IF NOT EXISTS repos (
	  repo_id TEXT PRIMARY KEY,
	  name TEXT NOT NULL UNIQUE,
	  mount_root TEXT NOT NULL,
	  mount_path TEXT NOT NULL,
	  remote_url TEXT NOT NULL DEFAULT '',
	  remote_url_redacted TEXT NOT NULL,
	  branch TEXT NOT NULL,
	  refresh_interval_seconds INTEGER NOT NULL,
	  refresh_interval_ns INTEGER NOT NULL DEFAULT 0,
	  git_dir TEXT NOT NULL,
	  overlay_dir TEXT NOT NULL,
	  blob_cache_dir TEXT NOT NULL,
	  meta_db_path TEXT NOT NULL,
	  overlay_db_path TEXT NOT NULL,
	  enabled INTEGER NOT NULL DEFAULT 1,
	  prepared_gitdir INTEGER NOT NULL DEFAULT 0,
	  adopt_gitdir INTEGER NOT NULL DEFAULT 0,
	  fetch_ref TEXT NOT NULL DEFAULT '',
	  prepare_state TEXT NOT NULL DEFAULT '',
	  prepare_error TEXT NOT NULL DEFAULT '',
	  mount_error TEXT NOT NULL DEFAULT '',
	  required_commit TEXT NOT NULL DEFAULT '',
	  history_depth INTEGER NOT NULL DEFAULT 0,
	  refresh_disabled INTEGER NOT NULL DEFAULT 0,
	  acquired_ref TEXT NOT NULL DEFAULT '',
	  acquired_commit TEXT NOT NULL DEFAULT '',
	  acquired_at_ns INTEGER NOT NULL DEFAULT 0,
	  config_version TEXT NOT NULL DEFAULT '',
	  created_at_ns INTEGER NOT NULL,
	  updated_at_ns INTEGER NOT NULL,
	  mount_request TEXT NOT NULL DEFAULT ''
	);`

// repoRuntimeTableV3 is the repo_runtime table migration 3 creates.
const repoRuntimeTableV3 = `CREATE TABLE repo_runtime (
	  repo_id TEXT PRIMARY KEY,
	  state TEXT NOT NULL DEFAULT '',
	  phase TEXT NOT NULL DEFAULT '',
	  last_error TEXT NOT NULL DEFAULT '',
	  last_error_phase TEXT NOT NULL DEFAULT '',
	  last_error_at_ns INTEGER NOT NULL DEFAULT 0,
	  next_retry_at_ns INTEGER NOT NULL DEFAULT 0,
	  attempts INTEGER NOT NULL DEFAULT 0,
	  served_generation INTEGER NOT NULL DEFAULT 0,
	  served_commit TEXT NOT NULL DEFAULT '',
	  served_ref TEXT NOT NULL DEFAULT '',
	  last_fetch_at_ns INTEGER NOT NULL DEFAULT 0,
	  last_fetch_result TEXT NOT NULL DEFAULT '',
	  ahead INTEGER NOT NULL DEFAULT 0,
	  behind INTEGER NOT NULL DEFAULT 0,
	  diverged INTEGER NOT NULL DEFAULT 0,
	  mount_request TEXT NOT NULL DEFAULT '',
	  updated_at_ns INTEGER NOT NULL DEFAULT 0
	)`

// legacyRegistries are databases that earlier builds created, named for the
// commit that created them or for the user_version a migrated one records.
// Keep each fixture as it is, and add one at the new user_version with each
// migration. The fixture at the current user_version fails when an edit to a
// shipped migration changes the schema a new database gets.
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
		want: model.RepoConfig{RepoPaths: model.RepoPaths{ID: "repo", Name: "repo", MountRoot: "/mnt", MountPath: "/mnt/repo", GitDir: "/git/repo", OverlayDir: "/overlay/repo", BlobCacheDir: "/cache/repo", MetaDBPath: "/meta/repo.sqlite", OverlayDBPath: "/overlay/repo/meta.sqlite"}, RepoSpec: model.RepoSpec{RemoteURLRedacted: "https://example.com/repo.git", Branch: "refs/heads/main", FetchRef: "refs/heads/main"}, RefreshPolicy: model.RefreshPolicy{RefreshInterval: 45 * time.Second}, Enabled: true, WarnKeptGenerations: 50},
	},
	// main before migrations.
	"25971f7": {
		stmts: []string{
			`CREATE TABLE IF NOT EXISTS repos (
			  repo_id TEXT PRIMARY KEY,
			  name TEXT NOT NULL UNIQUE,
			  mount_root TEXT NOT NULL,
			  mount_path TEXT NOT NULL,
			  remote_url TEXT NOT NULL DEFAULT '',
			  remote_url_redacted TEXT NOT NULL,
			  remote_url_secret_ref TEXT,
			  branch TEXT NOT NULL,
			  refresh_interval_seconds INTEGER NOT NULL,
			  refresh_interval_ns INTEGER NOT NULL DEFAULT 0,
			  git_dir TEXT NOT NULL,
			  overlay_dir TEXT NOT NULL,
			  blob_cache_dir TEXT NOT NULL,
			  meta_db_path TEXT NOT NULL,
			  overlay_db_path TEXT NOT NULL,
			  enabled INTEGER NOT NULL DEFAULT 1,
			  prepared_gitdir INTEGER NOT NULL DEFAULT 0,
			  adopt_gitdir INTEGER NOT NULL DEFAULT 0,
			  fetch_ref TEXT NOT NULL DEFAULT '',
			  prepare_state TEXT NOT NULL DEFAULT '',
			  prepare_error TEXT NOT NULL DEFAULT '',
			  mount_error TEXT NOT NULL DEFAULT '',
			  required_commit TEXT NOT NULL DEFAULT '',
			  history_depth INTEGER NOT NULL DEFAULT 0,
			  refresh_disabled INTEGER NOT NULL DEFAULT 0,
			  acquired_ref TEXT NOT NULL DEFAULT '',
			  acquired_commit TEXT NOT NULL DEFAULT '',
			  acquired_at_ns INTEGER NOT NULL DEFAULT 0,
			  config_version TEXT NOT NULL DEFAULT '',
			  created_at_ns INTEGER NOT NULL,
			  updated_at_ns INTEGER NOT NULL
			);`,
			fullRepoRow,
		},
		want: fullRepo,
	},
	// This schema, after migration 2.
	"user_version 2": {
		stmts: []string{
			`CREATE TABLE IF NOT EXISTS repos (
			  repo_id TEXT PRIMARY KEY,
			  name TEXT NOT NULL UNIQUE,
			  mount_root TEXT NOT NULL,
			  mount_path TEXT NOT NULL,
			  remote_url TEXT NOT NULL DEFAULT '',
			  remote_url_redacted TEXT NOT NULL,
			  branch TEXT NOT NULL,
			  refresh_interval_seconds INTEGER NOT NULL,
			  refresh_interval_ns INTEGER NOT NULL DEFAULT 0,
			  git_dir TEXT NOT NULL,
			  overlay_dir TEXT NOT NULL,
			  blob_cache_dir TEXT NOT NULL,
			  meta_db_path TEXT NOT NULL,
			  overlay_db_path TEXT NOT NULL,
			  enabled INTEGER NOT NULL DEFAULT 1,
			  prepared_gitdir INTEGER NOT NULL DEFAULT 0,
			  adopt_gitdir INTEGER NOT NULL DEFAULT 0,
			  fetch_ref TEXT NOT NULL DEFAULT '',
			  prepare_state TEXT NOT NULL DEFAULT '',
			  prepare_error TEXT NOT NULL DEFAULT '',
			  mount_error TEXT NOT NULL DEFAULT '',
			  required_commit TEXT NOT NULL DEFAULT '',
			  history_depth INTEGER NOT NULL DEFAULT 0,
			  refresh_disabled INTEGER NOT NULL DEFAULT 0,
			  acquired_ref TEXT NOT NULL DEFAULT '',
			  acquired_commit TEXT NOT NULL DEFAULT '',
			  acquired_at_ns INTEGER NOT NULL DEFAULT 0,
			  config_version TEXT NOT NULL DEFAULT '',
			  created_at_ns INTEGER NOT NULL,
			  updated_at_ns INTEGER NOT NULL
			);`,
			fullRepoRow,
			`PRAGMA user_version = 2`,
		},
		want: fullRepo,
	},
	// This schema, after migration 3.
	"user_version 3": {
		stmts: []string{
			`CREATE TABLE IF NOT EXISTS repos (
			  repo_id TEXT PRIMARY KEY,
			  name TEXT NOT NULL UNIQUE,
			  mount_root TEXT NOT NULL,
			  mount_path TEXT NOT NULL,
			  remote_url TEXT NOT NULL DEFAULT '',
			  remote_url_redacted TEXT NOT NULL,
			  branch TEXT NOT NULL,
			  refresh_interval_seconds INTEGER NOT NULL,
			  refresh_interval_ns INTEGER NOT NULL DEFAULT 0,
			  git_dir TEXT NOT NULL,
			  overlay_dir TEXT NOT NULL,
			  blob_cache_dir TEXT NOT NULL,
			  meta_db_path TEXT NOT NULL,
			  overlay_db_path TEXT NOT NULL,
			  enabled INTEGER NOT NULL DEFAULT 1,
			  prepared_gitdir INTEGER NOT NULL DEFAULT 0,
			  adopt_gitdir INTEGER NOT NULL DEFAULT 0,
			  fetch_ref TEXT NOT NULL DEFAULT '',
			  prepare_state TEXT NOT NULL DEFAULT '',
			  prepare_error TEXT NOT NULL DEFAULT '',
			  mount_error TEXT NOT NULL DEFAULT '',
			  required_commit TEXT NOT NULL DEFAULT '',
			  history_depth INTEGER NOT NULL DEFAULT 0,
			  refresh_disabled INTEGER NOT NULL DEFAULT 0,
			  acquired_ref TEXT NOT NULL DEFAULT '',
			  acquired_commit TEXT NOT NULL DEFAULT '',
			  acquired_at_ns INTEGER NOT NULL DEFAULT 0,
			  config_version TEXT NOT NULL DEFAULT '',
			  created_at_ns INTEGER NOT NULL,
			  updated_at_ns INTEGER NOT NULL,
			  mount_request TEXT NOT NULL DEFAULT ''
			);`,
			`CREATE TABLE repo_runtime (
			  repo_id TEXT PRIMARY KEY,
			  state TEXT NOT NULL DEFAULT '',
			  phase TEXT NOT NULL DEFAULT '',
			  last_error TEXT NOT NULL DEFAULT '',
			  last_error_phase TEXT NOT NULL DEFAULT '',
			  last_error_at_ns INTEGER NOT NULL DEFAULT 0,
			  next_retry_at_ns INTEGER NOT NULL DEFAULT 0,
			  attempts INTEGER NOT NULL DEFAULT 0,
			  served_generation INTEGER NOT NULL DEFAULT 0,
			  served_commit TEXT NOT NULL DEFAULT '',
			  served_ref TEXT NOT NULL DEFAULT '',
			  last_fetch_at_ns INTEGER NOT NULL DEFAULT 0,
			  last_fetch_result TEXT NOT NULL DEFAULT '',
			  ahead INTEGER NOT NULL DEFAULT 0,
			  behind INTEGER NOT NULL DEFAULT 0,
			  diverged INTEGER NOT NULL DEFAULT 0,
			  mount_request TEXT NOT NULL DEFAULT '',
			  updated_at_ns INTEGER NOT NULL DEFAULT 0
			)`,
			fullRepoRow,
			`UPDATE repos SET mount_request = 'request-1'`,
			`PRAGMA user_version = 3`,
		},
		want: fullRepoWithRequest,
	},
	// This schema, after migration 4.
	"user_version 4": {
		stmts: []string{
			reposTableV3,
			`ALTER TABLE repos ADD COLUMN prepare_request TEXT NOT NULL DEFAULT ''`,
			repoRuntimeTableV3,
			fullRepoRow,
			`UPDATE repos SET mount_request = 'request-1', prepare_request = 'prepare-1'`,
			`PRAGMA user_version = 4`,
		},
		want: fullRepoWithRequests,
	},
	// This schema, after migration 5.
	"user_version 5": {
		stmts: []string{
			reposTableV3,
			`ALTER TABLE repos ADD COLUMN prepare_request TEXT NOT NULL DEFAULT ''`,
			repoRuntimeTableV3,
			`ALTER TABLE repo_runtime ADD COLUMN claims_leftover INTEGER NOT NULL DEFAULT 0`,
			fullRepoRow,
			`UPDATE repos SET mount_request = 'request-1', prepare_request = 'prepare-1'`,
			`PRAGMA user_version = 5`,
		},
		want: fullRepoWithRequests,
	},
	// This schema, after migration 6.
	"user_version 6": {
		stmts: []string{
			reposTableV3,
			`ALTER TABLE repos ADD COLUMN prepare_request TEXT NOT NULL DEFAULT ''`,
			repoRuntimeTableV3,
			`ALTER TABLE repo_runtime ADD COLUMN claims_leftover INTEGER NOT NULL DEFAULT 0`,
			`ALTER TABLE repos ADD COLUMN warn_kept_generations INTEGER NOT NULL DEFAULT 50`,
			`ALTER TABLE repo_runtime ADD COLUMN kept_generations INTEGER NOT NULL DEFAULT 0`,
			`ALTER TABLE repo_runtime ADD COLUMN kept_generations_warning TEXT NOT NULL DEFAULT ''`,
			fullRepoRow,
			`UPDATE repos SET mount_request = 'request-1', prepare_request = 'prepare-1', warn_kept_generations = 7`,
			`PRAGMA user_version = 6`,
		},
		want: func() model.RepoConfig { cfg := fullRepoWithRequests; cfg.WarnKeptGenerations = 7; return cfg }(),
	},
}

// Migration 4 stores every ref of a repo that clones or fetches in full, with
// the fetch ref defaulting to the source ref, as add-repo now does. It leaves
// an adopted worktree's refs alone.
func TestMigrationStoresRefsInFull(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "repos.sqlite")
	verifiedRow := func(name, branch, fetchRef string) string {
		return `INSERT INTO repos (repo_id, name, mount_root, mount_path, remote_url_redacted, branch, refresh_interval_seconds, git_dir, overlay_dir, blob_cache_dir, meta_db_path, overlay_db_path, fetch_ref, required_commit, acquired_ref, created_at_ns, updated_at_ns)
			VALUES ('` + name + `', '` + name + `', '/mnt', '/mnt/r', '', '` + branch + `', 30, '/git', '/overlay', '/cache', '/meta', '/overlay/meta', '` + fetchRef + `', '0123456789012345678901234567890123456789', '` + branch + `', 1, 1)`
	}
	row := func(name, branch, fetchRef, adopt string) string {
		return `INSERT INTO repos (repo_id, name, mount_root, mount_path, remote_url_redacted, branch, refresh_interval_seconds, git_dir, overlay_dir, blob_cache_dir, meta_db_path, overlay_db_path, adopt_gitdir, fetch_ref, created_at_ns, updated_at_ns)
			VALUES ('` + name + `', '` + name + `', '/mnt', '/mnt/r', '', '` + branch + `', 30, '/git', '/overlay', '/cache', '/meta', '/overlay/meta', ` + adopt + `, '` + fetchRef + `', 1, 1)`
	}
	metatest.Exec(t, path,
		reposTableV3,
		repoRuntimeTableV3,
		row("bare", "main", "", "0"),
		row("short", "refs/heads/main", "main", "0"),
		row("origin", "refs/heads/main", "origin/dev", "0"),
		row("tracking", "refs/heads/main", "refs/remotes/origin/dev", "0"),
		row("tag", "refs/tags/v1", "refs/tags/v1", "0"),
		row("spaced", "refs/heads/main", " dev ", "0"),
		row("empty", "", "", "0"),
		row("remotes", "refs/remotes/origin/x", "x", "0"),
		row("literal-origin", "refs/heads/origin/x", "origin/x", "0"),
		row("short-origin", "origin/x", "", "0"),
		row("adopted", "HEAD", "", "1"),
		verifiedRow("verified-remotes", "refs/remotes/origin/x", "refs/remotes/origin/x"),
		verifiedRow("verified-short", "refs/remotes/origin/x", "x"),
		`PRAGMA user_version = 3`,
	)
	store, err := New(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for name, want := range map[string][2]string{
		"bare":     {"refs/heads/main", "refs/heads/main"},
		"short":    {"refs/heads/main", "refs/heads/main"},
		"origin":   {"refs/heads/main", "refs/heads/dev"},
		"tracking": {"refs/heads/main", "refs/heads/dev"},
		"tag":      {"refs/tags/v1", "refs/tags/v1"},
		"spaced":   {"refs/heads/main", "refs/heads/dev"},
		// add-repo's default source ref.
		"empty":   {"refs/heads/main", "refs/heads/main"},
		"remotes": {"refs/heads/x", "refs/heads/x"},
		// An older build cloned branch origin/x and fetched branch x.
		"literal-origin": {"refs/heads/origin/x", "refs/heads/x"},
		// A row from before add-repo stored full refs: the clone took
		// branch origin/x.
		"short-origin": {"refs/heads/origin/x", "refs/heads/origin/x"},
		"adopted":      {"HEAD", ""},
		// A verified source keeps the ref an older build fetched and recorded
		// in its acquisition receipt.
		"verified-remotes": {"refs/remotes/origin/x", "refs/remotes/origin/x"},
		"verified-short":   {"refs/remotes/origin/x", "refs/heads/x"},
	} {
		got, err := store.GetRepo(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		if got.Branch != want[0] || got.FetchRef != want[1] {
			t.Errorf("%s: branch %q fetch ref %q, want %q %q", name, got.Branch, got.FetchRef, want[0], want[1])
		}
	}
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

func TestConcurrentFirstOpensBuildTheSchema(t *testing.T) {
	want := metatest.Schema(t, createRegistry(t))
	path := filepath.Join(t.TempDir(), "repos.sqlite")
	metatest.OpenConcurrently(t, 16, openRegistry(path))
	if got := metatest.Schema(t, path); !slices.Equal(got, want) {
		t.Fatalf("schema = %q, want %q", got, want)
	}
	if got := metatest.UserVersion(t, path); got != len(migrations) {
		t.Fatalf("user_version = %d, want %d", got, len(migrations))
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
	metatest.RequireNewerSchemaError(t, err, path)
}

// The fsmonitor hook reads its repo with ReadRepo, which must return what New
// and GetRepo return, from a current registry and from every earlier schema,
// which it migrates as New does.
func TestReadRepoReadsWhatNewReads(t *testing.T) {
	ctx := context.Background()
	want := metatest.Schema(t, createRegistry(t))
	schemas := map[string][]string{}
	for name, legacy := range legacyRegistries {
		schemas[name] = legacy.stmts
	}
	schemas["current"] = nil
	for name, stmts := range schemas {
		t.Run(name, func(t *testing.T) {
			var paths [2]string
			for i := range paths {
				paths[i] = filepath.Join(t.TempDir(), "repos.sqlite")
				if stmts == nil {
					store, err := New(ctx, paths[i])
					if err != nil {
						t.Fatal(err)
					}
					err = store.AddRepo(ctx, fullRepoWithRequests)
					if closeErr := store.Close(); err != nil || closeErr != nil {
						t.Fatalf("add repo: %v, close: %v", err, closeErr)
					}
					continue
				}
				metatest.Exec(t, paths[i], stmts...)
			}
			store, err := New(ctx, paths[1])
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			wantRepo, err := store.GetRepo(ctx, "repo")
			if err != nil {
				t.Fatal(err)
			}
			got, err := ReadRepo(ctx, paths[0], "repo")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, wantRepo) {
				t.Fatalf("ReadRepo = %+v, want %+v", got, wantRepo)
			}
			_, wantErr := store.GetRepo(ctx, "missing")
			if _, err := ReadRepo(ctx, paths[0], "missing"); wantErr == nil || err == nil || err.Error() != wantErr.Error() {
				t.Fatalf("ReadRepo of a missing repo: %v, want %v", err, wantErr)
			}
			if got := metatest.Schema(t, paths[0]); !slices.Equal(got, want) {
				t.Fatalf("schema after ReadRepo = %q, want %q", got, want)
			}
		})
	}
}

func TestReadRepoRefusesANewerSchema(t *testing.T) {
	path := createRegistry(t)
	metatest.Exec(t, path, `PRAGMA user_version = 99`)
	if _, err := ReadRepo(context.Background(), path, "repo"); err == nil {
		t.Fatal("ReadRepo on a newer schema succeeded")
	} else {
		metatest.RequireNewerSchemaError(t, err, path)
	}
}

// Git prints the fsmonitor hook's error on every command, so ReadRepo fails
// with the error New gives for the same registry.
func TestReadRepoFailsAsNewFails(t *testing.T) {
	for name, setup := range map[string]func(t *testing.T, path string){
		"corrupt file": func(t *testing.T, path string) {
			if err := os.WriteFile(path, bytes.Repeat([]byte("not a database "), 512), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"directory": func(t *testing.T, path string) {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		},
		"unreadable file": func(t *testing.T, path string) {
			if os.Geteuid() == 0 {
				t.Skip("root reads a file whatever its mode")
			}
			metatest.Exec(t, path)
			if err := os.Chmod(path, 0); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "repos.sqlite")
			setup(t, path)
			store, wantErr := New(context.Background(), path)
			if wantErr == nil {
				store.Close()
				t.Fatal("New succeeded")
			}
			if _, err := ReadRepo(context.Background(), path, "repo"); err == nil || err.Error() != wantErr.Error() {
				t.Fatalf("ReadRepo: %v, want New's %v", err, wantErr)
			}
		})
	}
}
