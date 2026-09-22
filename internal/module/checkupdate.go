package module

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
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
	"strconv"
	"strings"
	"sync"
	"time"
)

// ImageCheckList 检查更新处理后的镜像列表
type ImageCheckList struct {
	NeedUpdate bool
	// LatestVersion 为版本号 tag 找到的更高版本,非空表示已发布新版本
	LatestVersion string
}
type ImageUpdateData struct {
	mu   sync.RWMutex
	Data map[string]ImageCheckList
}

const ContentDigestHeader = "Docker-Content-Digest"

// releaseTagPattern 只匹配纯发布版本 tag(v2.2.0 / 1.4 等),排除 latest、alpine、rc 等
var releaseTagPattern = regexp.MustCompile(`^v?\d+(?:\.\d+){1,3}$`)

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
		i.checkSingleImage(image)
	}
}

func (i *ImageUpdateData) checkSingleImage(image types.Image) {
	token, err := GetToken(image, "")
	if err != nil {
		logx.Error("获取token失败或者无需获取token，继续尝试检查" + err.Error())
	}
	needUpdate := false
	latestVersion := ""
	checkedOK := false

	// 同 tag 内容变化(滚动 tag 的检测路径)
	if len(image.RepoDigests) == 0 {
		logx.Error("未在本地获取到repoDigest" + image.ImageName + ":" + image.ImageTag)
	} else if remoteDigest, err := digestOfTag(image, token); err != nil {
		logx.Error("获取digest失败" + err.Error())
	} else {
		checkedOK = true
		if containsDigest(image.RepoDigests, remoteDigest) {
			logx.Info(image.ImageName + ":" + image.ImageTag + " not need update")
		} else {
			logx.Infof("%s:%s need update(local=%v remote=%s)", image.ImageName, image.ImageTag, image.RepoDigests, remoteDigest)
			needUpdate = true
		}
	}

	// 版本号 tag:检测 registry 是否已发布更高版本
	if releaseTagPattern.MatchString(image.ImageTag) {
		if latest, ok := fetchLatestVersion(image, token); ok {
			checkedOK = true
			if latest != "" {
				needUpdate = true
				latestVersion = latest
			}
		}
	}

	if !checkedOK {
		// 全部检查失败:保留上一次结果,避免瞬时网络故障清掉已有更新提示
		return
	}
	i.mu.Lock()
	i.Data[image.ID] = ImageCheckList{NeedUpdate: needUpdate, LatestVersion: latestVersion}
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
// 只要有一个本地 digest 与远端一致即视为当前版本,避免多 RepoDigests 时的顺序覆盖问题。
func containsDigest(repoDigests []string, digest string) bool {
	for _, repoDigest := range repoDigests {
		if idx := strings.LastIndex(repoDigest, "@"); idx >= 0 && repoDigest[idx+1:] == digest {
			return true
		}
	}
	return false
}

// fetchLatestVersion 返回 tag 列表中高于当前 tag 的最高版本。
// 第二个返回值表示本次检查是否成功完成。
func fetchLatestVersion(image types.Image, token string) (string, bool) {
	tagsURL, err := BuildTagsURL(image)
	if err != nil {
		logx.Error("获取tagsURL失败" + err.Error())
		return "", false
	}
	tags, err := GetTags(tagsURL, token)
	if err != nil {
		logx.Error("获取tags失败" + err.Error())
		return "", false
	}
	best := ""
	for _, tag := range tags {
		if !releaseTagPattern.MatchString(tag) || !versionGreater(tag, image.ImageTag) {
			continue
		}
		if versionGreater(tag, best) {
			best = tag
		}
	}
	return best, true
}

func parseVersionParts(tag string) []int {
	parts := strings.Split(strings.TrimPrefix(tag, "v"), ".")
	nums := make([]int, len(parts))
	for idx, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil
		}
		nums[idx] = n
	}
	return nums
}

// versionGreater 判断 a 是否为高于 b 的版本号;空字符串视为最低。
func versionGreater(a, b string) bool {
	if b == "" {
		return a != ""
	}
	pa, pb := parseVersionParts(a), parseVersionParts(b)
	if pa == nil || pb == nil {
		return false
	}
	for idx := 0; idx < len(pa) && idx < len(pb); idx++ {
		if pa[idx] != pb[idx] {
			return pa[idx] > pb[idx]
		}
	}
	return len(pa) > len(pb)
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

// BuildTagsURL 构造 registry 的 tag 列表地址。
func BuildTagsURL(image types.Image) (string, error) {
	normalizedRef, err := ref.ParseDockerRef(image.ImageName + ":" + image.ImageTag)
	if err != nil {
		return "", err
	}
	host, err := GetRegistryAddress(normalizedRef.Name())
	if err != nil {
		return "", err
	}
	url := url2.URL{
		Scheme: "https",
		Host:   host,
		Path:   fmt.Sprintf("/v2/%s/tags/list", ref.Path(normalizedRef)),
	}
	return url.String(), nil
}

// GetTags 拉取 registry 的 tag 列表。
func GetTags(url string, token string) ([]string, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Add("Authorization", token)
	}

	res, err := digestHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func(Body io.ReadCloser) {
		err := Body.Close()
		if err != nil {
			logx.Error("GetTags关闭body失败" + err.Error())
		}
	}(res.Body)

	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("registry responded to tags request with %q", res.Status)
	}
	var payload struct {
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(res.Body).Decode(&payload); err != nil {
		return nil, err
	}
	return payload.Tags, nil
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
	req.Header.Add("Accept", "application/vnd.docker.oci.image.index.v1+json")
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
