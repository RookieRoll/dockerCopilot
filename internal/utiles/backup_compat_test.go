package utiles

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	dockerClient "github.com/docker/docker/client"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"sigs.k8s.io/yaml"
)

const backupInspectFixture = `{
  "Id":"compat-container-id",
  "Name":"/compat-service",
  "Config":{"Image":"example/compat:2.3","Env":["MODE=compat","TZ=UTC"],"Hostname":"compat-host","Tty":true},
  "HostConfig":{
    "NetworkMode":"bridge",
    "Binds":["/srv/compat-data:/var/lib/compat:ro"],
    "PortBindings":{"8080/tcp":[{"HostIp":"127.0.0.1","HostPort":"18080"}]},
    "RestartPolicy":{"Name":"unless-stopped"},
    "Privileged":false
  },
  "NetworkSettings":{"Networks":{"compat-net":{
    "NetworkID":"network-id","EndpointID":"endpoint-id","Gateway":"172.28.0.1",
    "IPAddress":"172.28.0.8","IPPrefixLen":16,"MacAddress":"02:42:ac:1c:00:08",
    "Aliases":["compat-service","compat-alias"]
  }}},
  "Mounts":[{"Type":"bind","Source":"/srv/compat-data","Destination":"/var/lib/compat","Mode":"ro","RW":false,"Propagation":"rprivate"}]
}`

func TestBackupWritersPreserveLegacyContainerConfiguration(t *testing.T) {
	backupDir := t.TempDir()
	t.Setenv("BACKUP_DIR", backupDir)
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/version":
			fmt.Fprint(w, `{"ApiVersion":"1.45","MinAPIVersion":"1.24"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1.45/containers/json":
			fmt.Fprint(w, `[{"Id":"compat-container-id","Names":["/compat-service"],"Image":"example/compat:2.3","ImageID":"sha256:compat-image","State":"running","Status":"Up"}]`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1.45/containers/compat-container-id/json":
			fmt.Fprint(w, backupInspectFixture)
		default:
			http.NotFound(w, r)
		}
	}))
	defer daemon.Close()
	docker, err := dockerClient.NewClientWithOpts(dockerClient.WithHost(daemon.URL), dockerClient.WithVersion("1.45"))
	if err != nil {
		t.Fatalf("create fake Docker client: %v", err)
	}
	defer docker.Close()
	service := &svc.ServiceContext{DockerClient: docker}

	if err := BackupContainer(service); err != nil {
		t.Fatalf("write JSON backup: %v", err)
	}
	jsonPath := findBackupFile(t, backupDir, ".json")
	jsonData, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatalf("read JSON backup: %v", err)
	}
	var legacy []struct {
		Name   string `json:"Name"`
		Config struct {
			Image string   `json:"Image"`
			Env   []string `json:"Env"`
		} `json:"Config"`
		HostConfig struct {
			NetworkMode   string   `json:"NetworkMode"`
			Binds         []string `json:"Binds"`
			RestartPolicy struct {
				Name string `json:"Name"`
			} `json:"RestartPolicy"`
		} `json:"HostConfig"`
		NetworkingConfig struct {
			EndpointsConfig map[string]struct {
				Aliases []string `json:"Aliases"`
			} `json:"EndpointsConfig"`
		} `json:"NetworkingConfig"`
	}
	if err := json.Unmarshal(jsonData, &legacy); err != nil {
		t.Fatalf("decode generated JSON backup: %v", err)
	}
	if len(legacy) != 1 || legacy[0].Name != "compat-service" || legacy[0].Config.Image != "example/compat:2.3" {
		t.Fatalf("JSON backup lost container name or image: %s", jsonData)
	}
	if len(legacy[0].Config.Env) != 2 || legacy[0].Config.Env[0] != "MODE=compat" || legacy[0].Config.Env[1] != "TZ=UTC" {
		t.Fatalf("JSON backup lost environment: %s", jsonData)
	}
	if legacy[0].HostConfig.NetworkMode != "bridge" || legacy[0].HostConfig.RestartPolicy.Name != "unless-stopped" || len(legacy[0].HostConfig.Binds) != 1 || legacy[0].HostConfig.Binds[0] != "/srv/compat-data:/var/lib/compat:ro" {
		t.Fatalf("JSON backup lost host, mount, or restart configuration: %s", jsonData)
	}
	attachment, ok := legacy[0].NetworkingConfig.EndpointsConfig["compat-net"]
	if !ok || len(attachment.Aliases) != 2 || attachment.Aliases[1] != "compat-alias" {
		t.Fatalf("JSON backup lost network attachment: %s", jsonData)
	}

	if err := Backup2Compose(service); err != nil {
		t.Fatalf("write Compose backup: %v", err)
	}
	composePath := findBackupFile(t, backupDir, ".yaml")
	composeData, err := os.ReadFile(composePath)
	if err != nil {
		t.Fatalf("read Compose backup: %v", err)
	}
	var compose struct {
		Services map[string]struct {
			Image       string             `json:"image"`
			NetworkMode string             `json:"network_mode"`
			Environment map[string]*string `json:"environment"`
			Volumes     []struct {
				Source string `json:"source"`
				Target string `json:"target"`
			} `json:"volumes"`
		} `json:"services"`
	}
	if err := yaml.Unmarshal(composeData, &compose); err != nil {
		t.Fatalf("decode generated Compose YAML: %v\n%s", err, composeData)
	}
	serviceConfig, ok := compose.Services["compat-service"]
	if !ok || serviceConfig.Image != "example/compat:2.3" || serviceConfig.NetworkMode != "bridge" {
		t.Fatalf("Compose backup lost service image or network mode: %s", composeData)
	}
	if serviceConfig.Environment["MODE"] == nil || *serviceConfig.Environment["MODE"] != "compat" || serviceConfig.Environment["TZ"] == nil || *serviceConfig.Environment["TZ"] != "UTC" {
		t.Fatalf("Compose backup lost environment: %s", composeData)
	}
	if len(serviceConfig.Volumes) != 1 || serviceConfig.Volumes[0].Source != "/srv/compat-data" || serviceConfig.Volumes[0].Target != "/var/lib/compat" {
		t.Fatalf("Compose backup lost mount source/target: %s", composeData)
	}
}

func findBackupFile(t *testing.T, directory, extension string) string {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatalf("list backup directory: %v", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == extension {
			return filepath.Join(directory, entry.Name())
		}
	}
	t.Fatalf("no %s backup found in %s", extension, directory)
	return ""
}
