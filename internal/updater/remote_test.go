package updater

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/onlyLTY/dockerCopilot/internal/config"
)

func TestVersionCheckerRejectsInvalidResponsesAndDoesNotCacheFailures(t *testing.T) {
	var status int
	var body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = fmt.Fprint(w, body)
	}))
	defer server.Close()
	source := proxySource(server.URL, "RookieRoll/dockerCopilot")

	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{name: "404 HTML", status: http.StatusNotFound, body: "<html>not found</html>"},
		{name: "successful HTML", status: http.StatusOK, body: "<html>2.0.0</html>"},
		{name: "empty", status: http.StatusOK, body: "  \n"},
		{name: "malformed version", status: http.StatusOK, body: "2.0"},
		{name: "malformed suffix", status: http.StatusOK, body: "2.0.0-alpha..1"},
		{name: "oversized", status: http.StatusOK, body: strings.Repeat("a", maxVersionFileBytes+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checker := NewVersionChecker(server.Client())
			status, body = tc.status, tc.body
			if got, err := checker.Check(context.Background(), source); err == nil || got != "" {
				t.Fatalf("Check() = (%q,%v), want failure", got, err)
			}
			status, body = http.StatusOK, "v2.2.2-rc.1"
			got, err := checker.Check(context.Background(), source)
			if err != nil || got != "v2.2.2-rc.1" {
				t.Fatalf("recovery Check() = (%q,%v)", got, err)
			}
		})
	}
}

func TestVersionCheckerAcceptsDatedThreePartVersionAndUsesProxyPath(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = fmt.Fprintln(w, "2026.10.22+build.4")
	}))
	defer server.Close()
	checker := NewVersionChecker(server.Client())
	source := proxySource(server.URL+"/prefix/", "RookieRoll/dockerCopilot")
	version, err := checker.Check(context.Background(), source)
	if err != nil || version != "2026.10.22+build.4" {
		t.Fatalf("Check() = (%q,%v)", version, err)
	}
	want := "/prefix/https://raw.githubusercontent.com/RookieRoll/dockerCopilot/latest/version"
	if gotPath != want {
		t.Fatalf("request path = %q, want %q", gotPath, want)
	}
}

func TestVersionCheckerDefaultsRepository(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = fmt.Fprintln(w, "2.0.0")
	}))
	defer server.Close()
	checker := NewVersionChecker(server.Client())
	if _, err := checker.Check(context.Background(), config.UpdateConfig{GitHubProxy: server.URL}); err != nil {
		t.Fatal(err)
	}
	want := "/https://raw.githubusercontent.com/RookieRoll/dockerCopilot/latest/version"
	if gotPath != want {
		t.Fatalf("default repository path = %q, want %q", gotPath, want)
	}
}

func TestVersionCheckerCacheIsolatedBySourceURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/org/first/") {
			_, _ = fmt.Fprintln(w, "1.2.3")
		} else {
			_, _ = fmt.Fprintln(w, "4.5.6")
		}
	}))
	defer server.Close()
	checker := NewVersionChecker(server.Client())
	first := proxySource(server.URL, "org/first")
	second := proxySource(server.URL, "org/second")
	for _, tc := range []struct {
		source config.UpdateConfig
		want   string
	}{{first, "1.2.3"}, {second, "4.5.6"}, {first, "1.2.3"}} {
		got, err := checker.Check(context.Background(), tc.source)
		if err != nil || got != tc.want {
			t.Fatalf("Check(%q) = (%q,%v), want %q", tc.source.Repository, got, err, tc.want)
		}
	}
}

func TestVersionCheckerHonorsTimeoutAndCallerCancellation(t *testing.T) {
	t.Run("client timeout", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
		defer server.Close()
		client := &http.Client{Transport: server.Client().Transport, Timeout: 30 * time.Millisecond}
		checker := NewVersionChecker(client)
		source := proxySource(server.URL, "org/slow")
		if _, err := checker.Check(context.Background(), source); err == nil || !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "request remote version") {
			t.Fatalf("timeout error = %v", err)
		}
	})
	t.Run("in-flight caller cancellation", func(t *testing.T) {
		entered := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			close(entered)
			<-r.Context().Done()
		}))
		defer server.Close()
		client := &http.Client{Transport: server.Client().Transport, Timeout: 5 * time.Second}
		checker := NewVersionChecker(client)
		source := proxySource(server.URL, "org/cancel")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		result := make(chan error, 1)
		go func() {
			_, err := checker.Check(ctx, source)
			result <- err
		}()
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("caller-cancel request did not reach fake server")
		}
		cancel()
		select {
		case err := <-result:
			if err == nil || !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "request remote version") {
				t.Fatalf("cancellation error = %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("caller cancellation did not unblock version request")
		}
	})
}

func TestVersionCheckerCachesOnlySuccessAndExpires(t *testing.T) {
	status, body := http.StatusServiceUnavailable, "unavailable"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = fmt.Fprintln(w, body)
	}))
	defer server.Close()
	checker := NewVersionChecker(server.Client())
	now := time.Now()
	checker.now = func() time.Time { return now }
	source := proxySource(server.URL, "org/cache")
	if _, err := checker.Check(context.Background(), source); err == nil {
		t.Fatal("expected first request to fail")
	}
	status, body = http.StatusOK, "1.2.3"
	if got, err := checker.Check(context.Background(), source); err != nil || got != "1.2.3" {
		t.Fatalf("retry after failure = (%q,%v)", got, err)
	}
	body = "4.5.6"
	if got, err := checker.Check(context.Background(), source); err != nil || got != "1.2.3" {
		t.Fatalf("cached result = (%q,%v), want previous success", got, err)
	}
	now = now.Add(remoteVersionTTL + time.Second)
	if got, err := checker.Check(context.Background(), source); err != nil || got != "4.5.6" {
		t.Fatalf("expired result = (%q,%v)", got, err)
	}
}

func TestVersionCheckerPreservesNetworkCause(t *testing.T) {
	want := errors.New("synthetic connection reset")
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, want
	})}
	checker := NewVersionChecker(client)
	_, err := checker.Check(context.Background(), config.UpdateConfig{Repository: "org/cause", GitHubProxy: "http://proxy.example"})
	if !errors.Is(err, want) || !strings.Contains(err.Error(), "request remote version") {
		t.Fatalf("network error %v does not preserve cause %v", err, want)
	}
}

func TestRemoteVersionFNOSDoesNotMakeRequest(t *testing.T) {
	old := config.Version
	config.Version = "1.0.0-FNOS"
	t.Cleanup(func() { config.Version = old })
	got, err := CheckRemoteVersion(context.Background(), config.UpdateConfig{Repository: "invalid"})
	if err != nil || got != config.Version {
		t.Fatalf("FNOS version = (%q,%v), want local %q", got, err, config.Version)
	}
}

func proxySource(proxy, repository string) config.UpdateConfig {
	return config.UpdateConfig{Repository: repository, GitHubProxy: proxy}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}
