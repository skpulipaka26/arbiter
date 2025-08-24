package config

import (
	"fmt"
	"os"
	"time"
	
	"gopkg.in/yaml.v3"
)

type Config struct {
	Port      int         `yaml:"port"`
	Upstreams []Upstream  `yaml:"upstreams"`
	Queues    QueueConfig `yaml:"queues"`
}

type Upstream struct {
	Name    string        `yaml:"name"`
	URL     string        `yaml:"url"`
	Mode    string        `yaml:"mode"` // "individual" or "batch"
	
	// Batch settings (only used if mode = "batch")
	BatchSize    int           `yaml:"batch_size,omitempty"`
	BatchTimeout time.Duration `yaml:"batch_timeout,omitempty"`
	
	// Capacity settings
	MaxConcurrent int           `yaml:"max_concurrent"`
	Timeout       time.Duration `yaml:"timeout"`
}

type QueueConfig struct {
	MaxSize int `yaml:"max_size"`
}

func Load(filename string) (*Config, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}
	
	var config Config
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	
	// Set defaults
	if config.Port == 0 {
		config.Port = 8080
	}
	
	for i := range config.Upstreams {
		if config.Upstreams[i].MaxConcurrent == 0 {
			config.Upstreams[i].MaxConcurrent = 10
		}
		if config.Upstreams[i].Timeout == 0 {
			config.Upstreams[i].Timeout = 60 * time.Second
		}
		if config.Upstreams[i].Mode == "" {
			config.Upstreams[i].Mode = "individual"
		}
	}
	
	if config.Queues.MaxSize == 0 {
		config.Queues.MaxSize = 1000
	}
	
	return &config, nil
}