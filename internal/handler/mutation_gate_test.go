package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/onlyLTY/dockerCopilot/internal/svc"
)

func TestMutationGateMiddlewareBlocksWritesButAllowsReadAndTaskQuery(t *testing.T) {
	ctx := &svc.ServiceContext{}
	drain, err := ctx.MutationGate().BeginDrain("program-task")
	if err != nil {
		t.Fatal(err)
	}
	defer drain.Resume()
	middleware := mutationGateMiddleware(ctx)
	called := 0
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { called++; w.WriteHeader(http.StatusNoContent) })
	for _, tc := range []struct {
		method string
		path   string
		want   int
	}{
		{http.MethodGet, "/api/container/backup", http.StatusServiceUnavailable},
		{http.MethodPost, "/api/container/c1/start", http.StatusServiceUnavailable},
		{http.MethodPut, "/api/container/c1/configured-ports", http.StatusServiceUnavailable},
		{http.MethodDelete, "/api/image/sha256:abc", http.StatusServiceUnavailable},
		{http.MethodGet, "/api/tasks", http.StatusNoContent},
		{http.MethodPost, "/api/container/c1/update", http.StatusNoContent},
	} {
		recorder := httptest.NewRecorder()
		middleware(next).ServeHTTP(recorder, httptest.NewRequest(tc.method, tc.path, nil))
		if recorder.Code != tc.want {
			t.Errorf("%s %s status=%d, want %d", tc.method, tc.path, recorder.Code, tc.want)
		}
	}
	if called != 2 {
		t.Fatalf("read and task submission requests reaching next=%d, want 2", called)
	}
}
