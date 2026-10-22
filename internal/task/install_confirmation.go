package task

import (
	"errors"
	"fmt"
	"strings"
)

var ErrPendingInstallProofMismatch = errors.New("pending program install proof does not match the task handoff")

// ConfirmPendingInstall records success only after the new process has a
// verified proof for the exact persisted handoff. Repeating the same
// confirmation is idempotent; terminal failures can never be promoted.
func (s *Store) ConfirmPendingInstall(taskID string, proof PendingInstallProof) error {
	if s == nil {
		return errors.New("task store is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(); err != nil {
		return err
	}
	record, err := s.readRecord(taskID)
	if err != nil {
		return fmt.Errorf("read pending program install task: %w", err)
	}
	marker := record.PendingInstall
	if !matchesPendingInstallProof(record, proof) {
		return ErrPendingInstallProofMismatch
	}
	if record.Status == StatusSucceeded && record.Stage == "install-confirmed" {
		return nil
	}
	if record.Status != StatusRunning || record.Type != TaskTypeProgramUpdate || marker == nil {
		return fmt.Errorf("pending program install task is not confirmable from status %q", record.Status)
	}
	now := s.now().UTC()
	record.Status = StatusSucceeded
	record.IsDone = true
	record.Stage = "install-confirmed"
	record.Percentage = 100
	record.Message = "新程序已启动并通过就绪确认"
	record.Error = ""
	record.Recovery = Recovery{Status: "not-needed"}
	record.RequiresAttention = false
	record.FinishedAt = &now
	record.UpdatedAt = now
	if err := s.writeRecord(record); err != nil {
		return fmt.Errorf("persist confirmed program install task: %w", err)
	}
	return nil
}

func matchesPendingInstallProof(record Record, proof PendingInstallProof) bool {
	marker := record.PendingInstall
	return proof.Verified && proof.Ready && completePendingInstallMarker(marker) && record.Type == TaskTypeProgramUpdate &&
		proof.TaskID == record.TaskID && proof.HandoffID == marker.HandoffID &&
		proof.TargetVersion == marker.TargetVersion && proof.GOARCH == marker.GOARCH &&
		strings.EqualFold(proof.ArtifactSHA256, marker.ArtifactSHA256)
}
