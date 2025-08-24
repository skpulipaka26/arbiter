package queue

import (
	"context"
	"testing"
	"time"

	"arbiter/internal/config"
)

// TestPriorityOrdering verifies that requests are dequeued in priority order
// and FIFO within same priority. This is critical for the entire system.
func TestPriorityOrdering(t *testing.T) {
	cfg := config.Upstream{
		Queue: config.QueueConfig{
			MaxSize:              100,
			LowPriorityShedAt:    50,
			MediumPriorityShedAt: 80,
			RequestMaxAge:        time.Minute,
		},
	}
	q := New(cfg)

	// Create requests with different priorities and timestamps
	requests := []struct {
		id       string
		priority Priority
		delay    time.Duration
	}{
		{"low1", Low, 0},
		{"high1", High, 10 * time.Millisecond},
		{"med1", Medium, 20 * time.Millisecond},
		{"low2", Low, 30 * time.Millisecond},
		{"high2", High, 40 * time.Millisecond},
		{"med2", Medium, 50 * time.Millisecond},
	}

	// Enqueue requests with delays to ensure different timestamps
	for _, r := range requests {
		time.Sleep(r.delay)
		req := &Request{
			ID:           r.id,
			Priority:     r.priority,
			EnqueuedAt:   time.Now(),
			HTTPRequest:  nil,
			ResponseChan: make(chan *Response, 1),
			ctx:          context.Background(),
		}
		if err := q.Enqueue(req); err != nil {
			t.Fatalf("Failed to enqueue %s: %v", r.id, err)
		}
	}

	// Expected dequeue order: high1, high2, med1, med2, low1, low2
	expectedOrder := []string{"high1", "high2", "med1", "med2", "low1", "low2"}

	for i, expected := range expectedOrder {
		req := q.Dequeue()
		if req == nil {
			t.Fatalf("Expected request %s at position %d, got nil", expected, i)
		}
		if req.ID != expected {
			t.Errorf("Position %d: expected %s, got %s", i, expected, req.ID)
		}
	}

	// Queue should be empty now
	if req := q.Dequeue(); req != nil {
		t.Errorf("Expected empty queue, got request %s", req.ID)
	}
}

// TestLoadShedding verifies that low priority requests are shed under load
// This protects the system from overload
func TestLoadShedding(t *testing.T) {
	cfg := config.Upstream{
		Queue: config.QueueConfig{
			MaxSize:              10,
			LowPriorityShedAt:    3,
			MediumPriorityShedAt: 7,
			RequestMaxAge:        time.Minute,
		},
	}
	q := New(cfg)

	// Fill queue to just below low priority threshold
	for i := 0; i < 3; i++ {
		req := &Request{
			ID:           "existing",
			Priority:     Low,
			EnqueuedAt:   time.Now(),
			ResponseChan: make(chan *Response, 1),
			ctx:          context.Background(),
		}
		if err := q.Enqueue(req); err != nil {
			t.Fatalf("Failed to enqueue: %v", err)
		}
	}

	// Low priority should be rejected now
	lowReq := &Request{
		ID:           "low-shed",
		Priority:     Low,
		EnqueuedAt:   time.Now(),
		ResponseChan: make(chan *Response, 1),
		ctx:          context.Background(),
	}
	if err := q.Enqueue(lowReq); err == nil {
		t.Error("Expected low priority to be shed, but it was accepted")
	}

	// Medium priority should still be accepted
	medReq := &Request{
		ID:           "medium-ok",
		Priority:     Medium,
		EnqueuedAt:   time.Now(),
		ResponseChan: make(chan *Response, 1),
		ctx:          context.Background(),
	}
	if err := q.Enqueue(medReq); err != nil {
		t.Errorf("Medium priority should be accepted: %v", err)
	}

	// Fill to medium threshold
	for i := 0; i < 3; i++ {
		req := &Request{
			ID:           "filler",
			Priority:     High,
			EnqueuedAt:   time.Now(),
			ResponseChan: make(chan *Response, 1),
			ctx:          context.Background(),
		}
		q.Enqueue(req)
	}

	// Now medium should also be rejected
	medReq2 := &Request{
		ID:           "medium-shed",
		Priority:     Medium,
		EnqueuedAt:   time.Now(),
		ResponseChan: make(chan *Response, 1),
		ctx:          context.Background(),
	}
	if err := q.Enqueue(medReq2); err == nil {
		t.Error("Expected medium priority to be shed at high load")
	}

	// High priority should still work
	highReq := &Request{
		ID:           "high-ok",
		Priority:     High,
		EnqueuedAt:   time.Now(),
		ResponseChan: make(chan *Response, 1),
		ctx:          context.Background(),
	}
	if err := q.Enqueue(highReq); err != nil {
		t.Errorf("High priority should be accepted: %v", err)
	}
}

// TestLazyStaleRemoval verifies that stale requests are removed during dequeue,
// not through periodic scanning
func TestLazyStaleRemoval(t *testing.T) {
	cfg := config.Upstream{
		Queue: config.QueueConfig{
			MaxSize:              100,
			LowPriorityShedAt:    50,
			MediumPriorityShedAt: 80,
			RequestMaxAge:        50 * time.Millisecond, // Very short for testing
		},
	}
	q := New(cfg)

	// Create a mix of stale and fresh requests
	staleReq := &Request{
		ID:           "stale",
		Priority:     High,
		EnqueuedAt:   time.Now().Add(-100 * time.Millisecond), // Already expired
		ResponseChan: make(chan *Response, 1),
		ctx:          context.Background(),
	}
	q.Enqueue(staleReq)

	freshReq := &Request{
		ID:           "fresh",
		Priority:     Low, // Lower priority but fresh
		EnqueuedAt:   time.Now(),
		ResponseChan: make(chan *Response, 1),
		ctx:          context.Background(),
	}
	q.Enqueue(freshReq)

	// Dequeue should skip stale and return fresh
	req := q.Dequeue()
	if req == nil {
		t.Fatal("Expected fresh request, got nil")
	}
	if req.ID != "fresh" {
		t.Errorf("Expected fresh request, got %s", req.ID)
	}

	// Verify stale request received shed notification
	select {
	case resp := <-staleReq.ResponseChan:
		if resp.Error == nil {
			t.Error("Expected error in shed response")
		}
		if resp.Ready {
			t.Error("Shed response should not be ready")
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("Stale request didn't receive shed notification")
	}
}

// TestCancelledRequestRemoval verifies cancelled contexts are handled
func TestCancelledRequestRemoval(t *testing.T) {
	cfg := config.Upstream{
		Queue: config.QueueConfig{
			MaxSize:              100,
			LowPriorityShedAt:    50,
			MediumPriorityShedAt: 80,
			RequestMaxAge:        time.Minute,
		},
	}
	q := New(cfg)

	// Create a request with cancellable context
	ctx, cancel := context.WithCancel(context.Background())
	cancelledReq := &Request{
		ID:           "cancelled",
		Priority:     High,
		EnqueuedAt:   time.Now(),
		ResponseChan: make(chan *Response, 1),
		ctx:          ctx,
	}
	q.Enqueue(cancelledReq)

	// Add a normal request
	normalReq := &Request{
		ID:           "normal",
		Priority:     Low,
		EnqueuedAt:   time.Now(),
		ResponseChan: make(chan *Response, 1),
		ctx:          context.Background(),
	}
	q.Enqueue(normalReq)

	// Cancel the first request
	cancel()

	// Dequeue should skip cancelled and return normal
	req := q.Dequeue()
	if req == nil {
		t.Fatal("Expected normal request, got nil")
	}
	if req.ID != "normal" {
		t.Errorf("Expected normal request, got %s", req.ID)
	}

	// Verify cancelled request received notification
	select {
	case resp := <-cancelledReq.ResponseChan:
		if resp.Error == nil {
			t.Error("Expected error for cancelled request")
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("Cancelled request didn't receive notification")
	}
}

// TestQueueMetrics verifies priority counting for observability
func TestQueueMetrics(t *testing.T) {
	cfg := config.Upstream{
		Queue: config.QueueConfig{
			MaxSize:              100,
			LowPriorityShedAt:    50,
			MediumPriorityShedAt: 80,
			RequestMaxAge:        time.Minute,
		},
	}
	q := New(cfg)

	// Add specific numbers of each priority
	priorities := []struct {
		priority Priority
		count    int
	}{
		{High, 2},
		{Medium, 3},
		{Low, 5},
	}

	for _, p := range priorities {
		for i := 0; i < p.count; i++ {
			req := &Request{
				ID:           "test",
				Priority:     p.priority,
				EnqueuedAt:   time.Now(),
				ResponseChan: make(chan *Response, 1),
				ctx:          context.Background(),
			}
			q.Enqueue(req)
		}
	}

	metrics := q.GetMetricsByPriority()

	if metrics["high"] != 2 {
		t.Errorf("Expected 2 high priority, got %v", metrics["high"])
	}
	if metrics["medium"] != 3 {
		t.Errorf("Expected 3 medium priority, got %v", metrics["medium"])
	}
	if metrics["low"] != 5 {
		t.Errorf("Expected 5 low priority, got %v", metrics["low"])
	}
	if metrics["size"] != 10 {
		t.Errorf("Expected total size 10, got %v", metrics["size"])
	}
}

// TestConcurrentAccess verifies thread-safety under concurrent operations
func TestConcurrentAccess(t *testing.T) {
	cfg := config.Upstream{
		Queue: config.QueueConfig{
			MaxSize:              1000,
			LowPriorityShedAt:    500,
			MediumPriorityShedAt: 800,
			RequestMaxAge:        time.Minute,
		},
	}
	q := New(cfg)

	// Run concurrent enqueues and dequeues
	done := make(chan bool)

	// Enqueuers
	for i := range 10 {
		go func(id int) {
			for j := 0; j < 100; j++ {
				req := &Request{
					ID:           "req",
					Priority:     Priority(id % 3),
					EnqueuedAt:   time.Now(),
					ResponseChan: make(chan *Response, 1),
					ctx:          context.Background(),
				}
				q.Enqueue(req)
				time.Sleep(time.Microsecond)
			}
			done <- true
		}(i)
	}

	// Dequeuers
	for i := 0; i < 5; i++ {
		go func() {
			for j := 0; j < 200; j++ {
				q.Dequeue()
				time.Sleep(time.Microsecond)
			}
			done <- true
		}()
	}

	// Wait for all goroutines
	for i := 0; i < 15; i++ {
		<-done
	}

	// No panics = success for concurrent access test
}
