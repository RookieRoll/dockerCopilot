package container

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/onlyLTY/dockerCopilot/internal/types"
	"github.com/zeromicro/go-zero/rest/httpx"
	"github.com/zeromicro/go-zero/rest/pathvar"
)

func TestUpdateRequestParsesOptionalDeleteOldContainer(t *testing.T) {
	for _, test := range []struct {
		name       string
		fieldValue *string
		want       *bool
	}{
		{name: "omitted"},
		{name: "false", fieldValue: stringPointer("false"), want: boolPointer(false)},
		{name: "true", fieldValue: stringPointer("true"), want: boolPointer(true)},
	} {
		t.Run(test.name, func(t *testing.T) {
			var body bytes.Buffer
			writer := multipart.NewWriter(&body)
			if err := writer.WriteField("imageNameAndTag", "nginx:latest"); err != nil {
				t.Fatal(err)
			}
			if err := writer.WriteField("containerName", "web"); err != nil {
				t.Fatal(err)
			}
			if test.fieldValue != nil {
				if err := writer.WriteField("delOldContainer", *test.fieldValue); err != nil {
					t.Fatal(err)
				}
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, "/api/container/test/update", &body)
			req = pathvar.WithVars(req, map[string]string{"id": "container-id"})
			req.Header.Set("Content-Type", writer.FormDataContentType())
			var parsed types.ContainerUpdateReq
			if err := httpx.Parse(req, &parsed); err != nil {
				t.Fatalf("parse multipart update request: %v", err)
			}
			if parsed.Id != "container-id" || parsed.ImageNameAndTag != "nginx:latest" || parsed.ContainerName != "web" {
				t.Fatalf("required update fields were not parsed: %#v", parsed)
			}
			if test.want == nil {
				if parsed.DelOldContainer != nil {
					t.Fatalf("omitted field parsed as %v", *parsed.DelOldContainer)
				}
				return
			}
			if parsed.DelOldContainer == nil || *parsed.DelOldContainer != *test.want {
				t.Fatalf("parsed value = %v, want %v", parsed.DelOldContainer, *test.want)
			}
		})
	}
}

func stringPointer(value string) *string { return &value }
func boolPointer(value bool) *bool       { return &value }
