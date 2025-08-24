package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

// TestConfigValidation verifies all config validation rules
// This prevents invalid configurations from running in production
func TestConfigValidation(t *testing.T) {
	tests := []struct {
		name        string
		config      *Config
		shouldError bool
		errorMsg    string
	}{
		{
			name: "valid_individual_mode",
			config: &Config{
				Port: 8080,
				Upstream: Upstream{
					URL:           "http://localhost:8000",
					Mode:          "individual",
					MaxConcurrent: 10,
					Queue: QueueConfig{
						MaxSize:              100,
						LowPriorityShedAt:    30,
						MediumPriorityShedAt: 60,
						RequestMaxAge:        time.Minute,
					},
				},
			},
			shouldError: false,
		},
		{
			name: "valid_batch_mode",
			config: &Config{
				Port: 8080,
				Upstream: Upstream{
					URL:          "http://localhost:8000",
					Mode:         "batch",
					BatchSize:    10,
					BatchTimeout: time.Second,
					Queue: QueueConfig{
						MaxSize:              100,
						LowPriorityShedAt:    30,
						MediumPriorityShedAt: 60,
						RequestMaxAge:        time.Minute,
					},
				},
			},
			shouldError: false,
		},
		{
			name: "missing_upstream_url",
			config: &Config{
				Port: 8080,
				Upstream: Upstream{
					Mode: "individual",
					Queue: QueueConfig{
						MaxSize:              100,
						LowPriorityShedAt:    30,
						MediumPriorityShedAt: 60,
					},
				},
			},
			shouldError: true,
			errorMsg:    "upstream.url is required",
		},
		{
			name: "invalid_upstream_url",
			config: &Config{
				Port: 8080,
				Upstream: Upstream{
					URL:           "://invalid",
					Mode:          "individual",
					MaxConcurrent: 10,
					Queue: QueueConfig{
						MaxSize:              100,
						LowPriorityShedAt:    30,
						MediumPriorityShedAt: 60,
					},
				},
			},
			shouldError: true,
			errorMsg:    "invalid upstream.url",
		},
		{
			name: "invalid_mode",
			config: &Config{
				Port: 8080,
				Upstream: Upstream{
					URL:  "http://localhost:8000",
					Mode: "invalid",
					Queue: QueueConfig{
						MaxSize:              100,
						LowPriorityShedAt:    30,
						MediumPriorityShedAt: 60,
					},
				},
			},
			shouldError: true,
			errorMsg:    "upstream.mode must be 'individual' or 'batch'",
		},
		{
			name: "batch_mode_missing_size",
			config: &Config{
				Port: 8080,
				Upstream: Upstream{
					URL:          "http://localhost:8000",
					Mode:         "batch",
					BatchTimeout: time.Second,
					Queue: QueueConfig{
						MaxSize:              100,
						LowPriorityShedAt:    30,
						MediumPriorityShedAt: 60,
					},
				},
			},
			shouldError: true,
			errorMsg:    "batch_size must be > 0 for batch mode",
		},
		{
			name: "batch_mode_missing_timeout",
			config: &Config{
				Port: 8080,
				Upstream: Upstream{
					URL:       "http://localhost:8000",
					Mode:      "batch",
					BatchSize: 10,
					Queue: QueueConfig{
						MaxSize:              100,
						LowPriorityShedAt:    30,
						MediumPriorityShedAt: 60,
					},
				},
			},
			shouldError: true,
			errorMsg:    "batch_timeout must be > 0 for batch mode",
		},
		{
			name: "invalid_queue_thresholds_low_exceeds_max",
			config: &Config{
				Port: 8080,
				Upstream: Upstream{
					URL:           "http://localhost:8000",
					Mode:          "individual",
					MaxConcurrent: 10,
					Queue: QueueConfig{
						MaxSize:              100,
						LowPriorityShedAt:    150, // Exceeds max
						MediumPriorityShedAt: 80,
					},
				},
			},
			shouldError: true,
			errorMsg:    "low_priority_shed_at (150) cannot exceed queue.max_size (100)",
		},
		{
			name: "invalid_queue_thresholds_medium_exceeds_max",
			config: &Config{
				Port: 8080,
				Upstream: Upstream{
					URL:           "http://localhost:8000",
					Mode:          "individual",
					MaxConcurrent: 10,
					Queue: QueueConfig{
						MaxSize:              100,
						LowPriorityShedAt:    50,
						MediumPriorityShedAt: 150, // Exceeds max
					},
				},
			},
			shouldError: true,
			errorMsg:    "medium_priority_shed_at (150) cannot exceed queue.max_size (100)",
		},
		{
			name: "invalid_queue_thresholds_order",
			config: &Config{
				Port: 8080,
				Upstream: Upstream{
					URL:           "http://localhost:8000",
					Mode:          "individual",
					MaxConcurrent: 10,
					Queue: QueueConfig{
						MaxSize:              100,
						LowPriorityShedAt:    60,
						MediumPriorityShedAt: 50, // Less than low
					},
				},
			},
			shouldError: true,
			errorMsg:    "low_priority_shed_at (60) should be less than medium_priority_shed_at (50)",
		},
		{
			name: "invalid_port_zero",
			config: &Config{
				Port: 0,
				Upstream: Upstream{
					URL:           "http://localhost:8000",
					Mode:          "individual",
					MaxConcurrent: 10,
					Queue: QueueConfig{
						MaxSize:              100,
						LowPriorityShedAt:    30,
						MediumPriorityShedAt: 60,
					},
				},
			},
			shouldError: true,
			errorMsg:    "port must be between 1 and 65535",
		},
		{
			name: "invalid_port_too_high",
			config: &Config{
				Port: 70000,
				Upstream: Upstream{
					URL:           "http://localhost:8000",
					Mode:          "individual",
					MaxConcurrent: 10,
					Queue: QueueConfig{
						MaxSize:              100,
						LowPriorityShedAt:    30,
						MediumPriorityShedAt: 60,
					},
				},
			},
			shouldError: true,
			errorMsg:    "port must be between 1 and 65535",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateConfig(tt.config)
			if tt.shouldError {
				if err == nil {
					t.Errorf("Expected error containing '%s', got nil", tt.errorMsg)
				} else if !strings.Contains(err.Error(), tt.errorMsg) {
					t.Errorf("Expected error containing '%s', got '%s'", tt.errorMsg, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("Expected no error, got: %v", err)
				}
			}
		})
	}
}

// TestConfigDefaults verifies default values are applied correctly
func TestConfigDefaults(t *testing.T) {
	// Create minimal config file
	configYAML := `
upstream:
  url: "http://localhost:8000"
`
	tmpfile, err := os.CreateTemp("", "config*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpfile.Name())

	if _, err := tmpfile.Write([]byte(configYAML)); err != nil {
		t.Fatal(err)
	}
	tmpfile.Close()

	// Load config
	cfg, err := Load(tmpfile.Name())
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	// Verify defaults
	if cfg.Port != 8080 {
		t.Errorf("Expected default port 8080, got %d", cfg.Port)
	}
	if cfg.Upstream.Mode != "individual" {
		t.Errorf("Expected default mode 'individual', got %s", cfg.Upstream.Mode)
	}
	if cfg.Upstream.MaxConcurrent != 10 {
		t.Errorf("Expected default max_concurrent 10, got %d", cfg.Upstream.MaxConcurrent)
	}
	if cfg.Upstream.Queue.MaxSize != 1024 {
		t.Errorf("Expected default max_size 1024, got %d", cfg.Upstream.Queue.MaxSize)
	}
	if cfg.Upstream.Queue.LowPriorityShedAt != 500 {
		t.Errorf("Expected default low_priority_shed_at 500, got %d", cfg.Upstream.Queue.LowPriorityShedAt)
	}
	if cfg.Upstream.Queue.MediumPriorityShedAt != 800 {
		t.Errorf("Expected default medium_priority_shed_at 800, got %d", cfg.Upstream.Queue.MediumPriorityShedAt)
	}
	if cfg.Upstream.Queue.RequestMaxAge != 30*time.Second {
		t.Errorf("Expected default request_max_age 30s, got %v", cfg.Upstream.Queue.RequestMaxAge)
	}
}

// TestConfigLoadErrors verifies error handling for file operations
func TestConfigLoadErrors(t *testing.T) {
	// Test non-existent file
	_, err := Load("/non/existent/file.yaml")
	if err == nil {
		t.Error("Expected error for non-existent file")
	}

	// Test invalid YAML
	tmpfile, err := os.CreateTemp("", "invalid*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpfile.Name())

	invalidYAML := `
upstream:
  url: [this is not valid
`
	tmpfile.Write([]byte(invalidYAML))
	tmpfile.Close()

	_, err = Load(tmpfile.Name())
	if err == nil {
		t.Error("Expected error for invalid YAML")
	}
	if !strings.Contains(err.Error(), "parsing config") {
		t.Errorf("Expected parsing error, got: %v", err)
	}
}

// TestWarningsForUnusedSettings verifies warnings are shown for mode mismatches
func TestWarningsForUnusedSettings(t *testing.T) {
	// This test would capture stdout to verify warnings
	// For now, we just ensure the validation passes with warnings

	// Individual mode with batch settings
	cfg1 := &Config{
		Port: 8080,
		Upstream: Upstream{
			URL:           "http://localhost:8000",
			Mode:          "individual",
			MaxConcurrent: 10,
			BatchSize:     10,    // Should warn
			BatchTimeout:  time.Second, // Should warn
			Queue: QueueConfig{
				MaxSize:              100,
				LowPriorityShedAt:    30,
				MediumPriorityShedAt: 60,
			},
		},
	}

	err := validateConfig(cfg1)
	if err != nil {
		t.Errorf("Config with warnings should still validate: %v", err)
	}

	// Batch mode with non-default max_concurrent
	cfg2 := &Config{
		Port: 8080,
		Upstream: Upstream{
			URL:           "http://localhost:8000",
			Mode:          "batch",
			MaxConcurrent: 20, // Should warn (not default 10)
			BatchSize:     10,
			BatchTimeout:  time.Second,
			Queue: QueueConfig{
				MaxSize:              100,
				LowPriorityShedAt:    30,
				MediumPriorityShedAt: 60,
			},
		},
	}

	err = validateConfig(cfg2)
	if err != nil {
		t.Errorf("Config with warnings should still validate: %v", err)
	}
}