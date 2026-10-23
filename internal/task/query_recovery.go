package task

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

const (
	DefaultTaskPageSize    = 50
	MaximumTaskPageSize    = 100
	DefaultTaskRetention   = 10 * time.Minute
	DefaultTaskRecordLimit = 1000
	TaskTypeProgramUpdate  = "program-update"
)

var ErrContainerNotFound = errors.New("container not found")

type TaskQueryStore interface {
	Get(string) (Record, error)
	List() ([]Record, error)
	Delete(string) error
}

type QueryOptions struct {
	Retention  time.Duration
	MaxRecords int
	Now        func() time.Time
}

type QueryService struct {
	store TaskQueryStore
}

type TaskFilter struct {
	Status        Status
	Type          string
	ContainerID   string
	ContainerName string
	Page          int
	Limit         int
}

type TaskPage struct {
	Items   []PublicRecord `json:"items"`
	Page    int            `json:"page"`
	Limit   int            `json:"limit"`
	Total   int            `json:"total"`
	HasMore bool           `json:"hasMore"`
}

func NewQueryService(store TaskQueryStore, opts QueryOptions) (*QueryService, error) {
	if store == nil {
		return nil, errors.New("task query store is required")
	}
	if opts.Retention < 0 || opts.MaxRecords < 0 {
		return nil, errors.New("task retention and record limit cannot be negative")
	}
	return &QueryService{store: store}, nil
}

// List returns a deterministic newest-first page. Container filters match
// persisted IDs/names exactly so callers cannot accidentally use a substring
// to associate an unrelated task.
func (s *QueryService) List(filter TaskFilter) (TaskPage, error) {
	if filter.Status != "" && !filter.Status.valid() {
		return TaskPage{}, fmt.Errorf("invalid task status %q", filter.Status)
	}
	page, limit, err := normalizePage(filter.Page, filter.Limit)
	if err != nil {
		return TaskPage{}, err
	}
	records, err := s.store.List()
	if err != nil {
		return TaskPage{}, fmt.Errorf("list task records: %w", err)
	}
	matched := make([]Record, 0, len(records))
	for _, record := range records {
		if filter.Status != "" && record.Status != filter.Status {
			continue
		}
		if filter.Type != "" && record.Type != filter.Type {
			continue
		}
		if filter.ContainerID != "" && !recordHasContainerID(record, filter.ContainerID) {
			continue
		}
		if filter.ContainerName != "" && !recordHasContainerName(record, filter.ContainerName) {
			continue
		}
		matched = append(matched, record)
	}
	sort.Slice(matched, func(i, j int) bool {
		if matched[i].CreatedAt.Equal(matched[j].CreatedAt) {
			return matched[i].TaskID > matched[j].TaskID
		}
		return matched[i].CreatedAt.After(matched[j].CreatedAt)
	})
	total := len(matched)
	offset := (page - 1) * limit
	if offset >= total {
		return TaskPage{Items: []PublicRecord{}, Page: page, Limit: limit, Total: total}, nil
	}
	end := offset + limit
	if end > total {
		end = total
	}
	items := make([]PublicRecord, 0, end-offset)
	for _, record := range matched[offset:end] {
		items = append(items, Public(record))
	}
	return TaskPage{Items: items, Page: page, Limit: limit, Total: total, HasMore: end < total}, nil
}

// GetProgress preserves the legacy progress fields through the same explicit
// public projection used by task listing.
func (s *QueryService) GetProgress(taskID string) (PublicRecord, error) {
	record, err := s.store.Get(taskID)
	if err != nil {
		return PublicRecord{}, err
	}
	return Public(record), nil
}

type CleanupReport struct {
	Deleted   []string `json:"deleted"`
	Protected int      `json:"protected"`
	Retained  int      `json:"retained"`
}

// Cleanup is retained as a compatibility hook. Store access performs bounded
// in-memory expiration; query cleanup does not maintain a separate history.
func (s *QueryService) Cleanup() (CleanupReport, error) {
	records, err := s.store.List()
	if err != nil {
		return CleanupReport{}, fmt.Errorf("list tasks for cleanup: %w", err)
	}
	return CleanupReport{Retained: len(records)}, nil
}

func normalizePage(page, limit int) (int, int, error) {
	if page < 0 {
		return 0, 0, errors.New("page must be greater than or equal to 1")
	}
	if limit < 0 || limit > MaximumTaskPageSize {
		return 0, 0, fmt.Errorf("limit must be between 1 and %d", MaximumTaskPageSize)
	}
	if page == 0 {
		page = 1
	}
	if limit == 0 {
		limit = DefaultTaskPageSize
	}
	if page-1 > math.MaxInt/limit || (page-1)*limit > math.MaxInt-limit {
		return 0, 0, errors.New("page offset is too large")
	}
	return page, limit, nil
}

func recordHasContainerID(record Record, id string) bool {
	if record.OldContainerID == id || record.NewContainerID == id {
		return true
	}
	for _, candidate := range record.ContainerIDs {
		if candidate == id {
			return true
		}
	}
	if isContainerTaskType(record.Type) {
		for _, item := range record.Items {
			if item.ID == id {
				return true
			}
		}
	}
	return false
}

func recordHasContainerName(record Record, name string) bool {
	if record.Name == name {
		return true
	}
	for _, candidate := range record.ProtectedNames {
		if candidate == name {
			return true
		}
	}
	if isContainerTaskType(record.Type) {
		for _, item := range record.Items {
			if item.Name == name {
				return true
			}
		}
	}
	return false
}

func isContainerTaskType(taskType string) bool {
	return taskType == "container" || strings.HasPrefix(taskType, "container-")
}

// Legacy records may not have FinishedAt. UpdatedAt is the best available
// terminal timestamp before falling back to creation time.
func terminalRetentionTime(record Record) time.Time {
	if record.FinishedAt != nil && !record.FinishedAt.IsZero() {
		return record.FinishedAt.UTC()
	}
	if !record.UpdatedAt.IsZero() {
		return record.UpdatedAt.UTC()
	}
	return record.CreatedAt.UTC()
}

func itemRecoveryUnresolved(item Item) bool {
	if item.Recovery.Status == "" && item.SnapshotRef == "" {
		return false
	}
	return recoveryUnresolved(item.Recovery)
}
