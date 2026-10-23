package progress

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/onlyLTY/dockerCopilot/internal/task"
	"github.com/onlyLTY/dockerCopilot/internal/types"
)

func TestMissingProgressReportsUnknownResult(t *testing.T) {
	store, err := task.Open(task.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	query, err := task.NewQueryService(store, task.QueryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	logic := &GetProgressLogic{ctx: context.Background(), taskQuery: query}
	resp, err := logic.GetProgress(&types.GetProgressReq{TaskId: uuid.NewString()})
	var expired taskNotFoundError
	if !errors.As(err, &expired) || expired.HTTPStatus() != http.StatusGone || expired.ErrorCode() != "OPERATION_RESULT_EXPIRED" {
		t.Fatalf("expected expired progress, got response=%+v error=%v", resp, err)
	}
	if resp.Code != http.StatusGone {
		t.Fatalf("unexpected status: %d", resp.Code)
	}
	if resp.Msg != "进度已失效，服务可能已重启；操作结果未知，请核对容器状态和日志" {
		t.Fatalf("unexpected result: %q", resp.Msg)
	}
}
