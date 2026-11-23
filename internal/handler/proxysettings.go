package handler

import (
	"encoding/json"
	"errors"
	"github.com/RookieRoll/dockerCopilot/internal/module"
	"github.com/RookieRoll/dockerCopilot/internal/svc"
	"net/http"
	"net/url"
)

func proxySettingsHandler(s *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Method == http.MethodPut {
			var req struct {
				Enabled *bool   `json:"enabled"`
				Address *string `json:"address"`
			}
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&req); err != nil || req.Enabled == nil {
				http.Error(w, "设置格式错误", 400)
				return
			}
			if err := s.HubImageInfo.SaveSettings(*req.Enabled, req.Address); err != nil {
				if errors.Is(err, module.ErrProxyAddress) {
					http.Error(w, err.Error(), 400)
				} else {
					http.Error(w, "无法保存设置，请检查 /data 的写入权限", 500)
				}
				return
			}
		}
		cfg := s.HubImageInfo.Settings()
		endpoint := ""
		if u, err := url.Parse(cfg.Address); err == nil && u.Host != "" {
			endpoint = u.Scheme + "://" + u.Host
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"enabled": cfg.Enabled, "endpoint": endpoint})
	}
}
