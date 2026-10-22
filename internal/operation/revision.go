package operation

import "errors"

// CaptureVersion returns a monotonic generation for reservation, confirmation,
// alias, and release changes. Readers can compare it after an external
// snapshot to detect mutations that began and ended during that snapshot.
func (c *Coordinator) CaptureVersion() uint64 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.revision
}

// WithStableVersion runs a short commit callback while mutation admission is
// excluded, but only when no coordinator change occurred since CaptureVersion.
func (c *Coordinator) WithStableVersion(version uint64, commit func() error) (bool, error) {
	if c == nil || commit == nil {
		return false, errors.New("coordinator and stable commit callback are required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.revision != version || len(c.byTask) != 0 {
		return false, nil
	}
	return true, commit()
}
