package version

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/onlyLTY/dockerCopilot/internal/config"
	"github.com/onlyLTY/dockerCopilot/internal/observability"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/onlyLTY/dockerCopilot/internal/task"
	"github.com/onlyLTY/dockerCopilot/internal/types"
	"github.com/onlyLTY/dockerCopilot/internal/updater"
)

type programTaskState struct {
	mu          sync.Mutex
	drain       operationDrain
	lease       operationLease
	placed      *updater.InstallHandoff
	metadata    *updater.HandoffMetadata
	workDir     string
	stagingRoot string
	handoff     bool
}

// Tiny interfaces keep the adapter's synchronization state independent from
// operation package internals while retaining the process-wide gate handles.
type operationDrain interface {
	WaitOthers(context.Context) error
	Resume()
}
type operationLease interface{ Release() }

func (s *programTaskState) setDrain(d operationDrain) { s.mu.Lock(); s.drain = d; s.mu.Unlock() }
func (s *programTaskState) setLease(l operationLease) { s.mu.Lock(); s.lease = l; s.mu.Unlock() }
func (s *programTaskState) setPlaced(h updater.InstallHandoff) {
	s.mu.Lock()
	s.placed = &h
	s.mu.Unlock()
}

func (s *programTaskState) cleanupCandidate() error {
	s.mu.Lock()
	placed := s.placed
	s.mu.Unlock()
	if placed == nil {
		return nil
	}
	return updater.RemovePlacedBinary(updater.HandoffOptions{WorkDir: s.workDir, StagingParent: s.stagingRoot, GOARCH: runtime.GOARCH}, *placed)
}

func (s *programTaskState) setMetadata(metadata updater.HandoffMetadata) {
	s.mu.Lock()
	copy := metadata
	s.metadata = &copy
	s.mu.Unlock()
}

func (s *programTaskState) hasHandedOff() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.handoff
}

func (s *programTaskState) rollback() error {
	removeErr := s.cleanupCandidate()
	s.mu.Lock()
	metadata := s.metadata
	s.mu.Unlock()
	if metadata == nil {
		return removeErr
	}
	current, readErr := updater.ReadHandoffMetadata(s.workDir)
	if readErr != nil {
		return errors.Join(removeErr, fmt.Errorf("read handoff identity before rollback result: %w", readErr))
	}
	if !sameProgramHandoff(current, *metadata) {
		return errors.Join(removeErr, errors.New("handoff metadata belongs to a different task; preserving it without overwrite"))
	}
	if existing, resultErr := updater.ReadHandoffResult(s.workDir); resultErr == nil {
		if !sameProgramHandoff(existing.HandoffMetadata, *metadata) {
			return errors.Join(removeErr, errors.New("handoff result belongs to a different task; preserving it without overwrite"))
		}
	} else if !errors.Is(resultErr, os.ErrNotExist) {
		return errors.Join(removeErr, fmt.Errorf("read handoff result before rollback: %w", resultErr))
	}
	code := "handoff_aborted"
	if removeErr != nil {
		code = "rollback_failed"
	}
	resultErr := updater.WriteHandoffResult(s.workDir, updater.HandoffResult{HandoffMetadata: *metadata, Result: "failed-start", ErrorCode: code})
	return errors.Join(removeErr, resultErr)
}

func sameProgramHandoff(a, b updater.HandoffMetadata) bool {
	return a.TaskID == b.TaskID && a.HandoffID == b.HandoffID && a.TargetVersion == b.TargetVersion && a.GOARCH == b.GOARCH && strings.EqualFold(a.BinarySHA256, b.BinarySHA256)
}

func (s *programTaskState) resume() {
	s.mu.Lock()
	drain := s.drain
	s.mu.Unlock()
	if drain != nil {
		drain.Resume()
	}
}

func (s *programTaskState) settled(ctx *svc.ServiceContext) {
	s.mu.Lock()
	drain, lease, handedOff := s.drain, s.lease, s.handoff
	s.mu.Unlock()
	if !handedOff && drain != nil {
		drain.Resume()
	}
	if lease != nil {
		lease.Release()
	}
	ctx.ReleaseProgramUpdate()
}

func (l *UpdateProgramLogic) submitProgramUpdate() (*types.Resp, error) {
	svcCtx := l.serviceContext
	if svcCtx == nil || svcCtx.TaskRunner == nil || svcCtx.TaskRuntime == nil {
		return nil, observability.ServiceUnavailable(errors.New("program update task runtime is unavailable"))
	}
	if svcCtx.TaskRuntime.HasUnresolvedProgramInstall() {
		return nil, observability.ServiceUnavailable(errors.New("a program installation is still awaiting readiness confirmation"))
	}
	if !svcCtx.ReserveProgramUpdate() {
		return nil, observability.ServiceUnavailable(errors.New("a program update is already queued or running"))
	}
	releaseReservation := true
	defer func() {
		if releaseReservation {
			svcCtx.ReleaseProgramUpdate()
		}
	}()
	if err := l.validateHandoff(l.ctx); err != nil {
		return nil, observability.ServiceUnavailable(fmt.Errorf("online program update is unavailable in this deployment: %w", err))
	}
	options, err := svcCtx.ProgramHandoffOptions()
	if err != nil {
		return nil, err
	}
	requestID := l.requestID
	parsedRequestID, parseRequestErr := uuid.Parse(requestID)
	if parseRequestErr != nil {
		requestID = uuid.NewString()
	} else {
		requestID = parsedRequestID.String()
	}
	taskID := requestID
	state := &programTaskState{workDir: options.WorkDir, stagingRoot: options.StagingParent}
	source := l.updateSource
	preparedIdentity := updater.PrepareIdentity{TaskID: taskID, RequestID: requestID}

	_, err = svcCtx.TaskRunner.Submit(task.TaskSpec{
		Record: task.Record{
			TaskID: taskID, Type: task.TaskTypeProgramUpdate, Name: "在线更新程序",
			RequestID: requestID, Stage: "queued", Message: "等待程序更新任务执行",
		},
		OnAccepted: func() error {
			lease, acquireErr := svcCtx.MutationGate().Acquire(context.Background(), taskID)
			if acquireErr == nil {
				state.setLease(lease)
			}
			return acquireErr
		},
		Run: func(ctx context.Context, execution *task.Execution) error {
			if err := l.validateHandoff(ctx); err != nil {
				return fmt.Errorf("online program update preflight failed: %w", err)
			}
			targetVersion, err := updater.CheckRemoteVersion(ctx, source)
			if err != nil {
				return fmt.Errorf("check remote program version: %w", err)
			}
			if targetVersion == config.Version {
				return execution.Progress("already-current", 100, "当前程序已是最新版本")
			}
			preparedIdentity.TargetVersion = targetVersion
			preparer := updater.NewPreparer(updater.PrepareOptions{StagingParent: options.StagingParent, GOARCH: runtime.GOARCH})
			prepared, err := preparer.Prepare(ctx, preparedIdentity, source, func(update updater.PrepareProgress) error {
				return execution.Progress(update.Stage, update.Percent, update.Message)
			})
			if err != nil {
				return fmt.Errorf("prepare program update: %w", err)
			}
			defer func() { _ = prepared.Cleanup() }()
			drain, err := svcCtx.MutationGate().BeginDrain(taskID)
			if err != nil {
				return err
			}
			state.setDrain(drain)
			if err := execution.Progress("draining-mutations", 90, "等待其他变更任务完成"); err != nil {
				drain.Resume()
				return err
			}
			if err := drain.WaitOthers(ctx); err != nil {
				drain.Resume()
				return fmt.Errorf("wait for active mutations: %w", err)
			}
			placed, err := updater.PlacePreparedBinary(ctx, updater.HandoffOptions{WorkDir: options.WorkDir, StagingParent: options.StagingParent, GOARCH: runtime.GOARCH}, prepared)
			if err != nil {
				return err
			}
			state.setPlaced(placed)
			metadata := updater.HandoffMetadata{
				TaskID: taskID, HandoffID: uuid.NewString(), TargetVersion: targetVersion,
				GOARCH: runtime.GOARCH, BinarySHA256: prepared.BinarySHA256,
			}
			if err := updater.WriteHandoffMetadata(options.WorkDir, metadata); err != nil {
				return err
			}
			state.setMetadata(metadata)
			svcCtx.TaskRuntime.MarkProgramHandoff(metadata)
			state.mu.Lock()
			state.handoff = true
			state.mu.Unlock()
			if err := svcCtx.RequestProgramShutdown(); err != nil {
				state.mu.Lock()
				state.handoff = false
				state.mu.Unlock()
				svcCtx.TaskRuntime.ClearProgramHandoff(taskID)
				return fmt.Errorf("request program restart for handoff: %w", err)
			}
			// The task remains running while the old HTTP process drains. Close
			// cancels the execution after the listener stops; Recover recognizes
			// the handoff and must not remove the candidate or reopen mutations.
			<-ctx.Done()
			return ctx.Err()
		},
		Recover: func(ctx context.Context, cause error) (task.Recovery, error) {
			if state.hasHandedOff() {
				return task.Recovery{Status: "not-needed", Detail: "new process will verify program readiness"}, nil
			}
			rollbackErr := state.rollback()
			state.resume()
			if rollbackErr != nil {
				return task.Recovery{Status: "unknown", Detail: "program handoff rollback requires manual review"}, rollbackErr
			}
			if cause != nil {
				return task.Recovery{Status: "restored", Detail: "程序交接未完成，已恢复接收变更"}, nil
			}
			return task.Recovery{Status: "not-needed"}, nil
		},
		OnSettled: func() { state.settled(svcCtx) },
	})
	if err != nil {
		return nil, err
	}
	releaseReservation = false
	return &types.Resp{Code: 200, Msg: "程序更新任务已接受", Data: map[string]any{"taskID": taskID, "status": task.StatusQueued}}, nil
}

func (l *UpdateProgramLogic) validateHandoff(ctx context.Context) error {
	if l.validateProgramHandoff != nil {
		return l.validateProgramHandoff(ctx)
	}
	if l.serviceContext == nil {
		return errors.New("service context is unavailable")
	}
	return l.serviceContext.ValidateProgramHandoff(ctx)
}
