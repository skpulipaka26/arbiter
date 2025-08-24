package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"arbiter/internal/config"
	"arbiter/internal/logger"
	"arbiter/internal/metrics"
	"arbiter/internal/queue"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
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

	// Wrap the mux with OTEL HTTP instrumentation
	// This automatically extracts trace context from incoming requests
	handler := otelhttp.NewHandler(mux, "arbiter",
		otelhttp.WithMessageEvents(otelhttp.ReadEvents, otelhttp.WriteEvents),
	)

	s.server = &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.Port),
		Handler:      handler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 300 * time.Second,
	}

	return s
}

func (s *Server) handleRequest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	ctx, span := metrics.StartSpan(ctx, "arbiter.handleRequest",
		attribute.String("http.method", r.Method),
		attribute.String("http.url", r.URL.Path),
		attribute.String("http.remote_addr", r.RemoteAddr),
	)
	defer span.End()

	log := logger.FromContext(ctx)
	log.WithField("method", r.Method).
		WithField("path", r.URL.Path).
		WithField("remote_addr", r.RemoteAddr).
		Info("Received request")

	req := queue.NewRequestWithContext(ctx, r)
	defer req.Cancel()

	var upstreamConfig *config.Upstream
	for i := range s.config.Upstreams {
		if s.config.Upstreams[i].Name == req.UpstreamName {
			upstreamConfig = &s.config.Upstreams[i]
			break
		}
	}

	if upstreamConfig == nil {
		span.SetAttributes(attribute.String("error", "unknown_upstream"))
		log.WithField("upstream", req.UpstreamName).
			Warn("Unknown upstream requested")
		http.Error(w, fmt.Sprintf("Unknown upstream: %s", req.UpstreamName), http.StatusBadRequest)
		return
	}

	if s.queue.Length(req.UpstreamName) >= upstreamConfig.Queue.MaxSize {
		span.SetAttributes(attribute.String("error", "queue_full"))
		log.WithField("upstream", req.UpstreamName).
			WithField("queue_size", s.queue.Length(req.UpstreamName)).
			Warn("Queue full, rejecting request")
		http.Error(w, "Queue full", http.StatusTooManyRequests)
		return
	}

	span.SetAttributes(
		attribute.String("upstream", req.UpstreamName),
		attribute.String("priority", req.Priority.String()),
		attribute.String("request_id", req.ID),
	)

	if err := s.queue.Enqueue(req); err != nil {
		span.RecordError(err)
		if otel := metrics.GetOTEL(); otel != nil {
			otel.RecordShed(ctx, req.UpstreamName, req.Priority.String())
		}
		log.WithField("upstream", req.UpstreamName).
			WithField("priority", req.Priority.String()).
			WithField("error", err.Error()).
			Error("Request shed due to overload")
		http.Error(w, "Service overloaded", http.StatusServiceUnavailable)
		return
	}

	// Wait for the processor to signal the request is ready
	select {
	case response := <-req.ResponseChan:
		if response.Error != nil {
			span.RecordError(response.Error)
			log.WithField("request_id", req.ID).
				WithField("error", response.Error.Error()).
				Error("Request processing failed")
			http.Error(w, "Upstream error", http.StatusBadGateway)
			return
		}

		if !response.Ready {
			span.SetAttributes(attribute.String("error", "invalid_response"))
			log.WithField("request_id", req.ID).
				Error("Received invalid response format")
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		// Request has been dequeued and is ready to be proxied
		span.AddEvent("proxying_request")
		s.proxyRequest(ctx, w, r, response.UpstreamURL)

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

func (s *Server) proxyRequest(ctx context.Context, w http.ResponseWriter, r *http.Request, upstreamURL string) {
	ctx, span := metrics.StartSpan(ctx, "arbiter.proxyRequest",
		attribute.String("upstream.url", upstreamURL),
	)
	defer span.End()

	targetURL, err := url.Parse(upstreamURL)
	if err != nil {
		span.RecordError(err)
		http.Error(w, "Invalid upstream URL", http.StatusInternalServerError)
		return
	}

	// Create an HTTP client with OTEL instrumentation
	transport := otelhttp.NewTransport(http.DefaultTransport)

	proxy := httputil.NewSingleHostReverseProxy(targetURL)
	proxy.Transport = transport

	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		req.Host = targetURL.Host
	}

	proxy.ServeHTTP(w, r.WithContext(ctx))
}
