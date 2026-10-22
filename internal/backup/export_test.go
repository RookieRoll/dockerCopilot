package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	dockertypes "github.com/docker/docker/api/types"
	dockerBackend "github.com/docker/docker/api/types/backend"
	dockerClient "github.com/docker/docker/client"
	"github.com/onlyLTY/dockerCopilot/internal/operation"
	"sigs.k8s.io/yaml"
)

const snapshotInspect = `{
  "Id":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
  "Name":"/compat-service",
  "Config":{"Image":"example/compat:2.3","Env":["MODE=compat","TZ=UTC"],"Hostname":"compat-host","Tty":true},
  "HostConfig":{"NetworkMode":"bridge","Binds":["/srv/compat-data:/var/lib/compat:ro"],"PortBindings":{"8080/tcp":[{"HostIp":"127.0.0.1","HostPort":"18080"}],"8080/udp":[{"HostIp":"0.0.0.0","HostPort":"18081"}]},"RestartPolicy":{"Name":"unless-stopped"}},
  "NetworkSettings":{"Networks":{"compat-net":{"NetworkID":"network-id","EndpointID":"endpoint-id","Gateway":"172.28.0.1","IPAddress":"172.28.0.8","IPPrefixLen":16,"MacAddress":"02:42:ac:1c:00:08","Aliases":["compat-service","compat-alias"]}}},
  "Mounts":[{"Type":"bind","Source":"/srv/compat-data","Destination":"/var/lib/compat","Mode":"ro","RW":false,"Propagation":"rprivate"}]
}`

const snapshotContainerID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func newSnapshotTestService(t *testing.T, directory string, coordinator *operation.Coordinator) *Service {
	return newSnapshotTestServiceWithInspect(t, directory, coordinator, snapshotInspect, "/compat-service")
}

func newSnapshotTestServiceWithInspect(t *testing.T, directory string, coordinator *operation.Coordinator, inspectBody, listedName string) *Service {
	t.Helper()
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/version":
			fmt.Fprint(w, `{"ApiVersion":"1.45","MinAPIVersion":"1.24"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1.45/containers/json":
			fmt.Fprintf(w, `[{"Id":%q,"Names":[%q],"Image":"example/compat:2.3","ImageID":"sha256:compat","State":"running"}]`, snapshotContainerID, listedName)
		case r.Method == http.MethodGet && r.URL.Path == "/v1.45/containers/"+snapshotContainerID+"/json":
			fmt.Fprint(w, inspectBody)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(daemon.Close)
	docker, err := dockerClient.NewClientWithOpts(dockerClient.WithHost(daemon.URL), dockerClient.WithVersion("1.45"))
	if err != nil {
		t.Fatalf("create fake Docker client: %v", err)
	}
	t.Cleanup(func() { _ = docker.Close() })
	return NewService(docker, directory, coordinator)
}

func TestExportSnapshotsAreUniqueAtomicPrivateAndLegacyCompatible(t *testing.T) {
	directory := t.TempDir()
	service := newSnapshotTestService(t, directory, operation.NewCoordinator())
	first, err := service.ExportJSON(context.Background())
	if err != nil {
		t.Fatalf("export JSON snapshot: %v", err)
	}
	second, err := service.ExportJSON(context.Background())
	if err != nil {
		t.Fatalf("export second JSON snapshot on same day: %v", err)
	}
	compose, err := service.ExportComposeSnapshot(context.Background())
	if err != nil {
		t.Fatalf("export Compose snapshot: %v", err)
	}
	if first.Filename == second.Filename || filepath.Ext(first.Filename) != ".json" || filepath.Ext(compose.Filename) != ".yaml" {
		t.Fatalf("snapshots should have unique same-day names and expected formats: first=%q second=%q compose=%q", first.Filename, second.Filename, compose.Filename)
	}
	if len(first.Items) != 1 || first.Items[0].Status != "included" || first.Items[0].Name != "compat-service" {
		t.Fatalf("unexpected item results: %+v", first.Items)
	}
	files, err := service.List()
	if err != nil || len(files) != 3 {
		t.Fatalf("List() files=%v err=%v; want all three completed snapshots", files, err)
	}

	for _, filename := range []string{first.Filename, second.Filename, compose.Filename} {
		info, statErr := os.Stat(filepath.Join(directory, filename))
		if statErr != nil {
			t.Fatalf("stat snapshot %s: %v", filename, statErr)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
			t.Errorf("snapshot %s is readable by group/other: mode %04o", filename, info.Mode().Perm())
		}
	}

	jsonData, err := os.ReadFile(filepath.Join(directory, first.Filename))
	if err != nil {
		t.Fatal(err)
	}
	var configs []dockerBackend.ContainerCreateConfig
	if err := json.Unmarshal(jsonData, &configs); err != nil {
		t.Fatalf("legacy create-config array did not decode: %v", err)
	}
	if len(configs) != 1 || configs[0].Name != "compat-service" || configs[0].Config.Image != "example/compat:2.3" || len(configs[0].Config.Env) != 2 || configs[0].HostConfig.NetworkMode != "bridge" || configs[0].HostConfig.RestartPolicy.Name != "unless-stopped" {
		t.Fatalf("JSON snapshot lost legacy configuration: %+v", configs)
	}
	if configs[0].Config.Hostname != "compat-host" || len(configs[0].HostConfig.Binds) != 1 || configs[0].HostConfig.Binds[0] != "/srv/compat-data:/var/lib/compat:ro" || configs[0].NetworkingConfig.EndpointsConfig["compat-net"] == nil {
		t.Fatalf("JSON snapshot changed legacy hostname/mount/network behavior: %+v", configs[0])
	}
	var legacy []dockerBackend.ContainerCreateConfig
	legacyFixture, err := os.ReadFile(filepath.Join("..", "utiles", "testdata", "legacy-container-backup.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(legacyFixture, &legacy); err != nil || len(legacy) != 1 || legacy[0].Config.Image != "example/legacy:1.2" {
		t.Fatalf("existing JSON backup fixture is no longer compatible: len=%d err=%v", len(legacy), err)
	}

	composeData, err := os.ReadFile(filepath.Join(directory, compose.Filename))
	if err != nil {
		t.Fatal(err)
	}
	var composeDoc struct {
		Services map[string]struct {
			Image       string             `json:"image"`
			Hostname    string             `json:"hostname"`
			NetworkMode string             `json:"network_mode"`
			Environment map[string]*string `json:"environment"`
			Volumes     []struct {
				Source string `json:"source"`
				Target string `json:"target"`
			} `json:"volumes"`
		} `json:"services"`
	}
	if err := yaml.Unmarshal(composeData, &composeDoc); err != nil {
		t.Fatalf("decode Compose snapshot: %v\n%s", err, composeData)
	}
	serviceDoc := composeDoc.Services["compat-service"]
	if serviceDoc.Image != "example/compat:2.3" || serviceDoc.Hostname != "compat-host" || serviceDoc.NetworkMode != "bridge" || serviceDoc.Environment["MODE"] == nil || *serviceDoc.Environment["MODE"] != "compat" || len(serviceDoc.Volumes) != 1 || serviceDoc.Volumes[0].Source != "/srv/compat-data" {
		t.Fatalf("Compose snapshot lost compatible fields: %s", composeData)
	}
}

func TestExportRejectsMissingInspectConfigurationAndNameDrift(t *testing.T) {
	tests := []struct {
		name        string
		inspectBody string
		listedName  string
	}{
		{name: "missing inspect identity", inspectBody: `{}`, listedName: "/compat-service"},
		{name: "missing config", inspectBody: `{"Id":"` + snapshotContainerID + `","Name":"/compat-service","HostConfig":{},"NetworkSettings":{}}`, listedName: "/compat-service"},
		{name: "missing host config", inspectBody: `{"Id":"` + snapshotContainerID + `","Name":"/compat-service","Config":{},"NetworkSettings":{}}`, listedName: "/compat-service"},
		{name: "missing network settings", inspectBody: `{"Id":"` + snapshotContainerID + `","Name":"/compat-service","Config":{},"HostConfig":{}}`, listedName: "/compat-service"},
		{name: "list and inspect names differ", inspectBody: strings.Replace(snapshotInspect, `"/compat-service"`, `"/renamed-service"`, 1), listedName: "/compat-service"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			directory := t.TempDir()
			service := newSnapshotTestServiceWithInspect(t, directory, operation.NewCoordinator(), tt.inspectBody, tt.listedName)
			result, err := service.ExportJSON(context.Background())
			var snapshotErr *SnapshotError
			if !errors.As(err, &snapshotErr) || result.Filename != "" || len(result.Items) != 1 || result.Items[0].Status != "failed" {
				t.Fatalf("expected explicit item failure and no published file, result=%+v err=%v", result, err)
			}
			files, readErr := os.ReadDir(directory)
			if readErr != nil || len(files) != 0 {
				t.Fatalf("failed capture left snapshot artifacts: files=%v err=%v", files, readErr)
			}
		})
	}
}

func TestExportSkipsContainerUnderUpdateLeaseAndPublishesNoPartialFile(t *testing.T) {
	directory := t.TempDir()
	coordinator := operation.NewCoordinator()
	update, err := coordinator.Reserve(operation.Request{TaskID: "update-task", Kind: operation.ContainerUpdate, Target: operation.Target{ID: snapshotContainerID, Name: "compat-service"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := update.Lease.Confirm(); err != nil {
		t.Fatal(err)
	}
	service := newSnapshotTestService(t, directory, coordinator)
	result, err := service.ExportJSON(context.Background())
	var snapshotErr *SnapshotError
	if !errors.As(err, &snapshotErr) || !strings.Contains(err.Error(), "compat-service") {
		t.Fatalf("export error=%v; want a named per-container update conflict", err)
	}
	if result.Filename != "" || len(result.Items) != 1 || result.Items[0].Status != "failed" {
		t.Fatalf("partial snapshot should not publish: %+v", result)
	}
	if files, err := service.List(); err != nil || len(files) != 0 {
		t.Fatalf("failed snapshot left a listed file: files=%v err=%v", files, err)
	}
}

func TestSnapshotLeaseBlocksMutationsOnlyUntilInspectionCompletes(t *testing.T) {
	coordinator := operation.NewCoordinator()
	service := NewService(nil, t.TempDir(), coordinator)
	releaseSnapshot, err := service.reserveSnapshot(context.Background(), dockertypes.Container{
		ID: snapshotContainerID, Names: []string{"/compat-service"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = coordinator.Reserve(operation.Request{
		TaskID: "concurrent-update", Kind: operation.ContainerUpdate,
		Target: operation.Target{ID: snapshotContainerID, Name: "compat-service"},
	})
	var conflict *operation.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("mutation should conflict while snapshot lease is held: %v", err)
	}
	releaseSnapshot()
	reservation, err := coordinator.Reserve(operation.Request{
		TaskID: "after-snapshot", Kind: operation.ContainerUpdate,
		Target: operation.Target{ID: snapshotContainerID, Name: "compat-service"},
	})
	if err != nil {
		t.Fatalf("snapshot lease was not released after inspection: %v", err)
	}
	reservation.Lease.Release()
}

func TestAtomicSnapshotCollisionLeavesExistingFileAndCleansTemporaryFile(t *testing.T) {
	directory := t.TempDir()
	service := NewService(nil, directory)
	suffix := "fixed-test-suffix"
	service.newSuffix = func() string { return suffix }
	filename := fmt.Sprintf("backup-%s-%s.json", time.Now().Format("2006-01-02"), suffix)
	existing := []byte("existing complete snapshot")
	if err := os.WriteFile(filepath.Join(directory, filename), existing, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := service.writeSnapshot(".json", []byte("new snapshot")); err == nil {
		t.Fatal("expected unique-name allocation failure")
	}
	got, err := os.ReadFile(filepath.Join(directory, filename))
	if err != nil || string(got) != string(existing) {
		t.Fatalf("existing snapshot changed: %q err=%v", got, err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filename {
		t.Fatalf("failed publish left temporary/partial files: %v", entries)
	}
}

func TestSnapshotWriteFailureLeavesNoPublishedOrTemporaryFile(t *testing.T) {
	directory := t.TempDir()
	service := NewService(nil, directory)
	writeFailure := errors.New("simulated disk write failure")
	service.writeData = func(writer io.Writer, data []byte) (int, error) {
		written, err := writer.Write(data[:min(3, len(data))])
		if err != nil {
			return written, err
		}
		return written, writeFailure
	}
	if _, err := service.writeSnapshot(".json", []byte("partial body")); !errors.Is(err, writeFailure) {
		t.Fatalf("writeSnapshot error=%v; want injected failure", err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("write failure left visible or temporary artifacts: %v", entries)
	}
}
