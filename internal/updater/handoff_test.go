package updater

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPlacePreparedBinaryRevalidatesAndAtomicallyStagesHandoff(t *testing.T) {
	root := t.TempDir()
	stagingParent := filepath.Join(root, "task-state", "program-update-stage")
	workDir := filepath.Join(root, "work")
	if err := os.MkdirAll(stagingParent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(workDir, 0o700); err != nil {
		t.Fatal(err)
	}
	binary := minimalELF(elf.EM_X86_64)
	prepared := writePreparedFixture(t, stagingParent, "00000000-0000-4000-8000-000000000051", binary)
	current := filepath.Join(workDir, "dockerCopilot")
	if err := os.WriteFile(current, []byte("old executable"), 0o700); err != nil {
		t.Fatal(err)
	}

	result, err := PlacePreparedBinary(context.Background(), HandoffOptions{WorkDir: workDir, StagingParent: stagingParent, GOARCH: "amd64"}, prepared)
	if err != nil {
		t.Fatal(err)
	}
	wantHash := sha256.Sum256(binary)
	if result.TaskID != prepared.TaskID || result.TargetVersion != testTargetVersion || result.GOARCH != "amd64" || result.BinarySHA256 != hex.EncodeToString(wantHash[:]) || result.BinaryBytes != int64(len(binary)) || result.Path != filepath.Join(workDir, preparedBinaryName) {
		t.Fatalf("handoff result mismatch: %#v", result)
	}
	got, err := os.ReadFile(result.Path)
	if err != nil || !bytes.Equal(got, binary) {
		t.Fatalf("handoff content mismatch: bytes=%d err=%v", len(got), err)
	}
	if runtime.GOOS != "windows" {
		info, statErr := os.Stat(result.Path)
		if statErr != nil || info.Mode().Perm() != 0o755 {
			t.Fatalf("handoff permissions=%v err=%v", info, statErr)
		}
	}
	old, err := os.ReadFile(current)
	if err != nil || string(old) != "old executable" {
		t.Fatalf("running program was modified: %q err=%v", old, err)
	}
	if err := RemovePlacedBinary(HandoffOptions{WorkDir: workDir}, result); err != nil {
		t.Fatalf("remove owned handoff failed: %v", err)
	}
	if _, err := os.Lstat(result.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned handoff remains after cleanup: %v", err)
	}
}

func TestPlacePreparedBinaryRejectsChangedArtifactAndPreservesExistingTarget(t *testing.T) {
	root := t.TempDir()
	stagingParent := filepath.Join(root, "staging")
	workDir := filepath.Join(root, "work")
	if err := os.MkdirAll(stagingParent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(workDir, 0o700); err != nil {
		t.Fatal(err)
	}
	prepared := writePreparedFixture(t, stagingParent, "00000000-0000-4000-8000-000000000052", minimalELF(elf.EM_X86_64))
	changedBinary := minimalELF(elf.EM_X86_64)
	changedBinary[24] = 1 // entry address changes, while the file remains a valid amd64 ELF.
	if err := os.WriteFile(prepared.BinaryPath, changedBinary, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := PlacePreparedBinary(context.Background(), HandoffOptions{WorkDir: workDir, StagingParent: stagingParent, GOARCH: "amd64"}, prepared)
	assertHandoffFailure(t, err, "STAGED_BINARY_CHANGED")
	if _, statErr := os.Lstat(filepath.Join(workDir, preparedBinaryName)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("changed input created a handoff target: %v", statErr)
	}

	prepared = writePreparedFixture(t, stagingParent, "00000000-0000-4000-8000-000000000053", minimalELF(elf.EM_X86_64))
	target := filepath.Join(workDir, preparedBinaryName)
	if err := os.WriteFile(target, []byte("existing handoff"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = PlacePreparedBinary(context.Background(), HandoffOptions{WorkDir: workDir, StagingParent: stagingParent, GOARCH: "amd64"}, prepared)
	assertHandoffFailure(t, err, "HANDOFF_TARGET_EXISTS")
	data, readErr := os.ReadFile(target)
	if readErr != nil || string(data) != "existing handoff" {
		t.Fatalf("existing target changed: data=%q err=%v", data, readErr)
	}
	if err := RemovePlacedBinary(HandoffOptions{WorkDir: workDir}, InstallHandoff{TaskID: "00000000-0000-4000-8000-000000000053", Path: target, BinarySHA256: strings.Repeat("0", 64)}); handoffErrorCode(err) != "HANDOFF_BINARY_CHANGED" {
		t.Fatalf("cleanup removed unrelated target: %v", err)
	}
}

func TestPlacePreparedBinaryRejectsPathEscapeSymlinkAndCancellation(t *testing.T) {
	root := t.TempDir()
	stagingParent := filepath.Join(root, "staging")
	workDir := filepath.Join(root, "work")
	if err := os.MkdirAll(stagingParent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(workDir, 0o700); err != nil {
		t.Fatal(err)
	}
	prepared := writePreparedFixture(t, stagingParent, "00000000-0000-4000-8000-000000000054", minimalELF(elf.EM_X86_64))
	prepared.Directory = filepath.Join(root, "outside", filepath.Base(prepared.Directory))
	_, err := PlacePreparedBinary(context.Background(), HandoffOptions{WorkDir: workDir, StagingParent: stagingParent, GOARCH: "amd64"}, prepared)
	assertHandoffFailure(t, err, "INVALID_STAGED_ARTIFACT")

	prepared = writePreparedFixture(t, stagingParent, "00000000-0000-4000-8000-000000000055", minimalELF(elf.EM_X86_64))
	if runtime.GOOS != "windows" {
		outside := filepath.Join(root, "outside-binary")
		if err := os.WriteFile(outside, minimalELF(elf.EM_X86_64), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(prepared.BinaryPath); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, prepared.BinaryPath); err != nil {
			t.Fatal(err)
		}
		_, err = PlacePreparedBinary(context.Background(), HandoffOptions{WorkDir: workDir, StagingParent: stagingParent, GOARCH: "amd64"}, prepared)
		assertHandoffFailure(t, err, "INVALID_STAGED_ARTIFACT")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = PlacePreparedBinary(canceled, HandoffOptions{WorkDir: workDir, StagingParent: stagingParent, GOARCH: "amd64"}, prepared)
	assertHandoffFailure(t, err, "HANDOFF_CANCELED")
	if _, statErr := os.Lstat(filepath.Join(workDir, preparedBinaryName)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("canceled handoff created target: %v", statErr)
	}
}

func TestVerifyRunningProgramRequiresTargetVersionArchitectureAndBinaryDigest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dockerCopilot")
	binary := minimalELF(elf.EM_X86_64)
	if err := os.WriteFile(path, binary, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(binary)
	expected := InstallHandoff{TaskID: "00000000-0000-4000-8000-000000000056", TargetVersion: testTargetVersion, GOARCH: "amd64", BinarySHA256: hex.EncodeToString(digest[:])}
	if err := VerifyRunningProgram(context.Background(), path, testTargetVersion, expected, 1<<20); err != nil {
		t.Fatalf("valid running program rejected: %v", err)
	}
	if err := VerifyRunningProgram(context.Background(), path, "2.3.5", expected, 1<<20); handoffErrorCode(err) != "RUNNING_VERSION_MISMATCH" {
		t.Fatalf("version mismatch error=%v", err)
	}
	expected.BinarySHA256 = string(make([]byte, 64))
	if err := VerifyRunningProgram(context.Background(), path, testTargetVersion, expected, 1<<20); handoffErrorCode(err) != "RUNNING_BINARY_MISMATCH" {
		t.Fatalf("digest mismatch error=%v", err)
	}
	if err := VerifyRunningProgram(context.Background(), path, testTargetVersion, InstallHandoff{TaskID: expected.TaskID, TargetVersion: testTargetVersion, GOARCH: "arm64", BinarySHA256: hex.EncodeToString(digest[:])}, 1<<20); handoffErrorCode(err) != "RUNNING_BINARY_INVALID" {
		t.Fatalf("architecture mismatch error=%v", err)
	}
}

func writePreparedFixture(t *testing.T, stagingParent, taskID string, binary []byte) *PreparedProgram {
	t.Helper()
	stageDir := filepath.Join(stagingParent, stageDirectoryName(taskID))
	if err := os.Mkdir(stageDir, 0o700); err != nil {
		t.Fatal(err)
	}
	binaryPath := filepath.Join(stageDir, preparedBinaryName)
	if err := os.WriteFile(binaryPath, binary, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(binary)
	return &PreparedProgram{TaskID: taskID, TargetVersion: testTargetVersion, GOARCH: "amd64", Directory: stageDir, BinaryPath: binaryPath, BinaryBytes: int64(len(binary)), BinarySHA256: hex.EncodeToString(digest[:])}
}

func assertHandoffFailure(t *testing.T, err error, wantCode string) {
	t.Helper()
	var handoffErr *HandoffError
	if !errors.As(err, &handoffErr) || handoffErr.Code != wantCode {
		t.Fatalf("handoff error=%#v, want code %q", err, wantCode)
	}
}

func handoffErrorCode(err error) string {
	var handoffErr *HandoffError
	if errors.As(err, &handoffErr) {
		return handoffErr.Code
	}
	return ""
}
