package module

import (
	"context"
	"fmt"
	"github.com/RookieRoll/dockerCopilot/internal/types"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/zeromicro/go-zero/core/logx"
	"net/http"
	"net/url"
	"os"
	"time"
)

func EnvProxy() string {
	raw := os.Getenv("HTTPS_PROXY")
	if raw == "" {
		raw = os.Getenv("https_proxy")
	}
	return raw
}
func ProxyOptions(ctx context.Context, raw string) ([]remote.Option, error) {
	transport := remote.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	if raw != "" {
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5") {
			return nil, fmt.Errorf("代理镜像更新需要有效的 HTTPS_PROXY")
		}
		transport.Proxy = http.ProxyURL(u)
	}
	return []remote.Option{remote.WithContext(ctx), remote.WithAuthFromKeychain(authn.DefaultKeychain), remote.WithTransport(transport)}, nil
}

func (i *ImageUpdateData) checkProxyImage(img types.Image, address string) {
	tag, err := name.NewTag(img.ImageName + ":" + img.ImageTag)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	opts, err := ProxyOptions(ctx, address)
	if err != nil {
		logx.Error(err)
		return
	}
	desc, err := remote.Get(tag, opts...)
	if err != nil {
		logx.Errorf("代理查询 %s 失败", tag.Name())
		return
	}
	digest := desc.Digest.String()
	installed := containsDigest(img.RepoDigests, digest)
	i.mu.Lock()
	defer i.mu.Unlock()
	i.Data[img.ID] = ImageCheckList{NeedUpdate: !installed}
}
