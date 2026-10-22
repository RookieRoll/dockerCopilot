package settings

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"

	"github.com/google/uuid"
)

const (
	DefaultImageDir         = "/data/config/image"
	DefaultImageLogosPath   = "/data/config/imageLogos.js"
	StaticImageURLPrefix    = "/src/config/image/"
	DefaultMaxIconUpload    = int64(10 << 20)
	DefaultIconRequestSlack = int64(1 << 20)
	MaxImageNameBytes       = 512
)

var (
	// ErrImageNameRequired and the errors below are stable input-validation
	// results for HTTP callers. Other errors describe local storage failures.
	ErrImageNameRequired   = errors.New("imageName is required")
	ErrInvalidImageName    = errors.New("invalid imageName")
	ErrUnsupportedIconType = errors.New("only png, jpg, jpeg, webp and gif files are allowed")
	ErrIconTypeMismatch    = errors.New("uploaded file content does not match its extension")
	ErrIconTooLarge        = errors.New("uploaded file exceeds the maximum allowed size")
	ErrIconEmpty           = errors.New("uploaded file is empty")
	ErrImageLogosWrite     = errors.New("failed to save image icon mapping")

	imageNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:-]*$`)
	iconFileTypes    = map[string]string{
		".png":  "image/png",
		".jpg":  "image/jpeg",
		".jpeg": "image/jpeg",
		".webp": "image/webp",
		".gif":  "image/gif",
	}
	sharedIconLocks sync.Map
)

// IconOptions are the narrow dependencies needed by icon storage. The handler
// supplies the existing package paths; tests and later composition roots can
// supply temporary or configured paths without constructing ServiceContext.
type IconOptions struct {
	ImageDir        string
	MappingPath     string
	MaxUploadBytes  int64
	MaxRequestBytes int64
}

type IconService struct {
	imageDir        string
	mappingPath     string
	maxUploadBytes  int64
	maxRequestBytes int64
	lock            *sync.Mutex
	writeMapping    func(string, []byte) error
}

type UploadIconRequest struct {
	ImageName string
	Filename  string
	Content   io.Reader
}

func NewIconService(options IconOptions) (*IconService, error) {
	imageDir := strings.TrimSpace(options.ImageDir)
	if imageDir == "" {
		return nil, errors.New("icon image directory is required")
	}
	mappingPath := strings.TrimSpace(options.MappingPath)
	if mappingPath == "" {
		return nil, errors.New("icon mapping path is required")
	}
	maxUploadBytes := options.MaxUploadBytes
	if maxUploadBytes <= 0 {
		maxUploadBytes = DefaultMaxIconUpload
	}
	maxRequestBytes := options.MaxRequestBytes
	if maxRequestBytes <= 0 {
		maxRequestBytes = maxUploadBytes + DefaultIconRequestSlack
	}
	if maxRequestBytes <= maxUploadBytes {
		maxRequestBytes = maxUploadBytes + 1
	}

	service := &IconService{
		imageDir:        filepath.Clean(imageDir),
		mappingPath:     filepath.Clean(mappingPath),
		maxUploadBytes:  maxUploadBytes,
		maxRequestBytes: maxRequestBytes,
		lock:            sharedIconLock(mappingPath),
	}
	service.writeMapping = service.writeMappingFile
	return service, nil
}

func (s *IconService) MaxUploadBytes() int64 {
	return s.maxUploadBytes
}

func (s *IconService) MaxRequestBytes() int64 {
	return s.maxRequestBytes
}

// UploadIcon validates and stores one icon, then updates the legacy
// imageLogos.js mapping. The new icon file is published before the mapping is
// atomically replaced. If the mapping cannot be saved, the new file is
// removed and the old mapping and icon files remain usable.
func (s *IconService) UploadIcon(ctx context.Context, request UploadIconRequest) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	imageName, err := validateImageName(request.ImageName)
	if err != nil {
		return "", err
	}
	extension, expectedType, err := iconExtension(request.Filename)
	if err != nil {
		return "", err
	}
	if request.Content == nil {
		return "", ErrIconEmpty
	}

	s.lock.Lock()
	defer s.lock.Unlock()

	document, _, err := readImageLogosDocument(s.mappingPath)
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}

	header := make([]byte, 512)
	headerBytes, readErr := io.ReadFull(request.Content, header)
	if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
		return "", fmt.Errorf("read uploaded icon: %w", readErr)
	}
	if headerBytes == 0 {
		return "", ErrIconEmpty
	}
	if int64(headerBytes) > s.maxUploadBytes {
		return "", ErrIconTooLarge
	}
	detectedType := http.DetectContentType(header[:headerBytes])
	if detectedType != expectedType {
		return "", ErrIconTypeMismatch
	}

	if err := os.MkdirAll(s.imageDir, 0o755); err != nil {
		return "", fmt.Errorf("prepare icon directory: %w", err)
	}
	temporaryFile, err := os.CreateTemp(s.imageDir, ".icon-upload-*.tmp")
	if err != nil {
		return "", fmt.Errorf("create icon file: %w", err)
	}
	temporaryPath := temporaryFile.Name()
	defer os.Remove(temporaryPath)
	closeTemporary := func() {
		_ = temporaryFile.Close()
	}

	if err := temporaryFile.Chmod(0o644); err != nil {
		closeTemporary()
		return "", fmt.Errorf("set icon file permissions: %w", err)
	}
	if _, err := temporaryFile.Write(header[:headerBytes]); err != nil {
		closeTemporary()
		return "", fmt.Errorf("write icon file: %w", err)
	}
	remainingLimit := s.maxUploadBytes - int64(headerBytes) + 1
	copied, err := io.Copy(temporaryFile, io.LimitReader(request.Content, remainingLimit))
	if err != nil {
		closeTemporary()
		return "", fmt.Errorf("write icon file: %w", err)
	}
	if int64(headerBytes)+copied > s.maxUploadBytes {
		closeTemporary()
		return "", ErrIconTooLarge
	}
	if err := temporaryFile.Sync(); err != nil {
		closeTemporary()
		return "", fmt.Errorf("sync icon file: %w", err)
	}
	if err := temporaryFile.Close(); err != nil {
		return "", fmt.Errorf("close icon file: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}

	filename := uuid.NewString() + extension
	finalPath := filepath.Join(s.imageDir, filename)
	if err := os.Rename(temporaryPath, finalPath); err != nil {
		return "", fmt.Errorf("publish icon file: %w", err)
	}

	document.set(imageName, StaticImageURLPrefix+filename)
	if err := s.writeMapping(s.mappingPath, document.render()); err != nil {
		cleanupErr := os.Remove(finalPath)
		if cleanupErr != nil && !errors.Is(cleanupErr, fs.ErrNotExist) {
			return "", fmt.Errorf("%w: %v; additionally failed to remove uncommitted icon %s: %v", ErrImageLogosWrite, err, filename, cleanupErr)
		}
		return "", fmt.Errorf("%w: %v", ErrImageLogosWrite, err)
	}
	return filename, nil
}

// ImageLogos returns the current safe mapping and whether the legacy file
// exists. A malformed file is reported as ErrImageLogosInvalid and is never
// treated as an empty mapping.
func (s *IconService) ImageLogos() (map[string]string, bool, error) {
	s.lock.Lock()
	defer s.lock.Unlock()

	document, exists, err := readImageLogosDocument(s.mappingPath)
	if err != nil {
		return nil, exists, err
	}
	logos := make(map[string]string, len(document.entries))
	for _, entry := range document.entries {
		logos[entry.Key] = entry.Value
	}
	return logos, exists, nil
}

func (s *IconService) writeMappingFile(path string, content []byte) error {
	return writeFileAtomic(path, content, 0o644)
}

// IsIconValidationError reports errors caused by request input rather than
// server-side storage failures.
func IsIconValidationError(err error) bool {
	return errors.Is(err, ErrImageNameRequired) ||
		errors.Is(err, ErrInvalidImageName) ||
		errors.Is(err, ErrUnsupportedIconType) ||
		errors.Is(err, ErrIconTypeMismatch) ||
		errors.Is(err, ErrIconTooLarge) ||
		errors.Is(err, ErrIconEmpty)
}

func validateImageName(imageName string) (string, error) {
	if imageName == "" {
		return "", ErrImageNameRequired
	}
	if len(imageName) > MaxImageNameBytes || !imageNamePattern.MatchString(imageName) {
		return "", ErrInvalidImageName
	}
	for _, segment := range strings.Split(imageName, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return "", ErrInvalidImageName
		}
	}
	return imageName, nil
}

func iconExtension(filename string) (string, string, error) {
	base := filename
	if index := strings.LastIndexAny(base, `/\`); index >= 0 {
		base = base[index+1:]
	}
	extension := strings.ToLower(filepath.Ext(base))
	expectedType, ok := iconFileTypes[extension]
	if !ok {
		return "", "", ErrUnsupportedIconType
	}
	return extension, expectedType, nil
}

func sharedIconLock(mappingPath string) *sync.Mutex {
	key := filepath.Clean(mappingPath)
	if absolute, err := filepath.Abs(key); err == nil {
		key = absolute
	}
	if runtime.GOOS == "windows" {
		key = strings.ToLower(key)
	}
	lock, _ := sharedIconLocks.LoadOrStore(key, &sync.Mutex{})
	return lock.(*sync.Mutex)
}
