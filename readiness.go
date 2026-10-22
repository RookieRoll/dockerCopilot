package main

import (
	"context"
	"crypto/subtle"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/zeromicro/go-zero/rest"
)

const readinessProbeHeader = "X-DockerCopilot-Readiness-Probe"

// readinessMiddleware signals only after this server instance has completed a
// successful response to a probe carrying its private startup token.
func readinessMiddleware(token string, ready chan<- struct{}) rest.Middleware {
	return func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			probe := r.Method == http.MethodGet && r.URL.Path == "/" && token != "" &&
				constantTimeEqual(r.Header.Get(readinessProbeHeader), token)
			if !probe {
				next(w, r)
				return
			}
			recorder := &statusResponseWriter{ResponseWriter: w}
			next(recorder, r)
			if recorder.status >= 200 && recorder.status < 400 {
				select {
				case ready <- struct{}{}:
				default:
				}
			}
		}
	}
}

type statusResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusResponseWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(data)
}

func (w *statusResponseWriter) Flush() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func waitForInstanceReady(ctx context.Context, host string, port int, token string, ready <-chan struct{}) error {
	if port < 1 || port > 65535 || token == "" {
		return fmt.Errorf("invalid local readiness probe address")
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	address := net.JoinHostPort(host, strconv.Itoa(port))
	probeTarget := url.URL{Scheme: "http", Host: address, Path: "/"}
	probeURL := probeTarget.String()
	client := &http.Client{Timeout: 2 * time.Second}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, nil)
		if err == nil {
			request.Header.Set(readinessProbeHeader, token)
			response, requestErr := client.Do(request)
			if requestErr == nil {
				_, _ = io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for current HTTP server instance readiness: %w", ctx.Err())
		case <-ready:
			return nil
		case <-ticker.C:
		}
	}
}

func constantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimSpace(a)), []byte(b)) == 1
}
