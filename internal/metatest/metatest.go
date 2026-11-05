// Package metatest holds helpers for tests of the SQLite stores' schemas.
package metatest

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/meta"
)

// Exec runs stmts on the database at path, as an older build would have.
func Exec(t testing.TB, path string, stmts ...string) {
	t.Helper()
	db, err := meta.OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
}

// Schema describes the database at path: each table's columns, and each
// index's and trigger's SQL, sorted. Column order and whitespace do not count,
// because ALTER TABLE ADD COLUMN appends a column and older builds indented
// their SQL differently.
func Schema(t testing.TB, path string) []string {
	t.Helper()
	db, err := meta.OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`
		SELECT 'table ' || m.name || ' column ' || c.name || ' ' || c.type || ' notnull=' || c."notnull" || ' default=' || COALESCE(c.dflt_value, 'NULL') || ' pk=' || c.pk
		FROM sqlite_master AS m, pragma_table_info(m.name) AS c
		WHERE m.type = 'table' AND m.name NOT LIKE 'sqlite_%'
		UNION ALL
		SELECT type || ' ' || name || ' ' || sql FROM sqlite_master WHERE type IN ('index', 'trigger') AND sql IS NOT NULL`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var schema []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		schema = append(schema, strings.Join(strings.Fields(line), " "))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	slices.Sort(schema)
	return schema
}

// UserVersion reads PRAGMA user_version of the database at path.
func UserVersion(t testing.TB, path string) int {
	t.Helper()
	db, err := meta.OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	return version
}

// OpenWhileLocked calls open while another connection holds the write lock on
// the database at path, as a store's write does, and fails the test if open
// fails.
//
// The other connection commits after a fixed hold, because nothing marks a
// connection that waits in SQLite's busy handler. The hold is far below
// busy_timeout, so it cannot make an open that waits fail. An open that does
// not wait fails before the commit, unless the machine delays it past the
// hold; then the test passes without proof.
func OpenWhileLocked(t testing.TB, path string, open func() (io.Closer, error)) {
	t.Helper()
	ctx := context.Background()
	holder, err := meta.OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	conn, err := holder.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	committed := make(chan error, 1)
	time.AfterFunc(200*time.Millisecond, func() {
		_, err := conn.ExecContext(ctx, `COMMIT`)
		committed <- err
	})
	store, err := open()
	if err != nil {
		t.Fatalf("open while another connection held the write lock: %v", err)
	}
	store.Close()
	if err := <-committed; err != nil {
		t.Fatal(err)
	}
}

// OpenConcurrently calls open n times at once, as processes that start
// together do, and fails the test if any call fails.
func OpenConcurrently(t testing.TB, n int, open func() (io.Closer, error)) {
	t.Helper()
	start := make(chan struct{})
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			<-start
			store, err := open()
			if err != nil {
				errs <- fmt.Errorf("open %d: %w", i, err)
				return
			}
			store.Close()
		})
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
