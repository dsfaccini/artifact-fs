package meta

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestOpenDBConfiguresEveryConnection(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(2)

	ctx := context.Background()
	first, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	for _, conn := range []*sql.Conn{first, second} {
		var busyTimeout int
		if err := conn.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
			t.Fatal(err)
		}
		if busyTimeout != 5000 {
			t.Fatalf("busy_timeout = %d, want 5000", busyTimeout)
		}
		var foreignKeys int
		if err := conn.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
			t.Fatal(err)
		}
		if foreignKeys != 1 {
			t.Fatalf("foreign_keys = %d, want 1", foreignKeys)
		}
	}
}

// A walk by 16 threads keeps up to maxIdleConns queries in flight per
// database. The next burst of queries runs on the same connections, so no
// query pays for a new connection's pragmas and schema read. A temporary
// table lives only on the connection that created it, so it marks a
// connection that the pool kept.
func TestOpenDBKeepsTheConnectionsOfConcurrentQueries(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ctx := t.Context()
	burst := func(query func(*sql.Conn) error) {
		t.Helper()
		conns := make([]*sql.Conn, maxIdleConns)
		for i := range conns {
			if conns[i], err = db.Conn(ctx); err != nil {
				t.Fatal(err)
			}
		}
		for _, conn := range conns {
			if err := query(conn); err != nil {
				t.Fatal(err)
			}
			if err := conn.Close(); err != nil {
				t.Fatal(err)
			}
		}
	}
	burst(func(conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `CREATE TEMP TABLE kept (x)`)
		return err
	})
	kept := 0
	burst(func(conn *sql.Conn) error {
		var n int
		err := conn.QueryRowContext(ctx, `SELECT count(*) FROM temp.sqlite_master WHERE name = 'kept'`).Scan(&n)
		kept += n
		return err
	})
	if kept != maxIdleConns {
		t.Fatalf("the second burst ran on %d kept connections of %d", kept, maxIdleConns)
	}
}

// A connection that runs no query for connMaxIdleTime closes, so an idle
// daemon gives back each connection's page cache. database/sql's cleaner runs
// on a timer, so the test drives it with synctest's clock.
func TestOpenDBClosesIdleConnections(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		db, err := OpenDB(filepath.Join(t.TempDir(), "meta.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()

		ctx := t.Context()
		conns := make([]*sql.Conn, 3)
		for i := range conns {
			if conns[i], err = db.Conn(ctx); err != nil {
				t.Fatal(err)
			}
		}
		for _, conn := range conns {
			if err := conn.Close(); err != nil {
				t.Fatal(err)
			}
		}
		time.Sleep(connMaxIdleTime - time.Second)
		synctest.Wait()
		if open := db.Stats().OpenConnections; open != len(conns) {
			t.Fatalf("%d connections open before connMaxIdleTime passed, want %d", open, len(conns))
		}
		time.Sleep(connMaxIdleTime + time.Second)
		synctest.Wait()
		stats := db.Stats()
		if stats.OpenConnections != 0 || stats.MaxIdleTimeClosed != int64(len(conns)) {
			t.Fatalf("%d connections open and %d closed for idle time, want 0 and %d", stats.OpenConnections, stats.MaxIdleTimeClosed, len(conns))
		}
	})
}

// A first open switches the database to WAL. While a connection in rollback
// journal mode holds a lock, the switch fails with SQLITE_BUSY at once, and
// OpenDB retries it.
func TestOpenDBRetriesTheWALSwitch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meta.sqlite")
	holder, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path}).String())
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	ctx := context.Background()
	conn, err := holder.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	retried := false
	testHookAfterWALSwitchBusy = func() {
		if retried {
			return
		}
		retried = true
		if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
			t.Errorf("commit the holder: %v", err)
		}
	}
	defer func() { testHookAfterWALSwitchBusy = nil }()

	db, err := OpenDB(path)
	if err != nil {
		t.Fatalf("OpenDB while another connection held a lock: %v", err)
	}
	defer db.Close()
	if !retried {
		t.Fatal("the WAL switch succeeded while another connection held a lock")
	}
	var mode string
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", mode)
	}
}

// Migrate reads user_version before it takes the write lock. A migration that
// another process applies in between must not run again.
func TestMigrateAppliesEachMigrationOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meta.sqlite")
	var processes [2]*sql.DB
	for i := range processes {
		db, err := OpenDB(path)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		processes[i] = db
	}
	var calls [2]int
	migrations := []Migration{
		func(context.Context, *sql.Conn) error { calls[0]++; return nil },
		func(context.Context, *sql.Conn) error { calls[1]++; return nil },
	}
	ctx := context.Background()
	raced := false
	testHookBeforeBegin = func() {
		if raced {
			return
		}
		raced = true
		if err := Migrate(ctx, processes[1], migrations); err != nil {
			t.Errorf("concurrent Migrate: %v", err)
		}
	}
	defer func() { testHookBeforeBegin = nil }()

	if err := Migrate(ctx, processes[0], migrations); err != nil {
		t.Fatal(err)
	}
	if !raced {
		t.Fatal("Migrate took the write lock without running the hook")
	}
	if calls != [2]int{1, 1} {
		t.Fatalf("migration calls = %v, want each applied once", calls)
	}
	var version int
	if err := processes[0].QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != len(migrations) {
		t.Fatalf("user_version = %d, want %d", version, len(migrations))
	}
}

// OpenForReading sets busy_timeout on every connection and writes nothing:
// OpenDB's switch to WAL is a write.
func TestOpenForReadingWaitsOutLocksAndKeepsTheJournalMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meta.sqlite")
	rollback, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path}).String())
	if err != nil {
		t.Fatal(err)
	}
	defer rollback.Close()
	if _, err := rollback.Exec(`CREATE TABLE t (x)`); err != nil {
		t.Fatal(err)
	}
	db, err := OpenForReading(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(2)

	ctx := context.Background()
	first, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	for _, conn := range []*sql.Conn{first, second} {
		var busyTimeout int
		if err := conn.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
			t.Fatal(err)
		}
		if busyTimeout != 5000 {
			t.Fatalf("busy_timeout = %d, want 5000", busyTimeout)
		}
		var mode string
		if err := conn.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil {
			t.Fatal(err)
		}
		if mode != "delete" {
			t.Fatalf("journal_mode = %q, want the database's delete", mode)
		}
	}
}

func TestMigratedComparesUserVersionWithTheMigrations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meta.sqlite")
	db, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	migrations := []Migration{SQL(`CREATE TABLE a (x)`), SQL(`CREATE TABLE b (x)`)}
	for version, want := range []bool{false, false, true} {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, version)); err != nil {
			t.Fatal(err)
		}
		got, err := Migrated(ctx, db, migrations)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("Migrated at user_version %d = %v, want %v", version, got, want)
		}
	}
	if _, err := db.ExecContext(ctx, `PRAGMA user_version = 3`); err != nil {
		t.Fatal(err)
	}
	if _, err := Migrated(ctx, db, migrations); err == nil || !strings.Contains(err.Error(), "newer than this build's 2") {
		t.Fatalf("Migrated on a newer schema: %v, want the error Migrate returns", err)
	}
}
