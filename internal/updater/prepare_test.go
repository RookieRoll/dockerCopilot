package updater

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"debug/elf"
	"github.com/onlyLTY/dockerCopilot/internal/config"
)

const testTargetVersion = "2.3.4"

func TestPreparerDownloadsVerifiesAndStagesBinaryWithoutReplacingCurrent(t *testing.T) {
	binary := minimalELF(elf.EM_X86_64)
	archive := makeUpgradeArchive(t, tar.TypeReg, preparedBinaryName, binary, nil)
	server, source := newReleaseFixture(t, archive, nil, http.StatusOK)
	defer server.Close()
	staging := t.TempDir()
	current := filepath.Join(staging, "dockerCopilot")
	if err := os.WriteFile(current, []byte("current binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	var progress []PrepareProgress
	preparer := NewPreparer(PrepareOptions{Client: server.Client(), StagingParent: staging, GOARCH: "amd64"})
	prepared, err := preparer.Prepare(context.Background(), PrepareIdentity{TaskID: "task-123", RequestID: "request-456", TargetVersion: testTargetVersion}, source, func(update PrepareProgress) error {
		progress = append(progress, update)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = prepared.Cleanup() })
	if prepared.TaskID != "task-123" || prepared.RequestID != "request-456" || prepared.TargetVersion != testTargetVersion || prepared.GOARCH != "amd64" {
		t.Fatalf("prepared identity = %#v", prepared)
	}
	if prepared.Directory == staging || filepath.Dir(prepared.BinaryPath) != prepared.Directory || filepath.Base(prepared.BinaryPath) != preparedBinaryName {
		t.Fatalf("prepared files are not isolated: %#v", prepared)
	}
	gotBinary, err := os.ReadFile(prepared.BinaryPath)
	if err != nil || !bytes.Equal(gotBinary, binary) {
		t.Fatalf("staged binary mismatch: bytes=%d err=%v", len(gotBinary), err)
	}
	binaryHash := sha256.Sum256(binary)
	if prepared.ArchiveSHA256 != fmt.Sprintf("%x", sha256.Sum256(archive)) || prepared.BinarySHA256 != fmt.Sprintf("%x", binaryHash) || prepared.ArchiveBytes != int64(len(archive)) || prepared.BinaryBytes != int64(len(binary)) {
		t.Fatalf("prepared size/checksum mismatch: %#v", prepared)
	}
	mode, err := os.Stat(prepared.BinaryPath)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && mode.Mode().Perm() != 0o600 {
		t.Fatalf("staged mode=%v", mode.Mode().Perm())
	}
	archiveInfo, err := os.Stat(prepared.ArchivePath)
	if err != nil || (runtime.GOOS != "windows" && archiveInfo.Mode().Perm() != 0o600) {
		t.Fatalf("staged archive permissions=%v err=%v", archiveInfo, err)
	}
	directoryInfo, err := os.Stat(prepared.Directory)
	if err != nil || (runtime.GOOS != "windows" && directoryInfo.Mode().Perm() != 0o700) {
		t.Fatalf("staging directory permissions=%v err=%v", directoryInfo, err)
	}
	currentBytes, err := os.ReadFile(current)
	if err != nil || string(currentBytes) != "current binary" {
		t.Fatalf("live program changed: %q err=%v", currentBytes, err)
	}
	if len(progress) != 4 || progress[0].Stage != "download-checksum" || progress[len(progress)-1].Percent != 100 {
		t.Fatalf("progress updates = %#v", progress)
	}
}

func TestPreparerRejectsMissingOrInvalidChecksumsAndCleansStaging(t *testing.T) {
	archive := makeUpgradeArchive(t, tar.TypeReg, preparedBinaryName, minimalELF(elf.EM_X86_64), nil)
	for _, tc := range []struct {
		name     string
		status   int
		checksum string
		wantCode string
	}{
		{name: "missing checksum", status: http.StatusNotFound, wantCode: "CHECKSUM_UNAVAILABLE"},
		{name: "malformed checksum", status: http.StatusOK, checksum: "not-a-sha256", wantCode: "CHECKSUM_UNAVAILABLE"},
		{name: "checksum names another asset", status: http.StatusOK, checksum: fmt.Sprintf("%x other.tar.gz", sha256.Sum256(archive)), wantCode: "CHECKSUM_UNAVAILABLE"},
		{name: "mismatched checksum", status: http.StatusOK, checksum: strings.Repeat("0", 64), wantCode: "CHECKSUM_MISMATCH"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			staging := t.TempDir()
			current := filepath.Join(staging, "dockerCopilot")
			if err := os.WriteFile(current, []byte("old executable"), 0o700); err != nil {
				t.Fatal(err)
			}
			server, source := newReleaseFixture(t, archive, []byte(tc.checksum), tc.status)
			defer server.Close()
			preparer := NewPreparer(PrepareOptions{Client: server.Client(), StagingParent: staging, GOARCH: "amd64"})
			prepared, err := preparer.Prepare(context.Background(), PrepareIdentity{TaskID: "task", TargetVersion: testTargetVersion}, source, nil)
			assertPrepareFailure(t, prepared, err, tc.wantCode)
			assertNoStagingChildren(t, staging)
			if data, err := os.ReadFile(current); err != nil || string(data) != "old executable" {
				t.Fatalf("failed preparation changed current program: %q err=%v", data, err)
			}
		})
	}
}

func TestPreparerRejectsArchiveHTTPFailure(t *testing.T) {
	archive := makeUpgradeArchive(t, tar.TypeReg, preparedBinaryName, minimalELF(elf.EM_X86_64), nil)
	server, source := newReleaseFixtureWithArchiveStatus(t, archive, nil, http.StatusOK, http.StatusServiceUnavailable)
	defer server.Close()
	staging := t.TempDir()
	preparer := NewPreparer(PrepareOptions{Client: server.Client(), StagingParent: staging, GOARCH: "amd64"})
	prepared, err := preparer.Prepare(context.Background(), PrepareIdentity{TaskID: "task", TargetVersion: testTargetVersion}, source, nil)
	assertPrepareFailure(t, prepared, err, "ASSET_HTTP_STATUS")
	assertNoStagingChildren(t, staging)
}

func TestPreparerRejectsWrongArchitectureAndUnsafeArchiveEntries(t *testing.T) {
	armBinary := minimalELF(elf.EM_AARCH64)
	wrongArch := makeUpgradeArchive(t, tar.TypeReg, preparedBinaryName, armBinary, nil)
	traversal := makeUpgradeArchive(t, tar.TypeReg, "../dockerCopilot-new", minimalELF(elf.EM_X86_64), nil)
	symlink := makeUpgradeArchive(t, tar.TypeSymlink, preparedBinaryName, nil, nil)
	extraEntry := makeUpgradeArchive(t, tar.TypeReg, preparedBinaryName, minimalELF(elf.EM_X86_64), []tarEntry{{name: "other", data: []byte("extra")}})
	for _, tc := range []struct {
		name    string
		archive []byte
	}{
		{name: "wrong ELF architecture", archive: wrongArch},
		{name: "path traversal", archive: traversal},
		{name: "nested layout", archive: makeUpgradeArchive(t, tar.TypeReg, "dist/linux/amd64/dockerCopilot-new", minimalELF(elf.EM_X86_64), nil)},
		{name: "symlink", archive: symlink},
		{name: "hardlink", archive: makeUpgradeArchive(t, tar.TypeLink, preparedBinaryName, nil, nil)},
		{name: "extra file", archive: extraEntry},
	} {
		t.Run(tc.name, func(t *testing.T) {
			staging := t.TempDir()
			server, source := newReleaseFixture(t, tc.archive, nil, http.StatusOK)
			defer server.Close()
			preparer := NewPreparer(PrepareOptions{Client: server.Client(), StagingParent: staging, GOARCH: "amd64"})
			prepared, err := preparer.Prepare(context.Background(), PrepareIdentity{TaskID: "task", TargetVersion: testTargetVersion}, source, nil)
			assertPrepareFailure(t, prepared, err, "INVALID_UPGRADE_ARCHIVE")
			assertNoStagingChildren(t, staging)
		})
	}
}

func TestPreparerEnforcesArchiveAndExpandedSizeLimits(t *testing.T) {
	binary := minimalELF(elf.EM_X86_64)
	archive := makeUpgradeArchive(t, tar.TypeReg, preparedBinaryName, binary, nil)
	for _, tc := range []struct {
		name      string
		archive   []byte
		maxZip    int64
		maxBinary int64
		wantCode  string
	}{
		{name: "compressed archive limit", archive: archive, maxZip: int64(len(archive) - 1), maxBinary: 1 << 20, wantCode: "ASSET_TOO_LARGE"},
		{name: "expanded binary limit", archive: archive, maxZip: 1 << 20, maxBinary: 16, wantCode: "INVALID_UPGRADE_ARCHIVE"},
		{name: "expanded archive padding limit", archive: makeUpgradeArchiveWithPadding(t, tar.TypeReg, preparedBinaryName, binary, 2<<20), maxZip: 1 << 20, maxBinary: 1 << 20, wantCode: "INVALID_UPGRADE_ARCHIVE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			staging := t.TempDir()
			fixture, fixtureSource := newReleaseFixture(t, tc.archive, nil, http.StatusOK)
			defer fixture.Close()
			preparer := NewPreparer(PrepareOptions{Client: fixture.Client(), StagingParent: staging, GOARCH: "amd64", MaxArchiveSize: tc.maxZip, MaxBinarySize: tc.maxBinary})
			prepared, err := preparer.Prepare(context.Background(), PrepareIdentity{TaskID: "task", TargetVersion: testTargetVersion}, fixtureSource, nil)
			assertPrepareFailure(t, prepared, err, tc.wantCode)
			assertNoStagingChildren(t, staging)
		})
	}
}

func TestPreparerRejectsUnsupportedArchitectureAndProgressFailure(t *testing.T) {
	staging := t.TempDir()
	preparer := NewPreparer(PrepareOptions{StagingParent: staging, GOARCH: "386"})
	prepared, err := preparer.Prepare(context.Background(), PrepareIdentity{TaskID: "task", TargetVersion: testTargetVersion}, config.UpdateConfig{}, nil)
	assertPrepareFailure(t, prepared, err, "UNSUPPORTED_ARCHITECTURE")
	assertNoStagingChildren(t, staging)

	var requests atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return nil, errors.New("unexpected request before progress persistence")
	})}
	preparer = NewPreparer(PrepareOptions{Client: client, StagingParent: staging, GOARCH: "amd64"})
	prepared, err = preparer.Prepare(context.Background(), PrepareIdentity{TaskID: "task", TargetVersion: testTargetVersion}, config.UpdateConfig{Repository: "org/project"}, func(PrepareProgress) error {
		return errors.New("task progress persistence failed")
	})
	assertPrepareFailure(t, prepared, err, "PROGRESS_REPORT_FAILED")
	if requests.Load() != 0 {
		t.Fatalf("preparation continued after progress failure; requests=%d", requests.Load())
	}
	assertNoStagingChildren(t, staging)
}

func TestPreparerRequiresExplicitStagingParent(t *testing.T) {
	preparer := NewPreparer(PrepareOptions{GOARCH: "amd64"})
	prepared, err := preparer.Prepare(context.Background(), PrepareIdentity{TaskID: "task", TargetVersion: testTargetVersion}, config.UpdateConfig{}, nil)
	assertPrepareFailure(t, prepared, err, "STAGING_UNAVAILABLE")
}

func TestPreparerRejectsDuplicateTaskIDWithoutRemovingExistingStage(t *testing.T) {
	staging := t.TempDir()
	taskID := "00000000-0000-4000-8000-000000000099"
	existingDir := filepath.Join(staging, stageDirectoryName(taskID))
	if err := os.Mkdir(existingDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(existingDir, "pending-marker.json")
	if err := os.WriteFile(sentinel, []byte("preserve existing stage"), 0o600); err != nil {
		t.Fatal(err)
	}

	preparer := NewPreparer(PrepareOptions{StagingParent: staging, GOARCH: "amd64"})
	prepared, err := preparer.Prepare(context.Background(), PrepareIdentity{TaskID: taskID, TargetVersion: testTargetVersion}, config.UpdateConfig{}, nil)
	assertPrepareFailure(t, prepared, err, "STAGING_IN_USE")
	data, readErr := os.ReadFile(sentinel)
	if readErr != nil || string(data) != "preserve existing stage" {
		t.Fatalf("existing stage was modified or removed: data=%q err=%v", data, readErr)
	}
}

func TestPreparerCancellationDuringValidationPreservesCauseAndCleansStage(t *testing.T) {
	archive := makeUpgradeArchive(t, tar.TypeReg, preparedBinaryName, minimalELF(elf.EM_X86_64), nil)
	server, source := newReleaseFixture(t, archive, nil, http.StatusOK)
	defer server.Close()
	staging := t.TempDir()
	preparer := NewPreparer(PrepareOptions{Client: server.Client(), StagingParent: staging, GOARCH: "amd64"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	prepared, err := preparer.Prepare(ctx, PrepareIdentity{TaskID: "task", TargetVersion: testTargetVersion}, source, func(progress PrepareProgress) error {
		if progress.Stage == "verify-archive" {
			cancel()
		}
		return nil
	})
	assertPrepareFailure(t, prepared, err, "PREPARATION_CANCELED")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation cause was lost: %v", err)
	}
	assertNoStagingChildren(t, staging)
}

type tarEntry struct {
	name string
	data []byte
}

func makeUpgradeArchive(t *testing.T, typeflag byte, name string, binary []byte, extra []tarEntry) []byte {
	return makeUpgradeArchiveWithPadding(t, typeflag, name, binary, 0, extra...)
}

func makeUpgradeArchiveWithPadding(t *testing.T, typeflag byte, name string, binary []byte, padding int, extra ...tarEntry) []byte {
	t.Helper()
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tarWriter := tar.NewWriter(gz)
	header := &tar.Header{Name: name, Typeflag: typeflag, Mode: 0o755, Size: int64(len(binary))}
	if typeflag == tar.TypeSymlink || typeflag == tar.TypeLink {
		header.Linkname = "target"
		header.Size = 0
	}
	if err := tarWriter.WriteHeader(header); err != nil {
		t.Fatal(err)
	}
	if len(binary) > 0 {
		if _, err := tarWriter.Write(binary); err != nil {
			t.Fatal(err)
		}
	}
	for _, entry := range extra {
		if err := tarWriter.WriteHeader(&tar.Header{Name: entry.name, Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len(entry.data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write(entry.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if padding > 0 {
		if _, err := gz.Write(make([]byte, padding)); err != nil {
			t.Fatal(err)
		}
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

func newReleaseFixture(t *testing.T, archive, checksum []byte, checksumStatus int) (*httptest.Server, config.UpdateConfig) {
	return newReleaseFixtureWithArchiveStatus(t, archive, checksum, checksumStatus, http.StatusOK)
}

func newReleaseFixtureWithArchiveStatus(t *testing.T, archive, checksum []byte, checksumStatus, archiveStatus int) (*httptest.Server, config.UpdateConfig) {
	t.Helper()
	digest := sha256.Sum256(archive)
	if checksum == nil {
		checksum = []byte(fmt.Sprintf("%x  dockerCopilot-amd64.tar.gz\n", digest))
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, ".tar.gz.sha256"):
			w.WriteHeader(checksumStatus)
			_, _ = w.Write(checksum)
		case strings.HasSuffix(r.URL.Path, ".tar.gz"):
			w.WriteHeader(archiveStatus)
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	return server, config.UpdateConfig{Repository: "org/project", GitHubProxy: server.URL}
}

func assertPrepareFailure(t *testing.T, prepared *PreparedProgram, err error, wantCode string) {
	t.Helper()
	if err == nil || prepared != nil {
		t.Fatalf("Prepare() = (%#v,%v), want failure", prepared, err)
	}
	var prepareErr *PrepareError
	if !errors.As(err, &prepareErr) || prepareErr.Code != wantCode {
		t.Fatalf("Prepare() error=%#v, want code %q", err, wantCode)
	}
}

func assertNoStagingChildren(t *testing.T, parent string) {
	t.Helper()
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "dockerCopilot-update-") {
			t.Fatalf("failed preparation left staging artifact: %#v", entry)
		}
	}
}

func minimalELF(machine elf.Machine) []byte {
	b := make([]byte, 64)
	copy(b[:4], []byte{0x7f, 'E', 'L', 'F'})
	b[4], b[5], b[6] = byte(elf.ELFCLASS64), byte(elf.ELFDATA2LSB), byte(elf.EV_CURRENT)
	b[16], b[17] = byte(elf.ET_EXEC), byte(elf.ET_EXEC>>8)
	b[18], b[19] = byte(machine), byte(machine>>8)
	b[20] = byte(elf.EV_CURRENT)
	b[52], b[53] = 64, 0
	return b
}
