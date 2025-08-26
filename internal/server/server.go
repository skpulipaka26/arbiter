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
	"arbiter/internal/observability"
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
	mux.HandleFunc("/metrics", s.handleMetrics)

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

	ctx, span := observability.StartSpan(ctx, "arbiter.handleRequest",
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

	req := s.queue.NewRequestWithContext(ctx, r)
	defer req.Cancel()

	if s.queue.Length() >= s.config.Upstream.Queue.MaxSize {
		span.SetAttributes(attribute.String("error", "queue_full"))
		log.WithField("queue_size", s.queue.Length()).
			Warn("Queue full, rejecting request")
		http.Error(w, "Queue full", http.StatusTooManyRequests)
		return
	}

	span.SetAttributes(
		attribute.String("priority", req.Priority.String()),
		attribute.String("request_id", req.ID),
	)

	if err := s.queue.Enqueue(req); err != nil {
		span.RecordError(err)
		if otel := observability.GetOTEL(); otel != nil {
			otel.RecordShed(ctx, "upstream", req.Priority.String())
		}
		log.WithField("priority", req.Priority.String()).
			WithField("error", err.Error()).
			Error("Request shed due to overload")
		http.Error(w, "Service overloaded", http.StatusServiceUnavailable)
		return
	}

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

		span.AddEvent("proxying_request")
		s.proxyRequest(ctx, w, r, s.config.Upstream.URL)

	case <-r.Context().Done():
		return
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"status":"healthy","upstream":"%s"}`, s.config.Upstream.URL)
}

func (s *Server) ListenAndServe() error {
	return s.server.ListenAndServe()
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	observability.Handler().ServeHTTP(w, r)
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.server.Shutdown(ctx)
}

func (s *Server) proxyRequest(ctx context.Context, w http.ResponseWriter, r *http.Request, upstreamURL string) {
	ctx, span := observability.StartSpan(ctx, "arbiter.proxyRequest",
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
