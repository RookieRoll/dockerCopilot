package task

import (
	"errors"
	"fmt"
)

// ErrPersistenceOutcomeUncertain means an atomic replacement reached the
// destination but neither the new file nor its rollback could be confirmed
// durable. Callers must not treat such a commit as success or compensate it
// automatically; they must retain evidence for manual/read-only recovery.
var ErrPersistenceOutcomeUncertain = errors.New("task persistence outcome is uncertain")

type PersistenceOutcomeUncertainError struct {
	CommitErr   error
	RollbackErr error
}

func (e *PersistenceOutcomeUncertainError) Error() string {
	if e == nil {
		return ErrPersistenceOutcomeUncertain.Error()
	}
	return fmt.Sprintf("%s: commit sync: %v; rollback: %v", ErrPersistenceOutcomeUncertain, e.CommitErr, e.RollbackErr)
}

func (e *PersistenceOutcomeUncertainError) Is(target error) bool {
	return target == ErrPersistenceOutcomeUncertain
}

func (e *PersistenceOutcomeUncertainError) Unwrap() []error {
	if e == nil {
		return nil
	}
	var result []error
	if e.CommitErr != nil {
		result = append(result, e.CommitErr)
	}
	if e.RollbackErr != nil {
		result = append(result, e.RollbackErr)
	}
	return result
}

func IsPersistenceOutcomeUncertain(err error) bool {
	return errors.Is(err, ErrPersistenceOutcomeUncertain)
}
