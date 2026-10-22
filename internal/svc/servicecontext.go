package svc

import (
	"context"
	"fmt"
	"sync"

	"github.com/docker/docker/client"
	backupService "github.com/onlyLTY/dockerCopilot/internal/backup"
	"github.com/onlyLTY/dockerCopilot/internal/config"
	containerService "github.com/onlyLTY/dockerCopilot/internal/container"
	imageService "github.com/onlyLTY/dockerCopilot/internal/image"
	"github.com/onlyLTY/dockerCopilot/internal/legacyprogress"
	"github.com/onlyLTY/dockerCopilot/internal/module"
	"github.com/onlyLTY/dockerCopilot/internal/settings"
	"github.com/onlyLTY/dockerCopilot/internal/task"
	"github.com/onlyLTY/dockerCopilot/internal/types"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/rest"
)

type ServiceContext struct {
	Config                     config.Config
	CookieCheckMiddleware      rest.Middleware
	Jwtuuid                    string
	BearerTokenCheckMiddleware rest.Middleware
	JwtSecret                  string
	PortainerJwt               string
	HubImageInfo               *module.ImageUpdateData
	IndexCheckMiddleware       rest.Middleware
	ProgressStore              ProgressStoreType
	LegacyProgress             *legacyprogress.Store
	DockerClient               *client.Client
	Containers                 *containerService.Service
	Images                     *imageService.Service
	Backups                    *backupService.Service
	Ports                      *settings.PortOverridesService
	Icons                      *settings.IconService
	TaskRuntime                *TaskRuntime
	TaskStore                  *task.Store
	TaskRunner                 *task.Runner
	TaskQuery                  *task.QueryService
	mu                         sync.Mutex
}

type TaskProgress = types.TaskProgress

type ProgressStoreType map[string]types.TaskProgress

func NewServiceContext(c config.Config) *ServiceContext {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		logx.Errorf("Unable to create docker client: %s", err)
	}
	ctx, buildErr := newServiceContext(c, cli)
	if buildErr != nil {
		logx.Errorf("Unable to build service context: %s", buildErr)
	}
	return ctx
}

// NewServiceContextChecked is the startup constructor. Unlike the legacy
// constructor retained for direct logic tests, it never returns a context with
// a missing Docker client or task runtime.
func NewServiceContextChecked(c config.Config) (*ServiceContext, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("create docker client: %w", err)
	}
	ctx, err := newServiceContext(c, cli)
	if err != nil {
		if cli != nil {
			_ = cli.Close()
		}
		return nil, err
	}
	runtime, err := NewTaskRuntimeForConfig(context.Background(), c, serviceInspector{service: ctx.Containers})
	if err != nil {
		if cli != nil {
			_ = cli.Close()
		}
		return nil, err
	}
	ctx.TaskRuntime, ctx.TaskStore, ctx.TaskRunner, ctx.TaskQuery = runtime, runtime.Store, runtime.Runner, runtime.Query
	return ctx, nil
}

func newServiceContext(c config.Config, cli *client.Client) (*ServiceContext, error) {
	updates := module.NewImageCheck()
	progressStore := make(ProgressStoreType)
	progress := legacyprogress.NewStore(progressStore)
	ctx := &ServiceContext{
		Config:         c,
		HubImageInfo:   updates,
		ProgressStore:  progressStore,
		LegacyProgress: progress,
		DockerClient:   cli,
		Containers:     containerService.NewService(cli, updates),
		Images:         imageService.NewService(cli),
		Backups:        backupService.NewService(cli, c.BackupDirectory()),
		Ports:          settings.NewPortOverridesService(c.PortOverridesFilePath()),
	}
	icons, err := settings.NewIconService(settings.IconOptions{ImageDir: settings.DefaultImageDir, MappingPath: settings.DefaultImageLogosPath})
	if err != nil {
		return ctx, fmt.Errorf("create icon service: %w", err)
	}
	ctx.Icons = icons
	ctx.Backups.SetProgressCallbacks(backupService.ProgressCallbacks{Update: progress.Update, Get: progress.Get})
	ctx.Containers.SetProgressCallbacks(containerService.ProgressCallbacks{Update: progress.Update, Get: progress.Get})
	return ctx, nil
}

func (ctx *ServiceContext) UpdateProgress(taskID string, progress TaskProgress) {
	ctx.ProgressService().Update(taskID, progress)
}

func (ctx *ServiceContext) GetProgress(taskID string) (TaskProgress, bool) {
	return ctx.ProgressService().Get(taskID)
}

func (ctx *ServiceContext) ProgressService() *legacyprogress.Store {
	ctx.mu.Lock()
	defer ctx.mu.Unlock()
	return ctx.progressServiceLocked()
}

// ContainerService returns the process-wide container service, constructing
// and wiring it once for compatibility contexts used by legacy callers.
func (ctx *ServiceContext) ContainerService() *containerService.Service {
	ctx.mu.Lock()
	defer ctx.mu.Unlock()
	if ctx.Containers == nil {
		ctx.Containers = containerService.NewService(ctx.DockerClient, ctx.HubImageInfo)
		progress := ctx.progressServiceLocked()
		ctx.Containers.SetProgressCallbacks(containerService.ProgressCallbacks{Update: progress.Update, Get: progress.Get})
	}
	return ctx.Containers
}

// BackupService returns the process-wide backup service, constructing and
// wiring it once for compatibility contexts used by legacy callers.
func (ctx *ServiceContext) BackupService() *backupService.Service {
	ctx.mu.Lock()
	defer ctx.mu.Unlock()
	if ctx.Backups == nil {
		ctx.Backups = backupService.NewService(ctx.DockerClient, ctx.Config.BackupDirectory())
		progress := ctx.progressServiceLocked()
		ctx.Backups.SetProgressCallbacks(backupService.ProgressCallbacks{Update: progress.Update, Get: progress.Get})
	}
	return ctx.Backups
}

func (ctx *ServiceContext) progressServiceLocked() *legacyprogress.Store {
	if ctx.LegacyProgress != nil {
		return ctx.LegacyProgress
	}
	if ctx.ProgressStore == nil {
		ctx.ProgressStore = make(ProgressStoreType)
	}
	ctx.LegacyProgress = legacyprogress.NewStore(ctx.ProgressStore)
	return ctx.LegacyProgress
}
