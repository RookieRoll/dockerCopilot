package handler

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/onlyLTY/dockerCopilot/internal/config"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/zeromicro/go-zero/rest"
)

func TestImageCleanupRouteRequiresJWT(t *testing.T) {
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

	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/api/images/cleanup", port), strings.NewReader(`{"policy":"unused"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("call image cleanup route: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("POST /api/images/cleanup status=%d, want JWT rejection 401", resp.StatusCode)
	}
}
