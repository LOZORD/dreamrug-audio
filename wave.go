package main

import (
	"math"
	"sync"
)

// DEFAULT_SAMPLE_RATE is a good sample rate since it is at least twice as much as 20kHz,
// the upper bound for human hearing. See https://audio46.com/blogs/audio46-guidepost/what-is-sample-rate-and-bit-depth-do-they-matter for more.
const DEFAULT_SAMPLE_RATE = 44100

// Sine represents a sine wave.
type Sine struct {
	freq       float64
	sampleRate float64

	currentAngle float64
	// mu protects volume.
	mu sync.Mutex
	// The volume or amplitude of this sine wave. A real number in [0, 1].
	volume float64
}

// NewSineWave returns a new Sine wave.
func NewSineWave(freq, sampleRate, initialVolume float64) *Sine {
	return &Sine{
		freq:         freq,
		sampleRate:   sampleRate,
		currentAngle: 0,
		volume:       initialVolume,
	}
}

func (s *Sine) angleDelta() float64 {
	return 2 * math.Pi * s.freq / s.sampleRate
}

// Fill populates the buffer with sine wave data, applying its individual volume.
func (s *Sine) Fill(buf []float32) {
	s.mu.Lock()
	vol := s.volume
	s.mu.Unlock()

	for i := range buf {
		// Apply the individual volume to the sine wave calculation.
		buf[i] = float32(s.Next() * vol)
	}
}

// SetVolume provides a thread-safe way to update the sine wave's volume.
func (s *Sine) SetVolume(v float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.volume = v
}

// Next generates and returns the next single sample, without regard for the value.
func (s *Sine) Next() float64 {
	sample := math.Sin(s.currentAngle)
	s.currentAngle += s.angleDelta()
	return sample
}
