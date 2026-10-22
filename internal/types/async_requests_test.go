package types

import (
	"encoding/json"
	"testing"
)

func TestContainerBatchReqDeleteOldContainerOptionalContract(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want *bool
	}{
		{name: "omitted", body: `{"action":"update","containerIDs":["id"]}`},
		{name: "false", body: `{"action":"update","containerIDs":["id"],"deleteOldContainer":false}`, want: boolValue(false)},
		{name: "true", body: `{"action":"update","containerIDs":["id"],"deleteOldContainer":true}`, want: boolValue(true)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got ContainerBatchReq
			if err := json.Unmarshal([]byte(tc.body), &got); err != nil {
				t.Fatal(err)
			}
			if got.Action != "update" || len(got.ContainerIDs) != 1 || got.ContainerIDs[0] != "id" {
				t.Fatalf("decoded request = %+v", got)
			}
			if tc.want == nil {
				if got.DeleteOldContainer != nil {
					t.Fatalf("omitted option decoded as %v", *got.DeleteOldContainer)
				}
			} else if got.DeleteOldContainer == nil || *got.DeleteOldContainer != *tc.want {
				t.Fatalf("decoded option = %v, want %v", got.DeleteOldContainer, *tc.want)
			}
		})
	}
}

func TestImageCleanupReqJSONContract(t *testing.T) {
	var got ImageCleanupReq
	if err := json.Unmarshal([]byte(`{"policy":"unused"}`), &got); err != nil {
		t.Fatal(err)
	}
	if got.Policy != "unused" {
		t.Fatalf("policy = %q, want unused", got.Policy)
	}
}

func boolValue(value bool) *bool { return &value }
