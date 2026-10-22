package settings

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

var validPNG = []byte{137, 80, 78, 71, 13, 10, 26, 10, 0, 0, 0, 13, 73, 72, 68, 82, 0, 0, 0, 1, 0, 0, 0, 1, 8, 6, 0, 0, 0, 31, 21, 196, 137, 0, 0, 0, 13, 73, 68, 65, 84, 8, 215, 99, 248, 207, 192, 240, 31, 0, 5, 0, 1, 255, 137, 153, 61, 29, 0, 0, 0, 0, 73, 69, 78, 68, 174, 66, 96, 130}

func TestImageLogosReadsLegacyFileWithoutExecutingJavaScript(t *testing.T) {
	root := t.TempDir()
	mappingPath := filepath.Join(root, "imageLogos.js")
	legacy := []byte("// Existing user configuration format retained across upgrades.\n" +
		"export const customImageLogos = {\n" +
		"  'nginx': '/src/config/image/nginx-custom.png', // inline comment\n" +
		"  \"registry.example:5000/team/app\": \"/src/config/image/team-app.webp\",\n" +
		"};\n")
	if err := os.WriteFile(mappingPath, legacy, 0o600); err != nil {
		t.Fatalf("seed legacy mapping: %v", err)
	}
	service, err := NewIconService(IconOptions{ImageDir: filepath.Join(root, "image"), MappingPath: mappingPath})
	if err != nil {
		t.Fatalf("construct service: %v", err)
	}
	logos, exists, err := service.ImageLogos()
	if err != nil {
		t.Fatalf("read legacy mapping: %v", err)
	}
	if !exists {
		t.Fatal("expected legacy mapping to exist")
	}
	if len(logos) != 2 || logos["nginx"] != "/src/config/image/nginx-custom.png" || logos["registry.example:5000/team/app"] != "/src/config/image/team-app.webp" {
		t.Fatalf("unexpected legacy mapping: %#v", logos)
	}

	missingService, err := NewIconService(IconOptions{ImageDir: filepath.Join(root, "missing-image"), MappingPath: filepath.Join(root, "missing.js")})
	if err != nil {
		t.Fatalf("construct service for missing mapping: %v", err)
	}
	missing, exists, err := missingService.ImageLogos()
	if err != nil {
		t.Fatalf("read missing mapping: %v", err)
	}
	if exists || len(missing) != 0 {
		t.Fatalf("missing mapping must be empty and absent, got exists=%v data=%#v", exists, missing)
	}
}

func TestImageLogosUnicodeEscapesRoundTripWithoutCorruption(t *testing.T) {
	legacy := []byte(`export const customImageLogos = {
  "caf\xE9": "/src/config/image/caf\u00E9.png",
  "emoji-\uD83D\uDE00": "/src/config/image/emoji-\u{1F600}.png",
};`)
	document, err := parseImageLogos(legacy)
	if err != nil {
		t.Fatalf("parse unicode escapes: %v", err)
	}
	logos := make(map[string]string, len(document.entries))
	for _, entry := range document.entries {
		logos[entry.Key] = entry.Value
	}
	if logos["café"] != "/src/config/image/café.png" || logos["emoji-😀"] != "/src/config/image/emoji-😀.png" {
		t.Fatalf("unicode escapes decoded incorrectly: %#v", logos)
	}
	encoded := document.render()
	if !utf8.Valid(encoded) {
		t.Fatalf("rendered mapping is invalid UTF-8: %q", encoded)
	}
	roundTrip, err := parseImageLogos(encoded)
	if err != nil {
		t.Fatalf("parse rendered unicode mapping: %v", err)
	}
	roundTripLogos := make(map[string]string, len(roundTrip.entries))
	for _, entry := range roundTrip.entries {
		roundTripLogos[entry.Key] = entry.Value
	}
	if !reflect.DeepEqual(roundTripLogos, logos) {
		t.Fatalf("unicode mappings changed after render: got %#v want %#v", roundTripLogos, logos)
	}
}

func TestImageLogosRenderPreservesLegacyPrefixAndSuffix(t *testing.T) {
	prefix := "// retained banner\nimport logo from './logo.png';\nexport const customImageLogos = "
	suffix := ";\n\n// retained helper code\nexport const getLegacyLogo = (name) => name;\n"
	legacy := []byte(prefix + "{\"nginx\": \"/src/config/image/nginx.png\"}" + suffix)
	document, err := parseImageLogos(legacy)
	if err != nil {
		t.Fatalf("parse legacy mapping with surrounding module content: %v", err)
	}
	document.set("new/service", "/src/config/image/new.png")
	rendered := document.render()
	if !bytes.HasPrefix(rendered, []byte(prefix)) || !bytes.HasSuffix(rendered, []byte(suffix)) {
		t.Fatalf("mapping render changed surrounding legacy module content:\n%s", rendered)
	}
}

func TestUploadIconRejectsUnsupportedMappingsWithoutRewriting(t *testing.T) {
	invalidUTF8 := append([]byte(`export const customImageLogos = {"broken":"`), 0xE9)
	invalidUTF8 = append(invalidUTF8, []byte(`"};`)...)
	tests := []struct {
		name    string
		mapping []byte
	}{
		{name: "unpaired high surrogate", mapping: []byte(`export const customImageLogos = {"broken":"\uD83D"};`)},
		{name: "unpaired low surrogate", mapping: []byte(`export const customImageLogos = {"broken":"\uDE00"};`)},
		{name: "braced surrogate scalar", mapping: []byte(`export const customImageLogos = {"broken":"\u{D83D}"};`)},
		{name: "duplicate exports", mapping: []byte(`export const customImageLogos = {"first":"/first.png"}; export const customImageLogos = {"second":"/second.png"};`)},
		{name: "invalid raw UTF-8", mapping: invalidUTF8},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			imageDir := filepath.Join(root, "image")
			mappingPath := filepath.Join(root, "imageLogos.js")
			if err := os.WriteFile(mappingPath, test.mapping, 0o600); err != nil {
				t.Fatalf("seed mapping: %v", err)
			}
			service, err := NewIconService(IconOptions{ImageDir: imageDir, MappingPath: mappingPath})
			if err != nil {
				t.Fatalf("construct service: %v", err)
			}
			_, err = service.UploadIcon(context.Background(), UploadIconRequest{
				ImageName: "new/icon",
				Filename:  "logo.png",
				Content:   bytes.NewReader(validPNG),
			})
			if !errors.Is(err, ErrImageLogosInvalid) {
				t.Fatalf("expected invalid mapping error, got %v", err)
			}
			after, err := os.ReadFile(mappingPath)
			if err != nil {
				t.Fatalf("read mapping after rejected upload: %v", err)
			}
			if !bytes.Equal(after, test.mapping) {
				t.Fatalf("unsupported mapping was rewritten:\n%s", after)
			}
			assertNoIconFiles(t, imageDir)
		})
	}
}

func TestUploadIconPreservesLegacyMappingsAndWritesStaticURL(t *testing.T) {
	root := t.TempDir()
	imageDir := filepath.Join(root, "image")
	mappingPath := filepath.Join(root, "imageLogos.js")
	if err := os.MkdirAll(imageDir, 0o755); err != nil {
		t.Fatalf("create image directory: %v", err)
	}
	legacy := []byte("// Existing user configuration format retained across upgrades.\n" +
		"export const customImageLogos = {\n" +
		"  \"nginx\": \"/src/config/image/nginx-custom.png\",\n" +
		"  \"registry.example:5000/team/app\": \"/src/config/image/team-app.webp\"\n" +
		"};\n")
	if err := os.WriteFile(mappingPath, legacy, 0o600); err != nil {
		t.Fatalf("seed legacy mapping: %v", err)
	}
	service, err := NewIconService(IconOptions{ImageDir: imageDir, MappingPath: mappingPath})
	if err != nil {
		t.Fatalf("construct service: %v", err)
	}
	filename, err := service.UploadIcon(context.Background(), UploadIconRequest{
		ImageName: "custom/service",
		Filename:  "new-logo.png",
		Content:   bytes.NewReader(validPNG),
	})
	if err != nil {
		t.Fatalf("upload icon: %v", err)
	}
	if !strings.HasSuffix(filename, ".png") {
		t.Fatalf("expected generated png filename, got %q", filename)
	}
	if _, err := os.Stat(filepath.Join(imageDir, filename)); err != nil {
		t.Fatalf("uploaded icon missing: %v", err)
	}
	logos, exists, err := service.ImageLogos()
	if err != nil {
		t.Fatalf("read mapping after upload: %v", err)
	}
	if !exists {
		t.Fatal("mapping should exist after upload")
	}
	if logos["nginx"] != "/src/config/image/nginx-custom.png" || logos["registry.example:5000/team/app"] != "/src/config/image/team-app.webp" {
		t.Fatalf("legacy mappings were not preserved: %#v", logos)
	}
	expectedURL := StaticImageURLPrefix + filename
	if logos["custom/service"] != expectedURL {
		t.Fatalf("new mapping = %q, want %q", logos["custom/service"], expectedURL)
	}
	raw, err := os.ReadFile(mappingPath)
	if err != nil {
		t.Fatalf("read raw mapping: %v", err)
	}
	for _, fragment := range []string{"export const customImageLogos", `"nginx": "/src/config/image/nginx-custom.png"`, `"custom/service": "` + expectedURL + `"`} {
		if !bytes.Contains(raw, []byte(fragment)) {
			t.Fatalf("mapping does not contain %q:\n%s", fragment, raw)
		}
	}
}

func TestUploadIconSupportsDeclaredImageTypes(t *testing.T) {
	root := t.TempDir()
	imageDir := filepath.Join(root, "image")
	mappingPath := filepath.Join(root, "imageLogos.js")
	if err := os.MkdirAll(imageDir, 0o755); err != nil {
		t.Fatalf("create image directory: %v", err)
	}
	if err := os.WriteFile(mappingPath, []byte("export const customImageLogos = {\n};\n"), 0o600); err != nil {
		t.Fatalf("seed mapping: %v", err)
	}
	service, err := NewIconService(IconOptions{ImageDir: imageDir, MappingPath: mappingPath})
	if err != nil {
		t.Fatalf("construct service: %v", err)
	}

	webp := append([]byte("RIFF"), []byte{0x24, 0, 0, 0}...)
	webp = append(webp, []byte("WEBPVP8 ")...)
	tests := []struct {
		filename string
		content  []byte
	}{
		{filename: "logo.png", content: validPNG},
		{filename: "logo.jpg", content: []byte{0xff, 0xd8, 0xff, 0xe0, 0, 16, 'J', 'F', 'I', 'F', 0, 1}},
		{filename: "logo.jpeg", content: []byte{0xff, 0xd8, 0xff, 0xe0, 0, 16, 'J', 'F', 'I', 'F', 0, 1}},
		{filename: "logo.gif", content: []byte("GIF89a\x01\x00\x01\x00\x00\x00\x00;")},
		{filename: "logo.webp", content: webp},
	}
	for index, test := range tests {
		imageName := "supported/image-" + string(rune('a'+index))
		filename, err := service.UploadIcon(context.Background(), UploadIconRequest{
			ImageName: imageName,
			Filename:  test.filename,
			Content:   bytes.NewReader(test.content),
		})
		if err != nil {
			t.Fatalf("upload %s: %v", test.filename, err)
		}
		if got := filepath.Ext(filename); got != filepath.Ext(test.filename) {
			t.Fatalf("upload %s generated extension %q", test.filename, got)
		}
		if _, err := os.Stat(filepath.Join(imageDir, filename)); err != nil {
			t.Fatalf("uploaded %s missing: %v", test.filename, err)
		}
	}
	logos, _, err := service.ImageLogos()
	if err != nil {
		t.Fatalf("read mapping: %v", err)
	}
	if len(logos) != len(tests) {
		t.Fatalf("expected %d mappings, got %#v", len(tests), logos)
	}
}

func TestConcurrentUploadIconDifferentKeysKeepAllMappings(t *testing.T) {
	root := t.TempDir()
	imageDir := filepath.Join(root, "image")
	mappingPath := filepath.Join(root, "imageLogos.js")
	if err := os.MkdirAll(imageDir, 0o755); err != nil {
		t.Fatalf("create image directory: %v", err)
	}
	if err := os.WriteFile(mappingPath, []byte("export const customImageLogos = {\n};\n"), 0o600); err != nil {
		t.Fatalf("seed mapping: %v", err)
	}

	const uploads = 12
	start := make(chan struct{})
	errs := make(chan error, uploads)
	var wait sync.WaitGroup
	for index := 0; index < uploads; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			// Each request builds its own service, matching the thin handler.
			service, err := NewIconService(IconOptions{ImageDir: imageDir, MappingPath: mappingPath})
			if err != nil {
				errs <- err
				return
			}
			_, err = service.UploadIcon(context.Background(), UploadIconRequest{
				ImageName: "registry.example:5000/team/app-" + string(rune('a'+index)),
				Filename:  "logo.png",
				Content:   bytes.NewReader(validPNG),
			})
			errs <- err
		}(index)
	}
	close(start)
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent upload failed: %v", err)
		}
	}

	service, err := NewIconService(IconOptions{ImageDir: imageDir, MappingPath: mappingPath})
	if err != nil {
		t.Fatalf("construct verification service: %v", err)
	}
	logos, _, err := service.ImageLogos()
	if err != nil {
		t.Fatalf("read mapping after concurrent uploads: %v", err)
	}
	if len(logos) != uploads {
		t.Fatalf("lost concurrent mappings: got %d, want %d (%#v)", len(logos), uploads, logos)
	}
	for index := 0; index < uploads; index++ {
		key := "registry.example:5000/team/app-" + string(rune('a'+index))
		value, ok := logos[key]
		if !ok {
			t.Fatalf("missing concurrent mapping key %q", key)
		}
		filename := strings.TrimPrefix(value, StaticImageURLPrefix)
		if filename == value {
			t.Fatalf("mapping %q does not use the static icon URL: %q", key, value)
		}
		if _, err := os.Stat(filepath.Join(imageDir, filename)); err != nil {
			t.Fatalf("icon for %q missing: %v", key, err)
		}
	}
}

func TestUploadIconRejectsCorruptMappingWithoutResettingIt(t *testing.T) {
	root := t.TempDir()
	imageDir := filepath.Join(root, "image")
	mappingPath := filepath.Join(root, "imageLogos.js")
	corrupt := []byte("// comment pretending to be a declaration: export const customImageLogos = { \"fake\": \"/src/config/image/fake.png\" };\n" +
		"export const customImageLogos = {\n" +
		"  \"broken\": process.env.HOME\n" +
		"};\n")
	if err := os.WriteFile(mappingPath, corrupt, 0o600); err != nil {
		t.Fatalf("seed corrupt mapping: %v", err)
	}
	service, err := NewIconService(IconOptions{ImageDir: imageDir, MappingPath: mappingPath})
	if err != nil {
		t.Fatalf("construct service: %v", err)
	}
	_, err = service.UploadIcon(context.Background(), UploadIconRequest{
		ImageName: "valid/key",
		Filename:  "logo.png",
		Content:   bytes.NewReader(validPNG),
	})
	if !errors.Is(err, ErrImageLogosInvalid) {
		t.Fatalf("expected ErrImageLogosInvalid, got %v", err)
	}
	after, err := os.ReadFile(mappingPath)
	if err != nil {
		t.Fatalf("read mapping after rejected upload: %v", err)
	}
	if !bytes.Equal(after, corrupt) {
		t.Fatalf("corrupt mapping was modified:\n%s", after)
	}
	assertNoIconFiles(t, imageDir)
	_, exists, err := service.ImageLogos()
	if !errors.Is(err, ErrImageLogosInvalid) || !exists {
		t.Fatalf("image logos must report corrupt existing file, exists=%v err=%v", exists, err)
	}
}

func TestUploadIconMappingWriteFailureKeepsOldIconAndMapping(t *testing.T) {
	root := t.TempDir()
	imageDir := filepath.Join(root, "image")
	mappingPath := filepath.Join(root, "imageLogos.js")
	if err := os.MkdirAll(imageDir, 0o755); err != nil {
		t.Fatalf("create image directory: %v", err)
	}
	oldIcon := filepath.Join(imageDir, "old-icon.png")
	if err := os.WriteFile(oldIcon, validPNG, 0o644); err != nil {
		t.Fatalf("write old icon: %v", err)
	}
	oldMapping := []byte("export const customImageLogos = {\n  \"legacy\": \"/src/config/image/old-icon.png\"\n};\n")
	if err := os.WriteFile(mappingPath, oldMapping, 0o600); err != nil {
		t.Fatalf("seed old mapping: %v", err)
	}
	service, err := NewIconService(IconOptions{ImageDir: imageDir, MappingPath: mappingPath})
	if err != nil {
		t.Fatalf("construct service: %v", err)
	}
	service.writeMapping = func(string, []byte) error {
		return errors.New("injected mapping write failure")
	}
	filename, err := service.UploadIcon(context.Background(), UploadIconRequest{
		ImageName: "legacy",
		Filename:  "new-icon.png",
		Content:   bytes.NewReader(validPNG),
	})
	if filename != "" {
		t.Fatalf("failed upload returned filename %q", filename)
	}
	if !errors.Is(err, ErrImageLogosWrite) {
		t.Fatalf("expected ErrImageLogosWrite, got %v", err)
	}
	afterMapping, err := os.ReadFile(mappingPath)
	if err != nil {
		t.Fatalf("read old mapping: %v", err)
	}
	if !bytes.Equal(afterMapping, oldMapping) {
		t.Fatalf("old mapping changed after write failure:\n%s", afterMapping)
	}
	if _, err := os.Stat(oldIcon); err != nil {
		t.Fatalf("old icon must remain available: %v", err)
	}
	entries, err := os.ReadDir(imageDir)
	if err != nil {
		t.Fatalf("list image directory: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "old-icon.png" {
		t.Fatalf("uncommitted icon was not cleaned up: %#v", entries)
	}
	logos, _, err := service.ImageLogos()
	if err != nil {
		t.Fatalf("read mapping after failure: %v", err)
	}
	if logos["legacy"] != "/src/config/image/old-icon.png" {
		t.Fatalf("old mapping no longer usable: %#v", logos)
	}
}

func TestUploadIconRejectsInvalidInputs(t *testing.T) {
	root := t.TempDir()
	imageDir := filepath.Join(root, "image")
	mappingPath := filepath.Join(root, "imageLogos.js")
	service, err := NewIconService(IconOptions{
		ImageDir:       imageDir,
		MappingPath:    mappingPath,
		MaxUploadBytes: 128,
	})
	if err != nil {
		t.Fatalf("construct service: %v", err)
	}
	oversize := append(append([]byte(nil), validPNG...), bytes.Repeat([]byte{0}, 64)...)
	tests := []struct {
		name     string
		image    string
		filename string
		content  []byte
		want     error
	}{
		{name: "missing key", image: "", filename: "logo.png", content: validPNG, want: ErrImageNameRequired},
		{name: "parent traversal", image: "../etc/passwd", filename: "logo.png", content: validPNG, want: ErrInvalidImageName},
		{name: "nested traversal", image: "registry.example/../../secret", filename: "logo.png", content: validPNG, want: ErrInvalidImageName},
		{name: "absolute path", image: "/etc/passwd", filename: "logo.png", content: validPNG, want: ErrInvalidImageName},
		{name: "backslash path", image: `team\app`, filename: "logo.png", content: validPNG, want: ErrInvalidImageName},
		{name: "empty segment", image: "team//app", filename: "logo.png", content: validPNG, want: ErrInvalidImageName},
		{name: "oversized key", image: strings.Repeat("a", MaxImageNameBytes+1), filename: "logo.png", content: validPNG, want: ErrInvalidImageName},
		{name: "unsupported extension", image: "valid/key", filename: "logo.svg", content: validPNG, want: ErrUnsupportedIconType},
		{name: "spoofed extension", image: "valid/key", filename: "logo.png", content: []byte(`{"not":"an image"}`), want: ErrIconTypeMismatch},
		{name: "empty content", image: "valid/key", filename: "logo.png", content: nil, want: ErrIconEmpty},
		{name: "oversize content", image: "valid/key", filename: "logo.png", content: oversize, want: ErrIconTooLarge},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := service.UploadIcon(context.Background(), UploadIconRequest{
				ImageName: test.image,
				Filename:  test.filename,
				Content:   bytes.NewReader(test.content),
			})
			if !errors.Is(err, test.want) {
				t.Fatalf("upload error = %v, want %v", err, test.want)
			}
			assertNoIconFiles(t, imageDir)
		})
	}
}

func TestMappingAtomicWriteKeepsDestinationUntilPublish(t *testing.T) {
	path := filepath.Join(t.TempDir(), "imageLogos.js")
	oldContent := []byte("export const customImageLogos = {\n  \"old\": \"/src/config/image/old.png\"\n};\n")
	newContent := []byte("export const customImageLogos = {\n  \"new\": \"/src/config/image/new.png\"\n};\n")
	if err := os.WriteFile(path, oldContent, 0o600); err != nil {
		t.Fatalf("seed mapping: %v", err)
	}
	renameCalled := false
	err := writeFileAtomicWithRename(path, newContent, 0o644, func(temporaryPath, targetPath string) error {
		renameCalled = true
		destination, err := os.ReadFile(targetPath)
		if err != nil {
			t.Fatalf("read destination before rename: %v", err)
		}
		if !bytes.Equal(destination, oldContent) {
			t.Fatalf("reader saw partially written destination before rename:\n%s", destination)
		}
		temporary, err := os.ReadFile(temporaryPath)
		if err != nil {
			t.Fatalf("read temporary mapping: %v", err)
		}
		if !bytes.Equal(temporary, newContent) {
			t.Fatalf("temporary mapping is incomplete:\n%s", temporary)
		}
		return os.Rename(temporaryPath, targetPath)
	})
	if err != nil {
		t.Fatalf("atomic mapping write: %v", err)
	}
	if !renameCalled {
		t.Fatal("expected mapping to be published through rename")
	}
	published, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read published mapping: %v", err)
	}
	if !bytes.Equal(published, newContent) {
		t.Fatalf("published mapping = %s, want %s", published, newContent)
	}
}

func TestMappingAtomicWriteFailureLeavesDestinationIntact(t *testing.T) {
	path := filepath.Join(t.TempDir(), "imageLogos.js")
	oldContent := []byte("export const customImageLogos = {\n  \"old\": \"/src/config/image/old.png\"\n};\n")
	newContent := []byte("export const customImageLogos = {\n  \"new\": \"/src/config/image/new.png\"\n};\n")
	if err := os.WriteFile(path, oldContent, 0o600); err != nil {
		t.Fatalf("seed mapping: %v", err)
	}
	err := writeFileAtomicWithRename(path, newContent, 0o644, func(string, string) error {
		return errors.New("injected rename failure")
	})
	if err == nil {
		t.Fatal("expected rename failure")
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read destination after failure: %v", readErr)
	}
	if !bytes.Equal(after, oldContent) {
		t.Fatalf("destination changed after failed rename:\n%s", after)
	}
	entries, readErr := os.ReadDir(filepath.Dir(path))
	if readErr != nil {
		t.Fatalf("list mapping directory: %v", readErr)
	}
	if len(entries) != 1 || entries[0].Name() != "imageLogos.js" {
		t.Fatalf("temporary mapping was not cleaned up: %#v", entries)
	}
}

func assertNoIconFiles(t *testing.T, directory string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		t.Fatalf("list image directory %s: %v", directory, err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected no published icon files, found %#v", entries)
	}
}
