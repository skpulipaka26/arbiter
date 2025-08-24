package queue

import (
	"container/heap"
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
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
	UpstreamName string
	Priority     Priority
	EnqueuedAt   time.Time
	ResponseChan chan *Response
	ctx          context.Context
	cancel       context.CancelFunc
}

type Response struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
	Error      error
}

func NewRequest(req *http.Request) *Request {
	ctx, cancel := context.WithCancel(req.Context())
	
	return &Request{
		ID:           generateRequestID(),
		HTTPRequest:  req.WithContext(ctx),
		UpstreamName: extractUpstream(req),
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

// Priority queue implementation
type PriorityQueue []*Request

func (pq PriorityQueue) Len() int { return len(pq) }

func (pq PriorityQueue) Less(i, j int) bool {
	if pq[i] == nil || pq[j] == nil {
		return pq[i] != nil
	}
	
	// Higher priority (lower numeric value) goes first
	if pq[i].Priority != pq[j].Priority {
		return pq[i].Priority < pq[j].Priority
	}
	
	// FIFO for same priority
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

// Queue manager
type Queue struct {
	queues   map[string]*PriorityQueue
	mutex    sync.RWMutex
	stopChan chan struct{}
}

func New() *Queue {
	q := &Queue{
		queues:   make(map[string]*PriorityQueue),
		stopChan: make(chan struct{}),
	}
	
	// Start background maintenance for queue shedding
	q.startMaintenanceWorker()
	
	return q
}

// Shutdown stops the queue manager gracefully
func (q *Queue) Shutdown() {
	close(q.stopChan)
}

func (q *Queue) Enqueue(req *Request) error {
	q.mutex.Lock()
	defer q.mutex.Unlock()
	
	// Get or create queue for upstream
	pq, exists := q.queues[req.UpstreamName]
	if !exists {
		pq = &PriorityQueue{}
		heap.Init(pq)
		q.queues[req.UpstreamName] = pq
	}
	
	// Check if request should be shed (use internal length since we have lock)
	queueSize := pq.Len()
	if queueSize > 500 && req.Priority == Low {
		return fmt.Errorf("request shed due to overload")
	}
	if queueSize > 800 && (req.Priority == Low || req.Priority == Medium) {
		return fmt.Errorf("request shed due to extreme overload")
	}
	
	heap.Push(pq, req)
	return nil
}

func (q *Queue) Dequeue(upstreamName string) *Request {
	q.mutex.Lock()
	defer q.mutex.Unlock()
	
	pq, exists := q.queues[upstreamName]
	if !exists || pq.Len() == 0 {
		return nil
	}
	
	req := heap.Pop(pq).(*Request)
	return req
}

func (q *Queue) Length(upstreamName string) int {
	q.mutex.RLock()
	defer q.mutex.RUnlock()
	
	if pq, exists := q.queues[upstreamName]; exists {
		return pq.Len()
	}
	return 0
}

// Helper functions
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
		return Low // Default to low priority
	}
}

func extractUpstream(req *http.Request) string {
	upstream := strings.TrimSpace(req.Header.Get("Upstream"))
	if upstream == "" {
		return "default"
	}
	
	// Basic validation
	if len(upstream) > 100 {
		return "default"
	}
	
	// Only allow alphanumeric, dash, underscore
	for _, r := range upstream {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || 
			 (r >= '0' && r <= '9') || r == '-' || r == '_') {
			return "default"
		}
	}
	
	return upstream
}

func generateRequestID() string {
	return time.Now().Format("20060102150405.000000")
}

func (q *Queue) GetMetricsByPriority(upstreamName string) map[string]interface{} {
	q.mutex.RLock()
	defer q.mutex.RUnlock()
	
	pq, exists := q.queues[upstreamName]
	if !exists {
		return map[string]interface{}{
			"size": 0,
			"high": 0,
			"medium": 0,
			"low": 0,
		}
	}
	
	high, medium, low := 0, 0, 0
	for _, req := range *pq {
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
		"size": pq.Len(),
		"high": high,
		"medium": medium,
		"low": low,
	}
}