package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
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
	scanner := bufio.NewScanner(input)

	for scanner.Scan() {
		txt := scanner.Text()

		log.V(5).InfoContextf(ctx, "got content: %s", txt)
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("got error from scanner: %w", err)
	}

	return nil
}
