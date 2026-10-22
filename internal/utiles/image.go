package utiles

import (
	"context"
	imageService "github.com/onlyLTY/dockerCopilot/internal/image"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
	MyType "github.com/onlyLTY/dockerCopilot/internal/types"
)

func GetImagesList(ctx *svc.ServiceContext) ([]MyType.Image, error) {
	service := ctx.Images
	if service == nil {
		service = imageService.NewService(ctx.DockerClient)
	}
	return service.List(context.Background())
}

// splitImageNameAndTag remains as a package-private compatibility helper for
// existing utile tests; the implementation lives in the image service.
func splitImageNameAndTag(images []MyType.Image) []MyType.Image {
	return imageService.SplitImageNameAndTag(images)
}
