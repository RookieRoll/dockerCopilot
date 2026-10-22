package updater

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/onlyLTY/dockerCopilot/internal/config"
)

const (
	preparedBinaryName   = "dockerCopilot-new"
	defaultMaxArchive    = int64(128 << 20)
	defaultMaxBinary     = int64(256 << 20)
	maxChecksumFileBytes = int64(4 << 10)
	prepareHTTPTimeout   = 10 * time.Minute
)

// PrepareOptions controls isolated download and validation. StagingParent must
// be an explicit persistent directory supplied by the runtime composition.
// Each task ID maps to one exclusive, deterministic child directory; an
// existing child is never reused or removed by a failed attempt.
type PrepareOptions struct {
	Client         *http.Client
	StagingParent  string
	GOARCH         string
	MaxArchiveSize int64
	MaxBinarySize  int64
}

type PrepareIdentity struct {
	TaskID        string
	RequestID     string
	TargetVersion string
}

type PrepareProgress struct {
	Stage   string
	Percent int
	Message string
}

type PrepareProgressFunc func(PrepareProgress) error

// PreparedProgram is a verified artifact in a unique staging directory. It
// is not installed or handed off; that is a separate lifecycle operation.
type PreparedProgram struct {
	TaskID        string
	RequestID     string
	TargetVersion string
	GOARCH        string
	Directory     string
	ArchivePath   string
	BinaryPath    string
	ArchiveSHA256 string
	BinarySHA256  string
	ArchiveBytes  int64
	BinaryBytes   int64
	cleanupPath   string
}

func (p *PreparedProgram) Cleanup() error {
	if p == nil || p.cleanupPath == "" {
		return nil
	}
	return os.RemoveAll(p.cleanupPath)
}

// PrepareError classifies a preparation failure while preserving its cause.
// Callers can map Code and Stage into task failure facts without importing the
// task package here.
type PrepareError struct {
	Code  string
	Stage string
	Err   error
}

func (e *PrepareError) Error() string {
	if e == nil {
		return "program preparation failed"
	}
	return fmt.Sprintf("%s: %v", e.Stage, e.Err)
}

func (e *PrepareError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

type Preparer struct {
	client         *http.Client
	stagingParent  string
	goarch         string
	maxArchiveSize int64
	maxBinarySize  int64
}

func NewPreparer(opts PrepareOptions) *Preparer {
	if opts.Client == nil {
		opts.Client = &http.Client{Timeout: prepareHTTPTimeout}
	}
	if opts.GOARCH == "" {
		opts.GOARCH = runtime.GOARCH
	}
	if opts.MaxArchiveSize <= 0 {
		opts.MaxArchiveSize = defaultMaxArchive
	}
	if opts.MaxBinarySize <= 0 {
		opts.MaxBinarySize = defaultMaxBinary
	}
	return &Preparer{
		client: opts.Client, stagingParent: opts.StagingParent, goarch: opts.GOARCH,
		maxArchiveSize: opts.MaxArchiveSize, maxBinarySize: opts.MaxBinarySize,
	}
}

func (p *Preparer) Prepare(ctx context.Context, identity PrepareIdentity, source config.UpdateConfig, progress PrepareProgressFunc) (_ *PreparedProgram, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if p == nil || p.client == nil {
		return nil, prepareError("UPDATER_NOT_CONFIGURED", "validate request", errors.New("program preparer is not configured"))
	}
	if strings.TrimSpace(identity.TaskID) == "" {
		return nil, prepareError("INVALID_PREPARE_IDENTITY", "validate request", errors.New("task identity is required"))
	}
	if !versionPattern.MatchString(identity.TargetVersion) {
		return nil, prepareError("INVALID_TARGET_VERSION", "validate request", errors.New("target version has invalid format"))
	}
	if strings.TrimSpace(source.Repository) == "" {
		source.Repository = "RookieRoll/dockerCopilot"
	}
	if p.goarch != "amd64" && p.goarch != "arm64" {
		return nil, prepareError("UNSUPPORTED_ARCHITECTURE", "validate request", fmt.Errorf("unsupported architecture %q", p.goarch))
	}
	if strings.TrimSpace(p.stagingParent) == "" {
		return nil, prepareError("STAGING_UNAVAILABLE", "create staging directory", errors.New("an explicit persistent staging parent is required"))
	}
	if err := ctx.Err(); err != nil {
		return nil, prepareError("PREPARATION_CANCELED", "validate request", err)
	}
	if err := os.MkdirAll(p.stagingParent, 0o700); err != nil {
		return nil, prepareError("STAGING_UNAVAILABLE", "create staging directory", err)
	}
	parentInfo, err := os.Lstat(p.stagingParent)
	if err != nil {
		return nil, prepareError("STAGING_UNAVAILABLE", "inspect staging directory", err)
	}
	if !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 {
		return nil, prepareError("STAGING_UNAVAILABLE", "inspect staging directory", errors.New("staging parent must be a real directory"))
	}
	if err := os.Chmod(p.stagingParent, 0o700); err != nil {
		return nil, prepareError("STAGING_UNAVAILABLE", "secure staging directory", err)
	}
	stageDir := filepath.Join(p.stagingParent, stageDirectoryName(identity.TaskID))
	if err := os.Mkdir(stageDir, 0o700); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, prepareError("STAGING_IN_USE", "create staging directory", errors.New("staging already exists for this task ID"))
		}
		return nil, prepareError("STAGING_UNAVAILABLE", "create staging directory", err)
	}
	prepared := &PreparedProgram{
		TaskID: identity.TaskID, RequestID: identity.RequestID, TargetVersion: identity.TargetVersion,
		GOARCH: p.goarch, Directory: stageDir,
		ArchivePath: filepath.Join(stageDir, "dockerCopilot-"+p.goarch+".tar.gz"),
		BinaryPath:  filepath.Join(stageDir, preparedBinaryName), cleanupPath: stageDir,
	}
	succeeded := false
	defer func() {
		if !succeeded {
			_ = prepared.Cleanup()
		}
	}()

	if err := reportPrepareProgress(progress, PrepareProgress{Stage: "download-checksum", Percent: 10, Message: "正在获取升级校验文件"}); err != nil {
		return nil, prepareError("PROGRESS_REPORT_FAILED", "report progress", err)
	}
	baseURL := strings.TrimRight(source.ReleaseBaseURL(), "/") + "/" + identity.TargetVersion
	assetName := "dockerCopilot-" + p.goarch + ".tar.gz"
	checksum, err := p.downloadChecksum(ctx, baseURL+"/"+assetName+".sha256", assetName)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, prepareError("PREPARATION_CANCELED", "download checksum", err)
		}
		return nil, err
	}

	if err := reportPrepareProgress(progress, PrepareProgress{Stage: "download-archive", Percent: 30, Message: "正在下载升级包"}); err != nil {
		return nil, prepareError("PROGRESS_REPORT_FAILED", "report progress", err)
	}
	archiveHash := sha256.New()
	archiveSize, err := p.download(ctx, baseURL+"/"+assetName, prepared.ArchivePath, p.maxArchiveSize, archiveHash, "download upgrade archive")
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, prepareError("PREPARATION_CANCELED", "download upgrade archive", err)
		}
		return nil, err
	}
	archiveDigest := hex.EncodeToString(archiveHash.Sum(nil))
	if !strings.EqualFold(archiveDigest, checksum) {
		return nil, prepareError("CHECKSUM_MISMATCH", "verify archive checksum", fmt.Errorf("SHA256 mismatch for %s", assetName))
	}
	prepared.ArchiveSHA256 = archiveDigest
	prepared.ArchiveBytes = archiveSize

	if err := reportPrepareProgress(progress, PrepareProgress{Stage: "verify-archive", Percent: 65, Message: "正在验证升级包架构和布局"}); err != nil {
		return nil, prepareError("PROGRESS_REPORT_FAILED", "report progress", err)
	}
	binarySize, binaryDigest, err := extractPreparedBinary(ctx, prepared.ArchivePath, prepared.BinaryPath, p.goarch, p.maxBinarySize)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, prepareError("PREPARATION_CANCELED", "validate upgrade archive", err)
		}
		return nil, prepareError("INVALID_UPGRADE_ARCHIVE", "validate upgrade archive", err)
	}
	prepared.BinaryBytes = binarySize
	prepared.BinarySHA256 = binaryDigest
	if err := ctx.Err(); err != nil {
		return nil, prepareError("PREPARATION_CANCELED", "finish preparation", err)
	}
	if err := reportPrepareProgress(progress, PrepareProgress{Stage: "prepared", Percent: 100, Message: "升级包已验证并暂存"}); err != nil {
		return nil, prepareError("PROGRESS_REPORT_FAILED", "report progress", err)
	}
	succeeded = true
	return prepared, nil
}

func (p *Preparer) downloadChecksum(ctx context.Context, assetURL, assetName string) (string, error) {
	body, err := p.getSmallBody(ctx, assetURL, maxChecksumFileBytes, "download checksum")
	if err != nil {
		return "", err
	}
	fields := strings.Fields(string(body))
	if len(fields) == 0 || len(fields) > 2 || len(fields[0]) != sha256.Size*2 {
		return "", prepareError("CHECKSUM_UNAVAILABLE", "validate checksum", errors.New("release SHA256 file is missing or malformed"))
	}
	decoded, err := hex.DecodeString(fields[0])
	if err != nil || len(decoded) != sha256.Size {
		return "", prepareError("CHECKSUM_UNAVAILABLE", "validate checksum", errors.New("release SHA256 value is invalid"))
	}
	if len(fields) == 2 {
		fileName := strings.TrimPrefix(fields[1], "*")
		if fileName != assetName {
			return "", prepareError("CHECKSUM_UNAVAILABLE", "validate checksum", errors.New("release SHA256 file names a different asset"))
		}
	}
	return strings.ToLower(fields[0]), nil
}

func (p *Preparer) getSmallBody(ctx context.Context, assetURL string, maxBytes int64, stage string) ([]byte, error) {
	requestCtx, cancel := context.WithTimeout(ctx, prepareHTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, assetURL, nil)
	if err != nil {
		return nil, prepareError("INVALID_ASSET_URL", stage, err)
	}
	req.Header.Set("Accept", "application/octet-stream")
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, prepareError("ASSET_REQUEST_FAILED", stage, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if stage == "download checksum" && (resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone) {
			return nil, prepareError("CHECKSUM_UNAVAILABLE", stage, fmt.Errorf("release checksum asset returned HTTP %d", resp.StatusCode))
		}
		return nil, prepareError("ASSET_HTTP_STATUS", stage, fmt.Errorf("asset request returned HTTP %d", resp.StatusCode))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, prepareError("ASSET_READ_FAILED", stage, err)
	}
	if int64(len(body)) > maxBytes {
		return nil, prepareError("ASSET_TOO_LARGE", stage, fmt.Errorf("asset exceeds %d bytes", maxBytes))
	}
	return body, nil
}

func (p *Preparer) download(ctx context.Context, assetURL, path string, maxBytes int64, digest hash.Hash, stage string) (int64, error) {
	requestCtx, cancel := context.WithTimeout(ctx, prepareHTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, assetURL, nil)
	if err != nil {
		return 0, prepareError("INVALID_ASSET_URL", stage, err)
	}
	req.Header.Set("Accept", "application/octet-stream")
	resp, err := p.client.Do(req)
	if err != nil {
		return 0, prepareError("ASSET_REQUEST_FAILED", stage, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, prepareError("ASSET_HTTP_STATUS", stage, fmt.Errorf("asset request returned HTTP %d", resp.StatusCode))
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, prepareError("STAGING_WRITE_FAILED", stage, err)
	}
	limited := io.LimitReader(resp.Body, maxBytes+1)
	n, copyErr := io.Copy(io.MultiWriter(file, digest), limited)
	closeErr := file.Close()
	if copyErr != nil {
		return n, prepareError("ASSET_READ_FAILED", stage, copyErr)
	}
	if closeErr != nil {
		return n, prepareError("STAGING_WRITE_FAILED", stage, closeErr)
	}
	if n > maxBytes {
		return n, prepareError("ASSET_TOO_LARGE", stage, fmt.Errorf("archive exceeds %d bytes", maxBytes))
	}
	return n, nil
}

func extractPreparedBinary(ctx context.Context, archivePath, binaryPath, goarch string, maxBytes int64) (int64, string, error) {
	if err := ctx.Err(); err != nil {
		return 0, "", err
	}
	archive, err := os.Open(archivePath)
	if err != nil {
		return 0, "", err
	}
	defer archive.Close()
	gz, err := gzip.NewReader(archive)
	if err != nil {
		return 0, "", fmt.Errorf("open gzip archive: %w", err)
	}
	defer gz.Close()
	limitedGzip := &io.LimitedReader{R: contextReader{ctx: ctx, reader: gz}, N: maxBytes + (64 << 10) + 1}
	reader := tar.NewReader(limitedGzip)
	header, err := reader.Next()
	if err != nil {
		return 0, "", fmt.Errorf("read archive entry: %w", err)
	}
	if header.Name != preparedBinaryName {
		return 0, "", fmt.Errorf("unexpected archive entry %q; expected root %q", header.Name, preparedBinaryName)
	}
	if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
		return 0, "", fmt.Errorf("upgrade binary must be a regular file, got entry type %d", header.Typeflag)
	}
	if header.Size <= 0 || header.Size > maxBytes {
		return 0, "", fmt.Errorf("upgrade binary size %d is outside the allowed range", header.Size)
	}
	out, err := os.OpenFile(binaryPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, "", err
	}
	binaryHash := sha256.New()
	n, copyErr := io.CopyN(io.MultiWriter(out, binaryHash), reader, header.Size)
	closeErr := out.Close()
	if copyErr != nil {
		return n, "", fmt.Errorf("extract upgrade binary: %w", copyErr)
	}
	if n != header.Size {
		return n, "", fmt.Errorf("extracted binary size %d does not match archive size %d", n, header.Size)
	}
	if closeErr != nil {
		return n, "", closeErr
	}
	if _, err := reader.Next(); err != io.EOF {
		if err != nil {
			return n, "", fmt.Errorf("validate archive trailer: %w", err)
		}
		return n, "", errors.New("upgrade archive contains unexpected additional entries")
	}
	var tail [32 * 1024]byte
	for {
		read, readErr := limitedGzip.Read(tail[:])
		for _, b := range tail[:read] {
			if b != 0 {
				return n, "", errors.New("upgrade archive contains nonzero trailing data")
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return n, "", fmt.Errorf("validate gzip trailer: %w", readErr)
		}
		if read == 0 {
			return n, "", io.ErrNoProgress
		}
	}
	if limitedGzip.N == 0 {
		return n, "", fmt.Errorf("expanded archive exceeds %d bytes", maxBytes+(64<<10))
	}
	if err := ctx.Err(); err != nil {
		return n, "", err
	}
	if err := validateELFArchitecture(binaryPath, goarch); err != nil {
		return n, "", err
	}
	return n, hex.EncodeToString(binaryHash.Sum(nil)), nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func validateELFArchitecture(path, goarch string) error {
	file, err := elf.Open(path)
	if err != nil {
		return fmt.Errorf("staged binary is not a valid ELF executable: %w", err)
	}
	defer file.Close()
	wantMachine := elf.EM_X86_64
	if goarch == "arm64" {
		wantMachine = elf.EM_AARCH64
	}
	if file.Class != elf.ELFCLASS64 || file.Data != elf.ELFDATA2LSB || (file.Type != elf.ET_EXEC && file.Type != elf.ET_DYN) || file.Machine != wantMachine {
		return fmt.Errorf("binary ELF architecture does not match linux/%s", goarch)
	}
	return nil
}

func reportPrepareProgress(progress PrepareProgressFunc, update PrepareProgress) error {
	if progress == nil {
		return nil
	}
	return progress(update)
}

func prepareError(code, stage string, err error) *PrepareError {
	return &PrepareError{Code: code, Stage: stage, Err: err}
}

func stageDirectoryName(taskID string) string {
	digest := sha256.Sum256([]byte(taskID))
	return "dockerCopilot-update-" + hex.EncodeToString(digest[:])
}
