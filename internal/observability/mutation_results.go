package observability

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
)

// MutationResultMiddleware logs bounded request metadata for synchronous
// mutations and the two legacy GET export endpoints. It never reads headers or
// bodies; ordinary GET polling stays quiet.
func MutationResultMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !isMutationResultRequest(r.Method, r.URL.Path) {
			next(w, r)
			return
		}
		started := time.Now()
		capture := &mutationResponseWriter{ResponseWriter: w}
		next(capture, r)
		status := capture.status
		if status == 0 {
			status = http.StatusOK
		}
		result := "succeeded"
		severity := "INFO"
		if status >= http.StatusBadRequest {
			result = "failed"
			severity = "ERROR"
		}
		fields := []logx.LogField{
			logx.Field("event", "http.mutation_result"),
			logx.Field("requestId", requestIDFromContext(r.Context())),
			logx.Field("method", r.Method),
			logx.Field("path", RedactText(r.URL.EscapedPath())),
			logx.Field("status", status),
			logx.Field("result", result),
			logx.Field("durationMs", time.Since(started).Milliseconds()),
			logx.Field("severity", severity),
		}
		if severity == "ERROR" {
			logx.WithContext(r.Context()).Errorw("http mutation result", fields...)
		} else {
			logx.WithContext(r.Context()).Infow("http mutation result", fields...)
		}
	}
}

func isMutationResultRequest(method, path string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	case http.MethodGet:
		path = strings.TrimRight(path, "/")
		return strings.HasSuffix(path, "/container/backup") || strings.HasSuffix(path, "/container/backup2compose")
	default:
		return false
	}
}

// mutationResponseWriter records status while preserving common streaming
// interfaces. Unwrap lets http.ResponseController reach the original writer.
type mutationResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *mutationResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *mutationResponseWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *mutationResponseWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(data)
}

func (w *mutationResponseWriter) Flush() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *mutationResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return hijacker.Hijack()
}

func (w *mutationResponseWriter) Push(target string, options *http.PushOptions) error {
	pusher, ok := w.ResponseWriter.(http.Pusher)
	if !ok {
		return http.ErrNotSupported
	}
	return pusher.Push(target, options)
}

func (w *mutationResponseWriter) ReadFrom(reader io.Reader) (int64, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if readerFrom, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		return readerFrom.ReadFrom(reader)
	}
	return io.Copy(struct{ io.Writer }{w.ResponseWriter}, reader)
}

var _ http.Hijacker = (*mutationResponseWriter)(nil)
var _ http.Flusher = (*mutationResponseWriter)(nil)
var _ http.Pusher = (*mutationResponseWriter)(nil)
var _ io.ReaderFrom = (*mutationResponseWriter)(nil)
