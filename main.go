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
	"github.com/go-audio/transforms"
	"github.com/go-audio/wav"
	log "github.com/golang/glog"
	"github.com/gordonklaus/portaudio"
)

// Flags.
var (
	dataBufferSize  = flag.Int("data_buffer_size", 0, "The size of the data buffer, for gradual input delay. Use 0 for no delay/buffering.")
	audioBufferSize = flag.Int("audio_buffer_size", 512, "The size of the audio buffer, for Portaudio.")
	configFile      = flag.String("io_config", "config.toml", "The TOML I/O configuration file.")
	baseVolume      = flag.Float64("volume", 0.5, "The base volume.")
	inactiveLimit   = flag.Int("inactive_limit", 0, "Cutoff for determining if a sensor is inactive.")
	maxReading      = flag.Int("max_reading", (1024*4)-1, "The maximum vlaue that the Arduino can send.")
	alsoLogDevices  = flag.Bool("alsologdevices", false, "If true, log devices that portaudio is aware of.")
	outputWavFile   = flag.String("output_wav_file", "", "If set, write the audio output to this .wav file instead of playing audio.")
)

type mainConfig struct {
	input           io.Reader
	audioBufferSize int
	delayBufferSize int
	ioConfig        *IOConfig
	baseVolume      float32
	inactiveLimit   int
	maxReading      int
	alsoLogDevices  bool
	outputWavFile   string
}

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
	// TODO: log a warning if the user doesn't provide any data over stdin.

	ioCfg, err := ParseIOConfigFromFile(*configFile)
	if err != nil {
		log.ExitContextf(ctx, "failed to parse IO config from %q: %v", *configFile, ioCfg)
	}

	cfg := &mainConfig{
		input:           os.Stdin,
		audioBufferSize: *audioBufferSize,
		delayBufferSize: *dataBufferSize,
		ioConfig:        ioCfg,
		baseVolume:      float32(*baseVolume),
		inactiveLimit:   *inactiveLimit,
		maxReading:      *maxReading,
		alsoLogDevices:  *alsoLogDevices,
		outputWavFile:   *outputWavFile,
	}

	if err := doMain(ctx, cfg); err != nil {
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

func doMain(ctx context.Context, mc *mainConfig) error {
	scanner := bufio.NewScanner(mc.input)

	log.V(5).InfoContextf(ctx, "got config for %d sensors: %v", len(mc.ioConfig.Sensors), mc.ioConfig)

	// A map of input sensor names to sine waves.
	var inputToSines map[string]*Sine = map[string]*Sine{}
	for _, s := range mc.ioConfig.Sensors {
		log.V(5).InfoContextf(ctx, "got sensor config: %+v", s)
		if s.Disable {
			continue
		}

		// TODO: also set up some octaves.
		// We will need to configure octave-specific relative volumes in order to produce the Shepherd tone effect.
		inputToSines[s.SensorName] = NewSineWave(
			s.MainTone,
			DEFAULT_SAMPLE_RATE,
			1.0,
		)
	}

	if mc.alsoLogDevices {
		if err := logDevices(ctx); err != nil {
			log.WarningContextf(ctx, "failed to log devices: %v", err)
		}
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)

	payloads := make(chan sensorPayload)

	go func() {
		var delayBuffers map[string]*RingBuffer
		if mc.delayBufferSize >= 1 {
			delayBuffers = make(map[string]*RingBuffer)
			for i := range inputToSines {
				delayBuffers[i] = NewRingBuffer(mc.delayBufferSize)
			}
		}

		for scanner.Scan() {
			txt := scanner.Text()

			log.V(5).InfoContextf(ctx, "got content: %s", txt)

			txt = strings.TrimSpace(txt)
			if txt == "" {
				continue // Don't try to handle newlines from the Arduino.
			}
			var pld sensorPayload
			if err := json.Unmarshal([]byte(txt), &pld); err != nil {
				log.V(2).InfoContextf(ctx, "failed to unmarshall content to sensorPayload (skipping): %q: %v", txt, err)
				continue
			}
			if log.V(2) {
				bs, _ := json.Marshal(&pld)
				log.V(2).InfoContextf(ctx, "got JSON payload: %s", bs)
			}
			if err := handlePayload(ctx, delayBuffers, pld, payloads, mc.inactiveLimit); err != nil {
				log.WarningContextf(ctx, "failed to handle payload: %v", err)
				continue
			}
		}
		if err := scanner.Err(); err != nil {
			log.WarningContextf(ctx, "got error from scanner: %v", err)
		}
	}()

	runAudio(ctx,
		inputToSines,
		mc.audioBufferSize,
		mc.baseVolume,
		mc.maxReading,
		mc.outputWavFile,
		payloads,
		sig)
	return nil
}

func logDevices(ctx context.Context) error {
	ds, err := portaudio.Devices()
	if err != nil {
		return err
	}
	for _, d := range ds {
		log.InfoContextf(ctx, "device[%d] %q: %+v", d.Index, d.Name, d)
	}
	return nil
}

const DELAY_BUFFER_AVERAGE_BIAS = 0

// The amount to divide the signal by if it is below the configured inactive limit.
const BELOW_INACTIVE_LIMIT_SHRINK_AMOUNT = 2.0

func handlePayload(
	ctx context.Context,
	delayBuffers map[string]*RingBuffer,
	pld sensorPayload,
	payloads chan<- sensorPayload,
	inactiveLimit int,
) error {
	uptime := time.Duration(pld.UptimeMillis) * time.Millisecond
	log.V(2).InfoContextf(ctx, "got payload at uptime %s: %+v", uptime.String(), pld)
	// Just send the current payload without delay.
	if len(delayBuffers) == 0 {
		payloads <- pld
		return nil
	}
	pldToSend := sensorPayload{UptimeMillis: pld.UptimeMillis}
	// Update the delay buffers and generate a new payload based on the average values across the buffers.
	for _, s := range pld.SensorData {
		buf, ok := delayBuffers[s.Name]
		if !ok {
			log.WarningContextf(ctx, "no delay buffer configured for senor %q", s.Name)
			continue
		}
		buf.Insert(int(s.Value))
		avg, err := buf.Average(DELAY_BUFFER_AVERAGE_BIAS)
		if err != nil {
			log.WarningContextf(ctx, "failed to get average for delay buffer for %q: %v", s.Name, err)
			continue
		}
		// If the average seems like the sensor is inactive, make it quieter.
		if inactiveLimit > 0 && avg < float64(inactiveLimit) {
			avg /= BELOW_INACTIVE_LIMIT_SHRINK_AMOUNT
		}
		avgReading := &sensorReading{
			Name:  s.Name,
			Value: int32(avg),
		}
		pldToSend.SensorData = append(pldToSend.SensorData, avgReading)
	}
	payloads <- pldToSend
	return nil
}

// TODO: add an LFO for stereo channel panning.
const NUM_OUTPUT_CHANNELS = 2

// This program doesn't accept input besides the sensor JSON payloads.
const NUM_INPUT_CHANNELS = 0

// https://dsp.stackexchange.com/q/17685
const SAMPLE_RATE = 44100

// runAudio acts as a simple dispatcher to either play audio out of speaker outputs to writing it to a file.
func runAudio(ctx context.Context, sensorsToSines map[string]*Sine, audioBufferSize int, baseVolume float32, maxReading int, outputWavFile string, payloads <-chan sensorPayload, sig chan os.Signal) {
	if outputWavFile != "" {
		writeAudioToWav(ctx, sensorsToSines, audioBufferSize, baseVolume, maxReading, outputWavFile, payloads, sig)
	} else {
		playAudioLive(ctx, sensorsToSines, audioBufferSize, baseVolume, maxReading, payloads, sig)
	}
}

// playAudioLive contains the logic for playing audio to the default speaker device.
func playAudioLive(ctx context.Context, sensorsToSines map[string]*Sine, audioBufferSize int, baseVolume float32, maxReading int, payloads <-chan sensorPayload, sig chan os.Signal) {
	log.V(2).InfoContext(ctx, "outputting to default audio device")
	out := make([]float32, audioBufferSize)
	stream, err := portaudio.OpenDefaultStream(NUM_INPUT_CHANNELS, NUM_OUTPUT_CHANNELS, SAMPLE_RATE, len(out), &out)
	if err != nil {
		log.Fatal(err)
	}
	defer func() {
		log.InfoContext(ctx, "closing stream")
		if err := stream.Close(); err != nil {
			log.ErrorContextf(ctx, "failed to close stream: %v", err)
		}
	}()
	if err := stream.Start(); err != nil {
		log.Fatal(err)
	}
	defer func() {
		log.InfoContext(ctx, "stopping stream")
		if err := stream.Stop(); err != nil {
			log.ErrorContextf(ctx, "failed to stop stream: %v", err)
		}
	}()

	processAudio(ctx, sensorsToSines, maxReading, baseVolume, out, payloads, sig, func() error {
		return stream.Write()
	})
}

// https://lru.neocities.org and https://github.com/lozord.
const ARTIST_ENGINEER = "Rudberg, Leo"

// writeAudioToWav contains the logic for writing audio to a WAV file.
func writeAudioToWav(ctx context.Context, sensorsToSines map[string]*Sine, audioBufferSize int, baseVolume float32, maxReading int, outputWavFile string, payloads <-chan sensorPayload, sig chan os.Signal) {
	log.InfoContextf(ctx, "outputting to WAV file: %q", outputWavFile)
	out := make([]float32, audioBufferSize)
	outFile, err := os.Create(outputWavFile)
	if err != nil {
		log.ExitContextf(ctx, "failed to create WAV file: %v", err)
	}
	// Setup the WAV encoder with 16-bit depth, 2 channels (stereo), and PCM format.
	const bitDepth = 16
	// https://pkg.go.dev/github.com/go-audio/wav#Encoder.WavAudioFormat
	const pcmNoCompressionAudioFormat = 1
	encoder := wav.NewEncoder(outFile, SAMPLE_RATE, bitDepth, NUM_OUTPUT_CHANNELS, pcmNoCompressionAudioFormat)
	encoder.Metadata = &wav.Metadata{
		Artist:   ARTIST_ENGINEER,
		Engineer: ARTIST_ENGINEER,
	}
	defer func() {
		if err := encoder.Close(); err != nil {
			log.ErrorContextf(ctx, "failed to close wav encoder: %v", err)
		}
	}()

	audioBuf := &audio.Float32Buffer{
		Data: out,
		Format: &audio.Format{
			NumChannels: NUM_OUTPUT_CHANNELS,
			SampleRate:  SAMPLE_RATE,
		},
	}

	processAudio(ctx, sensorsToSines, maxReading, baseVolume, out, payloads, sig, func() error {
		if err := transforms.PCMScaleF32(audioBuf, bitDepth); err != nil {
			return fmt.Errorf("failed to transform to PCMScaleF32: %w", err)
		}

		ib := audioBuf.AsIntBuffer()

		return encoder.Write(ib)
	})
}

// processAudio contains the shared main loop logic to avoid duplication.
func processAudio(ctx context.Context, sensorsToSines map[string]*Sine, maxReading int, baseVolume float32, out []float32, payloads <-chan sensorPayload, sig chan os.Signal, consumer func() error) {
	var currentPayload sensorPayload
	numSines := len(sensorsToSines)

	for {
		// Clear the output buffer.
		for i := range out {
			out[i] = 0
		}

		select {
		case pld := <-payloads:
			currentPayload = pld
		case <-sig:
			log.InfoContext(ctx, "stopping audio processing.")
			return
		default:
			log.V(5).InfoContextf(ctx, "nothing from the input channel!")
		}

		// Synthesize and mix audio from sensors
		for name, sine := range sensorsToSines {
			tmp := make([]float32, len(out))
			fillBuffer(ctx, maxReading, name, sine, currentPayload, tmp)
			for i := range out {
				out[i] += tmp[i] / float32(numSines)
			}
		}

		// Apply base volume
		for i := range out {
			out[i] *= baseVolume
		}

		// Write the audio buffer using the provided consumer function
		if err := consumer(); err != nil {
			log.ErrorContextf(ctx, "failed to consume audio output: %v", err)
		}
	}
}

const USE_SINUSOIDAL_MAPPING = true

func fillBuffer(ctx context.Context, maxReading int, sensorName string, sine *Sine, pld sensorPayload, buf []float32) {
	// val is an int [0, maxReading].
	val, ok := pld.getValue(sensorName)
	if !ok {
		log.V(2).InfoContextf(ctx, "no sensor named %q configured; from payload: %s", sensorName, pld)
		return
	}

	if val > int32(maxReading) {
		val = int32(maxReading)
	}
	if val < 0 {
		val = 0
	}

	// newVol is a real number in [0, 1].
	newVol := float64(val) / float64(maxReading)

	if USE_SINUSOIDAL_MAPPING {
		// Apply the sinusoidal ease-in-out formula to 't' ( = newVol). [yes this is vibe coded]
		// This formula takes a linear input (t) and makes it curve smoothly.
		//   - When t=0, result is -(cos(0)-1)/2 = -(1-1)/2 = 0
		//   - When t=1, result is -(cos(PI)-1)/2 = -(-1-1)/2 = 1
		newVol = -(math.Cos(math.Pi*newVol) - 1) / 2
	}

	sine.SetVolume(newVol)
	sine.Fill(buf)
}
