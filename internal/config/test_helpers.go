package config

import "time"

// TestQueueConfig returns a QueueConfig with default priorities for testing
func TestQueueConfig() QueueConfig {
	return QueueConfig{
		MaxSize:       100,
		RequestMaxAge: 30 * time.Second,
		Priorities: []PriorityLevel{
			{
				Name:    "high",
				Value:   0,
				Aliases: []string{"urgent", "critical"},
			},
			{
				Name:    "medium",
				Value:   1,
				Aliases: []string{"normal", "standard"},
				ShedAt:  60,
			},
			{
				Name:    "low",
				Value:   2,
				Aliases: []string{"background", "batch"},
				ShedAt:  30,
			},
		},
	}
}

// TestQueueConfigWithShedding returns a QueueConfig with specific shedding thresholds for testing
func TestQueueConfigWithShedding(lowShedAt, mediumShedAt int) QueueConfig {
	cfg := TestQueueConfig()
	for i := range cfg.Priorities {
		switch cfg.Priorities[i].Name {
		case "low":
			cfg.Priorities[i].ShedAt = lowShedAt
		case "medium":
			cfg.Priorities[i].ShedAt = mediumShedAt
		}
	}
	return cfg
}
