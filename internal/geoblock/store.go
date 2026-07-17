// Package geoblock persists a blocklist of proxy node hosts that failed the
// Gemini reachability check, each with an expiry (TTL). Reads are served from
// an in-memory cache (the filter hot path); the SQLite file is touched only on
// writes, prune, and startup load, so it survives restarts.
package geoblock

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, registered as "sqlite" (works with CGO_ENABLED=0)
)

// Store is a TTL blocklist keyed by node host (server), backed by SQLite.
type Store struct {
	db  *sql.DB
	ttl time.Duration

	mu      sync.RWMutex
	blocked map[string]int64 // host -> unix expiry
}

// Open opens (creating if needed) the SQLite blocklist at path, loads the
// non-expired entries into memory and prunes expired ones.
func Open(path string, ttl time.Duration) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open geoblock db: %w", err)
	}
	// Reads hit the in-memory cache, so a single connection avoids lock
	// contention between the occasional Block/Prune writes.
	db.SetMaxOpenConns(1)
	if _, pragmaErr := db.ExecContext(context.Background(), `PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;`); pragmaErr != nil {
		_ = db.Close()
		return nil, fmt.Errorf("geoblock pragmas: %w", pragmaErr)
	}
	if _, schemaErr := db.ExecContext(context.Background(), `CREATE TABLE IF NOT EXISTS geoblock (host TEXT PRIMARY KEY, blocked_until INTEGER NOT NULL)`); schemaErr != nil {
		_ = db.Close()
		return nil, fmt.Errorf("geoblock schema: %w", schemaErr)
	}

	s := &Store{db: db, ttl: ttl, blocked: make(map[string]int64)}
	if loadErr := s.load(); loadErr != nil {
		_ = db.Close()
		return nil, loadErr
	}
	return s, nil
}

func (s *Store) load() error {
	now := time.Now().UnixNano()
	if _, pruneErr := s.db.ExecContext(context.Background(), `DELETE FROM geoblock WHERE blocked_until <= ?`, now); pruneErr != nil {
		return fmt.Errorf("geoblock prune on load: %w", pruneErr)
	}
	rows, err := s.db.QueryContext(context.Background(), `SELECT host, blocked_until FROM geoblock WHERE blocked_until > ?`, now)
	if err != nil {
		return fmt.Errorf("geoblock load: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var host string
		var exp int64
		if scanErr := rows.Scan(&host, &exp); scanErr != nil {
			return fmt.Errorf("geoblock scan: %w", scanErr)
		}
		s.blocked[host] = exp
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return fmt.Errorf("geoblock rows: %w", rowsErr)
	}
	return nil
}

// Blocked reports whether host is currently blocked (present and not expired).
func (s *Store) Blocked(host string) bool {
	if host == "" {
		return false
	}
	s.mu.RLock()
	exp, ok := s.blocked[host]
	s.mu.RUnlock()
	return ok && exp > time.Now().UnixNano()
}

// Block records host as blocked until now+ttl (upsert; refreshes the expiry).
func (s *Store) Block(host string) error {
	if host == "" {
		return nil
	}
	exp := time.Now().Add(s.ttl).UnixNano()
	s.mu.Lock()
	s.blocked[host] = exp
	s.mu.Unlock()
	_, err := s.db.ExecContext(
		context.Background(),
		`INSERT INTO geoblock(host, blocked_until) VALUES(?, ?) ON CONFLICT(host) DO UPDATE SET blocked_until=excluded.blocked_until`,
		host, exp,
	)
	if err != nil {
		return fmt.Errorf("geoblock write %q: %w", host, err)
	}
	return nil
}

// Prune drops expired entries from memory and the database.
func (s *Store) Prune() error {
	now := time.Now().UnixNano()
	s.mu.Lock()
	for h, e := range s.blocked {
		if e <= now {
			delete(s.blocked, h)
		}
	}
	s.mu.Unlock()
	if _, err := s.db.ExecContext(context.Background(), `DELETE FROM geoblock WHERE blocked_until <= ?`, now); err != nil {
		return fmt.Errorf("geoblock prune: %w", err)
	}
	return nil
}

// Count returns the number of currently cached (non-expired at load) entries.
func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.blocked)
}

func (s *Store) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("geoblock close: %w", err)
	}
	return nil
}
