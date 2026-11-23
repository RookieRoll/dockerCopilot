package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RookieRoll/dockerCopilot/internal/module"
	"github.com/RookieRoll/dockerCopilot/internal/svc"
)

func TestProxySettingsPersistAndHideCredentials(t *testing.T) {
	dir := t.TempDir()
	checks := module.NewImageCheck()
	t.Setenv("HTTPS_PROXY", "")
	if err := checks.InitSettings(dir, false); err != nil {
		t.Fatal(err)
	}
	h := proxySettingsHandler(&svc.ServiceContext{HubImageInfo: checks})
	send := func(body string) int {
		w := httptest.NewRecorder()
		h(w, httptest.NewRequest(http.MethodPut, "/", strings.NewReader(body)))
		if strings.Contains(w.Body.String(), "password") {
			t.Fatal("proxy credentials exposed")
		}
		return w.Code
	}
	if send(`{"enabled":true,"address":"bad"}`) != 400 || checks.Settings().Enabled {
		t.Fatal("invalid settings accepted")
	}
	if send(`{"enabled":true,"address":"http://user:password@proxy:3128"}`) != 200 {
		t.Fatal("save failed")
	}
	if send(`{"enabled":false}`) != 200 {
		t.Fatal("disable failed")
	}
	fresh := module.NewImageCheck()
	if err := fresh.InitSettings(dir, true); err != nil {
		t.Fatal(err)
	}
	if got := fresh.Settings(); got.Enabled || got.Address != "http://user:password@proxy:3128" {
		t.Fatal("settings did not survive restart")
	}
}
