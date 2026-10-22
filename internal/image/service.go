package image

import (
	"context"
	"fmt"
	"strings"

	dockercontainer "github.com/docker/docker/api/types/container"
	dockerimage "github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/onlyLTY/dockerCopilot/internal/types"
)

// Service owns image listing, usage annotation, and image removal.
type Service struct{ docker *client.Client }

func NewService(docker *client.Client) *Service { return &Service{docker: docker} }

func SplitImageNameAndTag(images []types.Image) []types.Image {
	for i, item := range images {
		if len(item.RepoTags) > 0 {
			tag := item.RepoTags[0]
			if idx := strings.LastIndex(tag, ":"); idx >= 0 && !strings.Contains(tag[idx+1:], "/") {
				images[i].ImageName, images[i].ImageTag = tag[:idx], tag[idx+1:]
			} else {
				images[i].ImageName, images[i].ImageTag = tag, "None"
			}
		} else if len(item.RepoDigests) > 0 {
			images[i].ImageName, images[i].ImageTag = strings.Split(item.RepoDigests[0], "@")[0], "None"
		} else {
			images[i].ImageName, images[i].ImageTag = "None", "None"
		}
	}
	return images
}

func CalculateImageSize(images []types.Image) []types.Image {
	for i := range images {
		if images[i].Size >= 1024*1024*1024 {
			images[i].SizeFormat = fmt.Sprintf("%d Gb", images[i].Size/1024/1024/1024)
		} else {
			images[i].SizeFormat = fmt.Sprintf("%d Mb", images[i].Size/1024/1024)
		}
	}
	return images
}

func (s *Service) List(ctx context.Context) ([]types.Image, error) {
	images, err := s.docker.ImageList(ctx, dockerimage.ListOptions{})
	if err != nil {
		return nil, err
	}
	result := make([]types.Image, 0, len(images))
	for _, item := range images {
		result = append(result, types.Image{Summary: item})
	}
	result = CalculateImageSize(SplitImageNameAndTag(result))
	containers, err := s.docker.ContainerList(ctx, dockercontainer.ListOptions{All: true})
	if err != nil {
		return result, err
	}
	used := make(map[string]struct{}, len(containers))
	for _, item := range containers {
		used[item.ImageID] = struct{}{}
	}
	for i := range result {
		_, result[i].InUsed = used[result[i].ID]
	}
	return result, nil
}

func (s *Service) Remove(ctx context.Context, id string, force bool) error {
	_, err := s.docker.ImageRemove(ctx, id, dockerimage.RemoveOptions{Force: force})
	return err
}
