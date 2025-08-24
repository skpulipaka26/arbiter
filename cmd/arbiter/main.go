package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"arbiter/internal/config"
	"arbiter/internal/metrics"
	"arbiter/internal/processor"
	"arbiter/internal/queue"
	"arbiter/internal/server"
)

func main() {
	fmt.Println("🚀 Arbiter - Universal Priority Gateway")
	
	// Load configuration
	cfg, err := config.Load("config.yaml")
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}
	
	// Initialize OTEL metrics
	otelMetrics, meter, err := metrics.InitOTEL()
	if err != nil {
		log.Fatalf("Failed to initialize OTEL metrics: %v", err)
	}
	
	// Create components
	q := queue.New()
	proc := processor.New(cfg, q)
	srv := server.New(cfg, q)
	
	// Register metric callbacks
	err = otelMetrics.RegisterCallbacks(meter,
		func() map[string]map[string]int64 {
			result := make(map[string]map[string]int64)
			for _, upstream := range cfg.Upstreams {
				qMetrics := q.GetMetricsByPriority(upstream.Name)
				result[upstream.Name] = map[string]int64{
					"high":   int64(qMetrics["high"].(int)),
					"medium": int64(qMetrics["medium"].(int)),
					"low":    int64(qMetrics["low"].(int)),
					"total":  int64(qMetrics["size"].(int)),
				}
			}
			return result
		},
		func() map[string]int64 {
			// For now, return empty - we'll track active requests differently
			result := make(map[string]int64)
			for _, upstream := range cfg.Upstreams {
				result[upstream.Name] = 0
			}
			return result
		})
	if err != nil {
		log.Fatalf("Failed to register metric callbacks: %v", err)
	}
	
	// Start processor
	proc.Start()
	defer proc.Stop()
	
	// Start HTTP server
	go func() {
		log.Printf("Starting server on port %d", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server failed: %v", err)
		}
	}()
	
	// Graceful shutdown
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	
	log.Println("Shutting down gracefully...")
	
	// Create shutdown context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	
	// Shutdown server first (stops accepting new requests)
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("Server shutdown error: %v", err)
	}
	log.Println("Server stopped")
	
	// Stop processor (waits for workers)
	proc.Stop()
	log.Println("Processor stopped")
	
	// Stop queue maintenance
	q.Shutdown()
	log.Println("Queue stopped")
}