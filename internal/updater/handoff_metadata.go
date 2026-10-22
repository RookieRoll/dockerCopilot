package updater

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	handoffMetadataFile = ".dockerCopilot-handoff"
	handoffResultFile   = ".dockerCopilot-handoff-result"
	maxHandoffFileBytes = 4 << 10
)

var handoffCodePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// HandoffMetadata is the fixed, path-free sidecar contract shared with
// start.sh. It identifies only the task, target binary, version and arch.
type HandoffMetadata struct {
	TaskID        string
	HandoffID     string
	TargetVersion string
	GOARCH        string
	BinarySHA256  string
}

type HandoffResult struct {
	HandoffMetadata
	Result    string
	ErrorCode string
}

func WriteHandoffMetadata(workDir string, metadata HandoffMetadata) error {
	if err := validateHandoffMetadata(metadata); err != nil {
		return handoffError("INVALID_HANDOFF_IDENTITY", "validate handoff metadata", err)
	}
	contents := serializeHandoffMetadata(metadata, "", "")
	return writeHandoffSidecar(workDir, handoffMetadataFile, contents)
}

func ReadHandoffMetadata(workDir string) (HandoffMetadata, error) {
	values, err := readHandoffSidecar(workDir, handoffMetadataFile, false)
	if err != nil {
		return HandoffMetadata{}, err
	}
	metadata := metadataFromFields(values)
	if err := validateHandoffMetadata(metadata); err != nil {
		return HandoffMetadata{}, handoffError("INVALID_HANDOFF_METADATA", "validate handoff metadata", err)
	}
	return metadata, nil
}

func WriteHandoffResult(workDir string, result HandoffResult) error {
	if err := validateHandoffMetadata(result.HandoffMetadata); err != nil {
		return handoffError("INVALID_HANDOFF_IDENTITY", "validate handoff result", err)
	}
	if result.Result != "ready" && result.Result != "failed-start" {
		return handoffError("INVALID_HANDOFF_RESULT", "validate handoff result", errors.New("result must be ready or failed-start"))
	}
	if (result.Result == "ready" && result.ErrorCode != "") || (result.Result == "failed-start" && !handoffCodePattern.MatchString(result.ErrorCode)) {
		return handoffError("INVALID_HANDOFF_RESULT", "validate handoff result", errors.New("error_code must be empty for ready and valid for failed-start"))
	}
	contents := serializeHandoffMetadata(result.HandoffMetadata, result.Result, result.ErrorCode)
	return writeHandoffSidecar(workDir, handoffResultFile, contents)
}

func ReadHandoffResult(workDir string) (HandoffResult, error) {
	values, err := readHandoffSidecar(workDir, handoffResultFile, true)
	if err != nil {
		return HandoffResult{}, err
	}
	result := HandoffResult{HandoffMetadata: metadataFromFields(values), Result: values["result"], ErrorCode: values["error_code"]}
	if err := validateHandoffMetadata(result.HandoffMetadata); err != nil {
		return HandoffResult{}, handoffError("INVALID_HANDOFF_RESULT", "validate handoff result", err)
	}
	if result.Result != "ready" && result.Result != "failed-start" {
		return HandoffResult{}, handoffError("INVALID_HANDOFF_RESULT", "validate handoff result", errors.New("result must be ready or failed-start"))
	}
	if (result.Result == "ready" && result.ErrorCode != "") || (result.Result == "failed-start" && !handoffCodePattern.MatchString(result.ErrorCode)) {
		return HandoffResult{}, handoffError("INVALID_HANDOFF_RESULT", "validate handoff result", errors.New("invalid error_code for handoff result"))
	}
	return result, nil
}

func serializeHandoffMetadata(metadata HandoffMetadata, result, errorCode string) string {
	contents := fmt.Sprintf("schema=1\ntask_id=%s\nhandoff_id=%s\ntarget_version=%s\ngoarch=%s\nbinary_sha256=%s\n", metadata.TaskID, metadata.HandoffID, metadata.TargetVersion, metadata.GOARCH, strings.ToLower(metadata.BinarySHA256))
	if result != "" {
		contents += "result=" + result + "\nerror_code=" + errorCode + "\n"
	}
	return contents
}

func metadataFromFields(fields map[string]string) HandoffMetadata {
	return HandoffMetadata{TaskID: fields["task_id"], HandoffID: fields["handoff_id"], TargetVersion: fields["target_version"], GOARCH: fields["goarch"], BinarySHA256: fields["binary_sha256"]}
}

func validateHandoffMetadata(metadata HandoffMetadata) error {
	if !canonicalUUID(metadata.TaskID) || !canonicalUUID(metadata.HandoffID) {
		return errors.New("task_id and handoff_id must be canonical UUIDs")
	}
	if !versionPattern.MatchString(metadata.TargetVersion) {
		return errors.New("target_version is invalid")
	}
	if metadata.GOARCH != "amd64" && metadata.GOARCH != "arm64" {
		return errors.New("goarch must be amd64 or arm64")
	}
	if !validSHA256(metadata.BinarySHA256) {
		return errors.New("binary_sha256 must be a 64-character SHA256 digest")
	}
	return nil
}

func canonicalUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index, char := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if char != '-' {
				return false
			}
			continue
		}
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
			return false
		}
	}
	return true
}

func writeHandoffSidecar(workDir, name, contents string) error {
	info, err := os.Lstat(workDir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		if err == nil {
			err = errors.New("working directory must be a real directory")
		}
		return handoffError("INSTALL_DIRECTORY_UNAVAILABLE", "inspect handoff metadata directory", err)
	}
	path := filepath.Join(workDir, name)
	temporary, err := os.CreateTemp(workDir, name+".tmp-*")
	if err != nil {
		return handoffError("INSTALL_DIRECTORY_UNAVAILABLE", "create handoff metadata", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return handoffError("INSTALL_DIRECTORY_UNAVAILABLE", "secure handoff metadata", err)
	}
	if _, err := io.WriteString(temporary, contents); err != nil {
		_ = temporary.Close()
		return handoffError("INSTALL_DIRECTORY_UNAVAILABLE", "write handoff metadata", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return handoffError("INSTALL_DIRECTORY_UNAVAILABLE", "sync handoff metadata", err)
	}
	if err := temporary.Close(); err != nil {
		return handoffError("INSTALL_DIRECTORY_UNAVAILABLE", "close handoff metadata", err)
	}
	if err := atomicReplaceHandoffFile(temporaryPath, path); err != nil {
		return handoffError("INSTALL_DIRECTORY_UNAVAILABLE", "publish handoff metadata", err)
	}
	if err := syncHandoffDirectory(workDir); err != nil {
		return handoffError("INSTALL_DIRECTORY_UNAVAILABLE", "sync handoff metadata directory", err)
	}
	return nil
}

func readHandoffSidecar(workDir, name string, hasResult bool) (map[string]string, error) {
	path := filepath.Join(workDir, name)
	info, err := os.Lstat(path)
	if err != nil {
		return nil, handoffError("HANDOFF_METADATA_UNAVAILABLE", "read handoff metadata", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= 0 || info.Size() > maxHandoffFileBytes {
		return nil, handoffError("INVALID_HANDOFF_METADATA", "read handoff metadata", errors.New("metadata must be a small regular non-symlink file"))
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, handoffError("HANDOFF_METADATA_UNAVAILABLE", "read handoff metadata", err)
	}
	defer file.Close()
	fields := make(map[string]string)
	scanner := bufio.NewScanner(io.LimitReader(file, maxHandoffFileBytes+1))
	scanner.Buffer(make([]byte, 256), maxHandoffFileBytes+1)
	allowed := map[string]bool{"schema": true, "task_id": true, "handoff_id": true, "target_version": true, "goarch": true, "binary_sha256": true}
	if hasResult {
		allowed["result"], allowed["error_code"] = true, true
	}
	for scanner.Scan() {
		line := scanner.Text()
		key, value, ok := strings.Cut(line, "=")
		if !ok || key == "" || strings.Contains(value, "=") || !allowed[key] {
			return nil, handoffError("INVALID_HANDOFF_METADATA", "parse handoff metadata", errors.New("unknown or malformed metadata field"))
		}
		if _, duplicate := fields[key]; duplicate {
			return nil, handoffError("INVALID_HANDOFF_METADATA", "parse handoff metadata", fmt.Errorf("duplicate metadata field %q", key))
		}
		fields[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, handoffError("INVALID_HANDOFF_METADATA", "parse handoff metadata", err)
	}
	required := []string{"schema", "task_id", "handoff_id", "target_version", "goarch", "binary_sha256"}
	if hasResult {
		required = append(required, "result", "error_code")
	}
	if len(fields) != len(required) || fields["schema"] != "1" {
		return nil, handoffError("INVALID_HANDOFF_METADATA", "parse handoff metadata", errors.New("metadata fields do not match schema 1"))
	}
	for _, key := range required {
		if _, ok := fields[key]; !ok {
			return nil, handoffError("INVALID_HANDOFF_METADATA", "parse handoff metadata", fmt.Errorf("required field %q is missing", key))
		}
	}
	return fields, nil
}
