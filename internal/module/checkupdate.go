package module

import (
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"fmt"
	ref "github.com/distribution/reference"
	"github.com/onlyLTY/dockerCopilot/internal/types"
	"github.com/zeromicro/go-zero/core/logx"
	"io"
	"net"
	"net/http"
	url2 "net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ImageCheckList 检查更新处理后的镜像列表
type ImageCheckList struct {
	NeedUpdate bool
}
type ImageUpdateData struct {
	mu   sync.RWMutex
	Data map[string]ImageCheckList
}

const ContentDigestHeader = "Docker-Content-Digest"

// pinnedVersionTagPattern 匹配锁版本的纯发布 tag(v2.2.0 / 1.4 等);锁版本 tag 不做更新检查
var pinnedVersionTagPattern = regexp.MustCompile(`^v?\d+(?:\.\d+){1,3}$`)

func NewImageCheck() *ImageUpdateData {
	return &ImageUpdateData{
		Data: map[string]ImageCheckList{},
	}
}
func isDockerCopilotImage(imageName string) bool {
	return strings.Contains(imageName, "0nlylty/dockercopilot") ||
		strings.Contains(imageName, "2807645688/dockercopilot")
}
func (i *ImageUpdateData) CheckUpdate(imageList []types.Image) {
	for _, image := range imageList {
		if isDockerCopilotImage(image.ImageName) {
			continue
		}
		if pinnedVersionTagPattern.MatchString(image.ImageTag) {
			continue
		}
		i.checkSingleImage(image)
	}
}

func (i *ImageUpdateData) checkSingleImage(image types.Image) {
	token, err := GetToken(image, "")
	if err != nil {
		logx.Error("获取token失败或者无需获取token，继续尝试检查" + err.Error())
	}
	if len(image.RepoDigests) == 0 {
		logx.Error("未在本地获取到repoDigest" + image.ImageName + ":" + image.ImageTag)
		return
	}
	remoteDigest, err := digestOfTag(image, token)
	if err != nil {
		logx.Error("获取digest失败" + err.Error())
		// 检查失败时保留上一次结果,避免瞬时网络故障清掉已有更新提示
		return
	}
	// 只要远端 digest 不在本地任一 RepoDigests 中,即认为该 tag 内容已更新
	needUpdate := !containsDigest(image.RepoDigests, remoteDigest)
	if needUpdate {
		logx.Infof("%s:%s need update(local=%v remote=%s)", image.ImageName, image.ImageTag, image.RepoDigests, remoteDigest)
	} else {
		logx.Info(image.ImageName + ":" + image.ImageTag + " not need update")
	}
	i.mu.Lock()
	i.Data[image.ID] = ImageCheckList{NeedUpdate: needUpdate}
	i.mu.Unlock()
}

func digestOfTag(image types.Image, token string) (string, error) {
	digestURL, err := BuildManifestURL(image)
	if err != nil {
		return "", err
	}
	return GetDigest(digestURL, token)
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
	return v, ok
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
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: true},
	},
}

func setManifestAccept(req *http.Request) {
	req.Header.Add("Accept", "application/vnd.docker.distribution.manifest.v2+json")
	req.Header.Add("Accept", "application/vnd.docker.distribution.manifest.list.v2+json")
	req.Header.Add("Accept", "application/vnd.docker.distribution.manifest.v1+json")
	req.Header.Add("Accept", "application/vnd.oci.image.index.v1+json")
}

func GetDigest(url string, token string) (string, error) {
	req, err := http.NewRequest("HEAD", url, nil)
	if err != nil {
		return "", err
	}
	if token != "" {
		req.Header.Add("Authorization", token)
	}
	setManifestAccept(req)

	res, err := digestHTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	_, _ = io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if res.StatusCode == http.StatusOK {
		if digest := res.Header.Get(ContentDigestHeader); digest != "" {
			return digest, nil
		}
	}
	// 部分镜像加速站不支持 HEAD 或不返回 Docker-Content-Digest,回退 GET 再取/计算 digest
	return getDigestByGet(url, token, res.Status)
}

func getDigestByGet(url string, token string, headStatus string) (string, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", err
	}
	if token != "" {
		req.Header.Add("Authorization", token)
	}
	setManifestAccept(req)

	res, err := digestHTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func(Body io.ReadCloser) {
		err := Body.Close()
		if err != nil {
			logx.Error("GetDigest关闭body失败" + err.Error())
		}
	}(res.Body)

	if res.StatusCode != http.StatusOK {
		wwwAuthHeader := res.Header.Get("www-authenticate")
		if wwwAuthHeader == "" {
			wwwAuthHeader = "not present"
		}
		return "", fmt.Errorf("registry responded to manifest request with %q (head: %s), auth: %q", res.Status, headStatus, wwwAuthHeader)
	}
	if digest := res.Header.Get(ContentDigestHeader); digest != "" {
		return digest, nil
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	return fmt.Sprintf("sha256:%x", sum), nil
}
