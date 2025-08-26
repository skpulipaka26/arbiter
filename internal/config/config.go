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
	MaxSize       int             `yaml:"max_size"`
	RequestMaxAge time.Duration   `yaml:"request_max_age"`
	Priorities    []PriorityLevel `yaml:"priorities"`
}

type PriorityLevel struct {
	Name    string   `yaml:"name"`
	Value   int      `yaml:"value"`
	Aliases []string `yaml:"aliases,omitempty"`
	ShedAt  int      `yaml:"shed_at,omitempty"`
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
	if config.Upstream.Queue.RequestMaxAge == 0 {
		config.Upstream.Queue.RequestMaxAge = 30 * time.Second
	}

	// Set default priorities if not configured
	if len(config.Upstream.Queue.Priorities) == 0 {
		config.Upstream.Queue.Priorities = []PriorityLevel{
			{
				Name:    "high",
				Value:   0,
				Aliases: []string{"urgent", "critical"},
			},
			{
				Name:    "medium",
				Value:   1,
				Aliases: []string{"normal", "standard"},
				ShedAt:  800,
			},
			{
				Name:    "low",
				Value:   2,
				Aliases: []string{"background", "batch"},
				ShedAt:  500,
			},
		}
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

	if len(config.Upstream.Queue.Priorities) == 0 {
		return fmt.Errorf("at least one priority level must be defined")
	}

	valueMap := make(map[int]string)
	shedAtByPriority := make(map[int]int)
	for i, p := range config.Upstream.Queue.Priorities {
		if existing, ok := valueMap[p.Value]; ok {
			return fmt.Errorf("duplicate priority value %d for '%s' and '%s'", p.Value, p.Name, existing)
		}
		valueMap[p.Value] = p.Name
		shedAtByPriority[p.Value] = p.ShedAt

		if p.ShedAt > 0 && p.ShedAt > config.Upstream.Queue.MaxSize {
			return fmt.Errorf("priorities[%d].shed_at (%d) cannot exceed queue.max_size (%d)",
				i, p.ShedAt, config.Upstream.Queue.MaxSize)
		}
	}

	var lastNonZeroShedAt = config.Upstream.Queue.MaxSize + 1
	for priorityValue := range 10 {
		if shedAt, exists := shedAtByPriority[priorityValue]; exists {
			if shedAt > 0 {
				if shedAt > lastNonZeroShedAt {
					return fmt.Errorf("priorities must have decreasing shed_at values (higher priority items are shed last)")
				}
				lastNonZeroShedAt = shedAt
			}
			// shed_at = 0 means never shed (highest priority), which is always valid
		}
	}

	return nil
}
