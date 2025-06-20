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
	"syscall"
	"time"

	log "github.com/golang/glog"
	"github.com/gordonklaus/portaudio"
)

// Flags.
var (
	dataBufferSize  = flag.Int("data_buffer_size", 1, "The size of the data buffer, for gradual input delay.")
	audioBufferSize = flag.Int("audio_buffer_size", 512, "The size of the audio buffer, for Portaudio.")
	configFile      = flag.String("io_config", "config.toml", "The TOML I/O configuration file.")
	baseVolume      = flag.Float64("volume", 0.5, "The base volume.")
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

	log.Info("starting up and reading from stdin")
	if err := doMain(ctx, os.Stdin, *audioBufferSize, *configFile, float32(*baseVolume)); err != nil {
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

func doMain(ctx context.Context, input io.Reader, audioBufferSize int, configFile string, baseVolume float32) error {
	scanner := bufio.NewScanner(input)

	cfg, err := ParseIOConfigFromFile(configFile)
	if err != nil {
		return fmt.Errorf("failed to parse config from %q: %w", configFile, err)
	}
	log.V(5).InfoContextf(ctx, "got config for %d sensors: %v", len(cfg.Sensors), cfg)

	// A map of input sensor names to sine waves.
	var inputToSines map[string]*Sine = map[string]*Sine{}
	for _, s := range cfg.Sensors {
		log.V(5).InfoContextf(ctx, "got sensor config: %+v", s)
		// TODO: also set up some octaves.
		// We will need to configure octave-specific relative volumes in order to produce the Shepherd tone effect.
		inputToSines[s.SensorName] = NewSineWave(
			s.MainTone,
			DEFAULT_SAMPLE_RATE,
			1.0,
		)
	}

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

			if err := handlePayload(ctx, pld, payloads); err != nil {
				log.WarningContextf(ctx, "failed to handle payload: %v", err)
				continue
			}
		}
		if err := scanner.Err(); err != nil {
			log.WarningContextf(ctx, "got error from scanner: %v", err)
		}
	}()

	runAudio(ctx, inputToSines, audioBufferSize, baseVolume, payloads, sig)

	return nil
}

const MAX_FSR_READING = 1024 - 1

func handlePayload(ctx context.Context, pld sensorPayload, payloads chan<- sensorPayload) error {
	uptime := time.Duration(pld.UptimeMillis) * time.Millisecond
	log.V(2).InfoContextf(ctx, "got payload at uptime %s: %+v", uptime.String(), pld)

	// TODO: use ring buffer for delay effect on changes for gradual smoothing.

	payloads <- pld

	return nil
}

func runAudio(ctx context.Context, sensorsToSines map[string]*Sine, audioBufferSize int, baseVolume float32, payloads <-chan sensorPayload, sig chan os.Signal) {
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

	numSines := len(sensorsToSines)

	var currentPayload sensorPayload
	for {

		// Clear the output buffer.
		for i := range out {
			out[i] = 0
		}

		select {
		case pld := <-payloads:
			currentPayload = pld
		case <-sig:
			log.InfoContext(ctx, "stopping audio")
			return
		default:
			log.V(5).InfoContextf(ctx, "nothing from the input channel!")
		}

		for name, sine := range sensorsToSines {
			tmp := make([]float32, len(out))
			fillBuffer(ctx, name, sine, currentPayload, tmp)
			for i := range out {
				out[i] += tmp[i] / float32(numSines)
			}
		}

		for i := range out {
			out[i] *= baseVolume
		}

		if err := stream.Write(); err != nil {
			log.ErrorContextf(ctx, "failed to write to portaudio stream: %v", err)
		}

		// // populate the out buffer
		// for sensorID, freq := range frequencyMap {
		// 	value, ok := currentPayload.getValue(sensorID)
		// 	if !ok {
		// 		log.WarningContextf(ctx, "no sensor value for ID %q", sensorID)
		// 		continue
		// 	}

		// 	osc.Reset()
		// 	osc.Freq = freq
		// 	osc.Amplitude = float64(value) / float64(MAX_FSR_READING)
		// 	if err := osc.Fill(buf); err != nil {
		// 		log.V(5).Infoln("error filling up the buffer")
		// 	}

		// 	// add that audio to the output buffer
		// 	cpy := buf.AsFloat32Buffer().Data
		// 	denom := float32(len(frequencyMap))
		// 	for i := range out {
		// 		// divide by the number of sines to avoid clipping
		// 		out[i] += cpy[i] / denom
		// 	}
		// }

		// currentVol = 3
		// log.V(2).InfoContextf(ctx, "current volume at %f", currentVol)

		// transforms.Gain(buf, currentVol)

		// // f64ToF32Copy(out, buf.Data)
		// // out = buf.AsFloat32Buffer().Data

		// // write to the stream
		// if err := stream.Write(); err != nil {
		// 	log.ErrorContextf(ctx, "error writing to stream : %v\n", err)
		// }
	}
}

func fillBuffer(ctx context.Context, sensorName string, sine *Sine, pld sensorPayload, buf []float32) {
	// val is an int [0, MAX_FSR_READING = 1024 - 1].
	val, ok := pld.getValue(sensorName)
	if !ok {
		log.WarningContextf(ctx, "no sensor named %q configured", sensorName)
		return
	}
	// newVol is a real number in [0, 1].
	newVol := float64(val) / float64(MAX_FSR_READING)
	sine.SetVolume(newVol)
	sine.Fill(buf)
}
