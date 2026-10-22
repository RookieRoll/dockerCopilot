package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/onlyLTY/dockerCopilot/internal/config"
	"github.com/onlyLTY/dockerCopilot/internal/handler"
	"github.com/onlyLTY/dockerCopilot/internal/observability"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/onlyLTY/dockerCopilot/internal/utiles"
	"github.com/robfig/cron/v3"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/proc"
	"github.com/zeromicro/go-zero/rest"
	"github.com/zeromicro/go-zero/rest/httpx"
)

//go:embed front/*
var embeddedFront embed.FS

var configFile = flag.String("f", "etc/dockerCopilot.yaml", "the config file")

func main() {
	flag.Parse()
	c, err := config.LoadFile(*configFile, true)
	if err != nil {
		log.Printf("无法加载配置文件: %s", observability.RedactText(err.Error()))
		log.Printf("请确认secretKey设置正确，要求非纯数字且大于八位")
		os.Exit(1)
	}
	if err := SetupLogWithOptions(c.LogOptions()); err != nil {
		log.Printf("failed to setup log: %s", observability.RedactText(err.Error()))
		os.Exit(1)
	}
	disableDefaultRequestLogging(&c)
	server := rest.MustNewServer(c.RestConf, rest.WithCors("*"), rest.WithUnauthorizedCallback(unauthorizedCallback))
	defer server.Stop()
	ctx, err := svc.NewServiceContextChecked(c)
	if err != nil {
		log.Printf("无法初始化服务: %s", observability.RedactText(err.Error()))
		os.Exit(1)
	}
	defer func() {
		if err := ctx.TaskRuntime.Close(); err != nil {
			logx.Errorf("关闭任务运行时失败: %v", err)
		}
		if ctx.DockerClient != nil {
			if err := ctx.DockerClient.Close(); err != nil {
				logx.Errorf("关闭Docker客户端失败: %v", err)
			}
		}
	}()

	// Ensure data directory and config exist (Auto-init)
	dataDir := "/data/config/image"
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		logx.Errorf("Failed to create data directory: %v", err)
	}

	imageLogosPath := "/data/config/imageLogos.js"
	if _, err := os.Stat(imageLogosPath); os.IsNotExist(err) {
		defaultConfig := []byte(`// 自定义镜像logo配置
export const customImageLogos = {
};
`)
		if err := os.WriteFile(imageLogosPath, defaultConfig, 0644); err != nil {
			logx.Errorf("Failed to create default imageLogos.js: %v", err)
		}
	}

	list, err := utiles.GetImagesList(ctx)
	if err != nil {
		logx.Errorf("panic获取镜像列表出错: %v", err)
		panic(err)
	}
	go ctx.HubImageInfo.CheckUpdate(list)
	corndanmu := cron.New(cron.WithParser(cron.NewParser(
		cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow,
	)))
	_, err = corndanmu.AddFunc("30 * * * *", func() {
		list, err := utiles.GetImagesList(ctx)
		if err != nil {
			logx.Errorf("定时检查镜像更新失败: %v", err)
			return
		}
		ctx.HubImageInfo.CheckUpdate(list)
	})
	if err != nil {
		logx.Errorf("panic添加定时任务出错: %v", err)
		panic(err)
	}
	corndanmu.Start()
	defer corndanmu.Stop()
	httpx.SetErrorHandler(func(err error) (int, any) {
		status, response := observability.HTTPError(err, nil)
		return status, response
	})
	probeToken := uuid.NewString()
	instanceReady := make(chan struct{}, 1)
	server.Use(readinessMiddleware(probeToken, instanceReady))
	handler.RegisterHandlers(server, ctx)
	RegisterHandlers(server)
	fmt.Printf("Starting server at %s:%d...\n", c.Host, c.Port)
	logx.Info("程序版本" + config.Version)
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		server.Start()
	}()
	if ctx.TaskRuntime.HasPendingInstall() {
		probeCtx, cancelProbe := context.WithTimeout(context.Background(), 45*time.Second)
		probeErr := waitForInstanceReady(probeCtx, c.Host, c.Port, probeToken, instanceReady)
		cancelProbe()
		if probeErr != nil {
			logx.Errorf("pending program install did not reach HTTP readiness: %v", probeErr)
		} else {
			confirmCtx, cancelConfirm := context.WithTimeout(context.Background(), 10*time.Second)
			confirmErr := ctx.TaskRuntime.ConfirmPendingInstalls(confirmCtx)
			cancelConfirm()
			if confirmErr != nil {
				logx.Errorf("pending program install task confirmation remains pending: %v", confirmErr)
			} else {
				ctx.ReleaseStartupProgramDrain()
			}
		}
	}
	select {
	case <-ctx.ProgramShutdown:
		proc.Shutdown()
		<-serverDone
	case <-serverDone:
	}
}

func unauthorizedCallback(w http.ResponseWriter, r *http.Request, _ error) {
	observability.WriteUnauthorized(w, r)
}

// disableDefaultRequestLogging leaves HTTP diagnostics to the redacted
// application middleware so go-zero cannot dump full requests or bodies.
func disableDefaultRequestLogging(c *config.Config) {
	c.Middlewares.Log = false
	c.Verbose = false
}

func RegisterHandlers(engine *rest.Server) {
	frontFS, err := fs.Sub(embeddedFront, "front")
	if err != nil {
		log.Fatal(err)
	}

	frontFileServer := http.StripPrefix("/manager", http.FileServer(http.FS(frontFS)))

	assetsHandler := http.FileServer(http.FS(frontFS))

	// Serve custom icons
	iconFileServer := http.StripPrefix("/src/config/image/", http.FileServer(http.Dir("/data/config/image")))
	engine.AddRoutes(
		[]rest.Route{
			{
				Method: http.MethodGet,
				Path:   "/src/config/image/:file",
				Handler: func(w http.ResponseWriter, r *http.Request) {
					iconFileServer.ServeHTTP(w, r)
				},
			},
		},
	)

	engine.AddRoutes(
		[]rest.Route{
			{
				Method: http.MethodGet,
				Path:   "/manager",
				Handler: func(w http.ResponseWriter, r *http.Request) {
					frontFileServer.ServeHTTP(w, r)
				},
			},
			{
				Method: http.MethodGet,
				Path:   "/manager/:path",
				Handler: func(w http.ResponseWriter, r *http.Request) {
					frontFileServer.ServeHTTP(w, r)
				},
			},
			{
				Method: http.MethodGet,
				Path:   "/manager/assets/:path",
				Handler: func(w http.ResponseWriter, r *http.Request) {
					frontFileServer.ServeHTTP(w, r)
				},
			},
			{
				Method: http.MethodGet,
				Path:   "/assets/:path",
				Handler: func(w http.ResponseWriter, r *http.Request) {
					assetsHandler.ServeHTTP(w, r)
				},
			},
		},
	)
}

// SetupLog 初始化日志设置
func SetupLog(logDir string) error {
	return SetupLogWithOptions(config.LogOptions{Directory: logDir, Level: "info", RetentionDays: 7, MaxFileSizeMiB: 20, MaxTotalSizeMiB: 200})
}

func SetupLogWithOptions(options config.LogOptions) error {
	const mib = int64(1024 * 1024)
	return observability.Setup(options.Directory, options.Level, options.RetentionDays, options.MaxFileSizeMiB*mib, options.MaxTotalSizeMiB*mib)
}
