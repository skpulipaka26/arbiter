package processor

import (
	"context"
	"sync"
	"time"

	"arbiter/internal/config"
	"arbiter/internal/logger"
	"arbiter/internal/observability"
	"arbiter/internal/queue"

	"go.opentelemetry.io/otel/attribute"
)

type Processor struct {
	config *config.Config
	queue  *queue.Queue
	worker *Worker
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

type Worker struct {
	upstream *config.Upstream
	queue    *queue.Queue
	ctx      context.Context
}

func New(cfg *config.Config, q *queue.Queue) *Processor {
	ctx, cancel := context.WithCancel(context.Background())

	p := &Processor{
		config: cfg,
		queue:  q,
		ctx:    ctx,
		cancel: cancel,
	}

	p.worker = &Worker{
		upstream: &cfg.Upstream,
		queue:    q,
		ctx:      ctx,
	}

	return p
}

func (p *Processor) Start() {
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()

		if p.worker.upstream.Mode == "batch" {
			p.worker.runBatchProcessor()
		} else {
			p.worker.runIndividualProcessor()
		}
	}()
}

func (p *Processor) Stop() {
	p.cancel()

	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
}

func (w *Worker) runIndividualProcessor() {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	semaphore := make(chan struct{}, w.upstream.MaxConcurrent)

	for {
		select {
		case <-w.ctx.Done():
			return

		case <-ticker.C:
			select {
			case semaphore <- struct{}{}:
				req := w.queue.Dequeue()
				if req == nil {
					<-semaphore
					continue
				}

				logger.FromContext(req.Context()).
					WithField("request_id", req.ID).
					WithField("priority", req.Priority.String()).
					Info("Request dequeued")

				go func(request *queue.Request) {
					defer func() { <-semaphore }()
					w.processIndividualRequest(request)
				}(req)
			default:
				continue
			}
		}
	}
}

func (w *Worker) processIndividualRequest(req *queue.Request) {
	ctx, span := observability.StartSpan(req.Context(), "arbiter.processRequest",
		attribute.String("request_id", req.ID),
		attribute.String("priority", req.Priority.String()),
	)
	defer span.End()

	start := time.Now()
	queueTime := time.Since(req.EnqueuedAt)

	log := logger.FromContext(ctx)
	log.WithField("request_id", req.ID).
		WithField("priority", req.Priority.String()).
		WithField("queue_time_ms", queueTime.Milliseconds()).
		Info("Forwarding request to upstream")

	if otel := observability.GetOTEL(); otel != nil {
		otel.RecordQueueTime(ctx, "upstream", req.Priority.String(), queueTime)
	}

	span.SetAttributes(
		attribute.Int64("queue_time_ms", queueTime.Milliseconds()),
	)

	req.ResponseChan <- &queue.Response{
		Ready:       true,
		UpstreamURL: w.upstream.URL,
	}

	if otel := observability.GetOTEL(); otel != nil {
		otel.RecordRequest(ctx, "upstream", req.Priority.String(), time.Since(start), "forwarded")
	}

	span.AddEvent("request_forwarded")
}

func (w *Worker) runBatchProcessor() {
	timeout := time.NewTicker(w.upstream.BatchTimeout)
	defer timeout.Stop()

	var batch []*queue.Request
	lastFlush := time.Now()

	// Smart polling: fast for aggressive timeouts, adaptive for relaxed ones
	basePoll := min(w.upstream.BatchTimeout/10, 50*time.Millisecond)
	pollInterval := basePoll
	pollTicker := time.NewTicker(pollInterval)
	defer pollTicker.Stop()

	emptyPolls := 0
	activePolls := 0

	for {
		select {
		case <-w.ctx.Done():
			if len(batch) > 0 {
				w.processBatch(batch)
			}
			return

		case <-timeout.C:
			// Timeout fallback - flush whatever we have
			if len(batch) > 0 {
				w.processBatch(batch)
				batch = batch[:0]
				lastFlush = time.Now()
			}

		case <-pollTicker.C:
			// Try to fill the batch
			foundAny := false
			for len(batch) < w.upstream.BatchSize {
				req := w.queue.Dequeue()
				if req == nil {
					break
				}
				foundAny = true
				batch = append(batch, req)
			}

			// Adaptive polling for relaxed timeouts (>= 100ms)
			if w.upstream.BatchTimeout >= 100*time.Millisecond {
				if foundAny || len(batch) > 0 {
					// Activity detected
					emptyPolls = 0
					activePolls++

					// Speed up after sustained activity (hysteresis)
					if activePolls >= 3 && pollInterval > 5*time.Millisecond {
						pollInterval = 5 * time.Millisecond
						pollTicker.Reset(pollInterval)
						activePolls = 0
					}
				} else {
					// No activity
					activePolls = 0
					emptyPolls++

					// Slow down after sustained idle (hysteresis)
					if emptyPolls >= 10 && pollInterval < basePoll {
						pollInterval = min(pollInterval*2, basePoll)
						pollTicker.Reset(pollInterval)
						emptyPolls = 0
					}
				}
			}

			// Check if we should flush
			if w.shouldFlushBatch(batch, time.Since(lastFlush)) {
				w.processBatch(batch)
				batch = batch[:0]
				lastFlush = time.Now()
				timeout.Reset(w.upstream.BatchTimeout)
			}
		}
	}
}

func (w *Worker) shouldFlushBatch(batch []*queue.Request, elapsed time.Duration) bool {
	if len(batch) == 0 {
		return false
	}

	size := len(batch)
	maxSize := w.upstream.BatchSize

	// Count priority distribution
	// Find the highest priority (lowest value) in the batch
	minPriority := int(queue.Low) // Start with lowest priority
	for _, req := range batch {
		if int(req.Priority) < minPriority {
			minPriority = int(req.Priority)
		}
	}

	// Priority-based flush rules based on numeric values
	switch {
	case minPriority <= int(queue.High):
		// High priority or better: flush immediately (after 10ms grace)
		return elapsed > 10*time.Millisecond

	case minPriority <= int(queue.Medium):
		// Medium priority: flush at 50% capacity or 50ms
		return size >= maxSize/2 || elapsed > 50*time.Millisecond

	default:
		// Low priority only: wait for full batch
		return size >= maxSize
	}
}

func (w *Worker) processBatch(batch []*queue.Request) {
	batchSize := len(batch)

	// Count priorities dynamically
	priorityCounts := make(map[int]int)
	for _, req := range batch {
		priorityCounts[int(req.Priority)]++

		if otel := observability.GetOTEL(); otel != nil {
			otel.RecordQueueTime(req.Context(), "upstream", req.Priority.String(), time.Since(req.EnqueuedAt))
		}
	}

	if otel := observability.GetOTEL(); otel != nil && len(batch) > 0 {
		otel.RecordBatch(batch[0].Context(), "upstream", int64(batchSize))
	}

	for _, req := range batch {
		go func(request *queue.Request) {
			reqStart := time.Now()

			request.ResponseChan <- &queue.Response{
				Ready:       true,
				UpstreamURL: w.upstream.URL,
			}

			if otel := observability.GetOTEL(); otel != nil {
				otel.RecordRequest(request.Context(), "upstream", request.Priority.String(), time.Since(reqStart), "forwarded")
			}
		}(req)
	}

}
