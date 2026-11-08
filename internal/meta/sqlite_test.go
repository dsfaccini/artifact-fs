package meta

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
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
