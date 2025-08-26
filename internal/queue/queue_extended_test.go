package queue

import (
	"context"
	"fmt"
	"net/http"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"arbiter/internal/config"
)

// TestQueueOverflowBehavior tests queue behavior at capacity limits
func TestQueueOverflowBehavior(t *testing.T) {
	tests := []struct {
		name     string
		setup    func() *Queue
		actions  func(*testing.T, *Queue)
		scenario string
	}{
		{
			name: "exact_capacity",
			setup: func() *Queue {
				cfg := config.Upstream{
					Queue: func() config.QueueConfig {
						cfg := config.TestQueueConfigWithShedding(5, 8)
						cfg.MaxSize = 10
						cfg.RequestMaxAge = time.Minute
						return cfg
					}(),
				}
				return New(cfg)
			},
			actions: func(t *testing.T, q *Queue) {
				// Fill to exact capacity
				for i := 0; i < 10; i++ {
					req := &Request{
						ID:           fmt.Sprintf("req-%d", i),
						Priority:     High, // High priority to avoid shedding
						EnqueuedAt:   time.Now(),
						ResponseChan: make(chan *Response, 1),
						ctx:          context.Background(),
					}
					if err := q.Enqueue(req); err != nil {
						t.Errorf("Failed to enqueue at position %d: %v", i, err)
					}
				}

				// Queue should be at max capacity
				if q.Length() != 10 {
					t.Errorf("Expected queue length 10, got %d", q.Length())
				}

				// Next enqueue should fail
				req := &Request{
					ID:           "overflow",
					Priority:     High,
					EnqueuedAt:   time.Now(),
					ResponseChan: make(chan *Response, 1),
					ctx:          context.Background(),
				}
				if err := q.Enqueue(req); err == nil {
					t.Error("Expected enqueue to fail at max capacity")
				}
			},
			scenario: "Queue should reject requests at max capacity",
		},
		{
			name: "graduated_shedding",
			setup: func() *Queue {
				cfg := config.Upstream{
					Queue: func() config.QueueConfig {
						cfg := config.TestQueueConfigWithShedding(8, 15)
						cfg.MaxSize = 20
						cfg.RequestMaxAge = time.Minute
						return cfg
					}(),
				}
				return New(cfg)
			},
			actions: func(t *testing.T, q *Queue) {
				// Fill to just below low priority threshold
				for i := 0; i < 7; i++ {
					req := &Request{
						ID:           fmt.Sprintf("initial-%d", i),
						Priority:     High,
						EnqueuedAt:   time.Now(),
						ResponseChan: make(chan *Response, 1),
						ctx:          context.Background(),
					}
					q.Enqueue(req)
				}

				// Low priority should still work
				lowReq := &Request{
					ID:           "low-ok",
					Priority:     Low,
					EnqueuedAt:   time.Now(),
					ResponseChan: make(chan *Response, 1),
					ctx:          context.Background(),
				}
				if err := q.Enqueue(lowReq); err != nil {
					t.Error("Low priority should be accepted below threshold")
				}

				// Now at threshold, low should be rejected
				lowReq2 := &Request{
					ID:           "low-shed",
					Priority:     Low,
					EnqueuedAt:   time.Now(),
					ResponseChan: make(chan *Response, 1),
					ctx:          context.Background(),
				}
				if err := q.Enqueue(lowReq2); err == nil {
					t.Error("Low priority should be shed at threshold")
				}

				// Medium should still work
				medReq := &Request{
					ID:           "med-ok",
					Priority:     Medium,
					EnqueuedAt:   time.Now(),
					ResponseChan: make(chan *Response, 1),
					ctx:          context.Background(),
				}
				if err := q.Enqueue(medReq); err != nil {
					t.Error("Medium priority should be accepted")
				}

				// Fill to medium threshold
				for i := range 6 {
					req := &Request{
						ID:           fmt.Sprintf("fill-%d", i),
						Priority:     High,
						EnqueuedAt:   time.Now(),
						ResponseChan: make(chan *Response, 1),
						ctx:          context.Background(),
					}
					q.Enqueue(req)
				}

				// Medium should now be rejected
				medReq2 := &Request{
					ID:           "med-shed",
					Priority:     Medium,
					EnqueuedAt:   time.Now(),
					ResponseChan: make(chan *Response, 1),
					ctx:          context.Background(),
				}
				if err := q.Enqueue(medReq2); err == nil {
					t.Error("Medium priority should be shed at threshold")
				}

				// High should still work
				highReq := &Request{
					ID:           "high-ok",
					Priority:     High,
					EnqueuedAt:   time.Now(),
					ResponseChan: make(chan *Response, 1),
					ctx:          context.Background(),
				}
				if err := q.Enqueue(highReq); err != nil {
					t.Error("High priority should be accepted")
				}
			},
			scenario: "Graduated shedding should work correctly",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := tt.setup()
			tt.actions(t, q)
		})
	}
}

// TestRapidEnqueueDequeue tests high-frequency operations
func TestRapidEnqueueDequeue(t *testing.T) {
	cfg := config.Upstream{
		Queue: func() config.QueueConfig {
			cfg := config.TestQueueConfigWithShedding(500, 800)
			cfg.MaxSize = 1000
			cfg.RequestMaxAge = time.Minute
			return cfg
		}(),
	}
	q := New(cfg)

	// Track operations
	enqueued := int32(0)
	dequeued := int32(0)
	errors := int32(0)

	// Start multiple enqueuers
	var wg sync.WaitGroup
	for i := range 10 {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				req := &Request{
					ID:           fmt.Sprintf("enq-%d-%d", id, j),
					Priority:     Priority(j % 3),
					EnqueuedAt:   time.Now(),
					ResponseChan: make(chan *Response, 1),
					ctx:          context.Background(),
				}
				if err := q.Enqueue(req); err != nil {
					atomic.AddInt32(&errors, 1)
				} else {
					atomic.AddInt32(&enqueued, 1)
				}
			}
		}(i)
	}

	// Start multiple dequeuers
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				if req := q.Dequeue(); req != nil {
					atomic.AddInt32(&dequeued, 1)
					// Send response
					select {
					case req.ResponseChan <- &Response{Ready: true}:
					default:
					}
				}
				time.Sleep(time.Microsecond) // Small delay
			}
		}(i)
	}

	wg.Wait()

	// Verify counts
	finalEnqueued := atomic.LoadInt32(&enqueued)
	finalDequeued := atomic.LoadInt32(&dequeued)
	finalErrors := atomic.LoadInt32(&errors)

	t.Logf("Enqueued: %d, Dequeued: %d, Errors: %d", finalEnqueued, finalDequeued, finalErrors)

	if finalEnqueued == 0 {
		t.Error("No requests were enqueued")
	}
	if finalDequeued == 0 {
		t.Error("No requests were dequeued")
	}
	if finalEnqueued < finalDequeued {
		t.Error("Dequeued more than enqueued")
	}
}

// TestStaleRequestHandling tests handling of expired requests
func TestStaleRequestHandling(t *testing.T) {
	cfg := config.Upstream{
		Queue: func() config.QueueConfig {
			cfg := config.TestQueueConfigWithShedding(50, 80)
			cfg.MaxSize = 100
			cfg.RequestMaxAge = 50 * time.Millisecond
			return cfg
		}(),
	}
	q := New(cfg)

	// Add requests with different ages
	oldReq := &Request{
		ID:           "old",
		Priority:     High,
		EnqueuedAt:   time.Now().Add(-100 * time.Millisecond), // Already expired
		ResponseChan: make(chan *Response, 1),
		ctx:          context.Background(),
	}
	q.Enqueue(oldReq)

	freshReq := &Request{
		ID:           "fresh",
		Priority:     Low,
		EnqueuedAt:   time.Now(),
		ResponseChan: make(chan *Response, 1),
		ctx:          context.Background(),
	}
	q.Enqueue(freshReq)

	// Add a request that will expire soon
	soonExpireReq := &Request{
		ID:           "soon-expire",
		Priority:     Medium,
		EnqueuedAt:   time.Now().Add(-40 * time.Millisecond),
		ResponseChan: make(chan *Response, 1),
		ctx:          context.Background(),
	}
	q.Enqueue(soonExpireReq)

	// First dequeue should skip old (expired) and return soon-expire (Medium priority)
	req := q.Dequeue()
	if req == nil {
		t.Fatal("Expected to dequeue soon-expire request")
	}
	if req.ID != "soon-expire" {
		t.Errorf("Expected soon-expire request (medium priority), got %s", req.ID)
	}

	// Old request should have received shed notification
	select {
	case resp := <-oldReq.ResponseChan:
		if resp.Ready {
			t.Error("Old request should not be ready")
		}
		if resp.Error == nil {
			t.Error("Expected error for expired request")
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("Old request didn't receive shed notification")
	}

	// Next dequeue should return fresh (Low priority)
	req = q.Dequeue()
	if req == nil {
		t.Fatal("Expected to dequeue fresh request")
	}
	if req.ID != "fresh" {
		t.Errorf("Expected fresh request, got %s", req.ID)
	}

	// Queue should be empty now
	req = q.Dequeue()
	if req != nil {
		t.Errorf("Expected nil (empty queue), got %s", req.ID)
	}
}

// TestCancelledContextHandling tests handling of cancelled contexts
func TestCancelledContextHandling(t *testing.T) {
	cfg := config.Upstream{
		Queue: func() config.QueueConfig {
			cfg := config.TestQueueConfigWithShedding(50, 80)
			cfg.MaxSize = 100
			cfg.RequestMaxAge = time.Minute
			return cfg
		}(),
	}
	q := New(cfg)

	// Create requests with cancellable contexts
	ctx1, cancel1 := context.WithCancel(context.Background())
	req1 := &Request{
		ID:           "cancel-1",
		Priority:     High,
		EnqueuedAt:   time.Now(),
		ResponseChan: make(chan *Response, 1),
		ctx:          ctx1,
	}
	q.Enqueue(req1)

	ctx2, cancel2 := context.WithCancel(context.Background())
	req2 := &Request{
		ID:           "cancel-2",
		Priority:     Medium,
		EnqueuedAt:   time.Now(),
		ResponseChan: make(chan *Response, 1),
		ctx:          ctx2,
	}
	q.Enqueue(req2)

	req3 := &Request{
		ID:           "normal",
		Priority:     Low,
		EnqueuedAt:   time.Now(),
		ResponseChan: make(chan *Response, 1),
		ctx:          context.Background(),
	}
	q.Enqueue(req3)

	// Cancel first two requests
	cancel1()
	cancel2()

	// Dequeue should skip cancelled and return normal
	req := q.Dequeue()
	if req == nil {
		t.Fatal("Expected to dequeue normal request")
	}
	if req.ID != "normal" {
		t.Errorf("Expected normal request, got %s", req.ID)
	}

	// Cancelled requests should have received notifications
	for _, r := range []*Request{req1, req2} {
		select {
		case resp := <-r.ResponseChan:
			if resp.Ready {
				t.Errorf("Cancelled request %s should not be ready", r.ID)
			}
			if resp.Error == nil {
				t.Errorf("Expected error for cancelled request %s", r.ID)
			}
		case <-time.After(100 * time.Millisecond):
			t.Errorf("Cancelled request %s didn't receive notification", r.ID)
		}
	}
}

// TestQueueMetricsAccuracy verifies metrics are accurate
func TestQueueMetricsAccuracy(t *testing.T) {
	cfg := config.Upstream{
		Queue: func() config.QueueConfig {
			cfg := config.TestQueueConfigWithShedding(50, 80)
			cfg.MaxSize = 100
			cfg.RequestMaxAge = time.Minute
			return cfg
		}(),
	}
	q := New(cfg)

	// Add specific numbers of each priority
	expected := map[string]int{
		"high":   5,
		"medium": 3,
		"low":    7,
	}

	for priority, count := range expected {
		var p Priority
		switch priority {
		case "high":
			p = High
		case "medium":
			p = Medium
		case "low":
			p = Low
		}

		for i := 0; i < count; i++ {
			req := &Request{
				ID:           fmt.Sprintf("%s-%d", priority, i),
				Priority:     p,
				EnqueuedAt:   time.Now(),
				ResponseChan: make(chan *Response, 1),
				ctx:          context.Background(),
			}
			q.Enqueue(req)
		}
	}

	metrics := q.GetMetricsByPriority()

	// Verify counts
	for priority, expectedCount := range expected {
		if metrics[priority] != expectedCount {
			t.Errorf("Priority %s: expected %d, got %v", priority, expectedCount, metrics[priority])
		}
	}

	totalExpected := expected["high"] + expected["medium"] + expected["low"]
	if metrics["size"] != totalExpected {
		t.Errorf("Total size: expected %d, got %v", totalExpected, metrics["size"])
	}

	// Dequeue some and verify metrics update
	for i := 0; i < 5; i++ {
		q.Dequeue()
	}

	metricsAfter := q.GetMetricsByPriority()
	if metricsAfter["size"] != totalExpected-5 {
		t.Errorf("After dequeue size: expected %d, got %v", totalExpected-5, metricsAfter["size"])
	}
}

// TestHeapInvariant verifies the heap maintains its invariant
func TestHeapInvariant(t *testing.T) {
	cfg := config.Upstream{
		Queue: func() config.QueueConfig {
			cfg := config.TestQueueConfigWithShedding(50, 80)
			cfg.MaxSize = 100
			cfg.RequestMaxAge = time.Minute
			return cfg
		}(),
	}
	q := New(cfg)

	// Add requests in random order
	priorities := []Priority{Low, High, Medium, Low, High, Medium, High, Low}
	for i, p := range priorities {
		req := &Request{
			ID:           fmt.Sprintf("req-%d", i),
			Priority:     p,
			EnqueuedAt:   time.Now().Add(time.Duration(i) * time.Millisecond),
			ResponseChan: make(chan *Response, 1),
			ctx:          context.Background(),
		}
		q.Enqueue(req)
	}

	// Dequeue all and verify order
	var lastPriority Priority = High - 1 // Start with value better than High
	var lastTime time.Time

	for {
		req := q.Dequeue()
		if req == nil {
			break
		}

		// Verify priority order
		if req.Priority < lastPriority {
			t.Errorf("Priority order violated: %v after %v", req.Priority, lastPriority)
		}

		// Within same priority, verify FIFO
		if req.Priority == lastPriority && req.EnqueuedAt.Before(lastTime) {
			t.Error("FIFO order violated within same priority")
		}

		lastPriority = req.Priority
		if req.Priority == lastPriority {
			lastTime = req.EnqueuedAt
		} else {
			lastTime = time.Time{}
		}
	}
}

// TestConcurrentMetrics tests metrics under concurrent access
func TestConcurrentMetrics(t *testing.T) {
	cfg := config.Upstream{
		Queue: func() config.QueueConfig {
			cfg := config.TestQueueConfigWithShedding(500, 800)
			cfg.MaxSize = 1000
			cfg.RequestMaxAge = time.Minute
			return cfg
		}(),
	}
	q := New(cfg)

	var wg sync.WaitGroup

	// Concurrent enqueuers
	for i := range 5 {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := range 20 {
				req := &Request{
					ID:           fmt.Sprintf("enq-%d-%d", id, j),
					Priority:     Priority(j % 3),
					EnqueuedAt:   time.Now(),
					ResponseChan: make(chan *Response, 1),
					ctx:          context.Background(),
				}
				q.Enqueue(req)
			}
		}(i)
	}

	// Concurrent metrics readers
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				metrics := q.GetMetricsByPriority()
				// Just access metrics to ensure no panic
				_ = metrics["size"]
				_ = metrics["high"]
				_ = metrics["medium"]
				_ = metrics["low"]
				time.Sleep(time.Millisecond)
			}
		}()
	}

	// Concurrent dequeuers
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 30; j++ {
				q.Dequeue()
				time.Sleep(time.Millisecond)
			}
		}()
	}

	wg.Wait()

	// Final metrics should be consistent
	metrics := q.GetMetricsByPriority()
	total := metrics["high"].(int) + metrics["medium"].(int) + metrics["low"].(int)
	if total != metrics["size"].(int) {
		t.Errorf("Metrics inconsistent: high+medium+low=%d, size=%d", total, metrics["size"])
	}
}

// TestMemoryManagement tests queue doesn't leak memory
func TestMemoryManagement(t *testing.T) {
	cfg := config.Upstream{
		Queue: func() config.QueueConfig {
			cfg := config.TestQueueConfigWithShedding(500, 800)
			cfg.MaxSize = 1000
			cfg.RequestMaxAge = time.Minute
			return cfg
		}(),
	}
	q := New(cfg)

	// Get initial memory stats
	var m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m1)

	// Perform many enqueue/dequeue cycles
	for cycle := range 100 {
		// Enqueue batch
		for i := 0; i < 100; i++ {
			req := &Request{
				ID:           fmt.Sprintf("cycle-%d-req-%d", cycle, i),
				Priority:     Priority(i % 3),
				EnqueuedAt:   time.Now(),
				ResponseChan: make(chan *Response, 1),
				ctx:          context.Background(),
			}
			q.Enqueue(req)
		}

		// Dequeue all
		for q.Length() > 0 {
			if req := q.Dequeue(); req != nil {
				// Close response channel to free resources
				close(req.ResponseChan)
			}
		}
	}

	// Get final memory stats
	runtime.GC()
	var m2 runtime.MemStats
	runtime.ReadMemStats(&m2)

	// Memory usage shouldn't grow significantly
	heapGrowth := int64(m2.HeapAlloc) - int64(m1.HeapAlloc)
	if heapGrowth > 10*1024*1024 { // 10MB threshold
		t.Errorf("Excessive heap growth: %d bytes", heapGrowth)
	}

	// Queue should be empty
	if q.Length() != 0 {
		t.Errorf("Queue not empty after cycles: %d items", q.Length())
	}
}

// TestEdgeCasePriorities tests boundary priority values
func TestEdgeCasePriorities(t *testing.T) {
	cfg := config.Upstream{
		Queue: func() config.QueueConfig {
			cfg := config.TestQueueConfigWithShedding(50, 80)
			cfg.MaxSize = 100
			cfg.RequestMaxAge = time.Minute
			return cfg
		}(),
	}
	q := New(cfg)

	// Test with extreme priority values
	req1 := &Request{
		ID:           "priority-0",
		Priority:     0, // High
		EnqueuedAt:   time.Now(),
		ResponseChan: make(chan *Response, 1),
		ctx:          context.Background(),
	}
	q.Enqueue(req1)

	req2 := &Request{
		ID:           "priority-max",
		Priority:     Priority(^uint(0) >> 1), // Max int value
		EnqueuedAt:   time.Now(),
		ResponseChan: make(chan *Response, 1),
		ctx:          context.Background(),
	}
	if err := q.Enqueue(req2); err != nil {
		t.Logf("Expected behavior: invalid priority rejected: %v", err)
	}

	// First should be high priority
	dequeued := q.Dequeue()
	if dequeued == nil || dequeued.ID != "priority-0" {
		t.Error("High priority (0) request should be dequeued first")
	}
}

// TestQueueShutdown tests graceful shutdown
func TestQueueShutdown(t *testing.T) {
	cfg := config.Upstream{
		Queue: func() config.QueueConfig {
			cfg := config.TestQueueConfigWithShedding(50, 80)
			cfg.MaxSize = 100
			cfg.RequestMaxAge = time.Minute
			return cfg
		}(),
	}
	q := New(cfg)

	// Add requests
	for i := 0; i < 10; i++ {
		req := &Request{
			ID:           fmt.Sprintf("req-%d", i),
			Priority:     Priority(i % 3),
			EnqueuedAt:   time.Now(),
			ResponseChan: make(chan *Response, 1),
			ctx:          context.Background(),
		}
		q.Enqueue(req)
	}

	// Test behavior after clearing the queue
	for q.Length() > 0 {
		q.Dequeue()
	}

	// Queue should be empty
	if q.Length() != 0 {
		t.Error("Queue should be empty after draining")
	}

	// Can still enqueue after draining
	req := &Request{
		ID:           "after-drain",
		Priority:     High,
		EnqueuedAt:   time.Now(),
		ResponseChan: make(chan *Response, 1),
		ctx:          context.Background(),
	}

	if err := q.Enqueue(req); err != nil {
		t.Errorf("Should be able to enqueue after draining: %v", err)
	}

	// Can dequeue the new request
	if dequeued := q.Dequeue(); dequeued == nil || dequeued.ID != "after-drain" {
		t.Error("Should be able to dequeue after re-enqueueing")
	}
}

// TestRequestExtraction tests extracting requests from HTTP
func TestRequestExtraction(t *testing.T) {
	tests := []struct {
		name             string
		headers          map[string]string
		expectedPriority Priority
	}{
		{
			name:             "high_priority_header",
			headers:          map[string]string{"Priority": "high"},
			expectedPriority: High,
		},
		{
			name:             "urgent_maps_to_high",
			headers:          map[string]string{"Priority": "urgent"},
			expectedPriority: High,
		},
		{
			name:             "medium_priority_header",
			headers:          map[string]string{"Priority": "medium"},
			expectedPriority: Medium,
		},
		{
			name:             "normal_maps_to_medium",
			headers:          map[string]string{"Priority": "normal"},
			expectedPriority: Medium,
		},
		{
			name:             "low_priority_header",
			headers:          map[string]string{"Priority": "low"},
			expectedPriority: Low,
		},
		{
			name:             "batch_maps_to_low",
			headers:          map[string]string{"Priority": "batch"},
			expectedPriority: Low,
		},
		{
			name:             "no_header_defaults_to_low",
			headers:          map[string]string{},
			expectedPriority: Low,
		},
		{
			name:             "invalid_priority_defaults_to_low",
			headers:          map[string]string{"Priority": "super-duper-high"},
			expectedPriority: Low,
		},
		{
			name:             "case_insensitive",
			headers:          map[string]string{"Priority": "HIGH"},
			expectedPriority: High,
		},
	}

	// Create a queue with test config
	cfg := config.Upstream{
		Queue: config.TestQueueConfig(),
	}
	q := New(cfg)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, _ := http.NewRequest("GET", "/test", nil)
			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}

			queueReq := q.NewRequest(req)
			if queueReq.Priority != tt.expectedPriority {
				t.Errorf("Expected priority %v, got %v", tt.expectedPriority, queueReq.Priority)
			}
		})
	}
}

// BenchmarkEnqueue benchmarks enqueue performance
func BenchmarkEnqueue(b *testing.B) {
	cfg := config.Upstream{
		Queue: func() config.QueueConfig {
			cfg := config.TestQueueConfigWithShedding(5000, 8000)
			cfg.MaxSize = 10000
			cfg.RequestMaxAge = time.Minute
			return cfg
		}(),
	}
	q := New(cfg)

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		req := &Request{
			ID:           fmt.Sprintf("bench-%d", i),
			Priority:     Priority(i % 3),
			EnqueuedAt:   time.Now(),
			ResponseChan: make(chan *Response, 1),
			ctx:          context.Background(),
		}
		q.Enqueue(req)
	}
}

// BenchmarkDequeue benchmarks dequeue performance
func BenchmarkDequeue(b *testing.B) {
	cfg := config.Upstream{
		Queue: func() config.QueueConfig {
			cfg := config.TestQueueConfigWithShedding(5000, 8000)
			cfg.MaxSize = 10000
			cfg.RequestMaxAge = time.Minute
			return cfg
		}(),
	}
	q := New(cfg)

	// Pre-fill queue
	for i := 0; i < b.N; i++ {
		req := &Request{
			ID:           fmt.Sprintf("bench-%d", i),
			Priority:     Priority(i % 3),
			EnqueuedAt:   time.Now(),
			ResponseChan: make(chan *Response, 1),
			ctx:          context.Background(),
		}
		q.Enqueue(req)
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		q.Dequeue()
	}
}

// BenchmarkConcurrentOperations benchmarks concurrent enqueue/dequeue
func BenchmarkConcurrentOperations(b *testing.B) {
	cfg := config.Upstream{
		Queue: func() config.QueueConfig {
			cfg := config.TestQueueConfigWithShedding(5000, 8000)
			cfg.MaxSize = 10000
			cfg.RequestMaxAge = time.Minute
			return cfg
		}(),
	}
	q := New(cfg)

	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			if i%2 == 0 {
				req := &Request{
					ID:           fmt.Sprintf("bench-%d", i),
					Priority:     Priority(i % 3),
					EnqueuedAt:   time.Now(),
					ResponseChan: make(chan *Response, 1),
					ctx:          context.Background(),
				}
				q.Enqueue(req)
			} else {
				q.Dequeue()
			}
			i++
		}
	})
}

// TestQueueWithRealHTTPRequests tests with actual HTTP requests
func TestQueueWithRealHTTPRequests(t *testing.T) {
	cfg := config.Upstream{
		Queue: func() config.QueueConfig {
			cfg := config.TestQueueConfigWithShedding(50, 80)
			cfg.MaxSize = 100
			cfg.RequestMaxAge = time.Minute
			return cfg
		}(),
	}
	q := New(cfg)

	// Create various HTTP requests
	requests := []struct {
		method   string
		path     string
		priority string
	}{
		{"GET", "/api/health", "high"},
		{"POST", "/api/users", "medium"},
		{"GET", "/api/analytics", "low"},
		{"DELETE", "/api/cache", "high"},
		{"PUT", "/api/config", "medium"},
	}

	for _, r := range requests {
		httpReq, err := http.NewRequest(r.method, r.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		httpReq.Header.Set("Priority", r.priority)

		queueReq := q.NewRequest(httpReq)
		if err := q.Enqueue(queueReq); err != nil {
			t.Errorf("Failed to enqueue %s %s: %v", r.method, r.path, err)
		}
	}

	// Verify requests are dequeued in priority order
	expectedOrder := []string{
		"/api/health",    // high
		"/api/cache",     // high
		"/api/users",     // medium
		"/api/config",    // medium
		"/api/analytics", // low
	}

	for _, expected := range expectedOrder {
		req := q.Dequeue()
		if req == nil {
			t.Fatal("Unexpected nil dequeue")
		}
		if req.HTTPRequest.URL.Path != expected {
			t.Errorf("Expected %s, got %s", expected, req.HTTPRequest.URL.Path)
		}
	}
}
