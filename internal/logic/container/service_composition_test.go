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

func TestLiteralContextSharesLazilyConstructedServices(t *testing.T) {
	ctx := &svc.ServiceContext{}
	var wg sync.WaitGroup
	containers := make(chan *containerService.Service, 16)
	backups := make(chan *backupService.Service, 16)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			containers <- serviceFromContext(ctx)
			backups <- backupServiceFromContext(ctx)
		}()
	}
	wg.Wait()
	close(containers)
	close(backups)
	var oneContainer *containerService.Service
	for service := range containers {
		if service == nil {
			t.Fatal("container service was not constructed")
		}
		if oneContainer == nil {
			oneContainer = service
		} else if service != oneContainer {
			t.Fatal("literal context constructed multiple container services")
		}
	}
	var oneBackup *backupService.Service
	for service := range backups {
		if service == nil {
			t.Fatal("backup service was not constructed")
		}
		if oneBackup == nil {
			oneBackup = service
		} else if service != oneBackup {
			t.Fatal("literal context constructed multiple backup services")
		}
	}
}
