package main

import "fmt"

// RingBuffer is a circular buffer for storing integer data.
type RingBuffer struct {
	data []int
	head int
	size int
}

// NewRingBuffer returns a new RingBuffer.
func NewRingBuffer(size int) *RingBuffer {
	return &RingBuffer{
		data: make([]int, size),
		head: 0,
		size: size,
	}
}

// Insert inserts the new value into the buffer and advances the head.
func (b *RingBuffer) Insert(val int) {
	b.data[b.head] = val
	b.head = (b.head + 1) % b.size
}

// Get returns the value at index relative to the head.
func (b *RingBuffer) Get(index int) int {
	i := (index + b.head) % b.size
	return b.data[i]
}

// Average returns the average value across the buffer, biasing more recent values with the bias parameter.
func (b *RingBuffer) Average(bias int) (float64, error) {
	if b.size < 1 {
		return 0, fmt.Errorf("buffer has bad size < 1: %d", b.size)
	}

	if bias > 100 {
		return 0, fmt.Errorf("bias can only be (-inf, 100], got %d", bias)
	}

	if bias > 0 {
		return 0, fmt.Errorf("bias is currently unsupported, got %d", bias)
	}

	var sum int64
	for i := range b.size {
		sum += int64(b.Get(i))
	}

	avg := float64(sum) / float64(b.size)

	return avg, nil
}
