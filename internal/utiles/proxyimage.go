package utiles

import (
	"context"
	"fmt"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/onlyLTY/dockerCopilot/internal/module"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"io"
	"os"
	"runtime"
	"sync"
)

var proxyUpdateLock sync.Mutex

func loadProxyImage(ctx context.Context, s *svc.ServiceContext, id, reference, task, address string) (string, error) {
	progress, _ := s.GetProgress(task)
	stage := func(msg string) {
		progress.Message = msg
		progress.DetailMsg = msg
		s.UpdateProgress(task, progress)
	}
	stage("正在解析代理镜像目标")
	tag, err := name.NewTag(reference)
	if err != nil {
		return "", err
	}
	ctr, err := s.DockerClient.ContainerInspect(ctx, id)
	if err != nil {
		return "", err
	}
	old, err := s.DockerClient.ImageInspect(ctx, ctr.Image)
	if err != nil {
		return "", err
	}
	opts, err := module.ProxyOptions(ctx, address)
	if err != nil {
		return "", err
	}
	platform := v1.Platform{OS: old.Os, Architecture: old.Architecture, Variant: old.Variant}
	opts = append(opts, remote.WithPlatform(platform))
	target, _ := s.HubImageInfo.Get(ctr.Image)
	var ref name.Reference = tag
	if target.Reference == tag.Name() && target.Digest != "" {
		ref = tag.Context().Digest(target.Digest)
	}
	desc, err := remote.Get(ref, opts...)
	if err != nil {
		return "", fmt.Errorf("代理无法获取清单 %s", ref.Name())
	}
	img, err := desc.Image()
	if err != nil {
		return "", fmt.Errorf("无法获取目标平台清单 %s", platform.String())
	}
	cfg, err := img.ConfigFile()
	if err != nil {
		return "", fmt.Errorf("无法读取镜像平台配置")
	}
	if cfg.OS != platform.OS || cfg.Architecture != platform.Architecture || (platform.Variant != "" && cfg.Variant != "" && cfg.Variant != platform.Variant) {
		return "", fmt.Errorf("镜像平台与原容器不符")
	}
	expected, err := img.ConfigName()
	if err != nil {
		return "", err
	}
	stage("正在通过代理下载 " + desc.Digest.String())
	tmp, err := os.CreateTemp("", "dockercopilot-image-*.tar")
	if err != nil {
		return "", err
	}
	if runtime.GOOS != "linux" {
		defer os.Remove(tmp.Name())
	}
	defer tmp.Close()
	// On NAS (Linux) the descriptor remains usable and the kernel cleans up even on process exit.
	if runtime.GOOS == "linux" {
		if err = os.Remove(tmp.Name()); err != nil {
			return "", err
		}
	}
	if err = tarball.Write(tag, img, tmp); err != nil {
		return "", fmt.Errorf("代理镜像下载或写入临时包失败")
	}
	if _, err = tmp.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	stage("正在导入并校验镜像")
	loaded, err := s.DockerClient.ImageLoad(ctx, tmp)
	if err != nil {
		return "", err
	}
	defer loaded.Body.Close()
	if err = decodePullResp(loaded.Body, s, task); err != nil {
		return "", err
	}
	actual, err := s.DockerClient.ImageInspect(ctx, reference)
	if err != nil {
		return "", err
	}
	if actual.ID != expected.String() {
		return "", fmt.Errorf("装载镜像校验失败: 期望 %s，得到 %s", expected, actual.ID)
	}
	if err = s.HubImageInfo.Remember(tag, actual.ID, desc.Digest.String()); err != nil {
		return "", err
	}
	return actual.ID, nil
}
