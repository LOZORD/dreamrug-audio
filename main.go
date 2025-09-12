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
	dataBufferSize    = flag.Int("data_buffer_size", 0, "The size of the data buffer, for gradual input delay. Use 0 for no delay/buffering.")
	audioBufferSize   = flag.Int("audio_buffer_size", 512, "The size of the audio buffer, for Portaudio.")
	configFile        = flag.String("io_config", "config.toml", "The TOML I/O configuration file.")
	baseVolume        = flag.Float64("volume", 0.5, "The base volume.")
	inactiveLimit     = flag.Int("inactive_limit", 0, "Cutoff for determining if a sensor is inactive.")
	maxReading        = flag.Int("max_reading", (1024*4)-1, "The maximum vlaue that the Arduino can send.")
	alsoLogDevices    = flag.Bool("alsologdevices", false, "If true, log devices that portaudio is aware of.")
	outputWavFile     = flag.String("output_wav_file", "", "If set, write the audio output to this .wav file instead of playing audio.")
	outputDeviceIndex = flag.Int("output_device", -1, "The index of the audio output device to use (from --alsologdevices). Leave unspecified to use the default device.")
)

type mainConfig struct {
	input             io.Reader
	audioBufferSize   int
	delayBufferSize   int
	ioConfig          *IOConfig
	baseVolume        float32
	inactiveLimit     int
	maxReading        int
	alsoLogDevices    bool
	outputWavFile     string
	outputDeviceIndex int
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

	ioCfg, err := ParseIOConfigFromFile(*configFile)
	if err != nil {
		log.ExitContextf(ctx, "failed to parse IO config from %q: %v", *configFile, ioCfg)
	}

	cfg := &mainConfig{
		input:             os.Stdin,
		audioBufferSize:   *audioBufferSize,
		delayBufferSize:   *dataBufferSize,
		ioConfig:          ioCfg,
		baseVolume:        float32(*baseVolume),
		inactiveLimit:     *inactiveLimit,
		maxReading:        *maxReading,
		alsoLogDevices:    *alsoLogDevices,
		outputWavFile:     *outputWavFile,
		outputDeviceIndex: *outputDeviceIndex,
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
	LocalIP      string           `json:"lip"`
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

			// We really only expect JSON on the input.
			// Allow the input to send debug payloads prefixed by `#`.
			if strings.HasPrefix(txt, "#") {
				continue
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

	// TODO: use a param struct for this function call.
	runAudio(ctx,
		inputToSines,
		mc.audioBufferSize,
		mc.baseVolume,
		mc.maxReading,
		mc.outputWavFile,
		mc.outputDeviceIndex,
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

func runAudio(ctx context.Context,
	sensorsToSines map[string]*Sine,
	audioBufferSize int,
	baseVolume float32,
	maxReading int,
	outputWavFile string,
	outputDeviceIndex int,
	payloads <-chan sensorPayload,
	sig chan os.Signal) {
	if outputWavFile != "" {
		writeAudioToWav(ctx,
			sensorsToSines,
			audioBufferSize,
			baseVolume,
			maxReading,
			outputWavFile,
			payloads,
			sig)
	} else {
		playAudioLive(ctx,
			sensorsToSines,
			audioBufferSize,
			baseVolume,
			maxReading,
			outputDeviceIndex,
			payloads,
			sig)
	}
}

// playAudioLive now accepts an outputDeviceIndex to select a specific audio device.
func playAudioLive(ctx context.Context, sensorsToSines map[string]*Sine, audioBufferSize int, baseVolume float32, maxReading int, outputDeviceIndex int, payloads <-chan sensorPayload, sig chan os.Signal) {
	out := make([]float32, audioBufferSize)
	var stream *portaudio.Stream
	var err error
	if outputDeviceIndex < 0 { // TODO: consider refactoring this into a `makeStream` function.
		// Default behavior: open the default stream.
		log.InfoContext(ctx, "Opening default audio device...")
		stream, err = portaudio.OpenDefaultStream(NUM_INPUT_CHANNELS, NUM_OUTPUT_CHANNELS, SAMPLE_RATE, len(out), &out)
	} else {
		// New behavior: open a stream for the specified device index.
		log.InfoContextf(ctx, "Attempting to open audio device with index %d...", outputDeviceIndex)
		devices, errDevices := portaudio.Devices()
		if errDevices != nil {
			log.ExitContextf(ctx, "failed to get audio devices: %v", errDevices)
		}
		if outputDeviceIndex >= len(devices) {
			log.ExitContextf(ctx, "invalid output device index %d; must be between 0 and %d", outputDeviceIndex, len(devices)-1)
		}

		device := devices[outputDeviceIndex]
		log.InfoContextf(ctx, "Selected device: %s", device.Name)

		streamParams := portaudio.StreamParameters{
			// Note: this is an output-only stream, so no Input field is declared nor is an input buffer passed below.
			Output: portaudio.StreamDeviceParameters{
				Device:   device,
				Channels: NUM_OUTPUT_CHANNELS,
				Latency:  device.DefaultHighOutputLatency,
			},
			SampleRate:      SAMPLE_RATE,
			FramesPerBuffer: len(out),
		}
		// Open a stream with no input and the specified output parameters.
		stream, err = portaudio.OpenStream(streamParams, &out)
	}

	// Uniform error handling for both cases.
	if err != nil {
		log.Exitf("failed to open portaudio stream: %v", err)
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
