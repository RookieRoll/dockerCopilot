package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestReadinessMiddlewareSignalsOnlySuccessfulPrivateProbe(t *testing.T) {
	ready := make(chan struct{}, 1)
	handler := readinessMiddleware("startup-secret", ready)(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	for _, tc := range []struct {
		name   string
		token  string
		want   bool
		status int
	}{
		{name: "ordinary request", status: http.StatusNoContent},
		{name: "wrong token", token: "other", status: http.StatusNoContent},
		{name: "private probe", token: "startup-secret", want: true, status: http.StatusNoContent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.token != "" {
				request.Header.Set(readinessProbeHeader, tc.token)
			}
			response := httptest.NewRecorder()
			handler(response, request)
			if response.Code != tc.status {
				t.Fatalf("status = %d, want %d", response.Code, tc.status)
			}
			select {
			case <-ready:
				if !tc.want {
					t.Fatal("unexpected readiness signal")
				}
			case <-time.After(10 * time.Millisecond):
				if tc.want {
					t.Fatal("expected readiness signal")
				}
			}
		})
	}
}

func TestReadinessMiddlewareDoesNotSignalFailedProbe(t *testing.T) {
	ready := make(chan struct{}, 1)
	handler := readinessMiddleware("startup-secret", ready)(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set(readinessProbeHeader, "startup-secret")
	response := httptest.NewRecorder()
	handler(response, request)
	select {
	case <-ready:
		t.Fatal("failed probe signaled readiness")
	default:
	}
}

func TestWaitForInstanceReadyUsesThisServersPrivateProbe(t *testing.T) {
	ready := make(chan struct{}, 1)
	server := httptest.NewServer(readinessMiddleware("private-token", ready)(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	host, portText, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := waitForInstanceReady(ctx, host, port, "private-token", ready); err != nil {
		t.Fatalf("wait for matching instance readiness: %v", err)
	}
}
