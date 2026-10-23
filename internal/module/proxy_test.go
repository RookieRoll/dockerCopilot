package module

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A fresh process isolates net/http's cached proxy environment from other
// registry tests. The only network target reached is the local fake proxy.
func TestRegistryClientUsesProxyEnvironment(t *testing.T) {
	if os.Getenv("DOCKERCOPILOT_TEST_PROXY_CHILD") == "1" {
		checker, err := NewImageCheckWithOptions(ImageCheckOptions{})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		digest, err := getDigestWithClient(ctx, checker.client, "http://registry.proxy-fixture.invalid/v2/app/manifests/latest", "")
		if err != nil || digest != "sha256:proxy-fixture" {
			t.Fatalf("registry client did not use configured proxy: digest=%q err=%v", digest, err)
		}
		request, _ := http.NewRequest(http.MethodGet, "http://registry.bypass.invalid/v2/", nil)
		transport := checker.client.Transport.(*http.Transport)
		proxy, err := transport.Proxy(request)
		if err != nil || proxy != nil {
			t.Fatalf("NO_PROXY did not bypass configured host: proxy=%v err=%v", proxy, err)
		}
		return
	}
	var calls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead || r.URL.Host != "registry.proxy-fixture.invalid" || r.URL.Path != "/v2/app/manifests/latest" {
			http.Error(w, "unexpected proxy request", http.StatusBadRequest)
			return
		}
		calls.Add(1)
		w.Header().Set(ContentDigestHeader, "sha256:proxy-fixture")
	}))
	defer proxy.Close()
	command := exec.Command(os.Args[0], "-test.run=^TestRegistryClientUsesProxyEnvironment$", "-test.count=1")
	for _, entry := range os.Environ() {
		key := strings.ToUpper(strings.SplitN(entry, "=", 2)[0])
		if key != "HTTP_PROXY" && key != "HTTPS_PROXY" && key != "ALL_PROXY" && key != "NO_PROXY" && key != "REQUEST_METHOD" && key != "DOCKERCOPILOT_TEST_PROXY_CHILD" {
			command.Env = append(command.Env, entry)
		}
	}
	command.Env = append(command.Env, "DOCKERCOPILOT_TEST_PROXY_CHILD=1", "HTTP_PROXY="+proxy.URL, "HTTPS_PROXY="+proxy.URL, "NO_PROXY=registry.bypass.invalid")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("proxy subprocess: %v\n%s", err, output)
	}
	if calls.Load() != 1 {
		t.Fatalf("proxy requests=%d, want one manifest request", calls.Load())
	}
}
