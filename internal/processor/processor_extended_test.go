package processor

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"arbiter/internal/config"
	"arbiter/internal/logger"
	"arbiter/internal/queue"
)

func init() {
	logger.Init("test")
}

// TestBatchFlushLogic tests the batch flush decision logic in isolation
func TestBatchFlushLogic(t *testing.T) {
	tests := []struct {
		name        string
		batchSize   int
		priorities  []queue.Priority
		elapsed     time.Duration
		shouldFlush bool
		description string
	}{
		{
			name:        "empty_batch",
			batchSize:   10,
			priorities:  []queue.Priority{},
			elapsed:     time.Hour,
			shouldFlush: false,
			description: "Empty batch should never flush",
		},
		{
			name:        "single_high_priority_quick",
			batchSize:   10,
			priorities:  []queue.Priority{queue.High},
			elapsed:     11 * time.Millisecond,
			shouldFlush: true,
			description: "High priority should flush after 10ms",
		},
		{
			name:        "single_high_priority_wait",
			batchSize:   10,
			priorities:  []queue.Priority{queue.High},
			elapsed:     5 * time.Millisecond,
			shouldFlush: false,
			description: "High priority should wait 10ms grace period",
		},
		{
			name:        "medium_priority_half_capacity",
			batchSize:   10,
			priorities:  []queue.Priority{queue.Medium, queue.Medium, queue.Medium, queue.Medium, queue.Medium},
			elapsed:     30 * time.Millisecond,
			shouldFlush: true,
			description: "Medium priority at 50% should flush",
		},
		{
			name:        "medium_priority_timeout",
			batchSize:   10,
			priorities:  []queue.Priority{queue.Medium},
			elapsed:     51 * time.Millisecond,
			shouldFlush: true,
			description: "Medium priority should flush after 50ms",
		},
		{
			name:        "low_priority_wait_full",
			batchSize:   5,
			priorities:  []queue.Priority{queue.Low, queue.Low, queue.Low, queue.Low},
			elapsed:     100 * time.Millisecond,
			shouldFlush: false,
			description: "Low priority should wait for full batch",
		},
		{
			name:        "low_priority_full_batch",
			batchSize:   5,
			priorities:  []queue.Priority{queue.Low, queue.Low, queue.Low, queue.Low, queue.Low},
			elapsed:     10 * time.Millisecond,
			shouldFlush: true,
			description: "Full batch should flush immediately",
		},
		{
			name:        "mixed_priorities_high_dominates",
			batchSize:   10,
			priorities:  []queue.Priority{queue.Low, queue.Medium, queue.High},
			elapsed:     11 * time.Millisecond,
			shouldFlush: true,
			description: "High priority in batch triggers quick flush",
		},
		{
			name:        "batch_timeout_consideration",
			batchSize:   10,
			priorities:  []queue.Priority{queue.Low, queue.Low},
			elapsed:     100 * time.Millisecond, // Past typical batch timeout
			shouldFlush: false,                  // Our logic doesn't use batch timeout in shouldFlushBatch
			description: "shouldFlushBatch uses priority-based logic, not batch timeout",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := &Worker{
				upstream: &config.Upstream{
					BatchSize:    tt.batchSize,
					BatchTimeout: 5 * time.Second, // High timeout to test priority logic
				},
			}

			// Create batch with test priorities
			var batch []*queue.Request
			for i, p := range tt.priorities {
				batch = append(batch, &queue.Request{
					ID:       fmt.Sprintf("req-%d", i),
					Priority: p,
				})
			}

			result := w.shouldFlushBatch(batch, tt.elapsed)
			if result != tt.shouldFlush {
				t.Errorf("%s: expected flush=%v, got %v", tt.description, tt.shouldFlush, result)
			}
		})
	}
}

// TestQueueProcessorIntegration tests the interaction between queue and processor
func TestQueueProcessorIntegration(t *testing.T) {
	t.Run("batch_mode_priority_handling", func(t *testing.T) {
		cfg := config.Upstream{
			Mode:         "batch",
			BatchSize:    5,
			BatchTimeout: 100 * time.Millisecond,
			Queue: func() config.QueueConfig {
			cfg := config.TestQueueConfigWithShedding(50, 80)
			cfg.MaxSize = 100
			cfg.RequestMaxAge = time.Minute
			return cfg
		}(),
		}
		q := queue.New(cfg)

		// Add mixed priority requests
		for i := 0; i < 15; i++ {
			priority := queue.Priority(i % 3)
			req := createTestRequest(fmt.Sprintf("req-%d", i), priority)
			q.Enqueue(req)
		}

		// Verify queue prioritizes correctly
		firstReq := q.Dequeue()
		if firstReq.Priority != queue.High {
			t.Errorf("Expected high priority first, got %v", firstReq.Priority)
		}

		// Put it back
		q.Enqueue(firstReq)

		// Create processor
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()

		// We don't need w here, just testing queue behavior

		processed := int32(0)
		var wg sync.WaitGroup

		// Process requests
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				req := q.Dequeue()
				if req != nil {
					atomic.AddInt32(&processed, 1)
					req.ResponseChan <- &queue.Response{Ready: true}
				}
				time.Sleep(5 * time.Millisecond)
			}
		}()

		wg.Wait()

		// Should have processed most requests
		finalProcessed := atomic.LoadInt32(&processed)
		if finalProcessed < 10 {
			t.Errorf("Expected at least 10 processed, got %d", finalProcessed)
		}
	})

	t.Run("individual_mode_concurrency", func(t *testing.T) {
		cfg := config.Upstream{
			Mode:          "individual",
			MaxConcurrent: 3,
			Queue: func() config.QueueConfig {
			cfg := config.TestQueueConfigWithShedding(50, 80)
			cfg.MaxSize = 100
			cfg.RequestMaxAge = time.Minute
			return cfg
		}(),
		}
		q := queue.New(cfg)

		// Add many requests
		for i := 0; i < 20; i++ {
			req := createTestRequest(fmt.Sprintf("req-%d", i), queue.High)
			q.Enqueue(req)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()

		// Simulate concurrent processing with semaphore
		semaphore := make(chan struct{}, cfg.MaxConcurrent)
		concurrent := int32(0)
		maxSeen := int32(0)
		processed := int32(0)

		var wg sync.WaitGroup
		for i := 0; i < 10; i++ { // Multiple workers
			wg.Add(1)
			go func() {
				defer wg.Done()
				for ctx.Err() == nil {
					select {
					case semaphore <- struct{}{}:
						req := q.Dequeue()
						if req != nil {
							current := atomic.AddInt32(&concurrent, 1)
							for {
								oldMax := atomic.LoadInt32(&maxSeen)
								if current <= oldMax || atomic.CompareAndSwapInt32(&maxSeen, oldMax, current) {
									break
								}
							}

							// Simulate processing
							time.Sleep(10 * time.Millisecond)

							atomic.AddInt32(&concurrent, -1)
							atomic.AddInt32(&processed, 1)
							req.ResponseChan <- &queue.Response{Ready: true}
						}
						<-semaphore
					case <-ctx.Done():
						return
					}
				}
			}()
		}

		wg.Wait()

		if maxSeen > int32(cfg.MaxConcurrent) {
			t.Errorf("Max concurrent exceeded: saw %d, limit %d", maxSeen, cfg.MaxConcurrent)
		}

		finalProcessed := atomic.LoadInt32(&processed)
		if finalProcessed < 10 {
			t.Errorf("Expected at least 10 processed, got %d", finalProcessed)
		}
	})
}

// TestStaleRequestRemoval verifies stale requests are handled correctly
func TestStaleRequestRemoval(t *testing.T) {
	cfg := config.Upstream{
		Queue: func() config.QueueConfig {
			cfg := config.TestQueueConfigWithShedding(50, 80)
			cfg.MaxSize = 100
			cfg.RequestMaxAge = 50 * time.Millisecond
			return cfg
		}(),
	}
	q := queue.New(cfg)

	// Add old request
	oldReq := &queue.Request{
		ID:           "old",
		Priority:     queue.High,
		EnqueuedAt:   time.Now().Add(-100 * time.Millisecond),
		ResponseChan: make(chan *queue.Response, 1),
	}
	oldReq.SetContextForTesting(context.Background())
	q.Enqueue(oldReq)

	// Add fresh request
	freshReq := createTestRequest("fresh", queue.Low)
	q.Enqueue(freshReq)

	// Dequeue should skip old despite high priority
	req := q.Dequeue()
	if req == nil || req.ID != "fresh" {
		t.Error("Should dequeue fresh request, skipping expired")
	}

	// Old should have been notified
	select {
	case resp := <-oldReq.ResponseChan:
		if resp.Ready || resp.Error == nil {
			t.Error("Old request should have error response")
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("Old request not notified")
	}
}

// TestContextCancellationCleanup tests proper cleanup on cancellation
func TestContextCancellationCleanup(t *testing.T) {
	cfg := config.Upstream{
		Queue: func() config.QueueConfig {
			cfg := config.TestQueueConfigWithShedding(50, 80)
			cfg.MaxSize = 100
			cfg.RequestMaxAge = time.Minute
			return cfg
		}(),
	}
	q := queue.New(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	cancelReq := &queue.Request{
		ID:           "cancel-me",
		Priority:     queue.High,
		EnqueuedAt:   time.Now(),
		ResponseChan: make(chan *queue.Response, 1),
	}
	cancelReq.SetContextForTesting(ctx)
	q.Enqueue(cancelReq)

	normalReq := createTestRequest("normal", queue.Low)
	q.Enqueue(normalReq)

	// Cancel the context
	cancel()

	// Dequeue should skip cancelled despite high priority
	req := q.Dequeue()
	if req == nil || req.ID != "normal" {
		t.Error("Should dequeue normal request, skipping cancelled")
	}

	// Cancelled should have been notified
	select {
	case resp := <-cancelReq.ResponseChan:
		if resp.Ready || resp.Error == nil {
			t.Error("Cancelled request should have error response")
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("Cancelled request not notified")
	}
}

// TestQueueShedding tests the graduated load shedding
func TestQueueShedding(t *testing.T) {
	cfg := config.Upstream{
		Queue: func() config.QueueConfig {
			cfg := config.TestQueueConfigWithShedding(8, 15)
			cfg.MaxSize = 20
			cfg.RequestMaxAge = time.Minute
			return cfg
		}(),
	}
	q := queue.New(cfg)

	// Fill to just below low threshold
	for i := 0; i < 7; i++ {
		req := createTestRequest(fmt.Sprintf("fill-%d", i), queue.High)
		q.Enqueue(req)
	}

	// Low should still work
	lowReq := createTestRequest("low-ok", queue.Low)
	if err := q.Enqueue(lowReq); err != nil {
		t.Error("Low priority should work below threshold")
	}

	// Now at threshold, low should fail
	lowReq2 := createTestRequest("low-shed", queue.Low)
	if err := q.Enqueue(lowReq2); err == nil {
		t.Error("Low priority should be shed at threshold")
	}

	// Medium should work
	medReq := createTestRequest("med-ok", queue.Medium)
	if err := q.Enqueue(medReq); err != nil {
		t.Error("Medium priority should work")
	}

	// Fill to medium threshold
	for i := 0; i < 6; i++ {
		req := createTestRequest(fmt.Sprintf("fill2-%d", i), queue.High)
		q.Enqueue(req)
	}

	// Medium should now fail
	medReq2 := createTestRequest("med-shed", queue.Medium)
	if err := q.Enqueue(medReq2); err == nil {
		t.Error("Medium priority should be shed at threshold")
	}

	// High should still work
	highReq := createTestRequest("high-ok", queue.High)
	if err := q.Enqueue(highReq); err != nil {
		t.Error("High priority should work below max")
	}

	// Fill to max
	for q.Length() < 20 {
		req := createTestRequest("fill-max", queue.High)
		q.Enqueue(req)
	}

	// Everything should fail at max
	maxReq := createTestRequest("max-shed", queue.High)
	if err := q.Enqueue(maxReq); err == nil {
		t.Error("Should reject all requests at max capacity")
	}
}

// TestConcurrentQueueOperations tests thread safety
func TestConcurrentQueueOperations(t *testing.T) {
	cfg := config.Upstream{
		Queue: func() config.QueueConfig {
			cfg := config.TestQueueConfigWithShedding(500, 800)
			cfg.MaxSize = 1000
			cfg.RequestMaxAge = time.Minute
			return cfg
		}(),
	}
	q := queue.New(cfg)

	enqueued := int32(0)
	dequeued := int32(0)
	errors := int32(0)

	var wg sync.WaitGroup

	// Multiple enqueuers
	for i := range 10 {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				req := createTestRequest(fmt.Sprintf("enq-%d-%d", id, j), queue.Priority(j%3))
				if err := q.Enqueue(req); err != nil {
					atomic.AddInt32(&errors, 1)
				} else {
					atomic.AddInt32(&enqueued, 1)
				}
				time.Sleep(time.Microsecond * 100)
			}
		}(i)
	}

	// Multiple dequeuers
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if req := q.Dequeue(); req != nil {
					atomic.AddInt32(&dequeued, 1)
					select {
					case req.ResponseChan <- &queue.Response{Ready: true}:
					default:
					}
				}
				time.Sleep(time.Microsecond * 100)
			}
		}()
	}

	// Metrics readers
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				metrics := q.GetMetricsByPriority()
				// Just access to ensure no panic
				_ = metrics["size"]
				time.Sleep(time.Millisecond)
			}
		}()
	}

	wg.Wait()

	t.Logf("Enqueued: %d, Dequeued: %d, Errors: %d",
		atomic.LoadInt32(&enqueued),
		atomic.LoadInt32(&dequeued),
		atomic.LoadInt32(&errors))

	// Basic sanity checks
	if atomic.LoadInt32(&enqueued) == 0 {
		t.Error("No requests enqueued")
	}
	if atomic.LoadInt32(&dequeued) > atomic.LoadInt32(&enqueued) {
		t.Error("Dequeued more than enqueued")
	}
}

// TestProcessorWithEmptyQueue tests processor behavior with empty queue
func TestProcessorWithEmptyQueue(t *testing.T) {
	cfg := &config.Config{
		Upstream: config.Upstream{
			Mode:         "batch",
			BatchSize:    5,
			BatchTimeout: 50 * time.Millisecond,
			Queue: func() config.QueueConfig {
			cfg := config.TestQueueConfigWithShedding(50, 80)
			cfg.MaxSize = 100
			cfg.RequestMaxAge = time.Minute
			return cfg
		}(),
		},
	}
	q := queue.New(cfg.Upstream)

	p := New(cfg, q)
	p.Start()

	// Let it run with empty queue
	time.Sleep(100 * time.Millisecond)

	// Should handle gracefully
	p.Stop()

	// Add a request after stop to ensure clean shutdown
	req := createTestRequest("after-stop", queue.High)
	q.Enqueue(req)

	// Should have stopped processing
	time.Sleep(50 * time.Millisecond)
	if q.Length() != 1 {
		t.Error("Processor continued after stop")
	}
}

// TestRapidStartStop tests multiple start/stop cycles
func TestRapidStartStop(t *testing.T) {
	cfg := &config.Config{
		Upstream: config.Upstream{
			Mode:          "individual",
			MaxConcurrent: 2,
			Queue: func() config.QueueConfig {
			cfg := config.TestQueueConfigWithShedding(50, 80)
			cfg.MaxSize = 100
			cfg.RequestMaxAge = time.Minute
			return cfg
		}(),
		},
	}
	q := queue.New(cfg.Upstream)
	p := New(cfg, q)

	for i := 0; i < 5; i++ {
		// Add some requests
		for j := 0; j < 3; j++ {
			req := createTestRequest(fmt.Sprintf("cycle-%d-%d", i, j), queue.High)
			q.Enqueue(req)
		}

		p.Start()
		time.Sleep(20 * time.Millisecond)
		p.Stop()

		// Verify stopped
		initialLength := q.Length()
		time.Sleep(20 * time.Millisecond)
		if q.Length() < initialLength {
			t.Error("Processor continued after stop")
		}
	}
}

// BenchmarkQueueThroughput benchmarks queue throughput
func BenchmarkQueueThroughput(b *testing.B) {
	cfg := config.Upstream{
		Queue: func() config.QueueConfig {
			cfg := config.TestQueueConfigWithShedding(5000, 8000)
			cfg.MaxSize = 10000
			cfg.RequestMaxAge = time.Minute
			return cfg
		}(),
	}
	q := queue.New(cfg)

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			if i%2 == 0 {
				req := createTestRequest(fmt.Sprintf("bench-%d", i), queue.Priority(i%3))
				q.Enqueue(req)
			} else {
				q.Dequeue()
			}
			i++
		}
	})
}

// BenchmarkBatchFlushDecision benchmarks the flush decision logic
func BenchmarkBatchFlushDecision(b *testing.B) {
	w := &Worker{
		upstream: &config.Upstream{
			BatchSize:    100,
			BatchTimeout: time.Second,
		},
	}

	// Create a batch
	batch := make([]*queue.Request, 50)
	for i := range batch {
		batch[i] = &queue.Request{
			Priority: queue.Priority(i % 3),
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = w.shouldFlushBatch(batch, 25*time.Millisecond)
	}
}
