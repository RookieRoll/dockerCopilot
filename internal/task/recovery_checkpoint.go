package task

import (
	"context"
	"errors"
	"reflect"
	"time"
)

// recoveryCheckpointStore persists only a small set of recovery facts without
// changing task identity, the terminal result, or the original error.
type recoveryCheckpointStore interface {
	saveRecoveryCheckpoint(Record) error
}

// RecoveryCheckpoint persists a recovery stage and newly confirmed container
// identity facts. It is valid only while this execution's recovery callback is
// running with the context supplied to that callback.
func (e *Execution) RecoveryCheckpoint(ctx context.Context, stage string, mutate func(*Record)) error {
	if e == nil {
		return errors.New("task execution is unavailable for recovery checkpoint")
	}
	if ctx == nil || stage == "" {
		return errors.New("recovery checkpoint requires its recovery context and stage")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.recoveryCtx != ctx {
		return errors.New("recovery checkpoint is not authorized outside the active recovery callback")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	next := cloneRecord(e.record)
	next.Stage = stage
	if mutate != nil {
		mutate(&next)
	}
	if next.TaskID != e.record.TaskID || next.Type != e.record.Type || next.Status != e.record.Status ||
		!next.CreatedAt.Equal(e.record.CreatedAt) || !sameOptionalTime(next.StartedAt, e.record.StartedAt) ||
		!sameOptionalTime(next.FinishedAt, e.record.FinishedAt) || next.RequestID != e.record.RequestID ||
		!optionsEqual(next.Options, e.record.Options) || next.Error != e.record.Error {
		return errors.New("recovery checkpoint cannot change task identity or terminal result")
	}
	expected := cloneRecord(e.record)
	expected.Stage = next.Stage
	expected.NewContainerID = next.NewContainerID
	expected.ContainerIDs = cloneStrings(next.ContainerIDs)
	expected.BackupContainerName = next.BackupContainerName
	expected.UpdatedAt = next.UpdatedAt
	if !reflect.DeepEqual(next, expected) {
		return errors.New("recovery checkpoint may only change stage and confirmed container identity facts")
	}
	store, ok := e.store.(recoveryCheckpointStore)
	if !ok {
		return errors.New("task record store does not support recovery checkpoints")
	}
	if err := store.saveRecoveryCheckpoint(next); err != nil {
		return err
	}
	e.record = next
	if err := ctx.Err(); err != nil {
		// The facts may already be durable, but an expired recovery context must
		// never authorize the next Docker mutation.
		return err
	}
	return nil
}

func sameOptionalTime(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Equal(*right)
}

func (e *Execution) authorizeRecovery(ctx context.Context) {
	e.mu.Lock()
	e.recoveryCtx = ctx
	e.mu.Unlock()
}

func (e *Execution) revokeRecovery(ctx context.Context) {
	e.mu.Lock()
	if e.recoveryCtx == ctx {
		e.recoveryCtx = nil
	}
	e.mu.Unlock()
}

// saveRecoveryCheckpoint applies the recovery checkpoint allowlist to both
// active and already-terminal tasks. In particular, it cannot rewrite Error,
// status, completion time, recovery outcome, or other final task results.
func (s *Store) saveRecoveryCheckpoint(next Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(); err != nil {
		return err
	}
	if err := next.validate(); err != nil {
		return err
	}
	old, err := s.readRecord(next.TaskID)
	if err != nil {
		return err
	}
	if old.Status != StatusRunning && !old.Status.Terminal() {
		return errors.New("recovery checkpoint requires a running or terminal task")
	}
	if old.TaskID != next.TaskID || old.Type != next.Type || old.Status != next.Status ||
		!old.CreatedAt.Equal(next.CreatedAt) || !sameOptionalTime(old.StartedAt, next.StartedAt) ||
		!sameOptionalTime(old.FinishedAt, next.FinishedAt) || old.RequestID != next.RequestID ||
		!optionsEqual(old.Options, next.Options) || old.Error != next.Error {
		return errors.New("recovery checkpoint cannot alter task identity or final result")
	}
	expected := cloneRecord(old)
	expected.Stage = next.Stage
	expected.NewContainerID = next.NewContainerID
	expected.ContainerIDs = cloneStrings(next.ContainerIDs)
	expected.BackupContainerName = next.BackupContainerName
	expected.UpdatedAt = next.UpdatedAt
	if !reflect.DeepEqual(next, expected) {
		return errors.New("recovery checkpoint contains fields outside the recovery allowlist")
	}
	expected.UpdatedAt = s.now().UTC()
	if err := expected.validate(); err != nil {
		return err
	}
	return s.writeRecord(expected)
}
