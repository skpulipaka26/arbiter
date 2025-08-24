package queue

import (
	"fmt"
	"time"
)

func (q *Queue) notifyShed(req *Request) {
	select {
	case req.ResponseChan <- &Response{
		Error: fmt.Errorf("request shed: expired or cancelled"),
		Ready: false,
	}:
	default:
		// Channel might be closed or full, ignore
	}
	req.Cancel()
}

func (q *Queue) shouldShedRequest(req *Request) bool {
	// Check if request has expired
	if time.Since(req.EnqueuedAt) > q.config.RequestMaxAge {
		return true
	}

	// Check if request context is cancelled
	select {
	case <-req.Context().Done():
		return true
	default:
	}

	return false
}
