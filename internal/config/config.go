package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Port      int        `yaml:"port"`
	Upstreams []Upstream `yaml:"upstreams"`
	Tracing   Tracing    `yaml:"tracing"`
}

type Tracing struct {
	Enabled  bool   `yaml:"enabled"`
	Endpoint string `yaml:"endpoint"`
}

type Upstream struct {
	Name string `yaml:"name"`
	URL  string `yaml:"url"`
	Mode string `yaml:"mode"`

	BatchSize    int           `yaml:"batch_size,omitempty"`
	BatchTimeout time.Duration `yaml:"batch_timeout,omitempty"`

	MaxConcurrent int           `yaml:"max_concurrent"`
	Timeout       time.Duration `yaml:"timeout"`

	Queue QueueConfig `yaml:"queue"`
}

type QueueConfig struct {
	MaxSize              int           `yaml:"max_size"`
	LowPriorityShedAt    int           `yaml:"low_priority_shed_at"`
	MediumPriorityShedAt int           `yaml:"medium_priority_shed_at"`
	RequestMaxAge        time.Duration `yaml:"request_max_age"`
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

		if config.Upstreams[i].Queue.MaxSize == 0 {
			config.Upstreams[i].Queue.MaxSize = 1024
		}
		if config.Upstreams[i].Queue.LowPriorityShedAt == 0 {
			config.Upstreams[i].Queue.LowPriorityShedAt = 500
		}
		if config.Upstreams[i].Queue.MediumPriorityShedAt == 0 {
			config.Upstreams[i].Queue.MediumPriorityShedAt = 800
		}
		if config.Upstreams[i].Queue.RequestMaxAge == 0 {
			config.Upstreams[i].Queue.RequestMaxAge = 30 * time.Second
		}
	}

	return &config, nil
}
