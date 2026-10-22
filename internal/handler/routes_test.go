package handler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/onlyLTY/dockerCopilot/internal/config"
	"github.com/onlyLTY/dockerCopilot/internal/observability"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/zeromicro/go-zero/rest"
)

func TestProgressRouteRequiresJWT(t *testing.T) {
	port := getFreePort(t)
	cfg := config.Config{}
	cfg.Host = "127.0.0.1"
	cfg.Port = port
	cfg.Auth.AccessSecret = "test-secret"

	server := rest.MustNewServer(cfg.RestConf)
	RegisterHandlers(server, &svc.ServiceContext{Config: cfg})
	defer server.Stop()

	go server.Start()
	waitForServer(t, port)

	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/api/progress/test-task", port))
	if err != nil {
		t.Fatalf("failed to call progress route: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without JWT, got %d", resp.StatusCode)
	}
}

func TestTaskListRouteRequiresJWT(t *testing.T) {
	port := getFreePort(t)
	cfg := config.Config{}
	cfg.Host = "127.0.0.1"
	cfg.Port = port
	cfg.Auth.AccessSecret = "test-secret"

	server := rest.MustNewServer(cfg.RestConf)
	RegisterHandlers(server, &svc.ServiceContext{Config: cfg})
	defer server.Stop()

	go server.Start()
	waitForServer(t, port)

	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/api/tasks", port))
	if err != nil {
		t.Fatalf("failed to call task list route: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without JWT, got %d", resp.StatusCode)
	}
}

func TestBatchRouteRequiresJWT(t *testing.T) {
	port := getFreePort(t)
	cfg := config.Config{}
	cfg.Host = "127.0.0.1"
	cfg.Port = port
	cfg.Auth.AccessSecret = "test-secret"

	server := rest.MustNewServer(cfg.RestConf)
	RegisterHandlers(server, &svc.ServiceContext{Config: cfg})
	defer server.Stop()

	go server.Start()
	waitForServer(t, port)

	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/api/containers/batch", port), bytes.NewBufferString(`{"action":"stop","containerIDs":["container-id"]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("failed to call batch route: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without JWT, got %d", resp.StatusCode)
	}
}

func TestConfiguredPortsRouteRequiresJWT(t *testing.T) {
	port := getFreePort(t)
	cfg := config.Config{}
	cfg.Host = "127.0.0.1"
	cfg.Port = port
	cfg.Auth.AccessSecret = "test-secret"

	server := rest.MustNewServer(cfg.RestConf)
	RegisterHandlers(server, &svc.ServiceContext{Config: cfg})
	defer server.Stop()

	go server.Start()
	waitForServer(t, port)

	req, err := http.NewRequest(
		http.MethodPut,
		fmt.Sprintf("http://127.0.0.1:%d/api/container/test/configured-ports", port),
		bytes.NewBufferString(`{"ports":[{"port":8080,"protocol":"tcp"}]}`),
	)
	if err != nil {
		t.Fatalf("failed to create configured ports request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("failed to call configured ports route: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without JWT, got %d", resp.StatusCode)
	}
}

func TestAuthRouteDoesNotRequireJWT(t *testing.T) {
	port := getFreePort(t)
	cfg := config.Config{}
	cfg.Host = "127.0.0.1"
	cfg.Port = port
	cfg.Auth.AccessSecret = "test-secret"

	server := rest.MustNewServer(cfg.RestConf)
	RegisterHandlers(server, &svc.ServiceContext{Config: cfg})
	defer server.Stop()

	go server.Start()
	waitForServer(t, port)

	req, err := http.NewRequest(
		http.MethodPost,
		fmt.Sprintf("http://127.0.0.1:%d/api/auth", port),
		bytes.NewBufferString("secretKey=test-secret"),
	)
	if err != nil {
		t.Fatalf("failed to create auth request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("failed to call auth route: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read auth response body: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from auth route without JWT, got %d with body %s", resp.StatusCode, string(body))
	}
}

func TestAuthWrongSecretReturnsUnauthorizedEnvelope(t *testing.T) {
	port := getFreePort(t)
	cfg := config.Config{}
	cfg.Host = "127.0.0.1"
	cfg.Port = port
	cfg.Auth.AccessSecret = "configured-login-secret"

	server := rest.MustNewServer(cfg.RestConf)
	server.Use(observability.RequestIDMiddleware)
	RegisterHandlers(server, &svc.ServiceContext{Config: cfg})
	defer server.Stop()

	go server.Start()
	waitForServer(t, port)

	const requestID = "d249424d-97d8-437e-9c46-c68eb666d5a6"
	const submittedSecret = "attacker-supplied-secret-value"
	form := url.Values{"secretKey": []string{submittedSecret}}
	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/api/auth", port), strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Request-ID", requestID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("failed to call auth route: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read auth response: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong secret HTTP status = %d, want 401; body=%s", resp.StatusCode, body)
	}
	var envelope observability.HTTPErrorResponse
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode auth error envelope: %v; body=%s", err, body)
	}
	if envelope.Code != http.StatusUnauthorized || envelope.Msg != "无效的secretKey" || envelope.ErrorCode != "UNAUTHORIZED" {
		t.Fatalf("unexpected auth error envelope: %#v", envelope)
	}
	if envelope.RequestID != requestID || resp.Header.Get("X-Request-ID") != requestID {
		t.Fatalf("auth request ID mismatch: body=%q header=%q", envelope.RequestID, resp.Header.Get("X-Request-ID"))
	}
	if strings.Contains(string(body), submittedSecret) {
		t.Fatalf("auth error response echoed submitted secret: %s", body)
	}
}

func getFreePort(t *testing.T) int {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to get free port: %v", err)
	}
	defer listener.Close()

	return listener.Addr().(*net.TCPAddr).Port
}

func waitForServer(t *testing.T, port int) {
	t.Helper()

	client := &http.Client{Timeout: 200 * time.Millisecond}
	url := fmt.Sprintf("http://127.0.0.1:%d/", port)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}

	t.Fatalf("server on port %d did not become ready", port)
}
