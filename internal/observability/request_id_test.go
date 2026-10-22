package observability

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/zeromicro/go-zero/core/logx/logtest"
)

func TestRequestIDMiddlewareNormalizesAndSharesRequestID(t *testing.T) {
	want := "123e4567-e89b-12d3-a456-426614174000"
	handler := RequestIDMiddleware(func(w http.ResponseWriter, r *http.Request) {
		if got := RequestIDFromContext(r.Context()); got != want {
			t.Errorf("context request ID = %q, want %q", got, want)
		}
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-ID", "123E4567-E89B-12D3-A456-426614174000")
	response := httptest.NewRecorder()
	handler(response, req)
	if got := response.Header().Get("X-Request-ID"); got != want {
		t.Fatalf("response request ID = %q, want %q", got, want)
	}
}

func TestLogTaskEventIncludesCorrelationFields(t *testing.T) {
	collector := logtest.NewCollector(t)
	LogTaskEvent(context.Background(), "task.accepted", "task-123", "123e4567-e89b-12d3-a456-426614174000")
	for _, value := range []string{"task.accepted", "task-123", "123e4567-e89b-12d3-a456-426614174000"} {
		if !strings.Contains(collector.String(), value) {
			t.Fatalf("task log lacks %q: %s", value, collector.String())
		}
	}
}

func TestRequestIDMiddlewareReplacesInvalidClientValue(t *testing.T) {
	handler := RequestIDMiddleware(func(w http.ResponseWriter, r *http.Request) {
		id := RequestIDFromContext(r.Context())
		if _, err := uuid.Parse(id); err != nil {
			t.Errorf("context ID is not a UUID: %q", id)
		}
		if id == "req-123" {
			t.Error("invalid client value was preserved")
		}
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-ID", "req-123")
	response := httptest.NewRecorder()
	handler(response, req)
	if _, err := uuid.Parse(response.Header().Get("X-Request-ID")); err != nil {
		t.Fatalf("response header is not a UUID: %q", response.Header().Get("X-Request-ID"))
	}
}
