package server

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"arbiter/internal/config"
	"arbiter/internal/metrics"
	"arbiter/internal/queue"
)

type Server struct {
	config *config.Config
	queue  *queue.Queue
	server *http.Server
}

func New(cfg *config.Config, q *queue.Queue) *Server {
	s := &Server{
		config: cfg,
		queue:  q,
	}
	
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleRequest)
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/stats", s.handleStats)
	mux.HandleFunc("/metrics", s.handleMetrics)
	
	s.server = &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.Port),
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 300 * time.Second,
	}
	
	return s
}

func (s *Server) handleRequest(w http.ResponseWriter, r *http.Request) {
	log.Printf("Received %s %s from %s", r.Method, r.URL.Path, r.RemoteAddr)
	
	// Create request
	req := queue.NewRequest(r)
	defer req.Cancel()
	
	log.Printf("Request %s has upstream: %s, priority: %s", req.ID, req.UpstreamName, req.Priority)
	
	// Validate upstream exists
	upstreamExists := false
	for _, upstream := range s.config.Upstreams {
		if upstream.Name == req.UpstreamName {
			upstreamExists = true
			break
		}
	}
	
	if !upstreamExists {
		log.Printf("Unknown upstream: %s (available: simple-api, batch-service)", req.UpstreamName)
		http.Error(w, fmt.Sprintf("Unknown upstream: %s", req.UpstreamName), http.StatusBadRequest)
		return
	}
	
	// Check queue size limit
	if s.queue.Length(req.UpstreamName) >= s.config.Queues.MaxSize {
		log.Printf("Queue full for upstream: %s", req.UpstreamName)
		http.Error(w, "Queue full", http.StatusTooManyRequests)
		return
	}
	
	// Enqueue request
	if err := s.queue.Enqueue(req); err != nil {
		if otel := metrics.GetOTEL(); otel != nil {
			otel.RecordShed(r.Context(), req.UpstreamName, req.Priority.String())
		}
		http.Error(w, "Service overloaded", http.StatusServiceUnavailable)
		return
	}
	
	// Wait for response
	select {
	case response := <-req.ResponseChan:
		if response.Error != nil {
			log.Printf("Request %s failed: %v", req.ID, response.Error)
			http.Error(w, "Upstream error", http.StatusBadGateway)
			return
		}
		
		// Copy headers
		for k, v := range response.Headers {
			for _, val := range v {
				w.Header().Add(k, val)
			}
		}
		
		// Write response
		w.WriteHeader(response.StatusCode)
		w.Write(response.Body)
		
		// Request completed
		
	case <-r.Context().Done():
		// Request cancelled by client
		return
		
	case <-time.After(5 * time.Minute):
		http.Error(w, "Request timeout", http.StatusGatewayTimeout)
		return
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"status":"healthy","upstreams":%d}`, len(s.config.Upstreams))
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	
	fmt.Fprintf(w, `{"queues":{`)
	first := true
	for _, upstream := range s.config.Upstreams {
		if !first {
			fmt.Fprintf(w, ",")
		}
		first = false
		length := s.queue.Length(upstream.Name)
		fmt.Fprintf(w, `"%s":%d`, upstream.Name, length)
	}
	fmt.Fprintf(w, `}}`)
}

func (s *Server) ListenAndServe() error {
	return s.server.ListenAndServe()
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	// Serve Prometheus metrics
	metrics.Handler().ServeHTTP(w, r)
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.server.Shutdown(ctx)
}