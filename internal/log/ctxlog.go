package log

import "github.com/rs/zerolog"

// Op returns a child logger with the "op" field set to the given operation name.
func Op(logger zerolog.Logger, op string) zerolog.Logger {
	return logger.With().Str("op", op).Logger()
}
