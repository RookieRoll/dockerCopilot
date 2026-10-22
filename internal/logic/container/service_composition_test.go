package container

import (
	"sync"
	"testing"

	backupService "github.com/onlyLTY/dockerCopilot/internal/backup"
	containerService "github.com/onlyLTY/dockerCopilot/internal/container"
	"github.com/onlyLTY/dockerCopilot/internal/module"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
)

func TestServiceAdaptersReuseComposedInstances(t *testing.T) {
	updates := module.NewImageCheck()
	ctx := &svc.ServiceContext{
		DockerClient: nil,
		HubImageInfo: updates,
		Containers:   containerService.NewService(nil, updates),
		Backups:      backupService.NewService(nil, t.TempDir()),
	}
	if got := serviceFromContext(ctx); got != ctx.Containers {
		t.Fatal("container adapter replaced the shared service instance")
	}
	if got := backupServiceFromContext(ctx); got != ctx.Backups {
		t.Fatal("backup adapter replaced the shared service instance")
	}
}

func TestLiteralContextSharesOneLegacyProgressStore(t *testing.T) {
	ctx := &svc.ServiceContext{}
	first := ctx.ProgressService()
	second := ctx.ProgressService()
	if first != second {
		t.Fatal("literal context created multiple legacy progress stores")
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx.UpdateProgress("same-task", svc.TaskProgress{TaskID: "same-task", Percentage: i})
			_, _ = second.Get("same-task")
		}(i)
	}
	wg.Wait()
	if _, ok := ctx.GetProgress("same-task"); !ok {
		t.Fatal("progress updated through one adapter was not visible through the context")
	}
}
