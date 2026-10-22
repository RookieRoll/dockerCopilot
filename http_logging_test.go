package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/onlyLTY/dockerCopilot/internal/config"
	"github.com/onlyLTY/dockerCopilot/internal/observability"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/rest"
	restHandler "github.com/zeromicro/go-zero/rest/handler"
)

func TestDefaultHTTPRequestDumpIsDisabled(t *testing.T) {
	root := t.TempDir()
	stdoutPath := filepath.Join(root, "stdout.log")
	stderrPath := filepath.Join(root, "stderr.log")
	logDir := filepath.Join(root, "logs")
	cmd := exec.Command(os.Args[0], "-test.run=^TestHTTPLoggingSuppressionHelper$")
	cmd.Env = append(os.Environ(),
		"DOCKERCOPILOT_HTTP_LOG_HELPER=1",
		"DOCKERCOPILOT_HTTP_TEST_ROOT="+root,
		"DOCKERCOPILOT_HTTP_TEST_STDOUT="+stdoutPath,
		"DOCKERCOPILOT_HTTP_TEST_STDERR="+stderrPath,
		"DOCKERCOPILOT_HTTP_TEST_LOG_DIR="+logDir,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("HTTP logging helper failed: %v; %s", err, output)
	}
	stdout, err := os.ReadFile(stdoutPath)
	if err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(logDir, "dockercopilot-*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("expected one persisted log, found %v", files)
	}
	fileContent, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, output := range [][]byte{stdout, fileContent} {
		for _, secret := range []string{"HTTPQUERYSECRET", "HTTPHEADERSECRET", "HTTPCOOKIESECRET", "HTTPBODYSECRET", "PANICHTTPSECRET"} {
			if bytes.Contains(output, []byte(secret)) {
				t.Fatalf("HTTP request dump leaked %q: %s", secret, output)
			}
		}
		if !bytes.Contains(output, []byte("http-log-test-started")) {
			t.Fatalf("test log marker missing from output: %s", output)
		}
	}
	if !bytes.Equal(fileContent, extractJSONLines(stdout)) {
		t.Fatalf("stdout and persistent file differ\nstdout: %s\nfile: %s", stdout, fileContent)
	}
	if stderr, err := os.ReadFile(stderrPath); err != nil || len(bytes.TrimSpace(stderr)) != 0 {
		t.Fatalf("unexpected stderr: %s (%v)", stderr, err)
	}
}

func TestHTTPLoggingSuppressionHelper(t *testing.T) {
	if os.Getenv("DOCKERCOPILOT_HTTP_LOG_HELPER") == "" {
		return
	}
	stdoutFile, err := os.Create(os.Getenv("DOCKERCOPILOT_HTTP_TEST_STDOUT"))
	if err != nil {
		panic(err)
	}
	stderrFile, err := os.Create(os.Getenv("DOCKERCOPILOT_HTTP_TEST_STDERR"))
	if err != nil {
		panic(err)
	}
	os.Stdout, os.Stderr = stdoutFile, stderrFile
	if err := observability.Setup(os.Getenv("DOCKERCOPILOT_HTTP_TEST_LOG_DIR"), "info", 7, 1<<20, 2<<20); err != nil {
		panic(err)
	}
	logx.Info("http-log-test-started")
	restHandler.SetSlowThreshold(time.Millisecond)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	cfg := config.Config{}
	cfg.Host, cfg.Port = "127.0.0.1", port
	cfg.Middlewares.Log, cfg.Middlewares.Recover, cfg.Verbose = true, true, true
	disableDefaultRequestLogging(&cfg)
	if cfg.Middlewares.Log || cfg.Verbose {
		panic("default request logging was not disabled")
	}
	server, err := rest.NewServer(cfg.RestConf)
	if err != nil {
		panic(err)
	}
	server.AddRoutes([]rest.Route{
		{Method: http.MethodPost, Path: "/fail", Handler: func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) }},
		{Method: http.MethodGet, Path: "/slow", Handler: func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(10 * time.Millisecond)
			_, _ = io.WriteString(w, "ok")
		}},
		{Method: http.MethodGet, Path: "/panic", Handler: func(http.ResponseWriter, *http.Request) { panic("panic: Bearer PANICHTTPSECRET") }},
	})
	go server.Start()
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	client := &http.Client{Timeout: 4 * time.Second}
	var failResponse *http.Response
	for deadline := time.Now().Add(4 * time.Second); time.Now().Before(deadline); {
		req, _ := http.NewRequest(http.MethodPost, baseURL+"/fail?token=HTTPQUERYSECRET", strings.NewReader(`{"password":"HTTPBODYSECRET"}`))
		req.Header.Set("Authorization", "Bearer HTTPHEADERSECRET")
		req.Header.Set("Cookie", "session=HTTPCOOKIESECRET")
		failResponse, err = client.Do(req)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		panic(err)
	}
	_ = failResponse.Body.Close()
	for _, path := range []string{"/slow?token=HTTPQUERYSECRET", "/panic?token=HTTPQUERYSECRET"} {
		response, err := client.Get(baseURL + path)
		if err != nil {
			panic(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}
	server.Stop()
	_ = stdoutFile.Close()
	_ = stderrFile.Close()
}

func extractJSONLines(data []byte) []byte {
	var result strings.Builder
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) > 0 && line[0] == '{' {
			result.Write(line)
			result.WriteByte('\n')
		}
	}
	return []byte(result.String())
}
