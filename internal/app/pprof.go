//go:build pprof

package app

import (
	"fmt"
	"net/http"
	"os"

	_ "net/http/pprof" // registers /debug/pprof handlers on DefaultServeMux
)

func init() {
	if addr := os.Getenv("PPROF_ADDR"); addr != "" {
		go func() {
			fmt.Fprintln(os.Stderr, "pprof listening on", addr)
			if err := http.ListenAndServe(addr, nil); err != nil {
				fmt.Fprintln(os.Stderr, "pprof error:", err)
			}
		}()
	}
}
