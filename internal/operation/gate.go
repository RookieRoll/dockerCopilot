package operation

import (
	"context"
	"errors"
	"net/http"
	"sync"
)

var ErrMutationDrain = errors.New("mutations are temporarily disabled for program handoff")

type MutationGate struct {
	mu       sync.Mutex
	active   map[uint64]string
	nextID   uint64
	changed  chan struct{}
	draining bool
	drainID  uint64
}

func NewMutationGate() *MutationGate {
	return &MutationGate{active: make(map[uint64]string), changed: make(chan struct{})}
}

// Acquire registers a synchronous request or a durable background task as an
// active mutation. The lease must remain held until the operation really ends.
func (g *MutationGate) Acquire(ctx context.Context, ownerTaskID string) (*MutationLease, error) {
	if g == nil {
		return nil, errors.New("mutation gate is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.draining {
		return nil, MutationDrainError()
	}
	g.nextID++
	id := g.nextID
	g.active[id] = ownerTaskID
	return &MutationLease{gate: g, id: id}, nil
}

// BeginDrain prevents new mutations and returns the exclusive drain handle.
// Existing work continues; WaitOthers must complete before replacing files.
func (g *MutationGate) BeginDrain(ownerTaskID string) (*MutationDrain, error) {
	if g == nil || ownerTaskID == "" {
		return nil, errors.New("mutation gate and drain owner are required")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.draining {
		return nil, MutationDrainError()
	}
	g.nextID++
	gen := g.nextID
	g.draining, g.drainID = true, gen
	g.signalLocked()
	return &MutationDrain{gate: g, id: gen, ownerTaskID: ownerTaskID}, nil
}

func (g *MutationGate) IsDraining() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.draining
}

type MutationLease struct {
	gate *MutationGate
	id   uint64
	once sync.Once
}

func (l *MutationLease) Release() {
	if l == nil || l.gate == nil {
		return
	}
	l.once.Do(func() {
		g := l.gate
		g.mu.Lock()
		delete(g.active, l.id)
		g.signalLocked()
		g.mu.Unlock()
	})
}

type MutationDrain struct {
	gate        *MutationGate
	id          uint64
	ownerTaskID string
	once        sync.Once
}

// WaitOthers waits for every active mutation except the program-update task
// that owns this drain handle. Cancellation leaves the gate closed; callers
// must call Resume when their handoff attempt fails.
func (d *MutationDrain) WaitOthers(ctx context.Context) error {
	if d == nil || d.gate == nil {
		return errors.New("mutation drain is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	g := d.gate
	for {
		g.mu.Lock()
		if !g.draining || g.drainID != d.id {
			g.mu.Unlock()
			return ErrMutationDrain
		}
		active := false
		for _, owner := range g.active {
			if owner != d.ownerTaskID {
				active = true
				break
			}
		}
		changed := g.changed
		g.mu.Unlock()
		if !active {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

// Resume reopens mutations if this drain still owns the gate.
func (d *MutationDrain) Resume() {
	if d == nil || d.gate == nil {
		return
	}
	d.once.Do(func() {
		g := d.gate
		g.mu.Lock()
		if g.draining && g.drainID == d.id {
			g.draining = false
			g.signalLocked()
		}
		g.mu.Unlock()
	})
}

type mutationDrainError struct{}

func (mutationDrainError) Error() string     { return ErrMutationDrain.Error() }
func (mutationDrainError) Unwrap() error     { return ErrMutationDrain }
func (mutationDrainError) HTTPStatus() int   { return http.StatusServiceUnavailable }
func (mutationDrainError) ErrorCode() string { return "MUTATIONS_DRAINING" }

func MutationDrainError() error { return mutationDrainError{} }

func (g *MutationGate) signalLocked() {
	close(g.changed)
	g.changed = make(chan struct{})
}
