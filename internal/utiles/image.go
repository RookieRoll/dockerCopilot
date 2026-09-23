package utiles

import (
	"context"
	"fmt"
	"github.com/docker/docker/api/types/image"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
	MyType "github.com/onlyLTY/dockerCopilot/internal/types"
	"strings"
)

func GetImagesList(ctx *svc.ServiceContext) ([]MyType.Image, error) {
	var imagesList []MyType.Image
	dockerImages, err := ctx.DockerClient.ImageList(context.Background(), image.ListOptions{})
	if err != nil {
		return nil, err
	}

	for _, img := range dockerImages {
		i := MyType.Image{
			Summary:    img,
			ImageName:  "",
			ImageTag:   "",
			InUsed:     false,
			SizeFormat: "",
		}
		imagesList = append(imagesList, i)
	}
	//看不明白就不要看了，这内存反复地申请，如果你看明白了 给这改成指针吧，啥？我为啥不直接写指针，我懒癌犯了就这样，欢迎pr
	imagesList, err = checkImageInUsed(ctx, splitImageNameAndTag(calculateImageSize(imagesList)))
	if err != nil {
		return imagesList, err
	}
	return imagesList, nil
}

func splitImageNameAndTag(imagesList []MyType.Image) []MyType.Image {
	for i, imageInfo := range imagesList {
		if len(imageInfo.RepoTags) != 0 {
			// 从最后一个 ":" 切分,registry 带端口(如 localhost:5000/app:v1)时不会切错
			if idx := strings.LastIndex(imageInfo.RepoTags[0], ":"); idx != -1 && !strings.Contains(imageInfo.RepoTags[0][idx+1:], "/") {
				imagesList[i].ImageName = imageInfo.RepoTags[0][:idx]
				imagesList[i].ImageTag = imageInfo.RepoTags[0][idx+1:]
			} else {
				imagesList[i].ImageName = imageInfo.RepoTags[0]
				imagesList[i].ImageTag = "None"
			}
		} else if len(imageInfo.RepoDigests) != 0 {
			imagesList[i].ImageName = strings.Split(imageInfo.RepoDigests[0], "@")[0]
			imagesList[i].ImageTag = "None"
		} else {
			imagesList[i].ImageName = "None"
			imagesList[i].ImageTag = "None"
		}
	}
	return imagesList
}
func checkImageInUsed(svc *svc.ServiceContext, imageList []MyType.Image) ([]MyType.Image, error) {
	list, err := GetContainerList(svc)
	if err != nil {
		return imageList, err
	}
	used := make(map[string]struct{}, len(list))
	for _, v := range list {
		used[v.ImageID] = struct{}{}
	}
	for i := range imageList {
		if _, ok := used[imageList[i].ID]; ok {
			imageList[i].InUsed = true
		}
	}
	return imageList, nil
}
func calculateImageSize(imagesList []MyType.Image) []MyType.Image {
	for i := range imagesList {
		if imagesList[i].Size >= 1024*1024*1024 {
			imagesList[i].SizeFormat = // Convert size to gigabytes
				fmt.Sprintf("%d Gb", imagesList[i].Size/1024/1024/1024)
		} else {
			imagesList[i].SizeFormat = // Convert size to megabytes
				fmt.Sprintf("%d Mb", imagesList[i].Size/1024/1024)
		}
	}
	return imagesList
}
