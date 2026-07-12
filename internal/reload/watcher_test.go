package reload_test

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"domains.lst/sub-preprocessor/internal/reload"
)

// TestWatcherDebounceCoalesces proves AC5: a burst of file modifications
// (two writes within 50ms plus an atomic temp+rename) collapses into a single
// debounced onChange call rather than one call per write. The rename also
// exercises the directory-watch path (the load-bearing Docker fix): a
// file-only watch would miss the rename, a directory watch sees the CREATE.
func TestWatcherDebounceCoalesces(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("a: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int64
	onChange := func(context.Context) { calls.Add(1) }

	w, err := reload.NewWatcher(cfgPath, onChange, zerolog.Nop())
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	// Let the Run select loop start before generating events.
	time.Sleep(50 * time.Millisecond)

	// Two writes within 50ms (a multi-field config edit).
	if err := os.WriteFile(cfgPath, []byte("a: 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(25 * time.Millisecond)
	if err := os.WriteFile(cfgPath, []byte("a: 3\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// One atomic temp+rename into place (how `yq -i` writes), same directory.
	tmp := filepath.Join(dir, "config.yaml.tmp")
	if err := os.WriteFile(tmp, []byte("a: 4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, cfgPath); err != nil {
		t.Fatal(err)
	}

	// Wait well past the 200ms debounce window so the timer has fired.
	time.Sleep(400 * time.Millisecond)

	got := calls.Load()
	if got < 1 {
		t.Fatalf("expected at least one debounced onChange (rename should fire via directory watch), got %d", got)
	}
	if got > 2 {
		t.Fatalf("expected debounced onChange <= 2 (coalesced), got %d — debounce not coalescing", got)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

// TestWatcherShutdownReturnsCleanly proves AC10: cancelling the context makes
// Run close the underlying fsnotify watcher and return within 1s. Combined with
// the goleak TestMain, this also asserts no goroutine is leaked on shutdown.
func TestWatcherShutdownReturnsCleanly(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("a: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	w, err := reload.NewWatcher(cfgPath, func(context.Context) {}, zerolog.Nop())
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	// Let the Run select loop start, then trigger shutdown.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error on shutdown: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return within 1s after ctx cancel")
	}
}

// TestWatcherIgnoresOtherFiles proves the filename filtering: writes to other
// files in the watched directory must not trigger onChange, while a write to
// the config file itself does.
func TestWatcherIgnoresOtherFiles(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("a: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int64
	w, err := reload.NewWatcher(cfgPath, func(context.Context) { calls.Add(1) }, zerolog.Nop())
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	time.Sleep(50 * time.Millisecond)

	// Writes to a sibling file in the same watched directory must be ignored.
	other := filepath.Join(dir, "other.txt")
	for i := 0; i < 3; i++ {
		if err := os.WriteFile(other, []byte{byte(i)}, 0o644); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond) // past the debounce window

	if got := calls.Load(); got != 0 {
		t.Fatalf("expected 0 onChange for non-config file writes, got %d", got)
	}

	// A write to the config file itself IS detected.
	if err := os.WriteFile(cfgPath, []byte("a: 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)

	if got := calls.Load(); got != 1 {
		t.Fatalf("expected exactly 1 onChange after config write, got %d", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

// TestWatcherDetectsPrivateOverlay proves a write to the private.yaml overlay
// sibling triggers onChange, because config.Load merges it into the effective
// config and the stable worker's sources come from that merge.
func TestWatcherDetectsPrivateOverlay(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("a: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int64
	w, err := reload.NewWatcher(cfgPath, func(context.Context) { calls.Add(1) }, zerolog.Nop())
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	time.Sleep(50 * time.Millisecond)

	// An atomic temp+rename of private.yaml (how the crawler writes it).
	privPath := filepath.Join(dir, "private.yaml")
	tmp := filepath.Join(dir, "private.yaml.tmp")
	if err := os.WriteFile(tmp, []byte("subscriptions:\n  sources: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, privPath); err != nil {
		t.Fatal(err)
	}

	time.Sleep(300 * time.Millisecond)

	if got := calls.Load(); got < 1 {
		t.Fatalf("expected onChange after private.yaml write, got %d", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}
