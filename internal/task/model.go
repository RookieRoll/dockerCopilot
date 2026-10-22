package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/onlyLTY/dockerCopilot/internal/observability"
)

const SchemaVersion = 1

type Status string

const (
	StatusQueued      Status = "queued"
	StatusRunning     Status = "running"
	StatusSucceeded   Status = "succeeded"
	StatusFailed      Status = "failed"
	StatusInterrupted Status = "interrupted"
)

func (s Status) Terminal() bool {
	return s == StatusSucceeded || s == StatusFailed || s == StatusInterrupted
}

func (s Status) valid() bool {
	return s == StatusQueued || s == StatusRunning || s.Terminal()
}

type Recovery struct {
	Status string `json:"status,omitempty"`
	Detail string `json:"detail,omitempty"`
}

type Item struct {
	ID     string `json:"id,omitempty"`
	Name   string `json:"name,omitempty"`
	Status string `json:"status,omitempty"`
	Stage  string `json:"stage,omitempty"`
	Error  string `json:"error,omitempty"`
}

// ContainerCheck contains only the fields needed to display a startup
// reconciliation. It deliberately cannot hold a Docker inspect payload.
type ContainerCheck struct {
	ContainerID string `json:"containerID"`
	Name        string `json:"name,omitempty"`
	State       string `json:"state,omitempty"`
	Running     bool   `json:"running"`
	Result      string `json:"result"`
}

// PendingInstallHandoff is an explicit, non-secret marker written after a
// verified updater package enters the installation handoff phase.
type PendingInstallHandoff struct {
	HandoffID      string    `json:"handoffID"`
	TargetVersion  string    `json:"targetVersion"`
	ArtifactSHA256 string    `json:"artifactSHA256"`
	PreparedAt     time.Time `json:"preparedAt"`
}

// TaskOptions deliberately enumerates accepted, non-secret operation options.
// Add fields here as the task API grows; never persist an arbitrary request map.
type TaskOptions struct {
	Image              string `json:"image,omitempty"`
	DeleteOldContainer *bool  `json:"deleteOldContainer,omitempty"`
	TimeoutSeconds     int    `json:"timeoutSeconds,omitempty"`
	HealthWaitSeconds  int    `json:"healthWaitSeconds,omitempty"`
}

type Record struct {
	SchemaVersion     int                    `json:"schemaVersion"`
	TaskID            string                 `json:"taskID"`
	Type              string                 `json:"type"`
	Status            Status                 `json:"status"`
	Stage             string                 `json:"stage,omitempty"`
	Percentage        int                    `json:"percentage,omitempty"`
	Message           string                 `json:"message,omitempty"`
	Name              string                 `json:"name,omitempty"`
	DetailMsg         string                 `json:"detailMsg,omitempty"`
	IsDone            bool                   `json:"isDone,omitempty"`
	ImageRef          string                 `json:"imageRef,omitempty"`
	OldContainerID    string                 `json:"oldContainerID,omitempty"`
	NewContainerID    string                 `json:"newContainerID,omitempty"`
	CreatedAt         time.Time              `json:"createdAt"`
	StartedAt         *time.Time             `json:"startedAt,omitempty"`
	UpdatedAt         time.Time              `json:"updatedAt"`
	FinishedAt        *time.Time             `json:"finishedAt,omitempty"`
	ContainerIDs      []string               `json:"containerIDs,omitempty"`
	ProtectedNames    []string               `json:"protectedNames,omitempty"`
	ImageIDs          []string               `json:"imageIDs,omitempty"`
	Options           TaskOptions            `json:"options,omitempty"`
	RequestID         string                 `json:"requestID,omitempty"`
	RequiresAttention bool                   `json:"requiresAttention,omitempty"`
	Recovery          Recovery               `json:"recovery,omitempty"`
	Items             []Item                 `json:"items,omitempty"`
	SuccessCount      int                    `json:"successCount"`
	FailedCount       int                    `json:"failedCount"`
	SkippedCount      int                    `json:"skippedCount"`
	ContainerChecks   []ContainerCheck       `json:"containerChecks,omitempty"`
	PendingInstall    *PendingInstallHandoff `json:"pendingInstall,omitempty"`
	Error             string                 `json:"error,omitempty"`
	Warnings          []string               `json:"warnings,omitempty"`
	SnapshotRef       string                 `json:"snapshotRef,omitempty"`
}

// PublicRecord is the deliberately restricted task view. It is an explicit
// projection; private snapshot content and arbitrary record fields never enter it.
type PublicRecord struct {
	TaskID            string           `json:"taskID"`
	Type              string           `json:"type"`
	Status            Status           `json:"status"`
	Stage             string           `json:"stage,omitempty"`
	Percentage        int              `json:"percentage,omitempty"`
	Message           string           `json:"message,omitempty"`
	Name              string           `json:"name,omitempty"`
	DetailMsg         string           `json:"detailMsg,omitempty"`
	IsDone            bool             `json:"isDone"`
	ImageRef          string           `json:"imageRef,omitempty"`
	OldContainerID    string           `json:"oldContainerID,omitempty"`
	NewContainerID    string           `json:"newContainerID,omitempty"`
	CreatedAt         time.Time        `json:"createdAt"`
	StartedAt         *time.Time       `json:"startedAt,omitempty"`
	UpdatedAt         time.Time        `json:"updatedAt"`
	FinishedAt        *time.Time       `json:"finishedAt,omitempty"`
	ContainerIDs      []string         `json:"containerIDs,omitempty"`
	ProtectedNames    []string         `json:"protectedNames,omitempty"`
	ImageIDs          []string         `json:"imageIDs,omitempty"`
	Options           TaskOptions      `json:"options,omitempty"`
	RequestID         string           `json:"requestID,omitempty"`
	RequiresAttention bool             `json:"requiresAttention,omitempty"`
	Recovery          Recovery         `json:"recovery,omitempty"`
	Items             []PublicItem     `json:"items,omitempty"`
	SuccessCount      int              `json:"successCount"`
	FailedCount       int              `json:"failedCount"`
	SkippedCount      int              `json:"skippedCount"`
	ContainerChecks   []ContainerCheck `json:"containerChecks,omitempty"`
	Error             string           `json:"error,omitempty"`
	Warnings          []string         `json:"warnings,omitempty"`
}

type PublicItem struct {
	ID     string `json:"id,omitempty"`
	Name   string `json:"name,omitempty"`
	Status string `json:"status,omitempty"`
	Stage  string `json:"stage,omitempty"`
	Error  string `json:"error,omitempty"`
}

func Public(r Record) PublicRecord {
	out := PublicRecord{
		TaskID: r.TaskID, Type: r.Type, Status: r.Status, Stage: r.Stage,
		Percentage: r.Percentage, Message: observability.RedactText(r.Message), Name: r.Name, DetailMsg: observability.RedactText(r.DetailMsg),
		IsDone: r.Status.Terminal(), ImageRef: r.ImageRef, OldContainerID: r.OldContainerID,
		NewContainerID: r.NewContainerID,
		CreatedAt:      r.CreatedAt, StartedAt: cloneTime(r.StartedAt), UpdatedAt: r.UpdatedAt,
		FinishedAt: cloneTime(r.FinishedAt), ContainerIDs: cloneStrings(r.ContainerIDs),
		ProtectedNames: cloneStrings(r.ProtectedNames), ImageIDs: cloneStrings(r.ImageIDs),
		Options: cloneOptions(r.Options), RequestID: r.RequestID, RequiresAttention: r.RequiresAttention,
		Recovery: publicRecovery(r.Recovery), Error: observability.RedactText(r.Error), Warnings: publicDiagnostics(r.Warnings),
		SuccessCount: r.SuccessCount, FailedCount: r.FailedCount, SkippedCount: r.SkippedCount,
		ContainerChecks: append([]ContainerCheck(nil), r.ContainerChecks...),
	}
	if len(r.Items) != 0 {
		out.Items = make([]PublicItem, 0, len(r.Items))
		for _, item := range r.Items {
			out.Items = append(out.Items, PublicItem{ID: item.ID, Name: item.Name, Status: item.Status, Stage: item.Stage, Error: observability.RedactText(item.Error)})
		}
	}
	return out
}

func publicRecovery(recovery Recovery) Recovery {
	recovery.Detail = observability.RedactText(recovery.Detail)
	return recovery
}

func publicDiagnostics(values []string) []string {
	if values == nil {
		return nil
	}
	result := make([]string, len(values))
	for index, value := range values {
		result[index] = observability.RedactText(value)
	}
	return result
}

func cloneRecord(r Record) Record {
	out := r
	out.StartedAt = cloneTime(r.StartedAt)
	out.FinishedAt = cloneTime(r.FinishedAt)
	out.ContainerIDs = cloneStrings(r.ContainerIDs)
	out.ProtectedNames = cloneStrings(r.ProtectedNames)
	out.ImageIDs = cloneStrings(r.ImageIDs)
	out.Options = cloneOptions(r.Options)
	out.Warnings = cloneStrings(r.Warnings)
	out.Items = append([]Item(nil), r.Items...)
	out.ContainerChecks = append([]ContainerCheck(nil), r.ContainerChecks...)
	if r.PendingInstall != nil {
		handoff := *r.PendingInstall
		out.PendingInstall = &handoff
	}
	return out
}

var safeID = regexp.MustCompile(`^[0-9a-fA-F-]{36}$`)

func (r Record) validate() error {
	if r.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported task schema version %d", r.SchemaVersion)
	}
	id, err := uuid.Parse(r.TaskID)
	if err != nil || !safeID.MatchString(r.TaskID) || id.String() != r.TaskID {
		return errors.New("task ID must be a canonical UUID")
	}
	if r.Type == "" {
		return errors.New("task type is required")
	}
	if !r.Status.valid() {
		return fmt.Errorf("invalid task status %q", r.Status)
	}
	if r.SnapshotRef != "" && r.SnapshotRef != r.TaskID+".snapshot" {
		return errors.New("invalid snapshot reference")
	}
	return nil
}

func cloneStrings(in []string) []string { return append([]string(nil), in...) }
func cloneOptions(in TaskOptions) TaskOptions {
	out := in
	if in.DeleteOldContainer != nil {
		value := *in.DeleteOldContainer
		out.DeleteOldContainer = &value
	}
	return out
}
func cloneTime(in *time.Time) *time.Time {
	if in == nil {
		return nil
	}
	t := *in
	return &t
}

var errClosed = errors.New("task store is closed")

func encodeRecord(r Record) ([]byte, error) {
	if err := r.validate(); err != nil {
		return nil, err
	}
	return json.MarshalIndent(r, "", "  ")
}
