package module

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	ref "github.com/distribution/reference"
	"github.com/onlyLTY/dockerCopilot/internal/types"
	"github.com/zeromicro/go-zero/core/logx"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const ChallengeHeader = "WWW-Authenticate"
const (
	DefaultRegistryDomain = "docker.io"
	DefaultRegistryHost   = "index.docker.io"
)

var DefaultAcceleratorHostList = []string{"docker.1ms.run", "docker.m.daocloud.io",
	"docker.1panel.top", "docker.1panel.live", "proxy.1panel.live", "dockerproxy.1panel.live", "docker.1panel.dev",
	"docker.anye.in", "hub.rat.dev", "docker.amingg.com"}

func GetToken(image types.Image, registryAuth string) (string, error) {
	return getTokenWithClient(context.Background(), image, registryAuth, &http.Client{Timeout: 15 * time.Second})
}

func getTokenWithClient(ctx context.Context, image types.Image, registryAuth string, client *http.Client) (string, error) {
	logx.Infof("image name %s", image.ImageName)
	normalizedRef, err := ref.ParseNormalizedNamed(image.ImageName)
	if err != nil {
		return "", err
	}

	URL := GetChallengeURL(normalizedRef)

	var req *http.Request
	if req, err = GetChallengeRequest(URL); err != nil {
		return "", err
	}
	req = req.WithContext(ctx)

	var res *http.Response
	if res, err = client.Do(req); err != nil {
		return "", err
	}
	defer func(Body io.ReadCloser) {
		err := Body.Close()
		if err != nil {
			logx.Error("GetToken关闭Body失败" + err.Error())
		}
	}(res.Body)
	v := res.Header.Get(ChallengeHeader)
	if v == "" && res.StatusCode >= http.StatusOK && res.StatusCode < http.StatusMultipleChoices {
		return "", nil
	}
	if v == "" {
		return "", fmt.Errorf("registry challenge endpoint returned %s without %s", res.Status, ChallengeHeader)
	}

	challengeType := strings.ToLower(v)
	if strings.HasPrefix(challengeType, "basic") {
		if registryAuth == "" {
			return "", fmt.Errorf("no credentials available")
		}

		return fmt.Sprintf("Basic %s", registryAuth), nil
	}
	if strings.HasPrefix(challengeType, "bearer") {
		return getBearerHeaderWithClient(ctx, v, normalizedRef, registryAuth, client)
	}

	return "", errors.New("unsupported challenge type from registry")
}

func GetChallengeRequest(URL url.URL) (*http.Request, error) {
	req, err := http.NewRequest("GET", URL.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "*/*")
	req.Header.Set("User-Agent", "Watchtower (Docker)")
	return req, nil
}

func GetBearerHeader(challenge string, imageRef ref.Named, registryAuth string) (string, error) {
	return getBearerHeaderWithClient(context.Background(), challenge, imageRef, registryAuth, &http.Client{Timeout: 15 * time.Second})
}

func getBearerHeaderWithClient(ctx context.Context, challenge string, imageRef ref.Named, registryAuth string, client *http.Client) (string, error) {
	authURL, err := GetAuthURL(challenge, imageRef)

	if err != nil {
		return "", err
	}
	if authURL.Scheme != "https" || authURL.User != nil {
		return "", errors.New("Bearer token realm must use HTTPS without URL credentials")
	}

	var r *http.Request
	if r, err = http.NewRequestWithContext(ctx, "GET", authURL.String(), nil); err != nil {
		return "", err
	}

	if registryAuth != "" {
		logx.Info("私有镜像，无法获取是否有更新")
		r.Header.Add("Authorization", fmt.Sprintf("Basic %s", registryAuth))
	} else {
		logx.Info("No credentials found.")
	}

	var authResponse *http.Response
	if authResponse, err = client.Do(r); err != nil {
		return "", err
	}
	defer func(Body io.ReadCloser) {
		err := Body.Close()
		if err != nil {
			logx.Error("GetBearerHeader关闭Body失败" + err.Error())
		}
	}(authResponse.Body)
	if authResponse.StatusCode != http.StatusOK {
		return "", fmt.Errorf("registry token endpoint returned %s", authResponse.Status)
	}

	body, err := io.ReadAll(io.LimitReader(authResponse.Body, 1<<20+1))
	if err != nil {
		return "", fmt.Errorf("read registry token response: %w", err)
	}
	if len(body) > 1<<20 {
		return "", errors.New("registry token response exceeds 1 MiB limit")
	}
	tokenResponse := &types.TokenResponse{}

	err = json.Unmarshal(body, tokenResponse)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("Bearer %s", tokenResponse.Token), nil
}

func GetAuthURL(challenge string, imageRef ref.Named) (*url.URL, error) {
	trimmedChallenge := strings.TrimSpace(challenge)
	scheme, raw, ok := strings.Cut(trimmedChallenge, " ")
	if !ok || !strings.EqualFold(scheme, "bearer") {
		return nil, fmt.Errorf("challenge is not a Bearer challenge")
	}

	pairs := strings.Split(raw, ",")
	values := make(map[string]string, len(pairs))

	for _, pair := range pairs {
		trimmed := strings.Trim(pair, " ")
		if key, val, ok := strings.Cut(trimmed, "="); ok {
			values[strings.ToLower(strings.TrimSpace(key))] = strings.Trim(val, `"`)
		}
	}
	if values["realm"] == "" || values["service"] == "" {

		return nil, fmt.Errorf("challenge header did not include all values needed to construct an auth url")
	}

	authURL, err := url.Parse(values["realm"])
	if err != nil || authURL.Scheme == "" || authURL.Host == "" {
		return nil, fmt.Errorf("Bearer challenge has an invalid realm URL")
	}
	q := authURL.Query()
	q.Add("service", values["service"])

	scopeImage := ref.Path(imageRef)

	scope := fmt.Sprintf("repository:%s:pull", scopeImage)
	q.Add("scope", scope)

	authURL.RawQuery = q.Encode()
	return authURL, nil
}

func GetChallengeURL(imageRef ref.Named) url.URL {
	host, _ := GetRegistryAddress(imageRef.Name())

	URL := url.URL{
		Scheme: "https",
		Host:   host,
		Path:   "/v2/",
	}
	return URL
}

func GetRegistryAddress(imageRef string) (string, error) {
	normalizedRef, err := ref.ParseNormalizedNamed(imageRef)
	if err != nil {
		return "", err
	}

	address := ref.Domain(normalizedRef)

	if address == DefaultRegistryDomain {
		address = resolveDockerHubHost()
	}
	return address, nil
}

var (
	resolvedDockerHubMu sync.Mutex
	resolvedDockerHub   string
	resolvedDockerHubAt time.Time
)

const dockerHubResolveTTL = 30 * time.Minute

// resolveDockerHubHost 返回可用的 Docker Hub 访问地址。
// 探测(最多 11 个加速站、各 5s 超时)结果缓存 30 分钟;
// 全部失败时返回并缓存 DefaultRegistryHost,避免故障期间重复探测。
func resolveDockerHubHost() string {
	resolvedDockerHubMu.Lock()
	defer resolvedDockerHubMu.Unlock()
	if resolvedDockerHub != "" && time.Since(resolvedDockerHubAt) < dockerHubResolveTTL {
		return resolvedDockerHub
	}
	host := DefaultRegistryHost
	if !checkHost(DefaultRegistryHost) {
		for _, candidate := range DefaultAcceleratorHostList {
			if checkHost(candidate) {
				host = candidate
				break
			}
		}
	}
	resolvedDockerHub = host
	resolvedDockerHubAt = time.Now()
	return host
}

func checkHost(host string) bool {
	URL := "https://" + host + "/v2/"
	// 创建带有超时设置的 http.Client
	client := http.Client{
		Timeout: 5 * time.Second,
	}
	// 发送 HEAD 请求
	resp, err := client.Get(URL)
	if err != nil {
		logx.Errorf("Failed to connect to %s: %s", URL, err)
		return false
	}
	defer func(Body io.ReadCloser) {
		err := Body.Close()
		if err != nil {
			logx.Errorf("关闭 body 失败: %v", err)
		}
	}(resp.Body)

	// 检查 HTTP 响应状态码
	if resp.StatusCode == http.StatusOK ||
		resp.StatusCode == http.StatusUnauthorized {
		return true
	}

	logx.Errorf("Failed to connect to %s: %s", URL, resp.Status)
	return false
}
