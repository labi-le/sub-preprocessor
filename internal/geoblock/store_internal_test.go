package geoblock

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TestBusyTimeoutAppliesToFreshConnections pins the connection scoping of the
// pragma setup: busy_timeout is per-connection state, so applying it once to
// the first pooled connection (a bare ExecContext) is silently lost when
// database/sql retires that connection and opens a replacement, and a write
// contending with another writer on the shared db file then fails instantly
// instead of waiting 5s. The pragmas ride in the DSN, which the driver applies
// on every connection it opens. The first connection is pinned out of the pool
// so the read below forces a fresh one: against the one-off-ExecContext bug
// the replacement reports 0.
func TestBusyTimeoutAppliesToFreshConnections(t *testing.T) {
	t.Parallel()

	s, err := Open(filepath.Join(t.TempDir(), "gb.db"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	s.db.SetMaxOpenConns(2)
	pinned, err := s.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pinned.Close() }()

	var busy int
	if scanErr := s.db.QueryRowContext(context.Background(), `PRAGMA busy_timeout`).Scan(&busy); scanErr != nil {
		t.Fatal(scanErr)
	}
	if busy != 5000 {
		t.Fatalf("busy_timeout must be 5000ms on a freshly opened connection, got %d", busy)
	}
}
