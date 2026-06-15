package main

import (
	"context"
	"os/signal"
	"syscall"

	"domains.lst/sub-preprocessor/internal/app"
	"domains.lst/sub-preprocessor/internal/log"
	zlog "github.com/rs/zerolog/log"
)

func main() {
	log.InitDefault()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)

	if err := app.Run(ctx); err != nil {
		stop()
		zlog.Fatal().Err(err).Msg("")
	}
	stop()
}
