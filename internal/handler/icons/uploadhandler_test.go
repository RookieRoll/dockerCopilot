package icons

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/onlyLTY/dockerCopilot/internal/svc"
)

func TestUploadPreservesLegacyImageLogosAndResponseFields(t *testing.T) {
	tempDir := t.TempDir()
	jsPath := filepath.Join(tempDir, "imageLogos.js")
	legacy, err := os.ReadFile(filepath.Join("testdata", "legacy-imageLogos.js"))
	if err != nil {
		t.Fatalf("read legacy icon fixture: %v", err)
	}
	if err := os.WriteFile(jsPath, legacy, 0o600); err != nil {
		t.Fatalf("seed legacy icon fixture: %v", err)
	}
	originalImageUploadDir, originalImageLogosPath := imageUploadDir, imageLogosPath
	imageUploadDir, imageLogosPath = filepath.Join(tempDir, "images"), jsPath
	t.Cleanup(func() { imageUploadDir, imageLogosPath = originalImageUploadDir, originalImageLogosPath })

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	fileWriter, err := writer.CreateFormFile("file", "new.png")
	if err != nil {
		t.Fatalf("create upload part: %v", err)
	}
	// A valid 1x1 PNG lets this compatibility test exercise the success path.
	png := []byte{137, 80, 78, 71, 13, 10, 26, 10, 0, 0, 0, 13, 73, 72, 68, 82, 0, 0, 0, 1, 0, 0, 0, 1, 8, 6, 0, 0, 0, 31, 21, 196, 137, 0, 0, 0, 13, 73, 68, 65, 84, 8, 215, 99, 248, 207, 192, 240, 31, 0, 5, 0, 1, 255, 137, 153, 61, 29, 0, 0, 0, 0, 73, 69, 78, 68, 174, 66, 96, 130}
	if _, err := fileWriter.Write(png); err != nil {
		t.Fatalf("write png: %v", err)
	}
	if err := writer.WriteField("imageName", "custom/service"); err != nil {
		t.Fatalf("write imageName: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/icons", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rec := httptest.NewRecorder()
	UploadHandler(&svc.ServiceContext{})(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var response struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data string `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode upload response: %v", err)
	}
	if response.Code != 200 || response.Msg != "Success" || response.Data == "" {
		t.Fatalf("unexpected upload response: %s", rec.Body.String())
	}
	updated, err := os.ReadFile(jsPath)
	if err != nil {
		t.Fatalf("read updated icon map: %v", err)
	}
	for _, preserved := range []string{`"nginx": "/src/config/image/nginx-custom.png"`, `"registry.example:5000/team/app": "/src/config/image/team-app.webp"`, `"custom/service": "/src/config/image/` + response.Data + `"`} {
		if !bytes.Contains(updated, []byte(preserved)) {
			t.Fatalf("legacy/custom mapping missing %q in %s", preserved, updated)
		}
	}
	if _, err := os.Stat(filepath.Join(imageUploadDir, response.Data)); err != nil {
		t.Fatalf("uploaded image missing: %v", err)
	}
}

func TestUploadHandlerRejectsNonImageFiles(t *testing.T) {
	tempDir := t.TempDir()
	jsPath := filepath.Join(tempDir, "imageLogos.js")
	imageDir := filepath.Join(tempDir, "image")
	testFilename := "codex-upload-vuln.json"

	originalImageUploadDir := imageUploadDir
	originalImageLogosPath := imageLogosPath
	imageUploadDir = imageDir
	imageLogosPath = jsPath
	t.Cleanup(func() {
		imageUploadDir = originalImageUploadDir
		imageLogosPath = originalImageLogosPath
	})

	originalContent, readErr := os.ReadFile(jsPath)
	hadOriginal := readErr == nil
	if err := os.WriteFile(jsPath, []byte("// test\nexport const customImageLogos = {\n};\n"), 0o644); err != nil {
		t.Fatalf("failed to seed imageLogos.js: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Remove(filepath.Join(imageDir, testFilename))
		if hadOriginal {
			_ = os.WriteFile(jsPath, originalContent, 0o644)
		} else {
			_ = os.Remove(jsPath)
		}
	})

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	fileWriter, err := writer.CreateFormFile("file", testFilename)
	if err != nil {
		t.Fatalf("failed to create form file: %v", err)
	}
	if _, err := io.WriteString(fileWriter, `{"not":"an image"}`); err != nil {
		t.Fatalf("failed to write form file: %v", err)
	}
	if err := writer.WriteField("imageName", "nginx"); err != nil {
		t.Fatalf("failed to write imageName: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("failed to close multipart writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/icons", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rec := httptest.NewRecorder()

	UploadHandler(&svc.ServiceContext{})(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for non-image upload, got %d with body %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(imageDir, testFilename)); !os.IsNotExist(err) {
		t.Fatalf("expected non-image upload to be rejected without writing file, stat err=%v", err)
	}
}
