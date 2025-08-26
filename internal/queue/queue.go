package queue

import (
	"container/heap"
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"arbiter/internal/config"

	"github.com/google/uuid"
)

type Priority int

// Default priority values for backward compatibility with tests
const (
	High   Priority = 0
	Medium Priority = 1
	Low    Priority = 2
)

func (p Priority) String() string {
	return fmt.Sprintf("priority-%d", p)
}

type Request struct {
	ID           string
	HTTPRequest  *http.Request
	Priority     Priority
	EnqueuedAt   time.Time
	ResponseChan chan *Response
	ctx          context.Context
	cancel       context.CancelFunc
}

type Response struct {
	Error       error
	Ready       bool // Indicates request is ready to be proxied
	UpstreamURL string
}

func (q *Queue) NewRequest(req *http.Request) *Request {
	return q.NewRequestWithContext(req.Context(), req)
}

func (q *Queue) NewRequestWithContext(ctx context.Context, req *http.Request) *Request {
	ctx, cancel := context.WithCancel(ctx)

	return &Request{
		ID:           generateRequestID(),
		HTTPRequest:  req.WithContext(ctx),
		Priority:     q.extractPriority(req),
		EnqueuedAt:   time.Now(),
		ResponseChan: make(chan *Response, 1),
		ctx:          ctx,
		cancel:       cancel,
	}
}

func (r *Request) Context() context.Context {
	return r.ctx
}

func (r *Request) Cancel() {
	if r.cancel != nil {
		r.cancel()
	}
}

// SetContextForTesting sets the context for testing purposes
func (r *Request) SetContextForTesting(ctx context.Context) {
	r.ctx = ctx
	r.cancel = func() {} // Dummy cancel for testing
}

type PriorityQueue []*Request

func (pq PriorityQueue) Len() int { return len(pq) }

func (pq PriorityQueue) Less(i, j int) bool {
	if pq[i] == nil || pq[j] == nil {
		return pq[i] != nil
	}

	if pq[i].Priority != pq[j].Priority {
		return pq[i].Priority < pq[j].Priority
	}

	return pq[i].EnqueuedAt.Before(pq[j].EnqueuedAt)
}

func (pq PriorityQueue) Swap(i, j int) { pq[i], pq[j] = pq[j], pq[i] }

func (pq *PriorityQueue) Push(x interface{}) {
	*pq = append(*pq, x.(*Request))
}

func (pq *PriorityQueue) Pop() interface{} {
	old := *pq
	n := len(old)
	if n == 0 {
		return nil
	}
	item := old[n-1]
	old[n-1] = nil
	*pq = old[0 : n-1]
	return item
}

func (pq *PriorityQueue) Peek() *Request {
	if len(*pq) == 0 {
		return nil
	}
	return (*pq)[0]
}

type Queue struct {
	queue      *PriorityQueue
	config     *config.QueueConfig
	priorities map[string]config.PriorityLevel // Map for fast lookup by name/alias
	mutex      sync.RWMutex
}

func New(upstream config.Upstream) *Queue {
	pq := &PriorityQueue{}
	heap.Init(pq)

	// Build priority lookup map
	priorities := make(map[string]config.PriorityLevel)
	for _, p := range upstream.Queue.Priorities {
		// Map the name
		priorities[strings.ToLower(p.Name)] = p
		// Map all aliases
		for _, alias := range p.Aliases {
			priorities[strings.ToLower(alias)] = p
		}
	}

	q := &Queue{
		queue:      pq,
		config:     &upstream.Queue,
		priorities: priorities,
	}

	return q
}

func (q *Queue) Enqueue(req *Request) error {
	q.mutex.Lock()
	defer q.mutex.Unlock()

	queueSize := q.queue.Len()

	// Check absolute max size first
	if queueSize >= q.config.MaxSize {
		return fmt.Errorf("queue full: at maximum capacity")
	}

	// Check priority-based shedding
	for _, p := range q.config.Priorities {
		if p.ShedAt > 0 && queueSize >= p.ShedAt && int(req.Priority) >= p.Value {
			return fmt.Errorf("request shed due to overload (priority %d, shed threshold %d)", req.Priority, p.ShedAt)
		}
	}

	heap.Push(q.queue, req)
	return nil
}

func (q *Queue) Dequeue() *Request {
	q.mutex.Lock()
	defer q.mutex.Unlock()

	// Keep checking until we find a valid request or queue is empty
	for q.queue.Len() > 0 {
		// Peek at the top request
		req := (*q.queue)[0]

		// Check if it should be shed
		if q.shouldShedRequest(req) {
			// Remove the stale/cancelled request
			heap.Pop(q.queue)
			// Notify asynchronously to avoid blocking
			go q.notifyShed(req)
			continue
		}

		// Valid request found, return it
		return heap.Pop(q.queue).(*Request)
	}

	return nil
}

func (q *Queue) Length() int {
	q.mutex.RLock()
	defer q.mutex.RUnlock()

	return q.queue.Len()
}

func (q *Queue) extractPriority(req *http.Request) Priority {
	priorityHeader := strings.ToLower(strings.TrimSpace(req.Header.Get("Priority")))

	// Look up priority in our configuration
	if p, ok := q.priorities[priorityHeader]; ok {
		return Priority(p.Value)
	}

	// Default to the highest priority value (lowest priority)
	maxValue := 0
	for _, p := range q.config.Priorities {
		if p.Value > maxValue {
			maxValue = p.Value
		}
	}
	return Priority(maxValue)
}

func generateRequestID() string {
	return uuid.New().String()
}

func (q *Queue) GetMetricsByPriority() map[string]interface{} {
	q.mutex.RLock()
	defer q.mutex.RUnlock()

	// Count requests by priority value
	counts := make(map[int]int)
	for _, req := range *q.queue {
		counts[int(req.Priority)]++
	}

	// Build metrics with priority names
	metrics := map[string]interface{}{
		"size": q.queue.Len(),
	}

	// Map counts back to priority names
	for _, p := range q.config.Priorities {
		metrics[p.Name] = counts[p.Value]
	}

	return metrics
}
