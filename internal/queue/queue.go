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

const (
	High Priority = iota
	Medium
	Low
)

func (p Priority) String() string {
	switch p {
	case High:
		return "high"
	case Medium:
		return "medium"
	case Low:
		return "low"
	default:
		return "unknown"
	}
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

func NewRequest(req *http.Request) *Request {
	return NewRequestWithContext(req.Context(), req)
}

func NewRequestWithContext(ctx context.Context, req *http.Request) *Request {
	ctx, cancel := context.WithCancel(ctx)

	return &Request{
		ID:           generateRequestID(),
		HTTPRequest:  req.WithContext(ctx),
		Priority:     extractPriority(req),
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
	queue  *PriorityQueue
	config *config.QueueConfig
	mutex  sync.RWMutex
}

func New(upstream config.Upstream) *Queue {
	pq := &PriorityQueue{}
	heap.Init(pq)

	q := &Queue{
		queue:  pq,
		config: &upstream.Queue,
	}

	return q
}

func (q *Queue) Enqueue(req *Request) error {
	q.mutex.Lock()
	defer q.mutex.Unlock()

	queueSize := q.queue.Len()
	if queueSize > q.config.LowPriorityShedAt && req.Priority == Low {
		return fmt.Errorf("request shed due to overload")
	}
	if queueSize > q.config.MediumPriorityShedAt && (req.Priority == Low || req.Priority == Medium) {
		return fmt.Errorf("request shed due to extreme overload")
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

func extractPriority(req *http.Request) Priority {
	priority := strings.ToLower(strings.TrimSpace(req.Header.Get("Priority")))

	switch priority {
	case "high", "urgent", "critical":
		return High
	case "medium", "normal", "standard":
		return Medium
	case "low", "background", "batch":
		return Low
	default:
		return Low
	}
}

func generateRequestID() string {
	return uuid.New().String()
}

func (q *Queue) GetMetricsByPriority() map[string]interface{} {
	q.mutex.RLock()
	defer q.mutex.RUnlock()

	high, medium, low := 0, 0, 0
	for _, req := range *q.queue {
		switch req.Priority {
		case High:
			high++
		case Medium:
			medium++
		case Low:
			low++
		}
	}

	return map[string]interface{}{
		"size":   q.queue.Len(),
		"high":   high,
		"medium": medium,
		"low":    low,
	}
}
