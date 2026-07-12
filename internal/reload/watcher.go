package reload

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/rs/zerolog"
)

// debounceInterval is the window over which a burst of filesystem events for
// the watched config file is coalesced into a single onChange call.
const debounceInterval = 200 * time.Millisecond

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
	// privatePath is the optional `private.yaml` overlay sibling of configPath.
	// config.Load merges it into the effective config, so a change to it must
	// trigger a reload just like a change to the main file.
	privatePath string
	onChange    func(context.Context)
	logger      zerolog.Logger
	debounce    time.Duration
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
	if err := fsw.Add(filepath.Dir(cleaned)); err != nil {
		_ = fsw.Close()
		return nil, fmt.Errorf("watch config directory: %w", err)
	}

	return &Watcher{
		fsw:         fsw,
		configPath:  cleaned,
		privatePath: filepath.Join(filepath.Dir(cleaned), "private.yaml"),
		onChange:    onChange,
		logger:      logger,
		debounce:    debounceInterval,
	}, nil
}

// Run debounces matching config-file events and invokes onChange once per burst,
// logging (but not stopping on) fsnotify errors. On ctx cancellation it closes
// the fsnotify watcher and returns nil; it returns ONLY after Close, so callers
// may treat the return as the goroutine join point.
func (w *Watcher) Run(ctx context.Context) error {
	var (
		timer  *time.Timer
		timerC <-chan time.Time
	)

	resetDebounce := func() {
		if timer == nil {
			timer = time.NewTimer(w.debounce)
		} else {
			if !timer.Stop() {
				// Drain a value that already fired but was not yet consumed.
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(w.debounce)
		}
		timerC = timer.C
	}

	for {
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			if err := w.fsw.Close(); err != nil {
				w.logger.Error().Err(err).Msg("reload: close fsnotify watcher")
			}
			return nil

		case ev, ok := <-w.fsw.Events:
			if !ok {
				return nil
			}
			if w.matches(ev) {
				resetDebounce()
			}

		case err, ok := <-w.fsw.Errors:
			if !ok {
				return nil
			}
			w.logger.Error().Err(err).Msg("reload: fsnotify watcher error")

		case <-timerC:
			timerC = nil
			w.onChange(ctx)
		}
	}
}

// matches reports whether ev is a create/write/rename/remove on the watched
// config file or its private.yaml overlay sibling. Chmod-only events are ignored.
func (w *Watcher) matches(ev fsnotify.Event) bool {
	name := filepath.Clean(ev.Name)
	if name != w.configPath && name != w.privatePath {
		return false
	}
	return ev.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Rename|fsnotify.Remove) != 0
}
