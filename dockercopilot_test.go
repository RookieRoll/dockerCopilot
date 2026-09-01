package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sync/atomic"
	"testing"
	"time"

	dockerClient "github.com/docker/docker/client"
	"github.com/onlyLTY/dockerCopilot/internal/config"
	apphandler "github.com/onlyLTY/dockerCopilot/internal/handler"
	"github.com/onlyLTY/dockerCopilot/internal/module"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/zeromicro/go-zero/rest"
)

func TestEmbeddedManagerAssets(t *testing.T) {
	port := getEmbeddedTestPort(t)
	cfg := config.Config{}
	cfg.Host = "127.0.0.1"
	cfg.Port = port

	server := rest.MustNewServer(cfg.RestConf)
	RegisterHandlers(server)
	defer server.Stop()

	go server.Start()
	waitForEmbeddedServer(t, port, "/manager")

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	indexResponse, err := http.Get(baseURL + "/manager")
	if err != nil {
		t.Fatalf("failed to request embedded manager page: %v", err)
	}
	defer indexResponse.Body.Close()

	if indexResponse.StatusCode != http.StatusOK {
		t.Fatalf("expected /manager to return 200, got %d", indexResponse.StatusCode)
	}

	indexBody, err := io.ReadAll(indexResponse.Body)
	if err != nil {
		t.Fatalf("failed to read embedded manager page: %v", err)
	}

	assetPattern := regexp.MustCompile(`/manager/assets/[^"']+\.(?:js|css)`)
	assetPaths := assetPattern.FindAllString(string(indexBody), -1)
	if len(assetPaths) < 2 {
		t.Fatalf("expected JavaScript and CSS asset references in /manager, got %q", string(indexBody))
	}

	for _, assetPath := range assetPaths {
		assetResponse, err := http.Get(baseURL + assetPath)
		if err != nil {
			t.Fatalf("failed to request embedded asset %s: %v", assetPath, err)
		}
		assetResponse.Body.Close()
		if assetResponse.StatusCode != http.StatusOK {
			t.Fatalf("expected embedded asset %s to return 200, got %d", assetPath, assetResponse.StatusCode)
		}
	}
}

func TestContainersAPIIncludesPublishedAndUnpublishedPorts(t *testing.T) {
	dockerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1.45/containers/json":
			fmt.Fprint(w, `[
				{
					"Id":"container-1",
					"Names":["/multi-port"],
					"Image":"example:latest",
					"ImageID":"sha256:image-1",
					"Created":1700000000,
					"Ports":[
						{"IP":"127.0.0.1","PrivatePort":53,"PublicPort":5353,"Type":"udp"},
						{"IP":"0.0.0.0","PrivatePort":80,"PublicPort":8080,"Type":"tcp"},
						{"PrivatePort":443,"Type":"tcp"},
						{"IP":"0.0.0.0","PrivatePort":8080,"PublicPort":8080,"Type":"tcp"}
					],
					"State":"running",
					"Status":"Up 1 minute"
				}
			]`)
		case r.URL.Path == "/v1.45/containers/container-1/json":
			fmt.Fprint(w, `{"Id":"container-1","Name":"/multi-port","Config":{"Image":"example:latest"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer dockerServer.Close()

	docker, err := dockerClient.NewClientWithOpts(
		dockerClient.WithHost(dockerServer.URL),
		dockerClient.WithVersion("1.45"),
	)
	if err != nil {
		t.Fatalf("failed to create Docker API client: %v", err)
	}
	defer docker.Close()

	port := getEmbeddedTestPort(t)
	cfg := config.Config{}
	cfg.Host = "127.0.0.1"
	cfg.Port = port
	cfg.Auth.AccessSecret = "api-test-secret"
	cfg.Auth.AccessExpire = 3600

	serviceContext := &svc.ServiceContext{
		Config:        cfg,
		DockerClient:  docker,
		HubImageInfo:  module.NewImageCheck(),
		ProgressStore: make(svc.ProgressStoreType),
	}
	server := rest.MustNewServer(cfg.RestConf)
	apphandler.RegisterHandlers(server, serviceContext)
	defer server.Stop()

	go server.Start()
	waitForEmbeddedServer(t, port, "/api/auth")

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	loginResponse, err := http.PostForm(baseURL+"/api/auth", url.Values{"secretKey": []string{cfg.Auth.AccessSecret}})
	if err != nil {
		t.Fatalf("failed to authenticate against API: %v", err)
	}
	loginBody, err := io.ReadAll(loginResponse.Body)
	loginResponse.Body.Close()
	if err != nil {
		t.Fatalf("failed to read authentication response: %v", err)
	}
	if loginResponse.StatusCode != http.StatusOK {
		t.Fatalf("expected authentication status 200, got %d: %s", loginResponse.StatusCode, loginBody)
	}

	var login struct {
		Code int `json:"code"`
		Data struct {
			JWT string `json:"jwt"`
		} `json:"data"`
	}
	if err := json.Unmarshal(loginBody, &login); err != nil {
		t.Fatalf("failed to decode authentication response: %v", err)
	}
	if login.Code != http.StatusOK || login.Data.JWT == "" {
		t.Fatalf("authentication response did not contain a JWT: %s", loginBody)
	}

	request, err := http.NewRequest(http.MethodGet, baseURL+"/api/containers", nil)
	if err != nil {
		t.Fatalf("failed to create containers request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+login.Data.JWT)
	containersResponse, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("failed to request containers API: %v", err)
	}
	containersBody, err := io.ReadAll(containersResponse.Body)
	containersResponse.Body.Close()
	if err != nil {
		t.Fatalf("failed to read containers response: %v", err)
	}
	if containersResponse.StatusCode != http.StatusOK {
		t.Fatalf("expected containers status 200, got %d: %s", containersResponse.StatusCode, containersBody)
	}

	var containers struct {
		Code int `json:"code"`
		Data []struct {
			Ports []struct {
				HostIP        string `json:"hostIp"`
				HostPort      uint16 `json:"hostPort"`
				ContainerPort uint16 `json:"containerPort"`
				Protocol      string `json:"protocol"`
			} `json:"ports"`
		} `json:"data"`
	}
	if err := json.Unmarshal(containersBody, &containers); err != nil {
		t.Fatalf("failed to decode containers response: %v", err)
	}
	if containers.Code != 0 && containers.Code != http.StatusOK || len(containers.Data) != 1 {
		t.Fatalf("unexpected containers response envelope: %s", containersBody)
	}

	expectedPorts := []struct {
		hostIP        string
		hostPort      uint16
		containerPort uint16
		protocol      string
	}{
		{hostIP: "", hostPort: 0, containerPort: 443, protocol: "tcp"},
		{hostIP: "127.0.0.1", hostPort: 5353, containerPort: 53, protocol: "udp"},
		{hostIP: "0.0.0.0", hostPort: 8080, containerPort: 80, protocol: "tcp"},
		{hostIP: "0.0.0.0", hostPort: 8080, containerPort: 8080, protocol: "tcp"},
	}
	actualPorts := containers.Data[0].Ports
	if len(actualPorts) != len(expectedPorts) {
		t.Fatalf("expected %d port mappings, got %d: %s", len(expectedPorts), len(actualPorts), containersBody)
	}
	for i, expected := range expectedPorts {
		actual := actualPorts[i]
		if actual.HostIP != expected.hostIP || actual.HostPort != expected.hostPort || actual.ContainerPort != expected.containerPort || actual.Protocol != expected.protocol {
			t.Fatalf("unexpected port at index %d: got %#v, want %#v", i, actual, expected)
		}
	}
}

func TestConfiguredPortsAPIPersistsHostNetworkPorts(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config", "container-port-overrides.json")
	if err := os.Setenv("CONTAINER_PORT_OVERRIDES_PATH", configPath); err != nil {
		t.Fatalf("set configured port path: %v", err)
	}
	defer os.Unsetenv("CONTAINER_PORT_OVERRIDES_PATH")

	var includeContainer atomic.Bool
	includeContainer.Store(true)
	var listStatus atomic.Int32
	listStatus.Store(http.StatusOK)
	dockerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1.45/containers/json":
			status := int(listStatus.Load())
			if status != http.StatusOK {
				w.WriteHeader(status)
				fmt.Fprint(w, `{"message":"container list unavailable"}`)
				return
			}
			if includeContainer.Load() {
				fmt.Fprint(w, `[{"Id":"host-container","Names":["/host-service"],"Image":"example:latest","ImageID":"sha256:image-1","Created":1700000000,"State":"running","Status":"Up 1 minute"}]`)
			} else {
				fmt.Fprint(w, `[]`)
			}
		case "/v1.45/containers/host-container/json":
			fmt.Fprint(w, `{"Id":"host-container","Name":"/host-service","Config":{"Image":"example:latest"},"HostConfig":{"NetworkMode":"host"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer dockerServer.Close()

	docker, err := dockerClient.NewClientWithOpts(
		dockerClient.WithHost(dockerServer.URL),
		dockerClient.WithVersion("1.45"),
	)
	if err != nil {
		t.Fatalf("create docker client: %v", err)
	}
	defer docker.Close()

	port := getEmbeddedTestPort(t)
	cfg := config.Config{}
	cfg.Host = "127.0.0.1"
	cfg.Port = port
	cfg.Auth.AccessSecret = "configured-port-test-secret"
	cfg.Auth.AccessExpire = 3600
	serviceContext := &svc.ServiceContext{
		Config:        cfg,
		DockerClient:  docker,
		HubImageInfo:  module.NewImageCheck(),
		ProgressStore: make(svc.ProgressStoreType),
	}
	server := rest.MustNewServer(cfg.RestConf)
	apphandler.RegisterHandlers(server, serviceContext)
	defer server.Stop()
	go server.Start()
	waitForEmbeddedServer(t, port, "/api/auth")

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	loginResponse, err := http.PostForm(baseURL+"/api/auth", url.Values{"secretKey": []string{cfg.Auth.AccessSecret}})
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	loginBody, err := io.ReadAll(loginResponse.Body)
	loginResponse.Body.Close()
	if err != nil {
		t.Fatalf("read login response: %v", err)
	}
	var login struct {
		Data struct {
			JWT string `json:"jwt"`
		} `json:"data"`
	}
	if err := json.Unmarshal(loginBody, &login); err != nil || login.Data.JWT == "" {
		t.Fatalf("unexpected login response: %s, err: %v", loginBody, err)
	}

	requestBody := bytes.NewBufferString(`{"ports":[{"port":12712,"protocol":"tcp","label":"Web"},{"port":5353,"protocol":"udp","label":"Discovery"}]}`)
	saveRequest, err := http.NewRequest(http.MethodPut, baseURL+"/api/container/host-container/configured-ports", requestBody)
	if err != nil {
		t.Fatalf("create save request: %v", err)
	}
	saveRequest.Header.Set("Authorization", "Bearer "+login.Data.JWT)
	saveRequest.Header.Set("Content-Type", "application/json")
	saveResponse, err := http.DefaultClient.Do(saveRequest)
	if err != nil {
		t.Fatalf("save configured ports: %v", err)
	}
	saveBody, err := io.ReadAll(saveResponse.Body)
	saveResponse.Body.Close()
	if err != nil {
		t.Fatalf("read save response: %v", err)
	}
	if saveResponse.StatusCode != http.StatusOK {
		t.Fatalf("expected save status 200, got %d: %s", saveResponse.StatusCode, saveBody)
	}

	listRequest, err := http.NewRequest(http.MethodGet, baseURL+"/api/containers", nil)
	if err != nil {
		t.Fatalf("create containers request: %v", err)
	}
	listRequest.Header.Set("Authorization", "Bearer "+login.Data.JWT)
	listResponse, err := http.DefaultClient.Do(listRequest)
	if err != nil {
		t.Fatalf("request containers: %v", err)
	}
	listBody, err := io.ReadAll(listResponse.Body)
	listResponse.Body.Close()
	if err != nil {
		t.Fatalf("read containers response: %v", err)
	}
	if listResponse.StatusCode != http.StatusOK {
		t.Fatalf("expected containers status 200, got %d: %s", listResponse.StatusCode, listBody)
	}

	var containers struct {
		Data []struct {
			NetworkMode     string `json:"networkMode"`
			ConfiguredPorts []struct {
				Port     int    `json:"port"`
				Protocol string `json:"protocol"`
				Label    string `json:"label"`
			} `json:"configuredPorts"`
		} `json:"data"`
	}
	if err := json.Unmarshal(listBody, &containers); err != nil {
		t.Fatalf("decode containers response: %v", err)
	}
	if len(containers.Data) != 1 || containers.Data[0].NetworkMode != "host" {
		t.Fatalf("unexpected containers response: %s", listBody)
	}
	ports := containers.Data[0].ConfiguredPorts
	if len(ports) != 2 || ports[0].Port != 5353 || ports[0].Protocol != "udp" || ports[1].Port != 12712 || ports[1].Protocol != "tcp" || ports[1].Label != "Web" {
		t.Fatalf("unexpected configured ports: %s", listBody)
	}

	if _, err := os.Stat(configPath); err != nil {
		t.Fatalf("expected persisted configured port file: %v", err)
	}

	updatedRequest, err := http.NewRequest(http.MethodPut, baseURL+"/api/container/host-container/configured-ports", bytes.NewBufferString(`{"ports":[{"port":18080,"protocol":"tcp","label":"Updated"}]}`))
	if err != nil {
		t.Fatalf("create update configured ports request: %v", err)
	}
	updatedRequest.Header.Set("Authorization", "Bearer "+login.Data.JWT)
	updatedRequest.Header.Set("Content-Type", "application/json")
	updatedResponse, err := http.DefaultClient.Do(updatedRequest)
	if err != nil {
		t.Fatalf("update configured ports: %v", err)
	}
	updatedResponse.Body.Close()
	if updatedResponse.StatusCode != http.StatusOK {
		t.Fatalf("expected updated configured ports status 200, got %d", updatedResponse.StatusCode)
	}

	updatedListRequest, err := http.NewRequest(http.MethodGet, baseURL+"/api/containers", nil)
	if err != nil {
		t.Fatalf("create updated containers request: %v", err)
	}
	updatedListRequest.Header.Set("Authorization", "Bearer "+login.Data.JWT)
	updatedListResponse, err := http.DefaultClient.Do(updatedListRequest)
	if err != nil {
		t.Fatalf("request updated containers: %v", err)
	}
	updatedListBody, err := io.ReadAll(updatedListResponse.Body)
	updatedListResponse.Body.Close()
	if err != nil {
		t.Fatalf("read updated containers response: %v", err)
	}
	if updatedListResponse.StatusCode != http.StatusOK {
		t.Fatalf("expected updated containers status 200, got %d: %s", updatedListResponse.StatusCode, updatedListBody)
	}
	var updatedContainers struct {
		Data []struct {
			ConfiguredPorts []struct {
				Port     int    `json:"port"`
				Protocol string `json:"protocol"`
				Label    string `json:"label"`
			} `json:"configuredPorts"`
		} `json:"data"`
	}
	if err := json.Unmarshal(updatedListBody, &updatedContainers); err != nil {
		t.Fatalf("decode updated containers response: %v", err)
	}
	if len(updatedContainers.Data) != 1 || len(updatedContainers.Data[0].ConfiguredPorts) != 1 {
		t.Fatalf("expected one replacement configured port: %s", updatedListBody)
	}
	updatedPort := updatedContainers.Data[0].ConfiguredPorts[0]
	if updatedPort.Port != 18080 || updatedPort.Protocol != "tcp" || updatedPort.Label != "Updated" {
		t.Fatalf("unexpected replacement configured port: %#v", updatedPort)
	}

	listStatus.Store(http.StatusInternalServerError)
	failureRequest, err := http.NewRequest(http.MethodGet, baseURL+"/api/containers", nil)
	if err != nil {
		t.Fatalf("create failed containers request: %v", err)
	}
	failureRequest.Header.Set("Authorization", "Bearer "+login.Data.JWT)
	failureResponse, err := http.DefaultClient.Do(failureRequest)
	if err != nil {
		t.Fatalf("request failed containers list: %v", err)
	}
	failureResponse.Body.Close()
	if failureResponse.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected failed containers list status 500, got %d", failureResponse.StatusCode)
	}
	contentAfterFailure, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read configured ports after list failure: %v", err)
	}
	if !bytes.Contains(contentAfterFailure, []byte(`"host-service"`)) {
		t.Fatalf("configured ports were removed after Docker list failure: %s", contentAfterFailure)
	}

	listStatus.Store(http.StatusOK)
	includeContainer.Store(false)
	deletedListRequest, err := http.NewRequest(http.MethodGet, baseURL+"/api/containers", nil)
	if err != nil {
		t.Fatalf("create deleted containers request: %v", err)
	}
	deletedListRequest.Header.Set("Authorization", "Bearer "+login.Data.JWT)
	deletedListResponse, err := http.DefaultClient.Do(deletedListRequest)
	if err != nil {
		t.Fatalf("request deleted containers list: %v", err)
	}
	deletedListBody, err := io.ReadAll(deletedListResponse.Body)
	deletedListResponse.Body.Close()
	if err != nil {
		t.Fatalf("read deleted containers response: %v", err)
	}
	if deletedListResponse.StatusCode != http.StatusOK {
		t.Fatalf("expected deleted containers list status 200, got %d: %s", deletedListResponse.StatusCode, deletedListBody)
	}
	var deletedContainers struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(deletedListBody, &deletedContainers); err != nil {
		t.Fatalf("decode deleted containers response: %v", err)
	}
	if len(deletedContainers.Data) != 0 {
		t.Fatalf("expected no containers after deletion, got %s", deletedListBody)
	}
	contentAfterCleanup, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read configured ports after cleanup: %v", err)
	}
	if bytes.Contains(contentAfterCleanup, []byte(`"host-service"`)) {
		t.Fatalf("stale configured ports remain after container deletion: %s", contentAfterCleanup)
	}
}

func getEmbeddedTestPort(t *testing.T) int {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to get free port: %v", err)
	}
	defer listener.Close()

	return listener.Addr().(*net.TCPAddr).Port
}

func waitForEmbeddedServer(t *testing.T, port int, path string) {
	t.Helper()

	client := &http.Client{Timeout: 200 * time.Millisecond}
	url := fmt.Sprintf("http://127.0.0.1:%d%s", port, path)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		response, err := client.Get(url)
		if err == nil {
			response.Body.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}

	t.Fatalf("embedded server on port %d did not become ready", port)
}
