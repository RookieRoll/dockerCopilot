package updater

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/onlyLTY/dockerCopilot/internal/config"
	"github.com/zeromicro/go-zero/core/logx"
)

const (
	remoteVersionTTL     = 5 * time.Minute
	remoteVersionTimeout = 15 * time.Second
	maxVersionFileBytes  = 4 * 1024
)

var versionPattern = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?|\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)

// VersionChecker fetches and validates the version file. Successful cache
// entries are keyed by the full source URL so different repositories/proxies
// cannot share values.
type VersionChecker struct {
	client *http.Client
	now    func() time.Time

	mu    sync.Mutex
	cache map[string]versionCacheEntry
}

type versionCacheEntry struct {
	version string
	stored  time.Time
}

func NewVersionChecker(client *http.Client) *VersionChecker {
	if client == nil {
		client = &http.Client{Timeout: remoteVersionTimeout}
	}
	return &VersionChecker{client: client, now: time.Now, cache: make(map[string]versionCacheEntry)}
}

var defaultVersionChecker = NewVersionChecker(nil)

// CheckRemoteVersion checks a configured remote source. Network work honors
// the caller context; failures are returned with their original cause and are
// never cached.
func CheckRemoteVersion(ctx context.Context, source config.UpdateConfig) (string, error) {
	if strings.Contains(config.Version, "FNOS") {
		logx.Infof("飞牛版本，无需在线更新")
		return config.Version, nil
	}
	return defaultVersionChecker.Check(ctx, source)
}

func (c *VersionChecker) Check(ctx context.Context, source config.UpdateConfig) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if c == nil || c.client == nil {
		return "", fmt.Errorf("remote version checker is not configured")
	}
	if strings.TrimSpace(source.Repository) == "" {
		source.Repository = "RookieRoll/dockerCopilot"
	}
	versionURL := source.VersionURL()
	if versionURL == "" {
		return "", fmt.Errorf("remote version URL is empty")
	}
	now := c.now()
	c.mu.Lock()
	if entry, ok := c.cache[versionURL]; ok && now.Sub(entry.stored) < remoteVersionTTL {
		c.mu.Unlock()
		return entry.version, nil
	}
	c.mu.Unlock()

	requestCtx, cancel := context.WithTimeout(ctx, remoteVersionTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, versionURL, nil)
	if err != nil {
		return "", fmt.Errorf("build remote version request: %w", err)
	}
	req.Header.Set("Accept", "text/plain")
	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request remote version: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			logx.Error("close remote version response body: ", closeErr)
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("remote version request returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxVersionFileBytes+1))
	if err != nil {
		return "", fmt.Errorf("read remote version response: %w", err)
	}
	if len(body) > maxVersionFileBytes {
		return "", fmt.Errorf("remote version response exceeds %d bytes", maxVersionFileBytes)
	}
	version := strings.TrimSpace(string(body))
	if !versionPattern.MatchString(version) {
		return "", fmt.Errorf("remote version response has invalid version format")
	}

	c.mu.Lock()
	c.cache[versionURL] = versionCacheEntry{version: version, stored: c.now()}
	c.mu.Unlock()
	return version, nil
}

// GetRemoteVersion preserves the legacy utility entry point.
func GetRemoteVersion() (string, error) {
	return GetRemoteVersionWithConfig(config.LegacyUpdateSource())
}

// GetRemoteVersionWithConfig preserves the legacy non-context API.
func GetRemoteVersionWithConfig(source config.UpdateConfig) (string, error) {
	version, err := CheckRemoteVersion(context.Background(), source)
	if err != nil {
		return "0.0.0", err
	}
	return version, nil
}
