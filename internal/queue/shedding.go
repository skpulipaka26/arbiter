package queue

import (
	"container/heap"
	"fmt"
	"time"
)

func (q *Queue) startMaintenanceWorker() {
	ticker := time.NewTicker(5 * time.Second)

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

		for pq.Len() > 0 {
			req := (*pq)[0]

			if q.shouldShedRequestUnsafe(req) {
				heap.Pop(pq)
				req.ResponseChan <- &Response{
					Error: fmt.Errorf("request shed due to overload"),
					Ready: false,
				}
				req.Cancel()
			} else {
				heap.Push(newQueue, heap.Pop(pq))
			}
		}

		q.queues[upstreamName] = newQueue

	}
}

func (q *Queue) shouldShedRequestUnsafe(req *Request) bool {
	cfg, exists := q.configs[req.UpstreamName]
	if !exists {
		return true
	}
	
	if time.Since(req.EnqueuedAt) > cfg.RequestMaxAge {
		return true
	}

	select {
	case <-req.Context().Done():
		return true
	default:
	}

	return false
}
