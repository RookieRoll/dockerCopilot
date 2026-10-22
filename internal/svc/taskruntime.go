package svc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/onlyLTY/dockerCopilot/internal/config"
	containerService "github.com/onlyLTY/dockerCopilot/internal/container"
	"github.com/onlyLTY/dockerCopilot/internal/task"
	"github.com/onlyLTY/dockerCopilot/internal/updater"
	"github.com/zeromicro/go-zero/core/logx"
)

// TaskRuntime owns the persistent task store, bounded runner, query service,
// startup reconciliation, and periodic retention cleanup for one process.
type TaskRuntime struct {
	Store  *task.Store
	Runner *task.Runner
	Query  *task.QueryService

	stopCleanup           chan struct{}
	cleanupDone           chan struct{}
	closeOnce             sync.Once
	closeErr              error
	pendingMu             sync.Mutex
	pendingInstallTaskIDs []string
	pendingValidator      task.PendingInstallValidator
	handoffWorkDir        string
}

type TaskRuntimeOptions struct {
	Directory        string
	MaxConcurrent    int
	QueueCapacity    int
	TaskTimeout      time.Duration
	RecoveryTimeout  time.Duration
	Retention        time.Duration
	MaxRecords       int
	StartupTimeout   time.Duration
	PendingValidator task.PendingInstallValidator
	HandoffWorkDir   string
}

// NewTaskRuntime opens the exclusive task store and reconciles interrupted
// work before constructing a runner that can accept new mutations.
func NewTaskRuntime(ctx context.Context, opts TaskRuntimeOptions, inspector task.ContainerInspector) (*TaskRuntime, error) {
	if opts.StartupTimeout <= 0 {
		opts.StartupTimeout = 30 * time.Second
	}
	store, err := task.Open(task.Options{Dir: opts.Directory})
	if err != nil {
		return nil, fmt.Errorf("open task store: %w", err)
	}
	closeOnError := func(err error) (*TaskRuntime, error) {
		_ = store.Close()
		return nil, err
	}
	reconciler, err := task.NewStartupReconciler(store, inspector, opts.PendingValidator, nil)
	if err != nil {
		return closeOnError(fmt.Errorf("create task startup reconciler: %w", err))
	}
	startupCtx, cancelStartup := context.WithTimeout(ctx, opts.StartupTimeout)
	report, reconcileErr := reconciler.Run(startupCtx)
	cancelStartup()
	if reconcileErr != nil {
		return closeOnError(fmt.Errorf("reconcile task store at startup: %w", reconcileErr))
	}
	if err := reconcileUnverifiedHandoffStartup(store, report, opts.HandoffWorkDir); err != nil {
		return closeOnError(err)
	}
	query, err := task.NewQueryService(store, task.QueryOptions{Retention: opts.Retention, MaxRecords: opts.MaxRecords})
	if err != nil {
		return closeOnError(fmt.Errorf("create task query service: %w", err))
	}
	if _, err := query.Cleanup(); err != nil {
		logx.Infof("warning: initial task cleanup failed: %v", err)
	}
	runner, err := task.NewRunner(store, task.RunnerOptions{
		MaxConcurrent: opts.MaxConcurrent, QueueCapacity: opts.QueueCapacity,
		TaskTimeout: opts.TaskTimeout, RecoveryTimeout: opts.RecoveryTimeout,
	})
	if err != nil {
		return closeOnError(fmt.Errorf("create task runner: %w", err))
	}
	runtime := &TaskRuntime{
		Store: store, Runner: runner, Query: query,
		stopCleanup: make(chan struct{}), cleanupDone: make(chan struct{}),
		pendingInstallTaskIDs: append([]string(nil), report.PendingInstallTaskIDs...),
		pendingValidator:      opts.PendingValidator, handoffWorkDir: opts.HandoffWorkDir,
	}
	go runtime.cleanupLoop()
	return runtime, nil
}

func reconcileUnverifiedHandoffStartup(store *task.Store, report task.StartupReport, workDir string) error {
	for _, taskID := range report.InterruptedTaskIDs {
		record, err := store.Get(taskID)
		if err != nil {
			return fmt.Errorf("inspect interrupted program handoff %s: %w", taskID, err)
		}
		marker := record.PendingInstall
		if record.Type != task.TaskTypeProgramUpdate || marker == nil {
			continue
		}
		result, resultErr := updater.ReadHandoffResult(workDir)
		metadata, metadataErr := updater.ReadHandoffMetadata(workDir)
		metadataMatches := metadataErr == nil && resultMatchesMarker(metadata, record)
		if metadataMatches && resultErr == nil && result.Result == "failed-start" && resultMatchesMarker(result.HandoffMetadata, record) {
			// The launcher already rejected the candidate and restarted the old
			// program. Keep the interrupted record for review but allow management.
			continue
		}
		resultMatches := resultErr == nil && resultMatchesMarker(result.HandoffMetadata, record)
		resultCanBeWritten := resultMatches || errors.Is(resultErr, os.ErrNotExist)
		if metadataMatches && resultCanBeWritten {
			if err := updater.WriteHandoffResult(workDir, updater.HandoffResult{
				HandoffMetadata: metadata, Result: "failed-start", ErrorCode: "running_binary_unverified",
			}); err != nil {
				return fmt.Errorf("record rejected program handoff %s: %w", taskID, err)
			}
		}
		cause := errors.Join(metadataErr, resultErr)
		if metadataMatches && !resultCanBeWritten {
			cause = errors.New("handoff result is corrupt or belongs to a different task")
		}
		if cause == nil {
			cause = errors.New("handoff sidecar identity does not match the durable task")
		}
		return fmt.Errorf("program handoff %s could not be verified at startup; the handoff-capable launcher must restore the previous program: %w", taskID, cause)
	}
	return nil
}

func resultMatchesMarker(metadata updater.HandoffMetadata, record task.Record) bool {
	marker := record.PendingInstall
	return marker != nil && metadata.TaskID == record.TaskID && metadata.HandoffID == marker.HandoffID &&
		metadata.TargetVersion == marker.TargetVersion && metadata.GOARCH == marker.GOARCH &&
		strings.EqualFold(metadata.BinarySHA256, marker.ArtifactSHA256)
}

func NewTaskRuntimeForConfig(ctx context.Context, c config.Config, inspector task.ContainerInspector) (*TaskRuntime, error) {
	return NewTaskRuntimeForConfigWithValidator(ctx, c, inspector, nil, "")
}

func NewTaskRuntimeForConfigWithValidator(ctx context.Context, c config.Config, inspector task.ContainerInspector, validator task.PendingInstallValidator, workDir string) (*TaskRuntime, error) {
	opts, err := c.TaskOptions()
	if err != nil {
		return nil, fmt.Errorf("resolve task runtime options: %w", err)
	}
	return NewTaskRuntime(ctx, TaskRuntimeOptions{
		Directory: opts.Directory, MaxConcurrent: opts.MaxConcurrent, QueueCapacity: opts.QueueCapacity,
		TaskTimeout: opts.Timeout, RecoveryTimeout: opts.RecoveryTimeout,
		Retention: opts.Retention, MaxRecords: opts.MaxRecords, StartupTimeout: opts.RecoveryTimeout,
		PendingValidator: validator, HandoffWorkDir: workDir,
	}, inspector)
}

// ConfirmPendingInstalls is called only after this process has served a local
// readiness probe. The sidecar records process readiness first; durable task
// confirmation is retried separately because the two files cannot be updated
// as one transaction.
func (r *TaskRuntime) ConfirmPendingInstalls(ctx context.Context) error {
	if r == nil {
		return nil
	}
	r.pendingMu.Lock()
	taskIDs := append([]string(nil), r.pendingInstallTaskIDs...)
	r.pendingMu.Unlock()
	if len(taskIDs) == 0 {
		return nil
	}
	if r.pendingValidator == nil || r.handoffWorkDir == "" {
		return errors.New("pending program handoff cannot be confirmed without validator and working directory")
	}
	for _, taskID := range taskIDs {
		record, err := r.Store.Get(taskID)
		if err != nil {
			return fmt.Errorf("load pending program task %s: %w", taskID, err)
		}
		proof, err := r.pendingValidator.VerifyPendingInstall(ctx, record)
		if err != nil {
			return fmt.Errorf("verify running program for task %s: %w", taskID, err)
		}
		marker := record.PendingInstall
		if marker == nil {
			return fmt.Errorf("pending program task %s has no handoff marker", taskID)
		}
		proof.Ready = true
		if existingResult, resultErr := updater.ReadHandoffResult(r.handoffWorkDir); resultErr == nil {
			if !resultMatchesMarker(existingResult.HandoffMetadata, record) {
				return fmt.Errorf("existing handoff result belongs to a different task; refusing to overwrite it")
			}
		} else if !errors.Is(resultErr, os.ErrNotExist) {
			return fmt.Errorf("read existing handoff result before confirmation: %w", resultErr)
		}
		if err := updater.WriteHandoffResult(r.handoffWorkDir, updater.HandoffResult{
			HandoffMetadata: updater.HandoffMetadata{TaskID: taskID, HandoffID: marker.HandoffID, TargetVersion: marker.TargetVersion, GOARCH: marker.GOARCH, BinarySHA256: marker.ArtifactSHA256},
			Result:          "ready",
		}); err != nil {
			return fmt.Errorf("write ready handoff result for task %s: %w", taskID, err)
		}
		var confirmErr error
		for attempt := 0; attempt < 10; attempt++ {
			confirmErr = r.Store.ConfirmPendingInstall(taskID, proof)
			if confirmErr == nil {
				break
			}
			if attempt == 9 {
				break
			}
			timer := time.NewTimer(500 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return fmt.Errorf("confirm pending program task %s: %w", taskID, ctx.Err())
			case <-timer.C:
			}
		}
		if confirmErr != nil {
			return fmt.Errorf("confirm pending program task %s: %w", taskID, confirmErr)
		}
	}
	r.pendingMu.Lock()
	r.pendingInstallTaskIDs = nil
	r.pendingMu.Unlock()
	return nil
}

func (r *TaskRuntime) HasPendingInstall() bool {
	if r == nil {
		return false
	}
	r.pendingMu.Lock()
	defer r.pendingMu.Unlock()
	return len(r.pendingInstallTaskIDs) != 0
}

// HasUnresolvedProgramInstall also detects handoffs retained for manual review
// after failed reconciliation or rollback. A confirmed succeeded marker is an
// audit record and does not block a later update.
func (r *TaskRuntime) HasUnresolvedProgramInstall() bool {
	if r == nil || r.Store == nil {
		return false
	}
	records, err := r.Store.List()
	if err != nil {
		return true
	}
	for _, record := range records {
		if record.Type == task.TaskTypeProgramUpdate && record.PendingInstall != nil && record.Status != task.StatusSucceeded {
			return true
		}
	}
	return false
}

func (r *TaskRuntime) cleanupLoop() {
	defer close(r.cleanupDone)
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if _, err := r.Query.Cleanup(); err != nil {
				logx.Errorf("periodic task cleanup failed: %v", err)
			}
		case <-r.stopCleanup:
			return
		}
	}
}

// Close stops cleanup, drains accepted tasks, and releases the exclusive
// writer lock. It is safe to call more than once.
func (r *TaskRuntime) Close() error {
	if r == nil {
		return nil
	}
	r.closeOnce.Do(func() {
		if r.stopCleanup != nil {
			close(r.stopCleanup)
			<-r.cleanupDone
		}
		if r.Runner != nil {
			r.Runner.Close()
		}
		if r.Store != nil {
			r.closeErr = r.Store.Close()
		}
	})
	return r.closeErr
}

// serviceInspector restricts startup checks to the read-only Inspect API.
type serviceInspector struct{ service *containerService.Service }

func (i serviceInspector) Inspect(ctx context.Context, id string) (task.ContainerObservation, error) {
	if i.service == nil {
		return task.ContainerObservation{}, errors.New("container inspector is unavailable")
	}
	info, err := i.service.Inspect(ctx, id)
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return task.ContainerObservation{}, task.ErrContainerNotFound
		}
		return task.ContainerObservation{}, err
	}
	if info.ContainerJSONBase == nil || info.State == nil {
		return task.ContainerObservation{}, errors.New("container inspect returned incomplete state")
	}
	return task.ContainerObservation{Name: info.Name, State: info.State.Status, Running: info.State.Running}, nil
}
