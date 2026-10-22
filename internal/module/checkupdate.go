package module

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	ref "github.com/distribution/reference"
	"github.com/onlyLTY/dockerCopilot/internal/types"
	"github.com/zeromicro/go-zero/core/logx"
	"io"
	"net"
	"net/http"
	url2 "net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ImageCheckList 检查更新处理后的镜像列表
type ImageCheckList struct {
	NeedUpdate    bool      `json:"needUpdate"`
	Known         bool      `json:"known"`
	Stale         bool      `json:"stale"`
	Error         string    `json:"error,omitempty"`
	LastCheckedAt time.Time `json:"lastCheckedAt,omitempty"`
	LastSuccessAt time.Time `json:"lastSuccessAt,omitempty"`
}
type ImageUpdateData struct {
	mu        sync.RWMutex
	checkMu   sync.Mutex
	Data      map[string]ImageCheckList
	client    *http.Client
	freshness time.Duration
}

type ImageCheckOptions struct {
	Client        *http.Client
	TrustedCAFile string
	Freshness     time.Duration
}

var ErrImageCheckInProgress = errors.New("image update check is already running")

const ContentDigestHeader = "Docker-Content-Digest"

// pinnedVersionTagPattern 匹配锁版本的纯发布 tag(v2.2.0 / 1.4 等);锁版本 tag 不做更新检查
var pinnedVersionTagPattern = regexp.MustCompile(`^v?\d+(?:\.\d+){1,3}$`)

func NewImageCheck() *ImageUpdateData {
	data, _ := NewImageCheckWithOptions(ImageCheckOptions{})
	return data
}

func NewImageCheckWithOptions(options ImageCheckOptions) (*ImageUpdateData, error) {
	client := options.Client
	if client == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		if options.TrustedCAFile != "" {
			pem, err := os.ReadFile(options.TrustedCAFile)
			if err != nil {
				return nil, fmt.Errorf("read trusted registry CA: %w", err)
			}
			pool, err := x509.SystemCertPool()
			if err != nil || pool == nil {
				pool = x509.NewCertPool()
			}
			if !pool.AppendCertsFromPEM(pem) {
				return nil, errors.New("trusted registry CA file contains no valid certificates")
			}
			transport.TLSClientConfig.RootCAs = pool
		}
		client = &http.Client{Timeout: 30 * time.Second, Transport: transport}
	}
	freshness := options.Freshness
	if freshness <= 0 {
		freshness = 2 * time.Hour
	}
	return &ImageUpdateData{
		Data: map[string]ImageCheckList{}, client: client, freshness: freshness,
	}, nil
}
func isDockerCopilotImage(imageName string) bool {
	return strings.Contains(imageName, "0nlylty/dockercopilot") ||
		strings.Contains(imageName, "2807645688/dockercopilot")
}
func (i *ImageUpdateData) CheckUpdate(imageList []types.Image) {
	if err := i.CheckUpdateContext(context.Background(), imageList); err != nil && !errors.Is(err, ErrImageCheckInProgress) {
		logx.Errorf("image registry digest check failed: %v", err)
	}
}

func (i *ImageUpdateData) CheckUpdateContext(ctx context.Context, imageList []types.Image) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !i.checkMu.TryLock() {
		return ErrImageCheckInProgress
	}
	defer i.checkMu.Unlock()
	if i.client == nil {
		return errors.New("registry HTTP client is unavailable")
	}
	var firstErr error
	for _, image := range imageList {
		if isDockerCopilotImage(image.ImageName) {
			continue
		}
		if pinnedVersionTagPattern.MatchString(image.ImageTag) {
			continue
		}
		if err := i.checkSingleImage(ctx, image); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (i *ImageUpdateData) checkSingleImage(ctx context.Context, image types.Image) error {
	token, err := getTokenWithClient(ctx, image, "", i.client)
	if err != nil {
		i.recordCheckFailure(image.ID, err)
		return fmt.Errorf("registry authentication check for %s:%s: %w", image.ImageName, image.ImageTag, err)
	}
	if len(image.RepoDigests) == 0 {
		err := errors.New("local image has no repo digest to compare")
		i.recordCheckFailure(image.ID, err)
		return fmt.Errorf("registry digest check for %s:%s: %w", image.ImageName, image.ImageTag, err)
	}
	remoteDigest, err := digestOfTagWithClient(ctx, image, token, i.client)
	if err != nil {
		i.recordCheckFailure(image.ID, err)
		return fmt.Errorf("registry manifest digest check for %s:%s: %w", image.ImageName, image.ImageTag, err)
	}
	// 只要远端 digest 不在本地任一 RepoDigests 中,即认为该 tag 内容已更新
	needUpdate := !containsDigest(image.RepoDigests, remoteDigest)
	if needUpdate {
		logx.Infof("%s:%s need update(local=%v remote=%s)", image.ImageName, image.ImageTag, image.RepoDigests, remoteDigest)
	} else {
		logx.Info(image.ImageName + ":" + image.ImageTag + " not need update")
	}
	i.mu.Lock()
	previous := i.Data[image.ID]
	now := time.Now().UTC()
	result := ImageCheckList{NeedUpdate: needUpdate, Known: true, LastCheckedAt: now, LastSuccessAt: now}
	if previous.LastSuccessAt.After(now) {
		result.LastSuccessAt = previous.LastSuccessAt
	}
	i.Data[image.ID] = result
	i.mu.Unlock()
	return nil
}

func (i *ImageUpdateData) recordCheckFailure(id string, cause error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	now := time.Now().UTC()
	previous := i.Data[id]
	previous.Stale = true
	previous.Error = cause.Error()
	previous.LastCheckedAt = now
	if !previous.LastSuccessAt.IsZero() {
		previous.Known = true
	} else {
		previous.Known = false
		previous.NeedUpdate = false
	}
	i.Data[id] = previous
}

func digestOfTag(image types.Image, token string) (string, error) {
	return digestOfTagWithClient(context.Background(), image, token, digestHTTPClient)
}

func digestOfTagWithClient(ctx context.Context, image types.Image, token string, client *http.Client) (string, error) {
	digestURL, err := BuildManifestURL(image)
	if err != nil {
		return "", err
	}
	return getDigestWithClient(ctx, client, digestURL, token)
}

// containsDigest 判断远端 digest 是否已存在于本地 RepoDigests 中。
func containsDigest(repoDigests []string, digest string) bool {
	for _, repoDigest := range repoDigests {
		if idx := strings.LastIndex(repoDigest, "@"); idx >= 0 && repoDigest[idx+1:] == digest {
			return true
		}
	}
	return false
}

// Get 返回单个镜像的更新检查结果,并发安全。
func (i *ImageUpdateData) Get(id string) (ImageCheckList, bool) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	v, ok := i.Data[id]
	if ok && !v.LastSuccessAt.IsZero() && time.Since(v.LastSuccessAt) > i.freshness {
		v.Stale = true
	}
	return v, ok
}

func (i *ImageUpdateData) GetAll() map[string]ImageCheckList {
	i.mu.RLock()
	defer i.mu.RUnlock()
	result := make(map[string]ImageCheckList, len(i.Data))
	for id, value := range i.Data {
		if !value.LastSuccessAt.IsZero() && time.Since(value.LastSuccessAt) > i.freshness {
			value.Stale = true
		}
		result[id] = value
	}
	return result
}

func BuildManifestURL(image types.Image) (string, error) {
	normalizedRef, err := ref.ParseDockerRef(image.ImageName + ":" + image.ImageTag)
	if err != nil {
		return "", err
	}
	normalizedTaggedRef, isTagged := normalizedRef.(ref.NamedTagged)
	if !isTagged {
		return "", errors.New("镜像无tag" + normalizedRef.String())
	}

	host, ErrGetRegistryAddress := GetRegistryAddress(normalizedTaggedRef.Name())
	img, tag := ref.Path(normalizedTaggedRef), normalizedTaggedRef.Tag()

	if ErrGetRegistryAddress != nil {
		return "", ErrGetRegistryAddress
	}

	url := url2.URL{
		Scheme: "https",
		Host:   host,
		Path:   fmt.Sprintf("/v2/%s/manifests/%s", img, tag),
	}
	return url.String(), nil
}

// digestHTTPClient 共享 Transport,复用到 registry 的 TLS 连接。
var digestHTTPClient = &http.Client{
	Timeout: 30 * time.Second,
	Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	},
}

func setManifestAccept(req *http.Request) {
	req.Header.Add("Accept", "application/vnd.docker.distribution.manifest.v2+json")
	req.Header.Add("Accept", "application/vnd.docker.distribution.manifest.list.v2+json")
	req.Header.Add("Accept", "application/vnd.docker.distribution.manifest.v1+json")
	req.Header.Add("Accept", "application/vnd.oci.image.index.v1+json")
}

func GetDigest(url string, token string) (string, error) {
	return getDigestWithClient(context.Background(), digestHTTPClient, url, token)
}

func getDigestWithClient(ctx context.Context, client *http.Client, manifestURL string, token string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "HEAD", manifestURL, nil)
	if err != nil {
		return "", err
	}
	if token != "" {
		req.Header.Add("Authorization", token)
	}
	setManifestAccept(req)

	res, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("HEAD manifest request: %w", err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if res.StatusCode == http.StatusOK {
		if digest := res.Header.Get(ContentDigestHeader); digest != "" {
			return digest, nil
		}
	}
	// 部分镜像加速站不支持 HEAD 或不返回 Docker-Content-Digest,回退 GET 再取/计算 digest
	return getDigestByGetWithClient(ctx, client, manifestURL, token, res.Status)
}

func getDigestByGet(url string, token string, headStatus string) (string, error) {
	return getDigestByGetWithClient(context.Background(), digestHTTPClient, url, token, headStatus)
}

func getDigestByGetWithClient(ctx context.Context, client *http.Client, manifestURL string, token string, headStatus string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", manifestURL, nil)
	if err != nil {
		return "", err
	}
	if token != "" {
		req.Header.Add("Authorization", token)
	}
	setManifestAccept(req)

	res, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("GET manifest request: %w", err)
	}
	defer func(Body io.ReadCloser) {
		err := Body.Close()
		if err != nil {
			logx.Error("GetDigest关闭body失败" + err.Error())
		}
	}(res.Body)

	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("registry manifest request returned %s (head: %s)", res.Status, headStatus)
	}
	if digest := res.Header.Get(ContentDigestHeader); digest != "" {
		return digest, nil
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, 4<<20+1))
	if err != nil {
		return "", err
	}
	if len(body) > 4<<20 {
		return "", errors.New("registry manifest exceeds 4 MiB limit")
	}
	sum := sha256.Sum256(body)
	return fmt.Sprintf("sha256:%x", sum), nil
}
