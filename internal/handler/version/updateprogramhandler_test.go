package version

import (
	"testing"

	"github.com/google/uuid"
)

func TestProgramUpdateRequestIDAcceptsUUIDAndReplacesInvalidHeader(t *testing.T) {
	valid := "123E4567-E89B-12D3-A456-426614174000"
	if got := programUpdateRequestID(valid); got != "123e4567-e89b-12d3-a456-426614174000" {
		t.Fatalf("valid UUID was not canonicalized: %q", got)
	}
	invalid := programUpdateRequestID("not-a-uuid")
	if _, err := uuid.Parse(invalid); err != nil {
		t.Fatalf("invalid header did not produce a UUID: %q", invalid)
	}
	if invalid == "not-a-uuid" {
		t.Fatal("invalid header was preserved")
	}
}
