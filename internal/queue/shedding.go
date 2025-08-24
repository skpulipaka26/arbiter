package queue

import (
	"container/heap"
	"fmt"
	"time"
)

func (q *Queue) startMaintenanceWorker() {
	ticker := time.NewTicker(5 * time.Second) // Check every 5 seconds

	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				q.performMaintenance()
			case <-q.stopChan:
				return
			}
		}
	}()
}

func (q *Queue) performMaintenance() {
	q.mutex.Lock()
	defer q.mutex.Unlock()

	for upstreamName, pq := range q.queues {
		newQueue := &PriorityQueue{}

		// Rebuild queue without expired/shed requests
		for pq.Len() > 0 {
			req := (*pq)[0] // Peek at top

			// Check if should be shed
			if q.shouldShedRequestUnsafe(req) {
				// Remove and send error response
				heap.Pop(pq)
				req.ResponseChan <- &Response{
					StatusCode: 503,
					Error:      fmt.Errorf("request shed due to overload"),
				}
				req.Cancel()
			} else {
				// Keep the request
				heap.Push(newQueue, heap.Pop(pq))
			}
		}

		q.queues[upstreamName] = newQueue

		// Queue maintenance completed
	}
}

func (q *Queue) shouldShedRequestUnsafe(req *Request) bool {
	// Age-based shedding
	if time.Since(req.EnqueuedAt) > 30*time.Second {
		return true
	}

	// Context cancelled
	select {
	case <-req.Context().Done():
		return true
	default:
	}

	return false
}
