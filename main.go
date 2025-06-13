package main

import (
	"context"
	"flag"
	"io"
	"os"

	log "github.com/golang/glog"
)

func main() {
	flag.Parse()
	ctx := context.Background()
	log.Info("starting up an reading from stdin")
	if err := doMain(ctx, os.Stdin); err != nil {
		log.Exitf("failed to run: %v", err)
	}
}

func doMain(ctx context.Context, input io.Reader) error {
	return nil
}
