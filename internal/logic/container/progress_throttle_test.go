package container

import (
	"testing"
	"time"
)

func TestPullProgressThrottleCoalescesFrequentUpdates(t *testing.T) {
	start := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	throttle := newPullProgressThrottle(time.Second)
	if !throttle.Allow(start) {
		t.Fatal("first pull update should pass")
	}
	if throttle.Allow(start.Add(250 * time.Millisecond)) {
		t.Fatal("frequent pull update should be suppressed")
	}
	if !throttle.Allow(start.Add(time.Second)) {
		t.Fatal("update at the throttle interval should pass")
	}
}
