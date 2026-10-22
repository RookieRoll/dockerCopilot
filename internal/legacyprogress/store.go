package legacyprogress

import (
	"sync"

	"github.com/onlyLTY/dockerCopilot/internal/types"
)

// Store supports legacy progress polling until the task HTTP endpoints use the
// persistent query service. It is intentionally independent of svc and task.
type Store struct {
	mu    sync.RWMutex
	items map[string]types.TaskProgress
}

func NewStore(items map[string]types.TaskProgress) *Store {
	if items == nil {
		items = make(map[string]types.TaskProgress)
	}
	return &Store{items: items}
}

func (s *Store) Update(taskID string, progress types.TaskProgress) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[taskID] = progress
}

func (s *Store) Get(taskID string) (types.TaskProgress, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	progress, ok := s.items[taskID]
	return progress, ok
}
