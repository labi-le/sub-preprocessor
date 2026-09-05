package reload

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// TestRunReturnsAndLogsWhenEventsCloseUnexpectedly covers the channel-closed
// arm of Run: fsnotify's readEvents closes both channels in a defer and returns
// on a fatal read error, a short read, or a handleEvent failure -- none of
// which is our Close. Run must log the unexpected closure, close the watcher
// itself and return, instead of leaking the inotify fd and returning silently
// as if shutdown had been requested. White-box because the only way to drive
// that teardown is closing the fsnotify watcher while ctx is still live.
func TestRunReturnsAndLogsWhenEventsCloseUnexpectedly(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("a: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var logBuf strings.Builder
	w, err := NewWatcher(cfgPath, func(context.Context) {}, zerolog.New(&logBuf))
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	time.Sleep(50 * time.Millisecond) // let the Run select loop start

	if closeErr := w.fsw.Close(); closeErr != nil {
		t.Fatalf("close fsnotify watcher: %v", closeErr)
	}

	select {
	case runErr := <-done:
		if runErr != nil {
			t.Fatalf("Run returned error: %v", runErr)
		}
	case <-time.After(time.Second):
		t.Fatal("Run must return when the event channels close on their own")
	}
	if logs := logBuf.String(); !strings.Contains(logs, "channel closed") {
		t.Fatalf("expected an error log for the unexpected closure, got:\n%s", logs)
	}
}
