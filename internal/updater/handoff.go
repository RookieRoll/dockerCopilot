package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const defaultHandoffBinaryLimit = int64(256 << 20)

// HandoffOptions identifies the controlled installation working directory.
// The running executable is never replaced by this package.
type HandoffOptions struct {
	WorkDir       string
	StagingParent string
	GOARCH        string
	MaxBinarySize int64
}

// InstallHandoff is a verified copy ready for the external startup script.
// It contains no caller-controlled destination path.
type InstallHandoff struct {
	TaskID        string
	TargetVersion string
	GOARCH        string
	BinarySHA256  string
	BinaryBytes   int64
	Path          string
	fileIdentity  os.FileInfo
}

// HandoffError classifies an installation handoff failure and preserves its cause.
type HandoffError struct {
	Code  string
	Stage string
	Err   error
}

func (e *HandoffError) Error() string {
	if e == nil {
		return "program handoff failed"
	}
	return fmt.Sprintf("%s: %v", e.Stage, e.Err)
}

func (e *HandoffError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// PlacePreparedBinary revalidates a prepared artifact and atomically places a
// private dockerCopilot-new copy in WorkDir. It refuses to reuse a task stage,
// follow symlinks, or overwrite an existing handoff target.
func PlacePreparedBinary(ctx context.Context, opts HandoffOptions, prepared *PreparedProgram) (_ InstallHandoff, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return InstallHandoff{}, handoffError("HANDOFF_CANCELED", "validate handoff", err)
	}
	if prepared == nil || strings.TrimSpace(prepared.TaskID) == "" || !versionPattern.MatchString(prepared.TargetVersion) {
		return InstallHandoff{}, handoffError("INVALID_HANDOFF_IDENTITY", "validate handoff", errors.New("verified task identity and target version are required"))
	}
	if opts.GOARCH != "amd64" && opts.GOARCH != "arm64" || prepared.GOARCH != opts.GOARCH {
		return InstallHandoff{}, handoffError("UNSUPPORTED_ARCHITECTURE", "validate handoff", fmt.Errorf("prepared binary architecture %q does not match runtime %q", prepared.GOARCH, opts.GOARCH))
	}
	if strings.TrimSpace(opts.WorkDir) == "" {
		return InstallHandoff{}, handoffError("INSTALL_DIRECTORY_UNAVAILABLE", "inspect install directory", errors.New("an explicit program working directory is required"))
	}
	if strings.TrimSpace(opts.StagingParent) == "" || filepath.Clean(filepath.Dir(prepared.Directory)) != filepath.Clean(opts.StagingParent) || !validStagedArtifactPath(prepared) {
		return InstallHandoff{}, handoffError("INVALID_STAGED_ARTIFACT", "inspect staged binary", errors.New("staged binary is outside its task-specific staging directory"))
	}
	stageParentInfo, err := os.Lstat(opts.StagingParent)
	if err != nil || !stageParentInfo.IsDir() || stageParentInfo.Mode()&os.ModeSymlink != 0 {
		if err == nil {
			err = errors.New("staging parent must be a real directory")
		}
		return InstallHandoff{}, handoffError("INVALID_STAGED_ARTIFACT", "inspect staging parent", err)
	}
	stageInfo, err := os.Lstat(prepared.Directory)
	if err != nil || !stageInfo.IsDir() || stageInfo.Mode()&os.ModeSymlink != 0 {
		if err == nil {
			err = errors.New("staging directory must be a real directory")
		}
		return InstallHandoff{}, handoffError("INVALID_STAGED_ARTIFACT", "inspect staged binary", err)
	}
	workInfo, err := os.Lstat(opts.WorkDir)
	if err != nil || !workInfo.IsDir() || workInfo.Mode()&os.ModeSymlink != 0 {
		if err == nil {
			err = errors.New("program working directory must be a real directory")
		}
		return InstallHandoff{}, handoffError("INSTALL_DIRECTORY_UNAVAILABLE", "inspect install directory", err)
	}
	if opts.MaxBinarySize <= 0 {
		opts.MaxBinarySize = defaultHandoffBinaryLimit
	}
	if err := validateELFArchitecture(prepared.BinaryPath, opts.GOARCH); err != nil {
		return InstallHandoff{}, handoffError("INVALID_STAGED_ARTIFACT", "verify staged binary", err)
	}
	size, digest, err := hashRegularFile(ctx, prepared.BinaryPath, opts.MaxBinarySize)
	if err != nil {
		return InstallHandoff{}, handoffError("INVALID_STAGED_ARTIFACT", "verify staged binary", err)
	}
	if !validSHA256(prepared.BinarySHA256) || !strings.EqualFold(digest, prepared.BinarySHA256) || size != prepared.BinaryBytes {
		return InstallHandoff{}, handoffError("STAGED_BINARY_CHANGED", "verify staged binary", errors.New("staged binary size or SHA256 differs from preparation result"))
	}
	if err := ctx.Err(); err != nil {
		return InstallHandoff{}, handoffError("HANDOFF_CANCELED", "copy staged binary", err)
	}
	destination := filepath.Join(opts.WorkDir, preparedBinaryName)
	if _, err := os.Lstat(destination); err == nil {
		return InstallHandoff{}, handoffError("HANDOFF_TARGET_EXISTS", "place upgrade binary", errors.New("dockerCopilot-new already exists"))
	} else if !errors.Is(err, os.ErrNotExist) {
		return InstallHandoff{}, handoffError("INSTALL_DIRECTORY_UNAVAILABLE", "inspect handoff target", err)
	}
	temporary, err := os.CreateTemp(opts.WorkDir, ".dockerCopilot-new-*")
	if err != nil {
		return InstallHandoff{}, handoffError("INSTALL_DIRECTORY_UNAVAILABLE", "create handoff temporary file", err)
	}
	temporaryPath := temporary.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err = temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return InstallHandoff{}, handoffError("INSTALL_DIRECTORY_UNAVAILABLE", "secure handoff temporary file", err)
	}
	source, err := os.Open(prepared.BinaryPath)
	if err != nil {
		_ = temporary.Close()
		return InstallHandoff{}, handoffError("INVALID_STAGED_ARTIFACT", "read staged binary", err)
	}
	copyHash := sha256.New()
	reader := contextReader{ctx: ctx, reader: io.LimitReader(source, opts.MaxBinarySize+1)}
	written, copyErr := io.Copy(io.MultiWriter(temporary, copyHash), reader)
	closeSourceErr := source.Close()
	if copyErr != nil {
		_ = temporary.Close()
		return InstallHandoff{}, handoffError("HANDOFF_CANCELED", "copy staged binary", copyErr)
	}
	if closeSourceErr != nil {
		_ = temporary.Close()
		return InstallHandoff{}, handoffError("INVALID_STAGED_ARTIFACT", "close staged binary", closeSourceErr)
	}
	if written != size || written > opts.MaxBinarySize || !strings.EqualFold(hex.EncodeToString(copyHash.Sum(nil)), digest) {
		_ = temporary.Close()
		return InstallHandoff{}, handoffError("STAGED_BINARY_CHANGED", "copy staged binary", errors.New("staged binary changed while copying"))
	}
	// start.sh may execute the placed artifact after moving it into place.
	if err = temporary.Chmod(0o755); err != nil {
		_ = temporary.Close()
		return InstallHandoff{}, handoffError("INSTALL_DIRECTORY_UNAVAILABLE", "make handoff binary executable", err)
	}
	if err = temporary.Sync(); err != nil {
		_ = temporary.Close()
		return InstallHandoff{}, handoffError("INSTALL_DIRECTORY_UNAVAILABLE", "sync handoff binary", err)
	}
	if err = temporary.Close(); err != nil {
		return InstallHandoff{}, handoffError("INSTALL_DIRECTORY_UNAVAILABLE", "close handoff binary", err)
	}
	temporaryInfo, err := os.Lstat(temporaryPath)
	if err != nil {
		return InstallHandoff{}, handoffError("INSTALL_DIRECTORY_UNAVAILABLE", "inspect handoff binary", err)
	}
	if err = ctx.Err(); err != nil {
		return InstallHandoff{}, handoffError("HANDOFF_CANCELED", "place upgrade binary", err)
	}
	if err = os.Link(temporaryPath, destination); err != nil {
		code := "INSTALL_DIRECTORY_UNAVAILABLE"
		if errors.Is(err, os.ErrExist) {
			code = "HANDOFF_TARGET_EXISTS"
		}
		return InstallHandoff{}, handoffError(code, "place upgrade binary", err)
	}
	if err = syncHandoffDirectory(opts.WorkDir); err != nil {
		removeLinkedFileIfOwned(temporaryInfo, destination)
		return InstallHandoff{}, handoffError("INSTALL_DIRECTORY_UNAVAILABLE", "sync handoff directory", err)
	}
	if err = os.Remove(temporaryPath); err != nil {
		removeLinkedFileIfOwned(temporaryInfo, destination)
		return InstallHandoff{}, handoffError("INSTALL_DIRECTORY_UNAVAILABLE", "finish handoff", err)
	}
	if err = syncHandoffDirectory(opts.WorkDir); err != nil {
		removeLinkedFileIfOwned(temporaryInfo, destination)
		return InstallHandoff{}, handoffError("INSTALL_DIRECTORY_UNAVAILABLE", "sync handoff directory", err)
	}
	fileIdentity, err := os.Lstat(destination)
	if err != nil {
		return InstallHandoff{}, handoffError("INSTALL_DIRECTORY_UNAVAILABLE", "verify placed handoff", err)
	}
	return InstallHandoff{
		TaskID: prepared.TaskID, TargetVersion: prepared.TargetVersion, GOARCH: opts.GOARCH,
		BinarySHA256: digest, BinaryBytes: written, Path: destination, fileIdentity: fileIdentity,
	}, nil
}

// RemovePlacedBinary removes only the exact dockerCopilot-new artifact created
// for this handoff. It is safe to call after a task-marker or shutdown-signal
// failure and will refuse a changed/replaced file.
func RemovePlacedBinary(opts HandoffOptions, handoff InstallHandoff) error {
	if strings.TrimSpace(opts.WorkDir) == "" || handoff.TaskID == "" || !validSHA256(handoff.BinarySHA256) {
		return handoffError("INVALID_HANDOFF_IDENTITY", "remove handoff binary", errors.New("handoff identity is incomplete"))
	}
	destination := filepath.Join(opts.WorkDir, preparedBinaryName)
	if filepath.Clean(handoff.Path) != filepath.Clean(destination) {
		return handoffError("INVALID_HANDOFF_IDENTITY", "remove handoff binary", errors.New("handoff path is not the controlled dockerCopilot-new target"))
	}
	workInfo, err := os.Lstat(opts.WorkDir)
	if err != nil || !workInfo.IsDir() || workInfo.Mode()&os.ModeSymlink != 0 {
		if err == nil {
			err = errors.New("program working directory must be a real directory")
		}
		return handoffError("INSTALL_DIRECTORY_UNAVAILABLE", "inspect install directory", err)
	}
	info, err := os.Lstat(destination)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return handoffError("INSTALL_DIRECTORY_UNAVAILABLE", "inspect handoff binary", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return handoffError("HANDOFF_BINARY_CHANGED", "verify handoff binary before cleanup", errors.New("handoff target is not a regular file"))
	}
	if handoff.fileIdentity == nil || !os.SameFile(handoff.fileIdentity, info) {
		return handoffError("HANDOFF_BINARY_CHANGED", "verify handoff binary before cleanup", errors.New("handoff target was replaced after placement"))
	}
	_, digest, err := hashRegularFile(context.Background(), destination, defaultHandoffBinaryLimit)
	if err != nil {
		return handoffError("HANDOFF_BINARY_CHANGED", "verify handoff binary before cleanup", err)
	}
	if !strings.EqualFold(digest, handoff.BinarySHA256) {
		return handoffError("HANDOFF_BINARY_CHANGED", "verify handoff binary before cleanup", errors.New("handoff target no longer matches this task"))
	}
	if err := os.Remove(destination); err != nil {
		return handoffError("INSTALL_DIRECTORY_UNAVAILABLE", "remove handoff binary", err)
	}
	if err := syncHandoffDirectory(opts.WorkDir); err != nil {
		return handoffError("INSTALL_DIRECTORY_UNAVAILABLE", "sync handoff directory cleanup", err)
	}
	return nil
}

func removeLinkedFileIfOwned(identity os.FileInfo, destination string) {
	if identity == nil {
		return
	}
	current, err := os.Lstat(destination)
	if err == nil && os.SameFile(identity, current) {
		_ = os.Remove(destination)
	}
}

// VerifyRunningProgram checks the actual executable after restart. The caller
// supplies the compiled version, and success requires version, architecture,
// and digest to match the pending handoff identity.
func VerifyRunningProgram(ctx context.Context, executablePath, runningVersion string, expected InstallHandoff, maxBytes int64) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return handoffError("HANDOFF_CANCELED", "verify running program", err)
	}
	if expected.TaskID == "" || !versionPattern.MatchString(expected.TargetVersion) || expected.TargetVersion != runningVersion {
		return handoffError("RUNNING_VERSION_MISMATCH", "verify running program", fmt.Errorf("running version %q does not match target version %q", runningVersion, expected.TargetVersion))
	}
	if expected.GOARCH != "amd64" && expected.GOARCH != "arm64" {
		return handoffError("UNSUPPORTED_ARCHITECTURE", "verify running program", fmt.Errorf("unsupported target architecture %q", expected.GOARCH))
	}
	if maxBytes <= 0 {
		maxBytes = defaultHandoffBinaryLimit
	}
	if err := validateELFArchitecture(executablePath, expected.GOARCH); err != nil {
		return handoffError("RUNNING_BINARY_INVALID", "verify running program", err)
	}
	_, digest, err := hashRegularFile(ctx, executablePath, maxBytes)
	if err != nil {
		return handoffError("RUNNING_BINARY_INVALID", "verify running program", err)
	}
	if !validSHA256(expected.BinarySHA256) || !strings.EqualFold(digest, expected.BinarySHA256) {
		return handoffError("RUNNING_BINARY_MISMATCH", "verify running program", errors.New("running executable SHA256 does not match pending handoff"))
	}
	return nil
}

func validStagedArtifactPath(prepared *PreparedProgram) bool {
	if filepath.Base(prepared.Directory) != stageDirectoryName(prepared.TaskID) || filepath.Base(prepared.BinaryPath) != preparedBinaryName {
		return false
	}
	return filepath.Clean(filepath.Dir(prepared.BinaryPath)) == filepath.Clean(prepared.Directory)
}

func hashRegularFile(ctx context.Context, path string, maxBytes int64) (int64, string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, "", err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return 0, "", errors.New("binary must be a regular file without symlinks")
	}
	if info.Size() <= 0 || info.Size() > maxBytes {
		return 0, "", fmt.Errorf("binary size %d is outside the allowed range", info.Size())
	}
	file, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer file.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(contextReader{ctx: ctx, reader: file}, maxBytes+1))
	if err != nil {
		return n, "", err
	}
	if n != info.Size() || n > maxBytes {
		return n, "", errors.New("binary changed while hashing or exceeded the allowed size")
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func handoffError(code, stage string, err error) *HandoffError {
	return &HandoffError{Code: code, Stage: stage, Err: err}
}
