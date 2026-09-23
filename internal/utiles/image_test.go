package utiles

import (
	"testing"

	"github.com/docker/docker/api/types/image"
	MyType "github.com/onlyLTY/dockerCopilot/internal/types"
)

func TestSplitImageNameAndTag(t *testing.T) {
	cases := []struct {
		repoTag string
		name    string
		tag     string
	}{
		{"nginx:latest", "nginx", "latest"},
		{"localhost:5000/app:v1", "localhost:5000/app", "v1"},
		{"ghcr.io/foo/bar:1.2.3", "ghcr.io/foo/bar", "1.2.3"},
		{"example.com:5000/app", "example.com:5000/app", "None"},
		{"myapp", "myapp", "None"},
	}
	for _, c := range cases {
		got := splitImageNameAndTag([]MyType.Image{{Summary: image.Summary{RepoTags: []string{c.repoTag}}}})
		if got[0].ImageName != c.name || got[0].ImageTag != c.tag {
			t.Errorf("splitImageNameAndTag(%q) = %q:%q, want %q:%q", c.repoTag, got[0].ImageName, got[0].ImageTag, c.name, c.tag)
		}
	}
}
