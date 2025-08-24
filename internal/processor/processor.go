package processor

import (
	"context"
	"net/http"
	"sync"
	"time"

	"arbiter/internal/config"
	"arbiter/internal/logger"
	"arbiter/internal/observability"
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

	for i := range cfg.Upstreams {
		upstream := &cfg.Upstreams[i]
		p.workers[upstream.Name] = &Worker{
			upstream: upstream,
			queue:    q,
			client:   client,
			ctx:      ctx,
		}
	}

	return p
}

func (p *Processor) Start() {

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

	}
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
				req := w.queue.Dequeue(w.upstream.Name)
				if req == nil {
					<-semaphore
					continue
				}
				
				logger.FromContext(req.Context()).
					WithField("request_id", req.ID).
					WithField("priority", req.Priority.String()).
					WithField("upstream", w.upstream.Name).
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
		attribute.String("upstream", w.upstream.Name),
		attribute.String("priority", req.Priority.String()),
	)
	defer span.End()
	
	start := time.Now()
	queueTime := time.Since(req.EnqueuedAt)

	log := logger.FromContext(ctx)
	log.WithField("request_id", req.ID).
		WithField("priority", req.Priority.String()).
		WithField("upstream", w.upstream.Name).
		WithField("queue_time_ms", queueTime.Milliseconds()).
		Info("Forwarding request to upstream")

	if otel := observability.GetOTEL(); otel != nil {
		otel.RecordQueueTime(ctx, w.upstream.Name, req.Priority.String(), queueTime)
	}
	
	span.SetAttributes(
		attribute.Int64("queue_time_ms", queueTime.Milliseconds()),
	)

	req.ResponseChan <- &queue.Response{
		Ready: true,
		UpstreamURL: w.upstream.URL,
	}

	if otel := observability.GetOTEL(); otel != nil {
		otel.RecordRequest(ctx, w.upstream.Name, req.Priority.String(), time.Since(start), "forwarded")
	}
	
	span.AddEvent("request_forwarded")
}

func (w *Worker) runBatchProcessor() {
	ticker := time.NewTicker(time.Duration(w.upstream.BatchTimeout))
	defer ticker.Stop()

	var batch []*queue.Request

	for {
		select {
		case <-w.ctx.Done():
			if len(batch) > 0 {
				w.processBatch(batch)
			}
			return

		case <-ticker.C:
			for len(batch) < w.upstream.BatchSize {
				req := w.queue.Dequeue(w.upstream.Name)
				if req == nil {
					break
				}
				batch = append(batch, req)
			}

			if len(batch) > 0 {
				w.processBatch(batch)
				batch = batch[:0]
			}
		}
	}
}

func (w *Worker) processBatch(batch []*queue.Request) {
	batchSize := len(batch)

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

		if otel := observability.GetOTEL(); otel != nil {
			otel.RecordQueueTime(req.Context(), w.upstream.Name, req.Priority.String(), time.Since(req.EnqueuedAt))
		}
	}


	if otel := observability.GetOTEL(); otel != nil && len(batch) > 0 {
		otel.RecordBatch(batch[0].Context(), w.upstream.Name, int64(batchSize))
	}

	for _, req := range batch {
		go func(request *queue.Request) {
			reqStart := time.Now()
			
			request.ResponseChan <- &queue.Response{
				Ready:       true,
				UpstreamURL: w.upstream.URL,
			}
			
			if otel := observability.GetOTEL(); otel != nil {
				otel.RecordRequest(request.Context(), w.upstream.Name, request.Priority.String(), time.Since(reqStart), "forwarded")
			}
		}(req)
	}

}

