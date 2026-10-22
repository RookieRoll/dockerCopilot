package task

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	DefaultTaskPageSize    = 50
	MaximumTaskPageSize    = 100
	DefaultTaskRetention   = 7 * 24 * time.Hour
	DefaultTaskRecordLimit = 1000
	TaskTypeProgramUpdate  = "program-update"
)

var ErrContainerNotFound = errors.New("container not found")

var pendingInstallVersionPattern = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?|\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)

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
	store      TaskQueryStore
	retention  time.Duration
	maxRecords int
	now        func() time.Time
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
	if opts.Retention == 0 {
		opts.Retention = DefaultTaskRetention
	}
	if opts.MaxRecords == 0 {
		opts.MaxRecords = DefaultTaskRecordLimit
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &QueryService{store: store, retention: opts.Retention, maxRecords: opts.MaxRecords, now: opts.Now}, nil
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

// Cleanup applies both age and count limits to settled terminal records.
// Active, protected, or unresolved records never count toward the deletable
// history budget and are never removed to satisfy it.
func (s *QueryService) Cleanup() (CleanupReport, error) {
	records, err := s.store.List()
	if err != nil {
		return CleanupReport{}, fmt.Errorf("list tasks for cleanup: %w", err)
	}
	sort.Slice(records, func(i, j int) bool {
		iTime, jTime := terminalRetentionTime(records[i]), terminalRetentionTime(records[j])
		if iTime.Equal(jTime) {
			return records[i].TaskID > records[j].TaskID
		}
		return iTime.After(jTime)
	})
	cutoff := s.now().UTC().Add(-s.retention)
	var report CleanupReport
	eligibleRetained := 0
	var errs []error
	for _, record := range records {
		if !cleanupEligible(record) {
			report.Protected++
			continue
		}
		if terminalRetentionTime(record).Before(cutoff) {
			if err := s.store.Delete(record.TaskID); err != nil {
				errs = append(errs, fmt.Errorf("delete task %s: %w", record.TaskID, err))
				continue
			}
			report.Deleted = append(report.Deleted, record.TaskID)
			continue
		}
		if eligibleRetained < s.maxRecords {
			eligibleRetained++
			report.Retained++
			continue
		}
		if err := s.store.Delete(record.TaskID); err != nil {
			errs = append(errs, fmt.Errorf("delete task %s: %w", record.TaskID, err))
			continue
		}
		report.Deleted = append(report.Deleted, record.TaskID)
	}
	if len(errs) != 0 {
		return report, errors.Join(errs...)
	}
	return report, nil
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
	for _, check := range record.ContainerChecks {
		if check.ContainerID == id {
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
	for _, check := range record.ContainerChecks {
		if check.Name == name {
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

func cleanupEligible(record Record) bool {
	if !record.Status.Terminal() || record.RequiresAttention || len(record.ProtectedNames) != 0 {
		return false
	}
	if record.Recovery.Status != "not-needed" && record.Recovery.Status != "restored" {
		return false
	}
	for _, item := range record.Items {
		if item.RequiresAttention || len(item.ProtectedNames) != 0 || itemRecoveryUnresolved(item) {
			return false
		}
	}
	return true
}

func itemRecoveryUnresolved(item Item) bool {
	if item.Recovery.Status == "" && item.SnapshotRef == "" {
		return false
	}
	return recoveryUnresolved(item.Recovery)
}

type StartupStore interface {
	List() ([]Record, error)
	Save(Record) error
}

type ContainerObservation struct {
	Name    string
	State   string
	Running bool
}

type ContainerInspector interface {
	Inspect(context.Context, string) (ContainerObservation, error)
}

type PendingInstallValidator interface {
	VerifyPendingInstall(context.Context, Record) (PendingInstallProof, error)
}

// PendingInstallProof is accepted only when every identity and artifact field
// matches the record marker. This prevents task type alone from bypassing
// ordinary startup interruption handling.
type PendingInstallProof struct {
	Verified       bool
	Ready          bool
	TaskID         string
	HandoffID      string
	TargetVersion  string
	GOARCH         string
	ArtifactSHA256 string
}

type StartupReport struct {
	InterruptedTaskIDs    []string `json:"interruptedTaskIDs"`
	PendingInstallTaskIDs []string `json:"pendingInstallTaskIDs"`
	Warnings              []string `json:"warnings,omitempty"`
}

type StartupReconciler struct {
	store     StartupStore
	inspector ContainerInspector
	validator PendingInstallValidator
	now       func() time.Time
}

func NewStartupReconciler(store StartupStore, inspector ContainerInspector, validator PendingInstallValidator, now func() time.Time) (*StartupReconciler, error) {
	if store == nil {
		return nil, errors.New("startup task store is required")
	}
	if now == nil {
		now = time.Now
	}
	return &StartupReconciler{store: store, inspector: inspector, validator: validator, now: now}, nil
}

// Run marks ordinary queued/running work interrupted, performs only read-only
// inspections, and returns verified install handoffs to the updater. It never
// replays a task callback or mutates Docker state.
func (r *StartupReconciler) Run(ctx context.Context) (StartupReport, error) {
	records, err := r.store.List()
	if err != nil {
		return StartupReport{}, fmt.Errorf("list tasks for startup reconciliation: %w", err)
	}
	report := StartupReport{}
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return report, fmt.Errorf("startup task reconciliation canceled: %w", err)
		}
		if record.Status != StatusQueued && record.Status != StatusRunning {
			continue
		}
		if record.Type == TaskTypeProgramUpdate && record.Status == StatusRunning && record.PendingInstall != nil {
			verified, warning := r.verifiedPendingInstall(ctx, record)
			if verified {
				report.PendingInstallTaskIDs = append(report.PendingInstallTaskIDs, record.TaskID)
				continue
			}
			if warning != "" {
				record.Warnings = append(record.Warnings, warning)
				report.Warnings = append(report.Warnings, warning)
			}
		}
		wasRunning := record.Status == StatusRunning
		record.Status = StatusInterrupted
		record.IsDone = true
		finished := r.now().UTC()
		record.FinishedAt = &finished
		record.UpdatedAt = finished
		if wasRunning {
			record.Error = "service restarted while the task was running; the operation was not replayed"
			record.Recovery = Recovery{Status: "unknown", Detail: "Docker state was inspected read-only; manual review is required before releasing protection"}
			record.RequiresAttention = true
		} else {
			record.Error = "service restarted before the queued task began; the operation was not replayed"
			record.Recovery = Recovery{Status: "not-needed"}
		}
		r.inspectRelated(ctx, &record, &report)
		if err := r.store.Save(record); err != nil {
			return report, fmt.Errorf("persist interrupted task %s: %w", record.TaskID, err)
		}
		report.InterruptedTaskIDs = append(report.InterruptedTaskIDs, record.TaskID)
	}
	return report, nil
}

func (r *StartupReconciler) verifiedPendingInstall(ctx context.Context, record Record) (bool, string) {
	marker := record.PendingInstall
	if !completePendingInstallMarker(marker) {
		return false, fmt.Sprintf("task %s has an incomplete pending-install marker; treating it as an ordinary interrupted task", record.TaskID)
	}
	if r.validator == nil {
		return false, fmt.Sprintf("task %s pending-install handoff has no validator; treating it as an ordinary interrupted task", record.TaskID)
	}
	proof, err := r.validator.VerifyPendingInstall(ctx, cloneRecord(record))
	if err != nil || !proof.Verified || proof.TaskID != record.TaskID || proof.HandoffID != marker.HandoffID || proof.TargetVersion != marker.TargetVersion || proof.GOARCH != marker.GOARCH || !strings.EqualFold(proof.ArtifactSHA256, marker.ArtifactSHA256) {
		return false, fmt.Sprintf("task %s pending-install handoff could not be fully verified; treating it as an ordinary interrupted task", record.TaskID)
	}
	return true, ""
}

func completePendingInstallMarker(marker *PendingInstallHandoff) bool {
	if marker == nil || !pendingInstallVersionPattern.MatchString(marker.TargetVersion) || marker.PreparedAt.IsZero() || !validTaskID(marker.HandoffID) || (marker.GOARCH != "amd64" && marker.GOARCH != "arm64") || len(marker.ArtifactSHA256) != 64 {
		return false
	}
	_, err := hex.DecodeString(marker.ArtifactSHA256)
	return err == nil
}

func (r *StartupReconciler) inspectRelated(ctx context.Context, record *Record, report *StartupReport) {
	ids := make([]string, 0, len(record.ContainerIDs)+2)
	seen := make(map[string]struct{})
	add := func(id string) {
		if id != "" {
			if _, ok := seen[id]; !ok {
				seen[id] = struct{}{}
				ids = append(ids, id)
			}
		}
	}
	add(record.OldContainerID)
	add(record.NewContainerID)
	for _, id := range record.ContainerIDs {
		add(id)
	}
	// Batch item IDs are container references only for container task types.
	// Image task item IDs must never be sent to the container inspector.
	if isContainerTaskType(record.Type) {
		for _, item := range record.Items {
			add(item.ID)
		}
	}
	checks := make([]ContainerCheck, 0, len(ids))
	for _, id := range ids {
		check := ContainerCheck{ContainerID: id}
		if err := ctx.Err(); err != nil {
			check.Result = "unavailable"
		} else if r.inspector == nil {
			check.Result = "unavailable"
			addStartupWarning(record, report, fmt.Sprintf("task %s has no read-only Docker inspector; protection retained", record.TaskID))
		} else if observation, err := r.inspector.Inspect(ctx, id); err != nil {
			check.Result = "unavailable"
			if errors.Is(err, ErrContainerNotFound) {
				check.Result = "not-found"
				addStartupWarning(record, report, fmt.Sprintf("task %s container %s was not found during read-only inspection; protection retained for manual review", record.TaskID, id))
			} else {
				addStartupWarning(record, report, fmt.Sprintf("task %s container inspection failed; protection retained", record.TaskID))
			}
		} else {
			check.Result = "found"
			check.Name = observation.Name
			check.State = observation.State
			check.Running = observation.Running
		}
		if check.Result == "unavailable" && ctx.Err() != nil {
			addStartupWarning(record, report, fmt.Sprintf("task %s container inspection stopped by context; protection retained", record.TaskID))
		}
		checks = append(checks, check)
	}
	if len(checks) != 0 {
		record.ContainerChecks = checks
	}
}

func addStartupWarning(record *Record, report *StartupReport, warning string) {
	record.Warnings = append(record.Warnings, warning)
	report.Warnings = append(report.Warnings, warning)
}
