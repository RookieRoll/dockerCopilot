package icons

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/onlyLTY/dockerCopilot/internal/settings"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
)

var handlerTestPNG = []byte{137, 80, 78, 71, 13, 10, 26, 10, 0, 0, 0, 13, 73, 72, 68, 82, 0, 0, 0, 1, 0, 0, 0, 1, 8, 6, 0, 0, 0, 31, 21, 196, 137, 0, 0, 0, 13, 73, 68, 65, 84, 8, 215, 99, 248, 207, 192, 240, 31, 0, 5, 0, 1, 255, 137, 153, 61, 29, 0, 0, 0, 0, 73, 69, 78, 68, 174, 66, 96, 130}

func TestUploadHandlerConcurrentDifferentKeysPreserveMappings(t *testing.T) {
	root := t.TempDir()
	mappingPath := filepath.Join(root, "imageLogos.js")
	if err := os.MkdirAll(filepath.Join(root, "image"), 0o755); err != nil {
		t.Fatalf("create image directory: %v", err)
	}
	if err := os.WriteFile(mappingPath, []byte("export const customImageLogos = {\n};\n"), 0o600); err != nil {
		t.Fatalf("seed mapping: %v", err)
	}
	restoreIconPaths := setIconPaths(filepath.Join(root, "image"), mappingPath)
	defer restoreIconPaths()

	handler := UploadHandler(&svc.ServiceContext{})
	const uploads = 8
	start := make(chan struct{})
	errs := make(chan error, uploads)
	var wait sync.WaitGroup
	for index := 0; index < uploads; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			body, contentType := multipartBody(t, "logo.png", handlerTestPNG, "registry.example:5000/team/app-"+string(rune('a'+index)))
			req := httptest.NewRequest(http.MethodPost, "/api/icons", body)
			req.Header.Set("Content-Type", contentType)
			recorder := httptest.NewRecorder()
			handler(recorder, req)
			if recorder.Code != http.StatusOK {
				errs <- fmt.Errorf("upload %d returned %d: %s", index, recorder.Code, recorder.Body.String())
				return
			}
			errs <- nil
		}(index)
	}
	close(start)
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	service, err := settings.NewIconService(settings.IconOptions{ImageDir: filepath.Join(root, "image"), MappingPath: mappingPath})
	if err != nil {
		t.Fatalf("construct service: %v", err)
	}
	logos, _, err := service.ImageLogos()
	if err != nil {
		t.Fatalf("read concurrent mappings: %v", err)
	}
	if len(logos) != uploads {
		t.Fatalf("lost mappings through handler: got %d, want %d (%#v)", len(logos), uploads, logos)
	}
}

func TestUploadHandlerRejectsCorruptMappingWithoutOverwriting(t *testing.T) {
	root := t.TempDir()
	mappingPath := filepath.Join(root, "imageLogos.js")
	corrupt := []byte("export const customImageLogos = {\n  \"broken\": process.env.SECRET\n};\n")
	if err := os.WriteFile(mappingPath, corrupt, 0o600); err != nil {
		t.Fatalf("seed corrupt mapping: %v", err)
	}
	restoreIconPaths := setIconPaths(filepath.Join(root, "image"), mappingPath)
	defer restoreIconPaths()

	body, contentType := multipartBody(t, "logo.png", handlerTestPNG, "valid/key")
	req := httptest.NewRequest(http.MethodPost, "/api/icons", body)
	req.Header.Set("Content-Type", contentType)
	recorder := httptest.NewRecorder()
	UploadHandler(&svc.ServiceContext{})(recorder, req)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for corrupt mapping, got %d: %s", recorder.Code, recorder.Body.String())
	}
	after, err := os.ReadFile(mappingPath)
	if err != nil {
		t.Fatalf("read mapping after rejected upload: %v", err)
	}
	if !bytes.Equal(after, corrupt) {
		t.Fatalf("corrupt mapping was reset or modified:\n%s", after)
	}
	entries, err := os.ReadDir(filepath.Join(root, "image"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("list image directory: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("rejected upload left icon files: %#v", entries)
	}
}

func TestUploadHandlerLogsStorageFailureAndHidesFilesystemDetails(t *testing.T) {
	root := t.TempDir()
	notDirectory := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(notDirectory, []byte("sentinel"), 0o600); err != nil {
		t.Fatalf("create blocking file: %v", err)
	}
	mappingPath := filepath.Join(root, "imageLogos.js")
	if err := os.WriteFile(mappingPath, []byte("export const customImageLogos = {};\n"), 0o600); err != nil {
		t.Fatalf("seed mapping: %v", err)
	}
	restoreIconPaths := setIconPaths(filepath.Join(notDirectory, "image"), mappingPath)
	defer restoreIconPaths()

	body, contentType := multipartBody(t, "logo.png", handlerTestPNG, "valid/key")
	req := httptest.NewRequest(http.MethodPost, "/api/icons", body)
	req.Header.Set("Content-Type", contentType)
	recorder := httptest.NewRecorder()
	UploadHandler(&svc.ServiceContext{})(recorder, req)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("expected storage failure 500, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if bytes.Contains(recorder.Body.Bytes(), []byte(root)) || bytes.Contains(recorder.Body.Bytes(), []byte("not-a-directory")) {
		t.Fatalf("filesystem details leaked in response: %s", recorder.Body.String())
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte("failed to update config")) {
		t.Fatalf("expected generic storage error response, got %s", recorder.Body.String())
	}
}

func TestUploadHandlerRejectsUnsafeImageName(t *testing.T) {
	root := t.TempDir()
	mappingPath := filepath.Join(root, "imageLogos.js")
	if err := os.MkdirAll(filepath.Join(root, "image"), 0o755); err != nil {
		t.Fatalf("create image directory: %v", err)
	}
	if err := os.WriteFile(mappingPath, []byte("export const customImageLogos = {\n};\n"), 0o600); err != nil {
		t.Fatalf("seed mapping: %v", err)
	}
	restoreIconPaths := setIconPaths(filepath.Join(root, "image"), mappingPath)
	defer restoreIconPaths()

	body, contentType := multipartBody(t, "logo.png", handlerTestPNG, "../../etc/passwd")
	req := httptest.NewRequest(http.MethodPost, "/api/icons", body)
	req.Header.Set("Content-Type", contentType)
	recorder := httptest.NewRecorder()
	UploadHandler(&svc.ServiceContext{})(recorder, req)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for traversal key, got %d: %s", recorder.Code, recorder.Body.String())
	}
	entries, err := os.ReadDir(filepath.Join(root, "image"))
	if err != nil {
		t.Fatalf("list image directory: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("rejected upload left icon files: %#v", entries)
	}
}

func TestUploadHandlerRejectsOversizedIconContent(t *testing.T) {
	root := t.TempDir()
	mappingPath := filepath.Join(root, "imageLogos.js")
	if err := os.MkdirAll(filepath.Join(root, "image"), 0o755); err != nil {
		t.Fatalf("create image directory: %v", err)
	}
	if err := os.WriteFile(mappingPath, []byte("export const customImageLogos = {\n};\n"), 0o600); err != nil {
		t.Fatalf("seed mapping: %v", err)
	}
	restoreIconPaths := setIconPaths(filepath.Join(root, "image"), mappingPath)
	defer restoreIconPaths()

	content := append(append([]byte(nil), handlerTestPNG...), make([]byte, settings.DefaultMaxIconUpload)...)
	body, contentType := multipartBody(t, "logo.png", content, "valid/key")
	req := httptest.NewRequest(http.MethodPost, "/api/icons", body)
	req.Header.Set("Content-Type", contentType)
	recorder := httptest.NewRecorder()
	UploadHandler(&svc.ServiceContext{})(recorder, req)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for oversized content, got %d: %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode oversized response: %v", err)
	}
	if response.Code != http.StatusBadRequest {
		t.Fatalf("unexpected oversized response: %s", recorder.Body.String())
	}
	entries, err := os.ReadDir(filepath.Join(root, "image"))
	if err != nil {
		t.Fatalf("list image directory: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("oversized upload left icon files: %#v", entries)
	}
}

func TestUploadHandlerRejectsOversizedMultipartRequest(t *testing.T) {
	root := t.TempDir()
	mappingPath := filepath.Join(root, "imageLogos.js")
	if err := os.MkdirAll(filepath.Join(root, "image"), 0o755); err != nil {
		t.Fatalf("create image directory: %v", err)
	}
	if err := os.WriteFile(mappingPath, []byte("export const customImageLogos = {\n};\n"), 0o600); err != nil {
		t.Fatalf("seed mapping: %v", err)
	}
	restoreIconPaths := setIconPaths(filepath.Join(root, "image"), mappingPath)
	defer restoreIconPaths()

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	fileWriter, err := writer.CreateFormFile("file", "logo.png")
	if err != nil {
		t.Fatalf("create multipart file: %v", err)
	}
	if _, err := fileWriter.Write(handlerTestPNG); err != nil {
		t.Fatalf("write multipart file: %v", err)
	}
	if err := writer.WriteField("imageName", "valid/key"); err != nil {
		t.Fatalf("write imageName field: %v", err)
	}
	padding := strings.Repeat("x", int(settings.DefaultMaxIconUpload+settings.DefaultIconRequestSlack+1))
	if err := writer.WriteField("padding", padding); err != nil {
		t.Fatalf("write padding field: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart body: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/icons", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	// Simulate a chunked/unknown-length request. The body contains a small PNG
	// plus an oversized non-file field, so the test covers the total request cap.
	req.ContentLength = -1
	req.TransferEncoding = []string{"chunked"}
	recorder := httptest.NewRecorder()
	UploadHandler(&svc.ServiceContext{})(recorder, req)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for oversized request, got %d: %s", recorder.Code, recorder.Body.String())
	}
	entries, err := os.ReadDir(filepath.Join(root, "image"))
	if err != nil {
		t.Fatalf("list image directory: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("oversized request left icon files: %#v", entries)
	}
}

func multipartBody(t *testing.T, filename string, content []byte, imageName string) (*bytes.Buffer, string) {
	t.Helper()
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	fileWriter, err := writer.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("create multipart file: %v", err)
	}
	if _, err := fileWriter.Write(content); err != nil {
		t.Fatalf("write multipart file: %v", err)
	}
	if err := writer.WriteField("imageName", imageName); err != nil {
		t.Fatalf("write imageName field: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart body: %v", err)
	}
	return body, writer.FormDataContentType()
}

func setIconPaths(imageDir, mappingPath string) func() {
	originalImageUploadDir, originalImageLogosPath := imageUploadDir, imageLogosPath
	imageUploadDir, imageLogosPath = imageDir, mappingPath
	return func() {
		imageUploadDir, imageLogosPath = originalImageUploadDir, originalImageLogosPath
	}
}

func TestObtainHandlerReadsLegacyMappingAtConfiguredPath(t *testing.T) {
	root := t.TempDir()
	mappingPath := filepath.Join(root, "imageLogos.js")
	legacy := []byte("// Existing user configuration format retained across upgrades.\n" +
		"export const customImageLogos = {\n" +
		"  \"nginx\": \"/src/config/image/nginx-custom.png\",\n" +
		"  \"registry.example:5000/team/app\": \"/src/config/image/team-app.webp\"\n" +
		"};\n")
	if err := os.WriteFile(mappingPath, legacy, 0o600); err != nil {
		t.Fatalf("seed legacy mapping: %v", err)
	}
	restoreIconPaths := setIconPaths(filepath.Join(root, "image"), mappingPath)
	defer restoreIconPaths()

	req := httptest.NewRequest(http.MethodGet, "/api/icons", nil)
	recorder := httptest.NewRecorder()
	ObtainHandler(&svc.ServiceContext{})(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Code int               `json:"code"`
		Msg  string            `json:"msg"`
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode obtain response: %v", err)
	}
	if response.Code != 200 || response.Msg != "Success" {
		t.Fatalf("unexpected obtain envelope: %s", recorder.Body.String())
	}
	if response.Data["nginx"] != "/src/config/image/nginx-custom.png" || response.Data["registry.example:5000/team/app"] != "/src/config/image/team-app.webp" {
		t.Fatalf("unexpected legacy data: %#v", response.Data)
	}
}

func TestObtainHandlerReportsCorruptMappingWithoutResetting(t *testing.T) {
	root := t.TempDir()
	mappingPath := filepath.Join(root, "imageLogos.js")
	corrupt := []byte("export const customImageLogos = {\n  \"broken\": process.env.HOME\n};\n")
	if err := os.WriteFile(mappingPath, corrupt, 0o600); err != nil {
		t.Fatalf("seed corrupt mapping: %v", err)
	}
	restoreIconPaths := setIconPaths(filepath.Join(root, "image"), mappingPath)
	defer restoreIconPaths()

	req := httptest.NewRequest(http.MethodGet, "/api/icons", nil)
	recorder := httptest.NewRecorder()
	ObtainHandler(&svc.ServiceContext{})(recorder, req)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected explicit error for corrupt mapping, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte("image icon mapping is invalid")) {
		t.Fatalf("expected locatable mapping error, got %s", recorder.Body.String())
	}
	after, err := os.ReadFile(mappingPath)
	if err != nil {
		t.Fatalf("read mapping after rejected read: %v", err)
	}
	if !bytes.Equal(after, corrupt) {
		t.Fatalf("corrupt mapping was modified:\n%s", after)
	}
}
