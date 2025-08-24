package config

import (
	"fmt"
	"net/url"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Port     int      `yaml:"port"`
	Upstream Upstream `yaml:"upstream"`
	Tracing  Tracing  `yaml:"tracing"`
}

type Tracing struct {
	Enabled  bool   `yaml:"enabled"`
	Endpoint string `yaml:"endpoint"`
}

type Upstream struct {
	URL  string `yaml:"url"`
	Mode string `yaml:"mode"`

	BatchSize    int           `yaml:"batch_size,omitempty"`
	BatchTimeout time.Duration `yaml:"batch_timeout,omitempty"`

	MaxConcurrent int `yaml:"max_concurrent"`

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

	// Set defaults
	if config.Port == 0 {
		config.Port = 8080
	}

	if config.Upstream.MaxConcurrent == 0 {
		config.Upstream.MaxConcurrent = 10
	}
	if config.Upstream.Mode == "" {
		config.Upstream.Mode = "individual"
	}

	if config.Upstream.Queue.MaxSize == 0 {
		config.Upstream.Queue.MaxSize = 1024
	}
	if config.Upstream.Queue.LowPriorityShedAt == 0 {
		config.Upstream.Queue.LowPriorityShedAt = 500
	}
	if config.Upstream.Queue.MediumPriorityShedAt == 0 {
		config.Upstream.Queue.MediumPriorityShedAt = 800
	}
	if config.Upstream.Queue.RequestMaxAge == 0 {
		config.Upstream.Queue.RequestMaxAge = 30 * time.Second
	}

	if err := validateConfig(&config); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	return &config, nil
}

func validateConfig(config *Config) error {
	if config.Port <= 0 || config.Port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535, got %d", config.Port)
	}

	if config.Upstream.URL == "" {
		return fmt.Errorf("upstream.url is required")
	}
	if _, err := url.Parse(config.Upstream.URL); err != nil {
		return fmt.Errorf("invalid upstream.url: %w", err)
	}

	if config.Upstream.Mode != "individual" && config.Upstream.Mode != "batch" {
		return fmt.Errorf("upstream.mode must be 'individual' or 'batch', got '%s'", config.Upstream.Mode)
	}

	if config.Upstream.Mode == "batch" {
		if config.Upstream.BatchSize <= 0 {
			return fmt.Errorf("upstream.batch_size must be > 0 for batch mode")
		}
		if config.Upstream.BatchTimeout <= 0 {
			return fmt.Errorf("upstream.batch_timeout must be > 0 for batch mode")
		}
		// Warn if max_concurrent is set in batch mode (it's not used)
		if config.Upstream.MaxConcurrent != 10 { // 10 is the default
			fmt.Printf("Warning: upstream.max_concurrent is ignored in batch mode\n")
		}
	}

	if config.Upstream.Mode == "individual" {
		if config.Upstream.MaxConcurrent <= 0 {
			return fmt.Errorf("upstream.max_concurrent must be > 0 for individual mode")
		}
		if config.Upstream.BatchSize > 0 || config.Upstream.BatchTimeout > 0 {
			fmt.Printf("Warning: batch_size and batch_timeout are ignored in individual mode\n")
		}
	}

	if config.Upstream.Queue.MaxSize <= 0 {
		return fmt.Errorf("queue.max_size must be > 0")
	}
	if config.Upstream.Queue.LowPriorityShedAt <= 0 {
		return fmt.Errorf("queue.low_priority_shed_at must be > 0")
	}
	if config.Upstream.Queue.MediumPriorityShedAt <= 0 {
		return fmt.Errorf("queue.medium_priority_shed_at must be > 0")
	}

	if config.Upstream.Queue.LowPriorityShedAt > config.Upstream.Queue.MaxSize {
		return fmt.Errorf("queue.low_priority_shed_at (%d) cannot exceed queue.max_size (%d)",
			config.Upstream.Queue.LowPriorityShedAt, config.Upstream.Queue.MaxSize)
	}
	if config.Upstream.Queue.MediumPriorityShedAt > config.Upstream.Queue.MaxSize {
		return fmt.Errorf("queue.medium_priority_shed_at (%d) cannot exceed queue.max_size (%d)",
			config.Upstream.Queue.MediumPriorityShedAt, config.Upstream.Queue.MaxSize)
	}
	if config.Upstream.Queue.LowPriorityShedAt >= config.Upstream.Queue.MediumPriorityShedAt {
		return fmt.Errorf("queue.low_priority_shed_at (%d) should be less than medium_priority_shed_at (%d)",
			config.Upstream.Queue.LowPriorityShedAt, config.Upstream.Queue.MediumPriorityShedAt)
	}

	return nil
}
