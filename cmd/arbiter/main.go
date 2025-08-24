package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"arbiter/internal/config"
	"arbiter/internal/logger"
	"arbiter/internal/observability"
	"arbiter/internal/processor"
	"arbiter/internal/queue"
	"arbiter/internal/server"
)

func main() {
	logger.Init("arbiter")

	// Load configuration
	cfg, err := config.Load("config.yaml")
	if err != nil {
		logger.Default().WithField("error", err.Error()).Error("Failed to load config")
		os.Exit(1)
	}

	// Initialize OTEL tracing
	ctx := context.Background()
	if cfg.Tracing.Enabled {
		tracerProvider, err := observability.InitTracing(ctx, "arbiter", cfg.Tracing.Endpoint)
		if err != nil {
			logger.Default().WithField("error", err.Error()).Warn("Failed to initialize tracing, continuing without it")
			// Continue without tracing
		} else {
			defer func() {
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := tracerProvider.Shutdown(shutdownCtx); err != nil {
					logger.Default().WithField("error", err.Error()).Error("Error shutting down tracer provider")
				}
			}()
		}
	}

	// Initialize OTEL metrics
	otelMetrics, meter, err := observability.InitOTEL()
	if err != nil {
		logger.Default().WithField("error", err.Error()).Error("Failed to initialize OTEL metrics")
		os.Exit(1)
	}

	// Create components
	q := queue.New(cfg.Upstream)
	proc := processor.New(cfg, q)
	srv := server.New(cfg, q)

	// Register metric callbacks
	err = otelMetrics.RegisterCallbacks(meter,
		func() map[string]map[string]int64 {
			result := make(map[string]map[string]int64)
			qMetrics := q.GetMetricsByPriority()
			result["upstream"] = map[string]int64{
				"high":   int64(qMetrics["high"].(int)),
				"medium": int64(qMetrics["medium"].(int)),
				"low":    int64(qMetrics["low"].(int)),
				"total":  int64(qMetrics["size"].(int)),
			}
			return result
		},
		func() map[string]int64 {
			// For now, return empty - we'll track active requests differently
			result := make(map[string]int64)
			result["upstream"] = 0
			return result
		})
	if err != nil {
		logger.Default().WithField("error", err.Error()).Error("Failed to register metric callbacks")
		os.Exit(1)
	}

	// Start processor
	proc.Start()
	defer proc.Stop()

	// Start HTTP server
	go func() {
		logger.Default().Info(fmt.Sprintf("Starting server on port %d", cfg.Port))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Default().WithField("error", err.Error()).Error("Server failed")
			os.Exit(1)
		}
	}()

	// Graceful shutdown
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	logger.Default().Info("Shutting down gracefully...")

	// Shutdown server first (stops accepting new requests)
	// Use context.Background() to wait indefinitely for connections to close
	if err := srv.Shutdown(context.Background()); err != nil {
		logger.Default().WithField("error", err.Error()).Error("Server shutdown error")
	}
	logger.Default().Info("Server stopped accepting new requests")

	// Wait for queues to drain and processor to finish all work
	proc.Stop()
	logger.Default().Info("Processor stopped - all queued requests processed")

	logger.Default().Info("Queue maintenance stopped")
}
