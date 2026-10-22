package svc

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/onlyLTY/dockerCopilot/internal/config"
	containerService "github.com/onlyLTY/dockerCopilot/internal/container"
	"github.com/onlyLTY/dockerCopilot/internal/task"
	"github.com/zeromicro/go-zero/core/logx"
)

// TaskRuntime owns the persistent task store, bounded runner, query service,
// startup reconciliation, and periodic retention cleanup for one process.
type TaskRuntime struct {
	Store  *task.Store
	Runner *task.Runner
	Query  *task.QueryService

	stopCleanup chan struct{}
	cleanupDone chan struct{}
	closeOnce   sync.Once
	closeErr    error
}

type TaskRuntimeOptions struct {
	Directory       string
	MaxConcurrent   int
	QueueCapacity   int
	TaskTimeout     time.Duration
	RecoveryTimeout time.Duration
	Retention       time.Duration
	MaxRecords      int
	StartupTimeout  time.Duration
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
	reconciler, err := task.NewStartupReconciler(store, inspector, nil, nil)
	if err != nil {
		return closeOnError(fmt.Errorf("create task startup reconciler: %w", err))
	}
	startupCtx, cancelStartup := context.WithTimeout(ctx, opts.StartupTimeout)
	_, reconcileErr := reconciler.Run(startupCtx)
	cancelStartup()
	if reconcileErr != nil {
		return closeOnError(fmt.Errorf("reconcile task store at startup: %w", reconcileErr))
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
	}
	go runtime.cleanupLoop()
	return runtime, nil
}

func NewTaskRuntimeForConfig(ctx context.Context, c config.Config, inspector task.ContainerInspector) (*TaskRuntime, error) {
	opts, err := c.TaskOptions()
	if err != nil {
		return nil, fmt.Errorf("resolve task runtime options: %w", err)
	}
	return NewTaskRuntime(ctx, TaskRuntimeOptions{
		Directory: opts.Directory, MaxConcurrent: opts.MaxConcurrent, QueueCapacity: opts.QueueCapacity,
		TaskTimeout: opts.Timeout, RecoveryTimeout: opts.RecoveryTimeout,
		Retention: opts.Retention, MaxRecords: opts.MaxRecords, StartupTimeout: opts.RecoveryTimeout,
	}, inspector)
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
