package svc

import (
	"github.com/docker/docker/client"
	backupService "github.com/onlyLTY/dockerCopilot/internal/backup"
	"github.com/onlyLTY/dockerCopilot/internal/config"
	containerService "github.com/onlyLTY/dockerCopilot/internal/container"
	imageService "github.com/onlyLTY/dockerCopilot/internal/image"
	"github.com/onlyLTY/dockerCopilot/internal/legacyprogress"
	"github.com/onlyLTY/dockerCopilot/internal/module"
	"github.com/onlyLTY/dockerCopilot/internal/settings"
	"github.com/onlyLTY/dockerCopilot/internal/types"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/rest"
	"sync"
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
	mu                         sync.Mutex
}

type TaskProgress = types.TaskProgress

type ProgressStoreType map[string]types.TaskProgress

func NewServiceContext(c config.Config) *ServiceContext {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		logx.Errorf("Unable to create docker client: %s", err)
	}
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
	ctx.Icons, _ = settings.NewIconService(settings.IconOptions{ImageDir: settings.DefaultImageDir, MappingPath: settings.DefaultImageLogosPath})
	ctx.Backups.SetProgressCallbacks(backupService.ProgressCallbacks{Update: progress.Update, Get: progress.Get})
	ctx.Containers.SetProgressCallbacks(containerService.ProgressCallbacks{Update: progress.Update, Get: progress.Get})
	return ctx
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
	if ctx.LegacyProgress != nil {
		return ctx.LegacyProgress
	}
	if ctx.ProgressStore == nil {
		ctx.ProgressStore = make(ProgressStoreType)
	}
	ctx.LegacyProgress = legacyprogress.NewStore(ctx.ProgressStore)
	return ctx.LegacyProgress
}
