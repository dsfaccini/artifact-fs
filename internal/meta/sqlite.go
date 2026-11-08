package meta

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

const busyTimeout = 5 * time.Second

func OpenDB(path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("mkdir db dir: %w", err)
	}
	params := url.Values{}
	params.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyTimeout.Milliseconds()))
	params.Add("_pragma", "foreign_keys(ON)")
	// In WAL mode NORMAL syncs the WAL only at checkpoints. A process crash
	// loses no commit; an OS crash can lose the commits since the last sync.
	params.Add("_pragma", "synchronous(NORMAL)")
	// A transaction takes the write lock at BEGIN, so another connection's
	// write waits out busy_timeout. SQLite fails a read transaction's upgrade
	// to a write at once with SQLITE_BUSY, without the busy handler. A
	// transaction begun with sql.TxOptions{ReadOnly: true} stays deferred.
	params.Add("_txlock", "immediate")
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: params.Encode()}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// Switching a new database to WAL upgrades a read lock to a write lock.
	// SQLite fails an upgrade with SQLITE_BUSY at once, without the busy
	// handler, so concurrent first opens retry it until busy_timeout.
	for deadline := time.Now().Add(busyTimeout); ; time.Sleep(10 * time.Millisecond) {
		_, err = db.Exec(`PRAGMA journal_mode=WAL;`)
		if err == nil {
			return db, nil
		}
		var sqliteErr *sqlite.Error
		if !errors.As(err, &sqliteErr) || sqliteErr.Code()&0xff != sqlite3.SQLITE_BUSY || time.Now().After(deadline) {
			db.Close()
			return nil, err
		}
	}
}

// A Migration changes the schema inside the transaction Migrate holds on
// conn. It must not begin, commit or roll back a transaction.
type Migration func(ctx context.Context, conn *sql.Conn) error

// SQL returns a Migration that runs stmts in order.
func SQL(stmts ...string) Migration {
	return func(ctx context.Context, conn *sql.Conn) error {
		for _, stmt := range stmts {
			if _, err := conn.ExecContext(ctx, stmt); err != nil {
				return err
			}
		}
		return nil
	}
}

// Migrate applies the migrations db lacks. PRAGMA user_version counts the
// migrations applied, so migrations[i] runs only while it is i. Append a
// migration to change a schema; never edit one that has shipped.
//
// Each migration runs in its own transaction, which takes the write lock
// before it reads: a deferred transaction that reads and then writes fails at
// once with SQLITE_BUSY while another connection writes, where BEGIN
// IMMEDIATE waits out busy_timeout. The transaction reads user_version again,
// so concurrent first opens apply each migration once. A current database
// costs one read and takes no lock.
//
// On an error the caller must close db: a connection whose ROLLBACK failed
// stays inside the transaction. The error names no database, so the caller
// wraps it with the path.
func Migrate(ctx context.Context, db *sql.DB, migrations []Migration) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	version, err := userVersion(ctx, conn, len(migrations))
	for err == nil && version < len(migrations) {
		version, err = migrateNext(ctx, conn, migrations)
	}
	return err
}

// testHookBeforeBegin, when set, runs in a migration step before it takes the
// write lock. Keep the call before BEGIN IMMEDIATE: the test catches a call
// that no longer runs, not a moved one.
var testHookBeforeBegin func()

// migrateNext applies the next migration, if any, and returns the version
// the database reached.
func migrateNext(ctx context.Context, conn *sql.Conn, migrations []Migration) (version int, err error) {
	if testHookBeforeBegin != nil {
		testHookBeforeBegin()
	}
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return 0, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.WithoutCancel(ctx), `ROLLBACK`)
		}
	}()
	version, err = userVersion(ctx, conn, len(migrations))
	if err != nil || version == len(migrations) {
		return version, err
	}
	if err := migrations[version](ctx, conn); err != nil {
		return 0, fmt.Errorf("migration %d: %w", version+1, err)
	}
	version++
	// PRAGMA takes no bound parameters.
	if _, err := conn.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, version)); err != nil {
		return 0, err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return 0, err
	}
	committed = true
	return version, nil
}

func userVersion(ctx context.Context, conn *sql.Conn, latest int) (int, error) {
	var version int
	if err := conn.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return 0, err
	}
	if version > latest {
		return 0, fmt.Errorf("schema version %d is newer than this build's %d: a newer artifact-fs wrote the database; upgrade artifact-fs to open it", version, latest)
	}
	return version, nil
}

// Columns returns the names of table's columns, for a migration that probes
// a schema older builds left behind.
func Columns(ctx context.Context, conn *sql.Conn, table string) (map[string]bool, error) {
	rows, err := conn.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		cols[name] = true
	}
	return cols, rows.Err()
}
