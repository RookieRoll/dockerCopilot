package types

// TaskProgress is the legacy HTTP progress projection used by update and
// restore callers. Persistent runner records are modeled separately.
type TaskProgress struct {
	TaskID     string
	Percentage int
	Message    string
	Name       string
	DetailMsg  string
	IsDone     bool
}
