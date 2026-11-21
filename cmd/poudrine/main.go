package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/inkeliz/poudrine"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := poudrine.Run(ctx, os.Args[1:]); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, flag.ErrHelp) {
		log.Fatal(err)
	}
}
