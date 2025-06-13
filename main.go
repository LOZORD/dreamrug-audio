package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-audio/audio"
	"github.com/go-audio/generator"
	"github.com/go-audio/transforms"
	log "github.com/golang/glog"
	"github.com/gordonklaus/portaudio"
)

// Flags.
var (
	audioBufferSize = flag.Int("audio_buffer_size", 512, "The size of the audio buffer.")
)

func main() {
	flag.Parse()
	ctx := context.Background()

	if err := portaudio.Initialize(); err != nil {
		log.ExitContextf(ctx, "failed to initialize portaudio: %v", err)
	}
	defer func() {
		log.InfoContextf(ctx, "terminating portaudio...")
		if err := portaudio.Terminate(); err != nil {
			log.ExitContextf(ctx, "failed to terminate portaudio: %v", err)
		}
	}()

	log.Info("starting up an reading from stdin")
	if err := doMain(ctx, os.Stdin, *audioBufferSize); err != nil {
		log.Exitf("failed to run: %v", err)
	}
}

type sensorPayload struct {
	FSRReading    int32 `json:"fsr"`
	LEDBrightness int32 `json:"led"`
	UptimeMillis  int64 `json:"upt"`
}

const DATA_BUFFER_SIZE = 8 // originally 32

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

func doMain(ctx context.Context, input io.Reader, audioBufferSize int) error {
	scanner := bufio.NewScanner(input)

	db := newDataBuffer()

	buf := &audio.FloatBuffer{
		Data:   make([]float64, audioBufferSize),
		Format: audio.FormatMono44100,
	}
	currentNote := 440.0
	osc := generator.NewOsc(generator.WaveSine, currentNote, buf.Format.SampleRate)
	osc.Amplitude = 1

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)

	fsrPercentage := make(chan int)

	go func() {
		for scanner.Scan() {
			txt := scanner.Text()

			log.V(5).InfoContextf(ctx, "got content: %s", txt)

			var pld sensorPayload
			if err := json.Unmarshal([]byte(txt), &pld); err != nil {
				log.V(2).InfoContextf(ctx, "failed to unmarshall content to sensorPayload (skipping): %q: %v", txt, err)
				continue
			}

			if err := handlePayload(ctx, pld, db, fsrPercentage); err != nil {
				log.WarningContextf(ctx, "failed to handle payload: %v", err)
				continue
			}
		}
		// if err := scanner.Err(); err != nil {
		// 	return fmt.Errorf("got error from scanner: %w", err)
		// }
	}()

	runAudio(ctx, osc, buf, audioBufferSize, fsrPercentage, sig)

	return nil
}

const MAX_FSR_READING = 1024 - 1
const WIDTH = 120

func handlePayload(ctx context.Context, pld sensorPayload, db *dataBuffer, fsrPercentage chan<- int) error {
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

	pct := int(math.Round(avg * 100.0 / MAX_FSR_READING))

	fsrPercentage <- pct

	return nil
}

func runAudio(ctx context.Context, osc *generator.Osc, buf *audio.FloatBuffer, audioBufferSize int, fsrPercentage <-chan int, sig chan os.Signal) {
	gainControl := 0.0
	currentVol := osc.Amplitude

	out := make([]float32, audioBufferSize)
	stream, err := portaudio.OpenDefaultStream(0, 1, 44100, len(out), &out)
	if err != nil {
		log.Fatal(err)
	}
	defer stream.Close()

	if err := stream.Start(); err != nil {
		log.Fatal(err)
	}
	defer stream.Stop()

	for {

		select {
		case pct := <-fsrPercentage:
			gainControl = float64(pct) / 100.0
			log.V(2).InfoContextf(ctx, "gain control: %f", gainControl)
		case <-sig:
			log.InfoContext(ctx, "stopping audio")
			return
		default:
			log.V(5).InfoContextf(ctx, "nothing from the input channel!")
		}

		// populate the out buffer
		if err := osc.Fill(buf); err != nil {
			log.V(5).Infoln("error filling up the buffer")
		}
		// apply vol control if needed (applied as a transform instead of a control
		// on the osc)
		/*
			if gainControl != 0 {
				currentVol += gainControl
				if currentVol < 0.1 {
					currentVol = 0
				}
				if currentVol > 6 {
					currentVol = 6
				}
				log.InfoContextf(ctx, "new vol %f.2", currentVol)
				gainControl = 0
			}
		*/

		currentVol = 6.0 * gainControl
		log.V(1).InfoContextf(ctx, "current volume at %f", currentVol)

		transforms.Gain(buf, currentVol)

		f64ToF32Copy(out, buf.Data)

		// write to the stream
		if err := stream.Write(); err != nil {
			log.ErrorContextf(ctx, "error writing to stream : %v\n", err)
		}
	}
}

// portaudio doesn't support float64 so we need to copy our data over to the
// destination buffer.
func f64ToF32Copy(dst []float32, src []float64) {
	for i := range src {
		dst[i] = float32(src[i])
	}
}
