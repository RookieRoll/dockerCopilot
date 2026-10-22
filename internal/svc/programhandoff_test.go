package svc

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	dockerClient "github.com/docker/docker/client"
	"github.com/onlyLTY/dockerCopilot/internal/config"
)

func TestProgramHandoffPreflightRequiresVerifiedRestartableWritableSelfContainer(t *testing.T) {
	for _, tc := range []struct {
		name         string
		nameInDocker string
		policy       string
		readOnly     bool
		launcher     bool
		wantErr      bool
	}{
		{name: "valid explicit name", nameInDocker: "copilot", policy: "always", launcher: true},
		{name: "not restartable", nameInDocker: "copilot", policy: "on-failure", launcher: true, wantErr: true},
		{name: "read-only root", nameInDocker: "copilot", policy: "unless-stopped", readOnly: true, launcher: true, wantErr: true},
		{name: "name mismatch", nameInDocker: "other", policy: "always", launcher: true, wantErr: true},
		{name: "unverified launcher", nameInDocker: "copilot", policy: "always", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.launcher {
				t.Setenv("DOCKERCOPILOT_HANDOFF_PROTOCOL", "1")
			} else {
				t.Setenv("DOCKERCOPILOT_HANDOFF_PROTOCOL", "")
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/v1.45/containers/copilot/json" {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"Id":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","Name":"/%s","Path":"/app/start.sh","Args":[],"Config":{"Cmd":["/app/start.sh"],"Entrypoint":[]},"HostConfig":{"RestartPolicy":{"Name":"%s"},"ReadonlyRootfs":%t}}`, tc.nameInDocker, tc.policy, tc.readOnly)
			}))
			defer server.Close()
			docker, err := dockerClient.NewClientWithOpts(dockerClient.WithHost(server.URL), dockerClient.WithVersion("1.45"))
			if err != nil {
				t.Fatal(err)
			}
			defer docker.Close()
			workDir := t.TempDir()
			executable := filepath.Join(workDir, "dockerCopilot")
			if err := os.WriteFile(executable, []byte("test executable"), 0o700); err != nil {
				t.Fatal(err)
			}
			validator := newProgramInstallValidator(config.Config{Docker: config.DockerConfig{SelfContainer: "copilot"}}, docker, workDir)
			validator.executablePath = executable
			err = validator.validateRuntime(context.Background())
			if tc.wantErr && err == nil {
				t.Fatal("expected handoff prerequisite rejection")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("valid runtime rejected: %v", err)
			}
		})
	}
}

func TestProgramHandoffRequiresExplicitIdentityForCustomHostname(t *testing.T) {
	validator := newProgramInstallValidator(config.Config{}, nil, t.TempDir())
	if err := validator.validateRuntime(context.Background()); err == nil {
		t.Fatal("missing Docker client and explicit self identity should reject handoff")
	}
}
