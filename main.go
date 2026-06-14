package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"domains.lst/sub-preprocessor/internal/app"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)

	if err := app.Run(ctx); err != nil {
		stop()
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	stop()
}
