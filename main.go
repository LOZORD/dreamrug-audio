package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

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

type sensorPayload struct {
	FSRReading    int32 `json:"fsr"`
	LEDBrightness int32 `json:"led"`
	UptimeMillis  int64 `json:"upt"`
}

func doMain(ctx context.Context, input io.Reader) error {
	scanner := bufio.NewScanner(input)

	for scanner.Scan() {
		txt := scanner.Text()

		log.V(5).InfoContextf(ctx, "got content: %s", txt)

		var pld sensorPayload
		if err := json.Unmarshal([]byte(txt), &pld); err != nil {
			log.WarningContextf(ctx, "failed to unmarshall content to sensorPayload (skipping): %q: %v", txt, err)
			continue
		}

		handlePayload(ctx, pld)
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("got error from scanner: %w", err)
	}

	return nil
}

const MAX_FSR_READING = 1024 - 1
const WIDTH = 120

func handlePayload(ctx context.Context, pld sensorPayload) {
	var mapped int = WIDTH * int(pld.FSRReading) / MAX_FSR_READING
	log.V(5).InfoContextf(ctx, "mapped fsr reading %d to %d", pld.FSRReading, mapped)
	log.V(2).InfoContextf(ctx, strings.Repeat("#", mapped))
}
