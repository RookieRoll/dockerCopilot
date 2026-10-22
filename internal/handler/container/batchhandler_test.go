package container

import (
	"net/http/httptest"
	"strings"
	"testing"

	containerLogic "github.com/onlyLTY/dockerCopilot/internal/logic/container"
	"github.com/zeromicro/go-zero/rest/httpx"
)

func TestBatchRequestPreservesOptionalDeleteOldValue(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
		want *bool
	}{
		{name: "omitted", body: `{"action":"update","containerIDs":["id"]}`},
		{name: "false", body: `{"action":"update","containerIDs":["id"],"deleteOldContainer":false}`, want: boolPointer(false)},
		{name: "true", body: `{"action":"update","containerIDs":["id"],"deleteOldContainer":true}`, want: boolPointer(true)},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest("POST", "/api/containers/batch", strings.NewReader(test.body))
			request.Header.Set("Content-Type", "application/json")
			var got containerLogic.BatchRequest
			if err := httpx.Parse(request, &got); err != nil {
				t.Fatal(err)
			}
			if got.Action != "update" || len(got.ContainerIDs) != 1 || got.ContainerIDs[0] != "id" {
				t.Fatalf("request fields parsed incorrectly: %#v", got)
			}
			if test.want == nil {
				if got.DeleteOldContainer != nil {
					t.Fatalf("omitted option became explicit: %v", *got.DeleteOldContainer)
				}
			} else if got.DeleteOldContainer == nil || *got.DeleteOldContainer != *test.want {
				t.Fatalf("option = %v, want %v", got.DeleteOldContainer, *test.want)
			}
		})
	}
}
