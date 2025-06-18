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
	dataBufferSize  = flag.Int("data_buffer_size", 1, "The size of the data buffer, for gradual input delay.")
	audioBufferSize = flag.Int("audio_buffer_size", 512, "The size of the audio buffer, for Portaudio.")
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
	if err := doMain(ctx, os.Stdin, *dataBufferSize, *audioBufferSize); err != nil {
		log.Exitf("failed to run: %v", err)
	}
}

type sensorReading struct {
	Name  string `json:"name"`
	Value int32  `json:"value"`
}

type sensorPayload struct {
	UptimeMillis int64            `json:"upt"`
	SensorData   []*sensorReading `json:"sensorData"`
}

type dataBuffer struct {
	data []int
	head int
	size int
}

func newDataBuffer(size int) *dataBuffer {
	return &dataBuffer{
		data: make([]int, size),
		head: 0,
		size: size,
	}
}

func (db *dataBuffer) insert(val int) {
	db.data[db.head] = val
	db.head = (db.head + 1) % db.size
}

func (db *dataBuffer) get(index int) int {
	i := (index + db.head) % db.size
	return db.data[i]
}

func (db *dataBuffer) average(bias int) (float64, error) {
	if db.size < 1 {
		return 0, fmt.Errorf("buffer has bad size < 1: %d", db.size)
	}

	if bias > 100 {
		return 0, fmt.Errorf("bias can only be (-inf, 100], got %d", bias)
	}

	if bias > 0 {
		return 0, fmt.Errorf("bias is currently unsupported, got %d", bias)
	}

	var sum int64
	for i := range db.size {
		sum += int64(db.get(i))
	}

	avg := float64(sum) / float64(db.size)

	return avg, nil
}

// frequencyMap is a mapping of sesor input names to their corresponding frequencies (in Hz).
var frequencyMap = map[string]float64{
	"input_1001": 261.63,
	"input_1002": 329.63,
	"input_1003": 392.00,
}

func doMain(ctx context.Context, input io.Reader, dataBufferSize int, audioBufferSize int) error {
	scanner := bufio.NewScanner(input)

	db := newDataBuffer(dataBufferSize)

	buf := &audio.FloatBuffer{
		Data:   make([]float64, audioBufferSize),
		Format: audio.FormatMono44100,
	}

	var oscs []*generator.Osc

	currentNote := 440.0
	osc := generator.NewOsc(generator.WaveSine, currentNote, buf.Format.SampleRate)
	osc.Amplitude = 1

	oscs = append(oscs, osc)

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

	runAudio(ctx, oscs, buf, audioBufferSize, fsrPercentage, sig)

	return nil
}

const MAX_FSR_READING = 1024 - 1
const WIDTH = 120

func handlePayload(ctx context.Context, pld sensorPayload, db *dataBuffer, fsrPercentage chan<- int) error {
	uptime := time.Duration(pld.UptimeMillis) * time.Millisecond
	log.V(2).InfoContextf(ctx, "got payload at uptime %s", uptime)
	var lastSensor = pld.SensorData[len(pld.SensorData)-1]
	var mapped int = WIDTH * int(lastSensor.Value) / MAX_FSR_READING
	log.V(5).InfoContextf(ctx, "mapped fsr reading %d to %d", lastSensor.Value, mapped)
	log.V(2).InfoContextf(ctx, strings.Repeat("#", mapped))

	db.insert(int(lastSensor.Value))
	avg, err := db.average(0)
	if err != nil {
		return fmt.Errorf("failed to get average for data buffer: %w", err)
	}
	log.V(2).InfoContextf(ctx, "got value: %d\trunning average:\t%f\n", lastSensor.Value, avg)

	pct := int(math.Round(avg * 100.0 / MAX_FSR_READING))

	fsrPercentage <- pct
	log.V(1).InfoContextf(ctx, "reading percentage: %02d / 100", pct)

	return nil
}

func runAudio(ctx context.Context, oscs []*generator.Osc, buf *audio.FloatBuffer, audioBufferSize int, fsrPercentage <-chan int, sig chan os.Signal) {
	gainControl := 0.0
	var currentVol float64

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
		for _, o := range oscs {
			if err := o.Fill(buf); err != nil {
				log.V(5).Infoln("error filling up the buffer")
			}
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
		log.V(2).InfoContextf(ctx, "current volume at %f", currentVol)

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
