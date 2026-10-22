package container

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/onlyLTY/dockerCopilot/internal/operation"
)

func TestRenameCallbackRunsBeforeMutationLeaseRelease(t *testing.T) {
	service := newOperationTestService(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1.45/containers/") && strings.HasSuffix(r.URL.Path, "/json"):
			writeInspectedContainer(w)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/rename"):
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})
	callbackCalled := false
	err := service.RenameWithCallback(context.Background(), "short-id", "renamed", func(oldName string) error {
		callbackCalled = true
		if oldName != "web" {
			t.Fatalf("callback old name = %q", oldName)
		}
		_, reserveErr := service.Operations().Reserve(operation.Request{TaskID: "concurrent-list-cleanup", Kind: "container-stop", Target: operation.Target{Name: "renamed"}})
		var conflict *operation.ConflictError
		if !errors.As(reserveErr, &conflict) {
			return errors.New("rename lease was released before metadata migration")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !callbackCalled {
		t.Fatal("metadata migration callback was not called")
	}
}
