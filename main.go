package main

import (
	"context"
	"fmt"
	"os"

	"domains.lst/sub-preprocessor/internal/app"
)

func main() {
	if err := app.Run(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
