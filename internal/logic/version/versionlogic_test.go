package version

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/onlyLTY/dockerCopilot/internal/config"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/onlyLTY/dockerCopilot/internal/types"
)

func TestVersionSuccessResponsesKeepLegacyFields(t *testing.T) {
	oldVersion, oldDate := config.Version, config.BuildDate
	config.Version, config.BuildDate = "1.0.0", "2026-10-22"
	t.Cleanup(func() { config.Version, config.BuildDate = oldVersion, oldDate })
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/https://raw.githubusercontent.com/RookieRoll/dockerCopilot/latest/version" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintln(w, "2.0.0")
	}))
	defer remote.Close()
	t.Setenv("githubProxy", remote.URL)

	logic := NewVersionLogic(context.Background(), &svc.ServiceContext{})
	for _, tc := range []struct {
		name string
		req  types.VersionReq
		want map[string]string
	}{
		{name: "local", req: types.VersionReq{Type: "local"}, want: map[string]string{"version": "1.0.0", "buildDate": "2026-10-22"}},
		{name: "remote", req: types.VersionReq{Type: "remote"}, want: map[string]string{"remoteVersion": "2.0.0"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := logic.Version(&tc.req)
			if err != nil {
				t.Fatalf("version: %v", err)
			}
			encoded, err := json.Marshal(resp)
			if err != nil {
				t.Fatalf("marshal response: %v", err)
			}
			var got struct {
				Code int               `json:"code"`
				Msg  string            `json:"msg"`
				Data map[string]string `json:"data"`
			}
			if err := json.Unmarshal(encoded, &got); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if got.Code != 200 || got.Msg == "" {
				t.Fatalf("unexpected successful response: %s", encoded)
			}
			for key, value := range tc.want {
				if got.Data[key] != value {
					t.Fatalf("response field %q = %q, want %q: %s", key, got.Data[key], value, encoded)
				}
			}
		})
	}
}
