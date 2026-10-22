// Package operation coordinates in-process mutations that target Docker
// containers. It intentionally protects only this service instance; it cannot
// serialize changes made by external Docker clients.
package operation

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
)

const ContainerUpdate = "container-update"

var (
	ErrInvalidRequest = errors.New("operation task ID, kind, and target are required")
	ErrLeaseNotFound  = errors.New("operation lease not found")
)

type Target struct {
	ID   string
	Name string
}

type Request struct {
	TaskID         string
	Kind           string
	Target         Target
	EquivalenceKey string
}

// Reservation distinguishes an accepted duplicate from a new lease. An
// existing update has ExistingTaskID set and Lease nil.
type Reservation struct {
	Lease          *Lease
	ExistingTaskID string
}

type ConflictError struct {
	TaskID string
	Kind   string
}

func (e *ConflictError) Error() string {
	if e == nil {
		return "container operation conflicts with an active operation"
	}
	return fmt.Sprintf("container operation conflicts with active %s task %s", e.Kind, e.TaskID)
}

func (*ConflictError) HTTPStatus() int   { return http.StatusConflict }
func (*ConflictError) ErrorCode() string { return "CONTAINER_OPERATION_CONFLICT" }

type Coordinator struct {
	mu     sync.Mutex
	byKey  map[string]*leaseState
	byTask map[string]*leaseState
}

type leaseState struct {
	request  Request
	keys     map[string]struct{}
	ready    chan struct{}
	accepted bool
}

type Lease struct {
	coordinator *Coordinator
	taskID      string
	state       *leaseState
}

func NewCoordinator() *Coordinator {
	return &Coordinator{byKey: make(map[string]*leaseState), byTask: make(map[string]*leaseState)}
}

func (c *Coordinator) Reserve(req Request) (Reservation, error) {
	return c.ReserveContext(context.Background(), req)
}

// ReserveContext allows an HTTP caller waiting on another provisional task to
// abandon the wait when its request context ends.
func (c *Coordinator) ReserveContext(ctx context.Context, req Request) (Reservation, error) {
	if c == nil || strings.TrimSpace(req.TaskID) == "" || strings.TrimSpace(req.Kind) == "" {
		return Reservation{}, ErrInvalidRequest
	}
	if ctx == nil {
		ctx = context.Background()
	}
	keys, err := targetKeys(req.Target)
	if err != nil {
		return Reservation{}, err
	}
	for {
		if err := ctx.Err(); err != nil {
			return Reservation{}, err
		}
		c.mu.Lock()
		if _, exists := c.byTask[req.TaskID]; exists {
			c.mu.Unlock()
			return Reservation{}, fmt.Errorf("task ID %q already owns an operation lease", req.TaskID)
		}
		var owner *leaseState
		for _, key := range keys {
			current := c.byKey[key]
			if current == nil {
				continue
			}
			if owner != nil && owner != current {
				c.mu.Unlock()
				return Reservation{}, conflict(current)
			}
			owner = current
		}
		if owner != nil {
			if !owner.accepted {
				ready := owner.ready
				c.mu.Unlock()
				select {
				case <-ready:
				case <-ctx.Done():
					return Reservation{}, ctx.Err()
				}
				c.mu.Lock()
				accepted := owner.accepted
				c.mu.Unlock()
				if accepted && equivalentUpdate(owner.request, req) {
					return Reservation{ExistingTaskID: owner.request.TaskID}, nil
				}
				continue
			}
			if equivalentUpdate(owner.request, req) {
				existingID := owner.request.TaskID
				c.mu.Unlock()
				return Reservation{ExistingTaskID: existingID}, nil
			}
			c.mu.Unlock()
			return Reservation{}, conflict(owner)
		}
		state := &leaseState{request: req, keys: make(map[string]struct{}, len(keys)), ready: make(chan struct{})}
		for _, key := range keys {
			state.keys[key] = struct{}{}
			c.byKey[key] = state
		}
		c.byTask[req.TaskID] = state
		c.mu.Unlock()
		return Reservation{Lease: &Lease{coordinator: c, taskID: req.TaskID, state: state}}, nil
	}
}

func equivalentUpdate(active, requested Request) bool {
	return active.Kind == ContainerUpdate && requested.Kind == ContainerUpdate &&
		active.EquivalenceKey != "" && active.EquivalenceKey == requested.EquivalenceKey
}

func conflict(state *leaseState) error {
	return &ConflictError{TaskID: state.request.TaskID, Kind: state.request.Kind}
}

func (l *Lease) TaskID() string {
	if l == nil {
		return ""
	}
	return l.taskID
}

// Confirm marks a provisional reservation as accepted after the task record
// has been durably queued. Duplicate submitters wait for this decision so they
// cannot observe a task ID whose initial persistence later fails.
func (l *Lease) Confirm() error {
	if l == nil || l.coordinator == nil {
		return ErrLeaseNotFound
	}
	c := l.coordinator
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byTask[l.taskID] != l.state {
		if l.state.accepted {
			return nil
		}
		return ErrLeaseNotFound
	}
	if !l.state.accepted {
		l.state.accepted = true
		close(l.state.ready)
	}
	return nil
}

// AddAlias extends the existing lease, for example when update creates a new
// container ID. Any identity already protected by another task is rejected.
func (l *Lease) AddAlias(target Target) error {
	if l == nil || l.coordinator == nil {
		return ErrLeaseNotFound
	}
	return l.coordinator.addAlias(l.taskID, l.state, target)
}

// AddAlias is useful to a callback that has the owning task ID but not the
// reservation handle. The owner check makes it impossible to extend another
// task's lease.
func (c *Coordinator) AddAlias(taskID string, target Target) error {
	if c == nil {
		return ErrLeaseNotFound
	}
	c.mu.Lock()
	state := c.byTask[taskID]
	c.mu.Unlock()
	if state == nil {
		return ErrLeaseNotFound
	}
	return c.addAlias(taskID, state, target)
}

func (c *Coordinator) addAlias(taskID string, expected *leaseState, target Target) error {
	keys, err := targetKeys(target)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	state := c.byTask[taskID]
	if state == nil || state != expected {
		return ErrLeaseNotFound
	}
	for _, key := range keys {
		if current := c.byKey[key]; current != nil && current != state {
			return conflict(current)
		}
	}
	for _, key := range keys {
		state.keys[key] = struct{}{}
		c.byKey[key] = state
	}
	return nil
}

// Owns verifies that the given task currently protects all supplied identities.
func (c *Coordinator) Owns(taskID string, target Target) bool {
	if c == nil {
		return false
	}
	keys, err := targetKeys(target)
	if err != nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	state := c.byTask[taskID]
	if state == nil {
		return false
	}
	for _, key := range keys {
		if c.byKey[key] != state {
			return false
		}
	}
	return true
}

// Release is idempotent and removes only keys still owned by this task.
func (l *Lease) Release() {
	if l == nil || l.coordinator == nil {
		return
	}
	c := l.coordinator
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byTask[l.taskID] != l.state {
		return
	}
	for key := range l.state.keys {
		if c.byKey[key] == l.state {
			delete(c.byKey, key)
		}
	}
	delete(c.byTask, l.taskID)
	if !l.state.accepted {
		close(l.state.ready)
	}
}

func targetKeys(target Target) ([]string, error) {
	keys := make([]string, 0, 2)
	if id := strings.ToLower(strings.TrimSpace(target.ID)); id != "" {
		keys = append(keys, "id:"+id)
	}
	if name := canonicalName(target.Name); name != "" {
		keys = append(keys, "name:"+name)
	}
	if len(keys) == 0 {
		return nil, ErrInvalidRequest
	}
	sort.Strings(keys)
	return keys, nil
}

func canonicalName(name string) string {
	name = strings.TrimSpace(name)
	name = strings.TrimLeft(name, "/")
	return name
}
