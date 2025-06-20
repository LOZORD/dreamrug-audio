package main

import (
	"fmt"
	"os"

	"github.com/BurntSushi/toml"
)

type IOConfig struct {
	Sensors []*SensorToneConfig `toml:"sensors"`
}

type SensorToneConfig struct {
	SensorName string  `toml:"sensor_name"`
	MainTone   float64 `toml:"main_tone"`
}

func ParseFromFile(file string) (*IOConfig, error) {
	bs, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("failed to read file at %q: %w", file, err)
	}

	var cfg IOConfig
	if err := toml.Unmarshal(bs, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse IOConfig from TOML file %q: %w", file, err)
	}

	return &cfg, nil
}
