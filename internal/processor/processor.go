package processor

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"time"

	"arbiter/internal/config"
	"arbiter/internal/metrics"
	"arbiter/internal/queue"
)

type Processor struct {
	config     *config.Config
	queue      *queue.Queue
	httpClient *http.Client
	workers    map[string]*Worker
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
}

type Worker struct {
	upstream *config.Upstream
	queue    *queue.Queue
	client   *http.Client
	ctx      context.Context
}

func New(cfg *config.Config, q *queue.Queue) *Processor {
	ctx, cancel := context.WithCancel(context.Background())

	// Create HTTP client
	client := &http.Client{
		Timeout: 120 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     90 * time.Second,
		},
	}

	p := &Processor{
		config:     cfg,
		queue:      q,
		httpClient: client,
		workers:    make(map[string]*Worker),
		ctx:        ctx,
		cancel:     cancel,
	}

	// Create workers for each upstream
	for i := range cfg.Upstreams {
		upstream := &cfg.Upstreams[i]
		p.workers[upstream.Name] = &Worker{
			upstream: upstream,
			queue:    q,
			client:   client,
			ctx:      ctx,
		}
		// No need to register - OTEL metrics are initialized in main
	}

	return p
}

func (p *Processor) Start() {
	// Starting processors

	for name, worker := range p.workers {
		p.wg.Add(1)
		go func(workerName string, w *Worker) {
			defer p.wg.Done()

			if w.upstream.Mode == "batch" {
				w.runBatchProcessor()
			} else {
				w.runIndividualProcessor()
			}
		}(name, worker)

		// Processor started
	}
}

func (p *Processor) Stop() {
	// Stopping processors
	p.cancel() // This cancels the context

	// Wait for workers with timeout
	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// All processors stopped
	case <-time.After(5 * time.Second):
		// Forcing shutdown
	}
}

// Individual mode: process requests one by one
func (w *Worker) runIndividualProcessor() {
	// Starting individual processor
	ticker := time.NewTicker(10 * time.Millisecond) // Check queue more frequently
	defer ticker.Stop()

	// Track active requests
	semaphore := make(chan struct{}, w.upstream.MaxConcurrent)

	for {
		select {
		case <-w.ctx.Done():
			// Shutting down
			return

		case <-ticker.C:
			// Try to get a request
			req := w.queue.Dequeue(w.upstream.Name)
			if req == nil {
				continue
			}
			// Got request from queue

			// Process request in goroutine with concurrency limit
			select {
			case semaphore <- struct{}{}:
				go func(request *queue.Request) {
					defer func() { <-semaphore }()
					w.processIndividualRequest(request)
				}(req)
			default:
				// Max concurrent reached, put request back
				w.queue.Enqueue(req)
			}
		}
	}
}

func (w *Worker) processIndividualRequest(req *queue.Request) {
	start := time.Now()
	queueTime := time.Since(req.EnqueuedAt)

	// Processing individual request

	// Track OTEL metrics
	if otel := metrics.GetOTEL(); otel != nil {
		otel.RecordQueueTime(req.Context(), w.upstream.Name, req.Priority.String(), queueTime)
	}

	// Forward to upstream
	resp, err := w.forwardRequest(req)
	status := "success"
	if err != nil {
		status = "failed"
		// Request failed
		req.ResponseChan <- &queue.Response{Error: err}
	}

	// Record OTEL request metrics
	if otel := metrics.GetOTEL(); otel != nil {
		otel.RecordRequest(req.Context(), w.upstream.Name, req.Priority.String(), time.Since(start), status)
	}

	if err != nil {
		return
	}

	// Send response back
	select {
	case req.ResponseChan <- resp:
		// Request completed
	case <-req.Context().Done():
		// Request cancelled
	}
}

// Batch mode: collect requests and process in batches
func (w *Worker) runBatchProcessor() {
	ticker := time.NewTicker(time.Duration(w.upstream.BatchTimeout))
	defer ticker.Stop()

	var batch []*queue.Request

	for {
		select {
		case <-w.ctx.Done():
			// Process remaining batch before exit
			if len(batch) > 0 {
				w.processBatch(batch)
			}
			return

		case <-ticker.C:
			// Collect requests for batch
			for len(batch) < w.upstream.BatchSize {
				req := w.queue.Dequeue(w.upstream.Name)
				if req == nil {
					break
				}
				// Added to batch
				batch = append(batch, req)
			}

			// Process batch if we have requests
			if len(batch) > 0 {
				w.processBatch(batch)
				batch = batch[:0] // Reset batch
			}
		}
	}
}

func (w *Worker) processBatch(batch []*queue.Request) {
	batchSize := len(batch)

	// Count priorities in batch
	high, medium, low := 0, 0, 0
	for _, req := range batch {
		switch req.Priority {
		case queue.High:
			high++
		case queue.Medium:
			medium++
		case queue.Low:
			low++
		}

		// Record queue time for each request
		if otel := metrics.GetOTEL(); otel != nil {
			otel.RecordQueueTime(req.Context(), w.upstream.Name, req.Priority.String(), time.Since(req.EnqueuedAt))
		}
	}

	// Processing batch

	// Track OTEL batch metrics
	if otel := metrics.GetOTEL(); otel != nil && len(batch) > 0 {
		otel.RecordBatch(batch[0].Context(), w.upstream.Name, int64(batchSize))
	}

	// For now, process batch requests individually
	// In a real implementation, you'd combine them into a single upstream call
	for _, req := range batch {
		go func(request *queue.Request) {
			reqStart := time.Now()
			resp, err := w.forwardRequest(request)

			// Record OTEL metrics for each request
			status := "success"
			if err != nil {
				status = "failed"
				// Batch request failed
				request.ResponseChan <- &queue.Response{Error: err}
			}

			if otel := metrics.GetOTEL(); otel != nil {
				otel.RecordRequest(request.Context(), w.upstream.Name, request.Priority.String(), time.Since(reqStart), status)
			}

			if err != nil {
				return
			}

			// Modify response to indicate it was batch processed
			resp.Headers.Set("X-Batch-Size", fmt.Sprintf("%d", batchSize))

			select {
			case request.ResponseChan <- resp:
			case <-request.Context().Done():
				// Batch request cancelled
			}
		}(req)
	}

	// Batch dispatched
}

func (w *Worker) forwardRequest(req *queue.Request) (*queue.Response, error) {
	// Parse upstream URL
	upstreamURL, err := url.Parse(w.upstream.URL)
	if err != nil {
		return nil, fmt.Errorf("invalid upstream URL: %w", err)
	}

	// Create a reverse proxy for this upstream
	proxy := httputil.NewSingleHostReverseProxy(upstreamURL)
	proxy.Transport = w.client.Transport

	// Capture the response using a custom ResponseWriter
	responseCapture := &responseCapture{
		headers: make(http.Header),
		body:    &bytes.Buffer{},
	}

	// Add our tracking headers
	req.HTTPRequest.Header.Set("X-Request-ID", req.ID)
	req.HTTPRequest.Header.Set("X-Priority", req.Priority.String())

	// Let ReverseProxy handle everything - body, headers, methods, etc.
	proxy.ServeHTTP(responseCapture, req.HTTPRequest)

	return &queue.Response{
		StatusCode: responseCapture.statusCode,
		Headers:    responseCapture.headers,
		Body:       responseCapture.body.Bytes(),
	}, nil
}

// responseCapture implements http.ResponseWriter to capture the response
type responseCapture struct {
	statusCode int
	headers    http.Header
	body       *bytes.Buffer
}

func (r *responseCapture) Header() http.Header {
	return r.headers
}

func (r *responseCapture) Write(b []byte) (int, error) {
	if r.statusCode == 0 {
		r.statusCode = http.StatusOK
	}
	return r.body.Write(b)
}

func (r *responseCapture) WriteHeader(statusCode int) {
	r.statusCode = statusCode
}
