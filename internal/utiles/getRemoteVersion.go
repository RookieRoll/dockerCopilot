package utiles

import (
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/onlyLTY/dockerCopilot/internal/config"
	"github.com/zeromicro/go-zero/core/logx"
)

func GetRemoteVersion() (remoteVersion string, err error) {
	githubProxy := os.Getenv("githubProxy")
	if githubProxy != "" {
		githubProxy = strings.TrimRight(githubProxy, "/") + "/"
	}
	versionURL := githubProxy + "https://raw.githubusercontent.com/onlyLTY/dockerCopilot/latest/version"
	remoteVersion, err = fetchVersionFromURL(versionURL)
	if err != nil {
		return "0.0.0", err
	}

	localVersion := config.Version
	if strings.Contains(localVersion, "FNOS") {
		logx.Infof("飞牛版本，无需在线更新")
		return localVersion, nil
	}
	if localVersion == remoteVersion {
		logx.Info("版本一致:", localVersion)
		return remoteVersion, nil
	} else {
		logx.Infof("版本不一致! 本地: %s, 远程: %s\n", localVersion, remoteVersion)
		return remoteVersion, nil
	}

}

var (
	remoteVersionMu sync.Mutex
	cachedRemoteVer string
	cachedRemoteAt  time.Time
)

const remoteVersionTTL = 5 * time.Minute

func fetchVersionFromURL(url string) (string, error) {
	remoteVersionMu.Lock()
	defer remoteVersionMu.Unlock()
	if cachedRemoteVer != "" && time.Since(cachedRemoteAt) < remoteVersionTTL {
		return cachedRemoteVer, nil
	}
	client := &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
		},
	}

	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer func(Body io.ReadCloser) {
		err := Body.Close()
		if err != nil {
			logx.Error("关闭Body失败:", err)
		}
	}(resp.Body)

	versionData, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	remoteVersion := strings.TrimSpace(string(versionData))
	cachedRemoteVer = remoteVersion
	cachedRemoteAt = time.Now()
	return remoteVersion, nil
}
