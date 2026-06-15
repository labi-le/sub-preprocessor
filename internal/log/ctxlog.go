package log

import "github.com/rs/zerolog"

// Op returns a child logger with the "op" field set to the given operation name.
// Use this to create contextual loggers scoped to a specific function/operation.
func Op(logger zerolog.Logger, op string) zerolog.Logger {
	return logger.With().Str("op", op).Logger()
}
