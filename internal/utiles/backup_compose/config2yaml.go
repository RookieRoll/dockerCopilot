package backupCompose

import (
	"fmt"
	composeType "github.com/compose-spec/compose-go/types"
	dockerTypes "github.com/docker/docker/api/types"
	composeNat "github.com/docker/go-connections/nat"
	"github.com/onlyLTY/dockerCopilot/internal/config"
	"github.com/zeromicro/go-zero/core/logx"
	"os"
	"path/filepath"
	"sigs.k8s.io/yaml"
	"strconv"
	"strings"
	"time"
)

// DockerConfig2ComposeYaml 将docker config转换为docker-compose.yaml
func DockerConfig2ComposeYaml(containerJSONs []dockerTypes.ContainerJSON) (err error) {
	return DockerConfig2ComposeYamlInDir(containerJSONs, config.LegacyBackupDirectory())
}

func DockerConfig2ComposeYamlInDir(containerJSONs []dockerTypes.ContainerJSON, backupDir string) (err error) {
	yamlData, err := MarshalComposeYaml(containerJSONs)
	if err != nil {
		return err
	}
	_, err = os.Stat(backupDir)
	if os.IsNotExist(err) {
		err = os.MkdirAll(backupDir, 0755)
		if err != nil {
			logx.Error("Error creating backup directory:", err)
			return err
		}
	}
	currentDate := time.Now().Format("2006-01-02")
	fileName := fmt.Sprintf("backup-%s-%s.yaml", currentDate, time.Now().Format("150405.000000000"))
	fullPath := filepath.Join(backupDir, fileName)
	err = os.WriteFile(fullPath, yamlData, 0600)
	if err != nil {
		logx.Error("Error writing to file:", err)
		return err
	}
	return nil
}

// MarshalComposeYaml returns the legacy Compose document without writing a
// file. Backup service owns naming, permissions, and atomic publication.
func MarshalComposeYaml(containerJSONs []dockerTypes.ContainerJSON) ([]byte, error) {
	var c composeYaml
	c.Services = make(map[string]composeType.ServiceConfig, len(containerJSONs))
	for _, containerJSON := range containerJSONs {
		var s composeType.ServiceConfig
		formatBaseServiceConfig(containerJSON, &s)
		formatEnvServiceConfig(containerJSON, &s)
		formatNetworkServiceConfig(containerJSON, &s)
		formatVolumeServiceConfig(containerJSON, &s)
		c.Services[s.Name] = s
	}
	yamlData, yamlMarshalErr := yaml.Marshal(c)
	if yamlMarshalErr != nil {
		return nil, fmt.Errorf("marshal Compose backup: %w", yamlMarshalErr)
	}
	return yamlData, nil
}

func formatBaseServiceConfig(containerJSON dockerTypes.ContainerJSON, s *composeType.ServiceConfig) {
	s.Image = containerJSON.Config.Image
	name, cutNameResult := strings.CutPrefix(containerJSON.Name, "/")
	if !cutNameResult {
		logx.Infof("cutting name is: %s", containerJSON.Name)
	}
	s.ContainerName = name
	s.Name = name
	s.Tty = containerJSON.Config.Tty
	s.Hostname = containerJSON.Config.Hostname
	if len(containerJSON.Config.Entrypoint) > 0 {
		s.Entrypoint = composeType.ShellCommand(containerJSON.Config.Entrypoint)
	}
	s.WorkingDir = containerJSON.Config.WorkingDir
	s.Restart = string(containerJSON.HostConfig.RestartPolicy.Name)
	s.Privileged = containerJSON.HostConfig.Privileged
	if len(containerJSON.Config.Cmd) > 0 {
		s.Command = composeType.ShellCommand(containerJSON.Config.Cmd)
	}
	return
}

func formatEnvServiceConfig(containerJSON dockerTypes.ContainerJSON, s *composeType.ServiceConfig) {
	s.Environment = composeType.NewMappingWithEquals(containerJSON.Config.Env)
	return
}

func formatNetworkServiceConfig(containerJSON dockerTypes.ContainerJSON, s *composeType.ServiceConfig) {
	s.NetworkMode = string(containerJSON.HostConfig.NetworkMode)
	for containerPort, v := range containerJSON.HostConfig.PortBindings {
		var p composeType.ServicePortConfig
		proto, port := composeNat.SplitProtoPort(string(containerPort))
		portNum, convertErr := strconv.Atoi(port)
		if convertErr != nil {
			logx.Errorf("Error converting port err is: %v", convertErr)
			continue
		}
		p.Target = uint32(portNum)
		p.Published = v[0].HostPort
		p.Protocol = proto
		s.Ports = append(s.Ports, p)
	}
}

func formatVolumeServiceConfig(containerJSON dockerTypes.ContainerJSON, s *composeType.ServiceConfig) {
	for _, containerVolume := range containerJSON.Mounts {
		var v composeType.ServiceVolumeConfig
		v.Type = string(containerVolume.Type)
		v.Source = containerVolume.Source
		v.Target = containerVolume.Destination
		v.ReadOnly = containerVolume.RW
		s.Volumes = append(s.Volumes, v)
	}
}
