package module

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

type ProxySettings struct {
	Enabled bool   `json:"enabled"`
	Address string `json:"address"`
}

func (i *ImageUpdateData) InitSettings(dir string, enabled bool) error {
	i.ProxyStateDir = dir
	i.settings = ProxySettings{Enabled: enabled, Address: EnvProxy()}
	data, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var saved ProxySettings
	if err = json.Unmarshal(data, &saved); err != nil {
		return err
	}
	i.settings = saved
	return nil
}
func (i *ImageUpdateData) Settings() ProxySettings {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.settings
}

var ErrProxyAddress = errors.New("请输入有效的 HTTP、HTTPS 或 SOCKS5 代理地址")

func (i *ImageUpdateData) SaveSettings(enabled bool, address *string) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	next := i.settings
	next.Enabled = enabled
	if address != nil {
		next.Address = *address
	}
	if enabled || (address != nil && next.Address != "") {
		if _, err := ProxyOptions(context.Background(), next.Address); err != nil {
			return ErrProxyAddress
		}
	}
	if err := os.MkdirAll(i.ProxyStateDir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(i.ProxyStateDir, "settings-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	err = json.NewEncoder(f).Encode(next)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(f.Name(), filepath.Join(i.ProxyStateDir, "settings.json")); err != nil {
		return err
	}
	i.settings = next
	return nil
}
