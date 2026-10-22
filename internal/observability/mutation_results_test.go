package observability

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zeromicro/go-zero/core/logx/logtest"
)

func TestMutationResultMiddlewareCorrelatesFailureWithoutLoggingQueryOrBody(t *testing.T) {
	collector := logtest.NewCollector(t)
	const requestID = "123e4567-e89b-12d3-a456-426614174099"
	handler := RequestIDMiddleware(MutationResultMiddleware(func(w http.ResponseWriter, r *http.Request) {
		WriteHTTPError(w, r, errors.New("permission denied"))
	}))
	req := httptest.NewRequest(http.MethodPost, "/api/image/remove?token=url-secret", strings.NewReader("body-secret"))
	req.Header.Set("X-Request-ID", requestID)
	recorder := httptest.NewRecorder()
	handler(recorder, req)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	logs := collector.String()
	if strings.Count(logs, requestID) < 2 || !strings.Contains(logs, `"event":"http.mutation_result"`) || !strings.Contains(logs, `"result":"failed"`) || !strings.Contains(logs, `"method":"POST"`) || !strings.Contains(logs, `"path":"/api/image/remove"`) {
		t.Fatalf("middleware and HTTP error logs are not correlated: %s", logs)
	}
	for _, forbidden := range []string{"url-secret", "body-secret", "?token"} {
		if strings.Contains(logs, forbidden) {
			t.Fatalf("request data leaked into mutation result log (%q): %s", forbidden, logs)
		}
	}
}

func TestMutationResultMiddlewareLogsSuccessAndLegacyExportButNotPolling(t *testing.T) {
	collector := logtest.NewCollector(t)
	const requestID = "123e4567-e89b-12d3-a456-426614174098"
	run := func(method, target string, status int) {
		handler := RequestIDMiddleware(MutationResultMiddleware(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
		}))
		req := httptest.NewRequest(method, target, nil)
		req.Header.Set("X-Request-ID", requestID)
		handler(httptest.NewRecorder(), req)
	}
	run(http.MethodPut, "/api/settings/ports?token=private-query", http.StatusNoContent)
	if logs := collector.String(); !strings.Contains(logs, `"event":"http.mutation_result"`) || !strings.Contains(logs, `"result":"succeeded"`) || strings.Contains(logs, "private-query") {
		t.Fatalf("successful mutation summary missing or query leaked: %s", logs)
	}
	beforePoll := collector.String()
	run(http.MethodGet, "/api/progress/task-123?token=poll-secret", http.StatusOK)
	if collector.String() != beforePoll {
		t.Fatalf("polling GET produced mutation log: %s", collector.String()[len(beforePoll):])
	}
	run(http.MethodGet, "/api/container/backup?token=export-secret", http.StatusOK)
	logs := collector.String()[len(beforePoll):]
	if !strings.Contains(logs, `"method":"GET"`) || !strings.Contains(logs, `"path":"/api/container/backup"`) || !strings.Contains(logs, `"result":"succeeded"`) {
		t.Fatalf("legacy GET export summary missing: %s", logs)
	}
	for _, forbidden := range []string{"export-secret", "poll-secret", "private-query"} {
		if strings.Contains(collector.String(), forbidden) {
			t.Fatalf("query data leaked into logs (%q): %s", forbidden, collector.String())
		}
	}
}

func TestMutationResponseWriterUnwrapsOriginalWriter(t *testing.T) {
	original := httptest.NewRecorder()
	wrapper := &mutationResponseWriter{ResponseWriter: original}
	if got := http.NewResponseController(wrapper).Flush(); got != nil {
		t.Fatalf("ResponseController could not reach the original writer: %v", got)
	}
	if wrapper.status != http.StatusOK {
		t.Fatalf("flush did not record implicit status: %d", wrapper.status)
	}
}
