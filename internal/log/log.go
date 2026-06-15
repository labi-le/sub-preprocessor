package log

import (
	"fmt"
	"os"
	"time"

	"github.com/rs/zerolog"
	zlog "github.com/rs/zerolog/log"
)

func shortCaller(_ uintptr, file string, line int) string {
	short := file
	for i := len(file) - 1; i > 0; i-- {
		if file[i] == '/' {
			short = file[i+1:]
			break
		}
	}
	return fmt.Sprintf("%s:%d", short, line)
}

// InitDefault sets up the global zerolog.Logger with a console writer, timestamps,
// and caller info at info level. Called early in main() so that any code importing
// the log package (or zerolog/log) can log before config is loaded.
func InitDefault() {
	_ = InitLogger("info")
}

// InitLogger overrides the global logger level with the configured value
// and returns the resulting logger. This should be called after config is loaded.
//
//nolint:reassign // zerolog intentionally exposes global state for logger configuration
func InitLogger(level string) zerolog.Logger {
	lvl, err := zerolog.ParseLevel(level)
	if err != nil {
		lvl = zerolog.InfoLevel
	}

	out := zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339}
	zerolog.CallerMarshalFunc = shortCaller
	logger := zerolog.New(out).
		Level(lvl).
		With().
		Timestamp().
		Caller().
		Logger()

	zlog.Logger = logger
	return logger
}
