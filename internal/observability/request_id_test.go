package observability

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/zeromicro/go-zero/core/logx"
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
	for _, value := range []string{"task.accepted", `"taskId":"task-123"`, `"taskID":"task-123"`, "123e4567-e89b-12d3-a456-426614174000"} {
		if !strings.Contains(collector.String(), value) {
			t.Fatalf("task log lacks %q: %s", value, collector.String())
		}
	}
}

func TestLogOperationResultIncludesOnlyCorrelationAndOutcome(t *testing.T) {
	collector := logtest.NewCollector(t)
	ctx := context.WithValue(context.Background(), requestIDKey{}, "123e4567-e89b-12d3-a456-426614174000")
	LogOperationResult(ctx, "container.stop", "container-123", "succeeded")
	for _, value := range []string{"container.stop", "container-123", "succeeded", "123e4567-e89b-12d3-a456-426614174000"} {
		if !strings.Contains(collector.String(), value) {
			t.Fatalf("operation log lacks %q: %s", value, collector.String())
		}
	}
}

func TestStructuredEventsCarrySeverityAndSafeErrorCode(t *testing.T) {
	collector := logtest.NewCollector(t)
	LogTaskEvent(context.Background(), "task.finished", "task-failed", "req-failed",
		logx.Field("status", "failed"), logx.Field("errorCode", "INTERNAL_ERROR"))
	LogTaskEvent(context.Background(), "task.recovery", "task-unknown", "req-unknown",
		logx.Field("recoveryStatus", "unknown"), logx.Field("requiresAttention", true))
	LogOperationResult(context.Background(), "container.stop", "container-id", "failed", errors.New("https://user:password@example.test/?token=secret"))
	logs := collector.String()
	for _, value := range []string{`"severity":"ERROR"`, `"severity":"WARN"`, `"errorCode":"INTERNAL_ERROR"`, "container.stop"} {
		if !strings.Contains(logs, value) {
			t.Fatalf("structured logs missing %q: %s", value, logs)
		}
	}
	for _, secret := range []string{"password", "secret"} {
		if strings.Contains(logs, secret) {
			t.Fatalf("structured logs exposed error text %q: %s", secret, logs)
		}
	}
	LogTaskEvent(context.Background(), "task.finished", "task-warning", "req-warning",
		logx.Field("status", "succeeded"), logx.Field("warningCount", 1))
	LogTaskEvent(context.Background(), "task.recovery", "task-recovery-failed", "req-recovery-failed",
		logx.Field("recoveryStatus", "failed"), logx.Field("requiresAttention", true))
	if !strings.Contains(collector.String(), `"task-warning"`) || strings.Count(collector.String(), `"severity":"WARN"`) < 2 || !strings.Contains(collector.String(), `"severity":"ERROR"`) {
		t.Fatalf("warning/recovery severity missing: %s", collector.String())
	}
}

func TestOperationResultIncludesDurationAndRedactedUnwrappedCause(t *testing.T) {
	collector := logtest.NewCollector(t)
	ctx := context.WithValue(context.Background(), requestIDKey{}, "123e4567-e89b-12d3-a456-426614174056")
	wrapped := errors.New("daemon rejected authorization=super-secret")
	LogOperationResultWithDuration(ctx, "container.rename", "container-789", "failed", 123*time.Millisecond,
		fmt.Errorf("rename failed: %w", wrapped))
	logs := collector.String()
	for _, value := range []string{`"requestId":"123e4567-e89b-12d3-a456-426614174056"`, `"containerId":"container-789"`, `"durationMs":123`, "daemon rejected", "[REDACTED]"} {
		if !strings.Contains(logs, value) {
			t.Fatalf("operation log lacks %q: %s", value, logs)
		}
	}
	if strings.Contains(logs, "super-secret") {
		t.Fatalf("operation log exposed secret: %s", logs)
	}
}

func TestLogTaskPullProgressEmitsDebugAndRedactsMessage(t *testing.T) {
	collector := logtest.NewCollector(t)
	logx.SetLevel(logx.DebugLevel)
	defer logx.SetLevel(logx.InfoLevel)
	LogTaskPullProgress(context.Background(), "task-pull", "request-pull", "pulling-image", 42, "token=top-secret layer 3")
	logs := collector.String()
	for _, value := range []string{`"event":"task.pull_progress"`, `"taskId":"task-pull"`, `"severity":"DEBUG"`, `"percentage":42`, "layer 3", "[REDACTED]"} {
		if !strings.Contains(logs, value) {
			t.Fatalf("pull debug log lacks %q: %s", value, logs)
		}
	}
	if strings.Contains(logs, "top-secret") {
		t.Fatalf("pull debug log exposed secret: %s", logs)
	}
}

func TestSafeErrorCodeDoesNotReturnFreeformCause(t *testing.T) {
	err := errors.New("daemon failed for https://user:password@example.test/?token=secret")
	if got := SafeErrorCode(err); got != "INTERNAL_ERROR" {
		t.Fatalf("unexpected error code: %q", got)
	}
	if got := SafeErrorCode(nil); got != "" {
		t.Fatalf("nil error code = %q", got)
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
