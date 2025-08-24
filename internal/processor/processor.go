package processor

import (
	"context"
	"net/http"
	"sync"
	"time"

	"arbiter/internal/config"
	"arbiter/internal/logger"
	"arbiter/internal/metrics"
	"arbiter/internal/queue"
	"go.opentelemetry.io/otel/attribute"
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
			// Only dequeue if we have capacity
			select {
			case semaphore <- struct{}{}:
				// We have capacity, try to get a request
				req := w.queue.Dequeue(w.upstream.Name)
				if req == nil {
					// No request available, release semaphore
					<-semaphore
					continue
				}
				
				logger.FromContext(req.Context()).
					WithField("request_id", req.ID).
					WithField("priority", req.Priority.String()).
					WithField("upstream", w.upstream.Name).
					Info("Request dequeued")
				
				// Process request in goroutine
				go func(request *queue.Request) {
					defer func() { <-semaphore }()
					w.processIndividualRequest(request)
				}(req)
			default:
				// Max concurrent reached, don't dequeue
				continue
			}
		}
	}
}

func (w *Worker) processIndividualRequest(req *queue.Request) {
	ctx, span := metrics.StartSpan(req.Context(), "arbiter.processRequest",
		attribute.String("request_id", req.ID),
		attribute.String("upstream", w.upstream.Name),
		attribute.String("priority", req.Priority.String()),
	)
	defer span.End()
	
	start := time.Now()
	queueTime := time.Since(req.EnqueuedAt)

	// Processing individual request
	log := logger.FromContext(ctx)
	log.WithField("request_id", req.ID).
		WithField("priority", req.Priority.String()).
		WithField("upstream", w.upstream.Name).
		WithField("queue_time_ms", queueTime.Milliseconds()).
		Info("Forwarding request to upstream")

	// Track OTEL metrics
	if otel := metrics.GetOTEL(); otel != nil {
		otel.RecordQueueTime(ctx, w.upstream.Name, req.Priority.String(), queueTime)
	}
	
	span.SetAttributes(
		attribute.Int64("queue_time_ms", queueTime.Milliseconds()),
	)

	// Signal that this request is ready to be forwarded
	// The server handler will do the actual proxying
	req.ResponseChan <- &queue.Response{
		Ready: true,
		UpstreamURL: w.upstream.URL,
	}

	// Record metrics (fire and forget)
	if otel := metrics.GetOTEL(); otel != nil {
		otel.RecordRequest(ctx, w.upstream.Name, req.Priority.String(), time.Since(start), "forwarded")
	}
	
	span.AddEvent("request_forwarded")
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

	// Fire and forget - signal all requests in batch are ready
	for _, req := range batch {
		go func(request *queue.Request) {
			reqStart := time.Now()
			
			// Signal that this request is ready to be forwarded
			request.ResponseChan <- &queue.Response{
				Ready:       true,
				UpstreamURL: w.upstream.URL,
			}
			
			// Record metrics
			if otel := metrics.GetOTEL(); otel != nil {
				otel.RecordRequest(request.Context(), w.upstream.Name, request.Priority.String(), time.Since(reqStart), "forwarded")
			}
		}(req)
	}

	// Batch dispatched
}

