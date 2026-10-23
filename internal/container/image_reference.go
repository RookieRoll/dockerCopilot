package container

import (
	"errors"
	"strings"

	"github.com/distribution/reference"
	dockercontainer "github.com/docker/docker/api/types/container"
)

const updateImageRefLabel = "io.dockercopilot.update-image-ref"

// ValidateUpdateImageReference distinguishes a registry reference from the
// local image ID used to pin ContainerCreate. A named repo@digest is valid.
func ValidateUpdateImageReference(value string) error {
	value = strings.TrimSpace(value)
	if value == "" || strings.HasPrefix(strings.ToLower(value), "sha256:") {
		return errors.New("本地镜像ID不能用于拉取更新，请填写仓库镜像名称和标签")
	}
	if _, err := reference.ParseNormalizedNamed(value); err != nil {
		return errors.New("镜像引用无效，请填写仓库镜像名称和标签，或仓库名@摘要")
	}
	return nil
}

// UpdateImageReference preserves the user's update source independently of
// Config.Image, which becomes an immutable local ID after replacement.
func UpdateImageReference(config *dockercontainer.Config) (string, error) {
	if config != nil {
		for _, candidate := range []string{config.Image, config.Labels[updateImageRefLabel]} {
			candidate = strings.TrimSpace(candidate)
			if ValidateUpdateImageReference(candidate) == nil {
				return candidate, nil
			}
		}
	}
	return "", errors.New("容器未保留可拉取的镜像名称，请在更新时填写仓库镜像名称和标签")
}
