package observability

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
)

func TestRotatingWriterRotatesByDayAndSize(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	w := NewRotatingWriter(Options{Directory: dir, RetentionDays: 7, MaxFileSize: 8, MaxTotalSize: 32, Now: func() time.Time { return now }})
	defer w.Close()
	for _, p := range [][]byte{[]byte("1234"), []byte("5678"), []byte("90")} {
		if _, err := w.Write(p); err != nil {
			t.Fatal(err)
		}
	}
	now = now.Add(24 * time.Hour)
	if _, err := w.Write([]byte("day2")); err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, filepath.Join(dir, "dockercopilot-2026-10-22.jsonl"), "12345678")
	assertFileContent(t, filepath.Join(dir, "dockercopilot-2026-10-22.1.jsonl"), "90")
	assertFileContent(t, filepath.Join(dir, "dockercopilot-2026-10-23.jsonl"), "day2")
}

func TestRotatingWriterCleansRetentionAndTotalCapacity(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	old := filepath.Join(dir, "dockercopilot-2026-10-01.jsonl")
	if err := os.WriteFile(old, []byte("expired"), 0600); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(old, now.Add(-30*24*time.Hour), now.Add(-30*24*time.Hour))
	w := NewRotatingWriter(Options{Directory: dir, RetentionDays: 7, MaxFileSize: 6, MaxTotalSize: 6, Now: func() time.Time { return now }})
	defer w.Close()
	if _, err := w.Write([]byte("123456")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("abcdef")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("expired log remains: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		total += info.Size()
	}
	if total > 6 {
		t.Fatalf("retained files use %d bytes, cap is 6", total)
	}
	assertFileContent(t, filepath.Join(dir, "dockercopilot-2026-10-22.1.jsonl"), "abcdef")
}

func TestRotatingWriterRestartDoesNotOverflowExistingFile(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	active := filepath.Join(dir, "dockercopilot-2026-10-22.jsonl")
	if err := os.WriteFile(active, []byte("1234567"), 0600); err != nil {
		t.Fatal(err)
	}
	w := NewRotatingWriter(Options{Directory: dir, MaxFileSize: 8, MaxTotalSize: 32, Now: func() time.Time { return now }})
	defer w.Close()
	if _, err := w.Write([]byte("abcd")); err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, active, "1234567")
	assertFileContent(t, filepath.Join(dir, "dockercopilot-2026-10-22.1.jsonl"), "abcd")
}

func TestCleanupLeavesUnrecognizedFilesAlone(t *testing.T) {
	dir := t.TempDir()
	other := filepath.Join(dir, "dockercopilot-notes.jsonl")
	if err := os.WriteFile(other, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	w := NewRotatingWriter(Options{Directory: dir, MaxFileSize: 16, MaxTotalSize: 16})
	defer w.Close()
	if _, err := w.Write([]byte("entry")); err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, other, "keep")
}

func TestRotatingWriterDegradesAndRecoversWithoutRecursion(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(dir, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	now := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	w := NewRotatingWriter(Options{Directory: dir, MaxFileSize: 16, MaxTotalSize: 32, Now: func() time.Time { return now }, Stderr: &stderr})
	if n, err := w.Write([]byte("stdout survives")); err != nil || n != len("stdout survives") {
		t.Fatalf("Write() = %d, %v", n, err)
	}
	_ = os.Remove(dir)
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if n, err := w.Write([]byte("recovered")); err != nil || n != len("recovered") {
		t.Fatalf("Write() after recovery = %d, %v", n, err)
	}
	if got := stderr.String(); strings.Count(got, "file logging unavailable") != 1 || strings.Contains(got, "stdout survives") {
		t.Fatalf("unexpected fallback warning: %q", got)
	}
	assertFileContent(t, filepath.Join(dir, "dockercopilot-2026-10-22.jsonl"), "recovered")
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRotatingWriterRejectsOversizedRecordOnlyFromFile(t *testing.T) {
	dir := t.TempDir()
	var stderr bytes.Buffer
	w := NewRotatingWriter(Options{Directory: dir, MaxFileSize: 4, MaxTotalSize: 8, Stderr: &stderr})
	defer w.Close()
	if n, err := w.Write([]byte("too large")); err != nil || n != len("too large") {
		t.Fatalf("Write() = %d, %v", n, err)
	}
	if !strings.Contains(stderr.String(), "exceeds per-file limit") {
		t.Fatalf("missing size warning: %q", stderr.String())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("oversized record created file: %#v", entries)
	}
}

func TestFanoutWriterWritesSameRecordOnceToBothOutputs(t *testing.T) {
	var stdout, file bytes.Buffer
	w := &fanoutWriter{stdout: &stdout, file: &file}
	if n, err := w.Write([]byte("{\"level\":\"info\"}\n")); err != nil || n != len("{\"level\":\"info\"}\n") {
		t.Fatalf("Write() = %d, %v", n, err)
	}
	if stdout.String() != file.String() || strings.Count(stdout.String(), "level") != 1 {
		t.Fatalf("outputs differ or duplicate record: stdout=%q file=%q", stdout.String(), file.String())
	}
}

func TestLogPipelineSetupInSubprocess(t *testing.T) {
	root := t.TempDir()
	stdoutPath := filepath.Join(root, "stdout.log")
	stderrPath := filepath.Join(root, "stderr.log")
	firstDir := filepath.Join(root, "first")
	secondDir := filepath.Join(root, "second")
	cmd := exec.Command(os.Args[0], "-test.run=^TestLogPipelineHelper$")
	cmd.Env = append(os.Environ(),
		"DOCKERCOPILOT_LOG_HELPER=normal",
		"DOCKERCOPILOT_TEST_ROOT="+root,
		"DOCKERCOPILOT_TEST_STDOUT="+stdoutPath,
		"DOCKERCOPILOT_TEST_STDERR="+stderrPath,
		"DOCKERCOPILOT_TEST_LOG_DIR="+firstDir,
		"DOCKERCOPILOT_TEST_SECOND_DIR="+secondDir,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("log pipeline helper: %v; %s", err, output)
	}
	stdout, err := os.ReadFile(stdoutPath)
	if err != nil {
		t.Fatal(err)
	}
	filePaths, err := filepath.Glob(filepath.Join(firstDir, "dockercopilot-*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(filePaths) != 1 {
		t.Fatalf("expected one log file, got %v", filePaths)
	}
	fileContent, err := os.ReadFile(filePaths[0])
	if err != nil {
		t.Fatal(err)
	}
	stdoutEntries := parseLogEntries(t, string(stdout))
	fileEntries := parseLogEntries(t, string(fileContent))
	if len(stdoutEntries) != 2 || len(fileEntries) != 2 {
		t.Fatalf("expected two INFO/ERROR records (DEBUG filtered), stdout=%v file=%v", stdoutEntries, fileEntries)
	}
	for i := range stdoutEntries {
		if !reflect.DeepEqual(stdoutEntries[i], fileEntries[i]) {
			t.Fatalf("stdout/file record differ: %v / %v", stdoutEntries[i], fileEntries[i])
		}
	}
	if stdoutEntries[0]["content"] != "pipeline-info" || stdoutEntries[1]["content"] != "pipeline-error" {
		t.Fatalf("unexpected records: %#v", stdoutEntries)
	}
	if stdoutEntries[0]["probe"] != "field" {
		t.Fatalf("structured field missing: %#v", stdoutEntries[0])
	}
	if _, err := os.Stat(secondDir); !os.IsNotExist(err) {
		t.Fatalf("second Setup unexpectedly changed the pipeline: %v", err)
	}
	stderr, err := os.ReadFile(stderrPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(bytes.TrimSpace(stderr)) != 0 {
		t.Fatalf("unexpected fallback stderr: %s", stderr)
	}
}

func TestLogPipelineHelper(t *testing.T) {
	mode := os.Getenv("DOCKERCOPILOT_LOG_HELPER")
	if mode == "" {
		return
	}
	stdoutFile, err := os.Create(os.Getenv("DOCKERCOPILOT_TEST_STDOUT"))
	if err != nil {
		panic(err)
	}
	stderrFile, err := os.Create(os.Getenv("DOCKERCOPILOT_TEST_STDERR"))
	if err != nil {
		panic(err)
	}
	os.Stdout, os.Stderr = stdoutFile, stderrFile
	if mode == "normal" {
		if err := Setup(os.Getenv("DOCKERCOPILOT_TEST_LOG_DIR"), "info", 7, 1<<20, 2<<20); err != nil {
			panic(err)
		}
		if err := Setup(os.Getenv("DOCKERCOPILOT_TEST_SECOND_DIR"), "debug", 7, 1<<20, 2<<20); err != nil {
			panic(err)
		}
		logx.Infow("pipeline-info", logx.Field("probe", "field"))
		logx.Debug("filtered-debug")
		logx.Error("pipeline-error")
	} else {
		badDir := filepath.Join(os.Getenv("DOCKERCOPILOT_TEST_ROOT"), "not-a-directory")
		if err := os.WriteFile(badDir, []byte("file"), 0600); err != nil {
			panic(err)
		}
		if err := Setup(badDir, "info", 7, 1<<20, 2<<20); err != nil {
			panic(err)
		}
		logx.Info("stdout-on-file-failure")
	}
	if err := logx.Close(); err != nil {
		panic(err)
	}
	_ = stdoutFile.Close()
	_ = stderrFile.Close()
}

func TestLogPipelineFileFailureKeepsStdout(t *testing.T) {
	root := t.TempDir()
	stdoutPath, stderrPath := filepath.Join(root, "stdout.log"), filepath.Join(root, "stderr.log")
	cmd := exec.Command(os.Args[0], "-test.run=^TestLogPipelineHelper$")
	cmd.Env = append(os.Environ(), "DOCKERCOPILOT_LOG_HELPER=failure", "DOCKERCOPILOT_TEST_ROOT="+root, "DOCKERCOPILOT_TEST_STDOUT="+stdoutPath, "DOCKERCOPILOT_TEST_STDERR="+stderrPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("failure helper: %v; %s", err, output)
	}
	stdout, err := os.ReadFile(stdoutPath)
	if err != nil {
		t.Fatal(err)
	}
	entries := parseLogEntries(t, string(stdout))
	if len(entries) != 1 || entries[0]["content"] != "stdout-on-file-failure" {
		t.Fatalf("stdout did not retain business log: %#v", entries)
	}
	stderr, err := os.ReadFile(stderrPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(stderr), "file logging unavailable") || strings.Contains(string(stderr), "stdout-on-file-failure") {
		t.Fatalf("fallback warning missing or recursive: %q", stderr)
	}
}

func parseLogEntries(t *testing.T, content string) []map[string]any {
	t.Helper()
	var entries []map[string]any
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("invalid JSON log line %q: %v", line, err)
		}
		entries = append(entries, entry)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(fmt.Errorf("scan log lines: %w", err))
	}
	return entries
}

func assertFileContent(t *testing.T, name, want string) {
	t.Helper()
	got, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("%s content = %q, want %q", filepath.Base(name), got, want)
	}
}
