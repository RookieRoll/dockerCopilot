package container

import (
	"sync"
	"time"
)

type pullProgressThrottle struct {
	mu       sync.Mutex
	interval time.Duration
	last     time.Time
}

func newPullProgressThrottle(interval time.Duration) *pullProgressThrottle {
	return &pullProgressThrottle{interval: interval}
}

func (t *pullProgressThrottle) Allow(now time.Time) bool {
	if t == nil || t.interval <= 0 {
		return true
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.last.IsZero() || now.Sub(t.last) >= t.interval {
		t.last = now
		return true
	}
	return false
}
