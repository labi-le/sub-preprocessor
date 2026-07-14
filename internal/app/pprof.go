//go:build pprof

package app

import (
	"net/http"
	"os"
	"time"

	_ "net/http/pprof" // registers /debug/pprof handlers on DefaultServeMux

	zlog "github.com/rs/zerolog/log"
)

func init() {
	if addr := os.Getenv("PPROF_ADDR"); addr != "" {
		go func() {
			zlog.Info().Str("addr", addr).Msg("pprof listening")
			srv := &http.Server{Addr: addr, ReadHeaderTimeout: 5 * time.Second}
			if err := srv.ListenAndServe(); err != nil {
				zlog.Error().Err(err).Msg("pprof error")
			}
		}()
	}
}
