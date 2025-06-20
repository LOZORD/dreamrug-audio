package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
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
	configFile      = flag.String("io_config", "config.toml", "The TOML I/O configuration file.")
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
	if err := doMain(ctx, os.Stdin, *dataBufferSize, *audioBufferSize, *configFile); err != nil {
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

func (pld *sensorPayload) getValue(sid string) (int32, bool) {
	for _, s := range pld.SensorData {
		if s.Name == sid {
			return s.Value, true
		}
	}
	return 0, false
}

const defaultFreq = 440.0

// frequencyMap is a mapping of sesor input names to their corresponding frequencies (in Hz).
var frequencyMap = map[string]float64{
	"input_1001": 261.63,
	"input_1002": 329.63,
	"input_1003": 392.00,
}

func doMain(ctx context.Context, input io.Reader, dataBufferSize int, audioBufferSize int, configFile string) error {
	scanner := bufio.NewScanner(input)

	db := NewRingBuffer(dataBufferSize)

	cfg, err := ParseIOConfigFromFile(configFile)
	if err != nil {
		return fmt.Errorf("failed to parse config from %q: %w", configFile, err)
	}
	log.V(5).Info("got config for %d sensors", len(cfg.Sensors))

	buf := &audio.FloatBuffer{
		Data:   make([]float64, audioBufferSize),
		Format: audio.FormatMono44100,
	}

	osc := generator.NewOsc(generator.WaveSine, defaultFreq, buf.Format.SampleRate)
	osc.Amplitude = 1

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)

	payloads := make(chan sensorPayload)

	go func() {
		for scanner.Scan() {
			txt := scanner.Text()

			log.V(5).InfoContextf(ctx, "got content: %s", txt)

			var pld sensorPayload
			if err := json.Unmarshal([]byte(txt), &pld); err != nil {
				log.V(2).InfoContextf(ctx, "failed to unmarshall content to sensorPayload (skipping): %q: %v", txt, err)
				continue
			}

			if err := handlePayload(ctx, pld, db, payloads); err != nil {
				log.WarningContextf(ctx, "failed to handle payload: %v", err)
				continue
			}
		}
		// if err := scanner.Err(); err != nil {
		// 	return fmt.Errorf("got error from scanner: %w", err)
		// }
	}()

	runAudio(ctx, osc, buf, audioBufferSize, payloads, sig)

	return nil
}

const MAX_FSR_READING = 1024 - 1
const WIDTH = 120

func handlePayload(ctx context.Context, pld sensorPayload, db *RingBuffer, payloads chan<- sensorPayload) error {
	uptime := time.Duration(pld.UptimeMillis) * time.Millisecond
	log.V(2).InfoContextf(ctx, "got payload at uptime %s", uptime)
	var lastSensor = pld.SensorData[len(pld.SensorData)-1]
	var mapped int = WIDTH * int(lastSensor.Value) / MAX_FSR_READING
	log.V(5).InfoContextf(ctx, "mapped fsr reading %d to %d", lastSensor.Value, mapped)
	log.V(2).InfoContextf(ctx, strings.Repeat("#", mapped))

	// TODO: use ring buffer for delay effect on changes for gradual smoothing.

	payloads <- pld

	return nil
}

func runAudio(ctx context.Context, osc *generator.Osc, buf *audio.FloatBuffer, audioBufferSize int, payloads <-chan sensorPayload, sig chan os.Signal) {
	var currentVol float64 = 1

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

	var currentPayload sensorPayload
	for {

		// Clear the output buffer.
		for i := range out {
			out[i] = 0
		}

		select {
		case pld := <-payloads:
			if len(currentPayload.SensorData) == 0 { // FIXME
				log.V(2).Infoln("set current payload")
				currentPayload = pld
			}
		case <-sig:
			log.InfoContext(ctx, "stopping audio")
			return
		default:
			log.V(5).InfoContextf(ctx, "nothing from the input channel!")
		}

		// populate the out buffer
		for sensorID, freq := range frequencyMap {
			value, ok := currentPayload.getValue(sensorID)
			if !ok {
				log.WarningContextf(ctx, "no sensor value for ID %q", sensorID)
				continue
			}

			osc.Reset()
			osc.Freq = freq
			osc.Amplitude = float64(value) / float64(MAX_FSR_READING)
			if err := osc.Fill(buf); err != nil {
				log.V(5).Infoln("error filling up the buffer")
			}

			// add that audio to the output buffer
			cpy := buf.AsFloat32Buffer().Data
			denom := float32(len(frequencyMap))
			for i := range out {
				// divide by the number of sines to avoid clipping
				out[i] += cpy[i] / denom
			}
		}

		currentVol = 3
		log.V(2).InfoContextf(ctx, "current volume at %f", currentVol)

		transforms.Gain(buf, currentVol)

		// f64ToF32Copy(out, buf.Data)
		// out = buf.AsFloat32Buffer().Data

		// write to the stream
		if err := stream.Write(); err != nil {
			log.ErrorContextf(ctx, "error writing to stream : %v\n", err)
		}
	}
}
