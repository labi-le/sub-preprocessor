package reload

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/rs/zerolog"

	"domains.lst/sub-preprocessor/internal/config"
)

// debounceInterval is the window over which a burst of filesystem events for
// the watched config file is coalesced into a single onChange call.
const debounceInterval = 200 * time.Millisecond

// watchRecheckInterval is how often Run verifies the directory watch is still
// registered. Deleting or moving the watched directory drops the inotify watch,
// and the backend's IN_IGNORED path emits no event at all, so without a
// periodic check a watcher that lost its directory would stay deaf forever.
const watchRecheckInterval = time.Second

// Watcher observes the directory containing a config file and invokes onChange
// (debounced) whenever the config file is created, written, renamed, or removed.
//
// It deliberately watches the parent DIRECTORY rather than the file itself:
// editors and tools such as `yq -i` write atomically (temp file + rename), which
// replaces the file's inode. A file-only watch is pinned to the old inode and
// goes silent after the first rename; a directory watch keeps firing.
type Watcher struct {
	fsw        *fsnotify.Watcher
	configPath string
	// watchDir is the parent directory NewWatcher registered the fsnotify watch
	// on; Run re-adds it when the watch is lost.
	watchDir string
	// overlayPaths are the overlay files config.Load merges beside configPath,
	// from config.OverlayFiles: a change to any of them must trigger a reload
	// just like a change to the main file.
	overlayPaths []string
	onChange     func(context.Context)
	logger       zerolog.Logger
	debounce     time.Duration
	// closeOnce keeps closeWatcher idempotent: Run's ctx arm and the
	// channel-closed arms can both reach a close of the same watcher.
	closeOnce sync.Once
}

// NewWatcher creates a Watcher for configPath. It registers a watch on the
// file's parent directory and remembers the cleaned config path for event
// filtering. The caller drives the lifecycle via Run.
func NewWatcher(configPath string, onChange func(context.Context), logger zerolog.Logger) (*Watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("create fsnotify watcher: %w", err)
	}

	cleaned := filepath.Clean(configPath)
	if addErr := fsw.Add(filepath.Dir(cleaned)); addErr != nil {
		_ = fsw.Close()
		return nil, fmt.Errorf("watch config directory: %w", addErr)
	}

	dir := filepath.Dir(cleaned)
	var overlayPaths []string
	for _, name := range config.OverlayFiles() {
		overlayPaths = append(overlayPaths, filepath.Join(dir, name))
	}
	return &Watcher{
		fsw:          fsw,
		configPath:   cleaned,
		watchDir:     dir,
		overlayPaths: overlayPaths,
		onChange:     onChange,
		logger:       logger,
		debounce:     debounceInterval,
	}, nil
}

// Run debounces matching config-file events and invokes onChange once per burst,
// logging (but not stopping on) fsnotify errors. It returns only after the
// fsnotify watcher is closed: on ctx cancellation, or when the event channels
// close on their own (the backend closes both in readEvents' defer). The latter
// is unexpected, so it is logged at error level and the watcher is closed on
// the way out, keeping the documented join-point contract and the inotify fd
// from leaking.
//
// The directory watch is the single point of failure for the whole watcher:
// replacing the directory (a bind mount re-created, rm -rf + restore) drops it
// from the inotify table, and the backend's IN_IGNORED path emits no event for
// us to re-arm on. Run therefore re-verifies the registration on a timer and
// re-adds a lost watch, logging the loss at error level and firing onChange
// once so the reload catches whatever changed while the watcher was deaf.
func (w *Watcher) Run(ctx context.Context) error {
	deb := &debouncer{interval: w.debounce}
	var timerC <-chan time.Time
	recheck := time.NewTicker(watchRecheckInterval)
	defer recheck.Stop()
	// deaf latches one lost-watch episode so the error is logged once, not on
	// every recheck tick while the directory stays gone.
	deaf := false

	for {
		select {
		case <-ctx.Done():
			deb.stop()
			w.closeWatcher()
			return nil

		case ev, ok := <-w.fsw.Events:
			if !ok {
				w.shutdownOnChannelClose(deb, "event")
				return nil
			}
			if w.isConfigActivity(ev) {
				timerC = deb.reset()
			}

		case err, ok := <-w.fsw.Errors:
			if !ok {
				w.shutdownOnChannelClose(deb, "error")
				return nil
			}
			w.logger.Error().Err(err).Msg("reload: fsnotify watcher error")

		case <-timerC:
			timerC = nil
			w.onChange(ctx)

		case <-recheck.C:
			if w.recheckWatch(&deaf) {
				w.onChange(ctx)
			}
		}
	}
}

// isConfigActivity reports whether ev announces a config change worth
// debouncing: a create/write/rename/remove on the config file or an overlay,
// or a remove/rename of the watched directory itself (fsnotify drops it on
// IN_DELETE_SELF / IN_MOVE_SELF), which debounces so the reload fires once the
// directory is back.
func (w *Watcher) isConfigActivity(ev fsnotify.Event) bool {
	return w.matches(ev) ||
		(ev.Name == w.watchDir && ev.Op&(fsnotify.Remove|fsnotify.Rename) != 0)
}

// shutdownOnChannelClose tears Run down after one of the fsnotify channels
// closed on its own (readEvents closes both and returns on a fatal read error,
// a short read, or a handleEvent failure -- none of which is our Close). The
// watcher is closed here, keeping the documented join-point contract and the
// inotify fd from leaking.
func (w *Watcher) shutdownOnChannelClose(deb *debouncer, channel string) {
	w.logger.Error().Msg("reload: fsnotify " + channel + " channel closed; config hot reload is no longer possible")
	deb.stop()
	w.closeWatcher()
}

// recheckWatch re-verifies the directory watch on a periodic tick and reports
// whether a lost watch was re-added, in which case Run fires onChange once so
// the reload catches whatever changed while the watcher was deaf.
func (w *Watcher) recheckWatch(deaf *bool) bool {
	if w.watchRegistered() {
		return false
	}
	if !*deaf {
		*deaf = true
		w.logger.Error().Str("dir", w.watchDir).
			Msg("reload: lost the config directory watch; hot reload is deaf until the directory is back and the watch is re-added")
	}
	if err := w.fsw.Add(w.watchDir); err != nil {
		return false // the directory is still gone; the next tick retries
	}
	*deaf = false
	return true
}

// watchRegistered reports whether the fsnotify watcher still holds the config
// directory watch. fsnotify drops it when the directory is deleted, renamed or
// unmounted, so absence means the event loop is deaf.
func (w *Watcher) watchRegistered() bool {
	return slices.Contains(w.fsw.WatchList(), w.watchDir)
}

// closeWatcher closes the underlying fsnotify watcher, logging (but not
// returning) any close error, matching Run's non-fatal shutdown contract. It is
// safe to call more than once: fsnotify.Close is not, so the first call wins.
func (w *Watcher) closeWatcher() {
	w.closeOnce.Do(func() {
		if err := w.fsw.Close(); err != nil {
			w.logger.Error().Err(err).Msg("reload: close fsnotify watcher")
		}
	})
}

// debouncer coalesces a burst of events into a single fire on its channel after
// interval elapses with no further reset. It is not safe for concurrent use;
// Run drives it from a single goroutine.
type debouncer struct {
	timer    *time.Timer
	interval time.Duration
}

// reset (re)starts the debounce window and returns the channel that fires once
// the window elapses. It drains a stale value from an already-fired timer so a
// reset never leaves a spurious tick queued.
func (d *debouncer) reset() <-chan time.Time {
	if d.timer == nil {
		d.timer = time.NewTimer(d.interval)
	} else {
		if !d.timer.Stop() {
			// Drain a value that already fired but was not yet consumed.
			select {
			case <-d.timer.C:
			default:
			}
		}
		d.timer.Reset(d.interval)
	}
	return d.timer.C
}

// stop halts the debounce timer if one is running.
func (d *debouncer) stop() {
	if d.timer != nil {
		d.timer.Stop()
	}
}

// matches reports whether ev is a create/write/rename/remove on the watched
// config file or one of its overlay siblings. Chmod-only events are ignored.
func (w *Watcher) matches(ev fsnotify.Event) bool {
	name := filepath.Clean(ev.Name)
	if name != w.configPath && !slices.Contains(w.overlayPaths, name) {
		return false
	}
	return ev.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Rename|fsnotify.Remove) != 0
}
