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
	"time"

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

const DATA_BUFFER_SIZE = 32

type dataBuffer struct {
	data [DATA_BUFFER_SIZE]int
	head int
}

func newDataBuffer() *dataBuffer {
	return &dataBuffer{}
}

func (db *dataBuffer) insert(val int) {
	db.data[db.head] = val
	db.head = (db.head + 1) % DATA_BUFFER_SIZE
}

func (db *dataBuffer) get(index int) int {
	i := (index + db.head) % DATA_BUFFER_SIZE
	return db.data[i]
}

func (db *dataBuffer) average(bias int) (float64, error) {
	if bias > 100 {
		return 0, fmt.Errorf("bias can only be (-inf, 100], got %d", bias)
	}

	if bias > 0 {
		return 0, fmt.Errorf("bias is currently unsupported, got %d", bias)
	}

	var sum int64
	for i := range DATA_BUFFER_SIZE {
		sum += int64(db.get(i))
	}

	avg := float64(sum) / DATA_BUFFER_SIZE

	return avg, nil
}

func doMain(ctx context.Context, input io.Reader) error {
	scanner := bufio.NewScanner(input)

	db := newDataBuffer()
	for scanner.Scan() {
		txt := scanner.Text()

		log.V(5).InfoContextf(ctx, "got content: %s", txt)

		var pld sensorPayload
		if err := json.Unmarshal([]byte(txt), &pld); err != nil {
			log.WarningContextf(ctx, "failed to unmarshall content to sensorPayload (skipping): %q: %v", txt, err)
			continue
		}

		if err := handlePayload(ctx, pld, db); err != nil {
			log.WarningContextf(ctx, "failed to handle payload: %v", err)
			continue
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("got error from scanner: %w", err)
	}

	return nil
}

const MAX_FSR_READING = 1024 - 1
const WIDTH = 120

func handlePayload(ctx context.Context, pld sensorPayload, db *dataBuffer) error {
	uptime := time.Duration(pld.UptimeMillis) * time.Millisecond
	log.V(2).InfoContextf(ctx, "got payload at uptime %s", uptime)
	var mapped int = WIDTH * int(pld.FSRReading) / MAX_FSR_READING
	log.V(5).InfoContextf(ctx, "mapped fsr reading %d to %d", pld.FSRReading, mapped)
	log.V(2).InfoContextf(ctx, strings.Repeat("#", mapped))

	db.insert(int(pld.FSRReading))
	avg, err := db.average(0)
	if err != nil {
		return fmt.Errorf("failed to get average for data buffer: %w", err)
	}
	log.V(2).InfoContextf(ctx, "got value: %d\trunning average:\t%f\n", pld.FSRReading, avg)
	return nil
}
