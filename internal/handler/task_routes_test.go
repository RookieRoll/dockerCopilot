package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v4"
	"github.com/onlyLTY/dockerCopilot/internal/config"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/onlyLTY/dockerCopilot/internal/task"
	"github.com/onlyLTY/dockerCopilot/internal/types"
	"github.com/zeromicro/go-zero/rest"
)

func TestTaskListRouteReturnsPersistedRecordsWithJWT(t *testing.T) {
	port := getFreePort(t)
	cfg := config.Config{}
	cfg.Host, cfg.Port = "127.0.0.1", port
	cfg.Auth.AccessSecret, cfg.Auth.AccessExpire = "task-route-test-secret", 3600
	runtime, err := svc.NewTaskRuntime(context.Background(), svc.TaskRuntimeOptions{
		Directory: t.TempDir(), MaxConcurrent: 1, QueueCapacity: 2,
		TaskTimeout: time.Minute, RecoveryTimeout: time.Second,
		Retention: 24 * time.Hour, MaxRecords: 10,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if _, err := runtime.Runner.Submit(task.TaskSpec{
		Record: task.Record{Type: "route-test", Name: "test record"},
		Run:    func(context.Context, *task.Execution) error { return nil },
	}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Runner.WaitIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx := &svc.ServiceContext{Config: cfg, TaskRuntime: runtime, TaskStore: runtime.Store, TaskRunner: runtime.Runner, TaskQuery: runtime.Query}
	server := rest.MustNewServer(cfg.RestConf)
	RegisterHandlers(server, ctx)
	defer server.Stop()
	go server.Start()
	waitForServer(t, port)

	claims := jwt.MapClaims{"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix()}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString([]byte(cfg.Auth.AccessSecret))
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/api/tasks?status=succeeded", port), nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+signed)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("authenticated task list returned %d: %s", response.StatusCode, body)
	}
	var envelope types.Resp
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(envelope.Data)
	if err != nil {
		t.Fatal(err)
	}
	var page task.TaskPage
	if err := json.Unmarshal(encoded, &page); err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].Status != task.StatusSucceeded {
		t.Fatalf("authenticated task query returned wrong records: %#v", page)
	}
}
