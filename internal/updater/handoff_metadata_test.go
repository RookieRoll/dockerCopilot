package updater

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestHandoffSidecarRoundTripAndStrictParsing(t *testing.T) {
	dir := t.TempDir()
	want := testHandoffMetadata()
	if err := WriteHandoffMetadata(dir, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadHandoffMetadata(dir)
	if err != nil || got != want {
		t.Fatalf("ReadHandoffMetadata() = %#v, %v; want %#v", got, err, want)
	}
	result := HandoffResult{HandoffMetadata: want, Result: "ready"}
	if err := WriteHandoffResult(dir, result); err != nil {
		t.Fatal(err)
	}
	gotResult, err := ReadHandoffResult(dir)
	if err != nil || gotResult != result {
		t.Fatalf("ReadHandoffResult() = %#v, %v; want %#v", gotResult, err, result)
	}
	// A new failed result atomically replaces the previous ready result.
	result.Result, result.ErrorCode = "failed-start", "candidate_exit"
	if err := WriteHandoffResult(dir, result); err != nil {
		t.Fatal(err)
	}
	gotResult, err = ReadHandoffResult(dir)
	if err != nil || gotResult != result {
		t.Fatalf("replacement result = %#v, %v; want %#v", gotResult, err, result)
	}
	if info, err := os.Stat(filepath.Join(dir, handoffMetadataFile)); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm()&0o077 != 0 && runtime.GOOS != "windows" {
		t.Fatalf("metadata permissions = %o, want owner-only", info.Mode().Perm())
	}
}

func TestHandoffSidecarRejectsMalformedFields(t *testing.T) {
	base := serializeHandoffMetadata(testHandoffMetadata(), "", "")
	cases := map[string]string{
		"duplicate": strings.Replace(base, "schema=1\n", "schema=1\nschema=1\n", 1),
		"unknown":   base + "path=/tmp/binary\n",
		"missing":   strings.Replace(base, "goarch=amd64\n", "", 1),
		"uuid":      strings.Replace(base, "task_id=123e4567-e89b-12d3-a456-426614174000", "task_id=../bad", 1),
		"version":   strings.Replace(base, "target_version=2.2.2", "target_version=<html>", 1),
		"arch":      strings.Replace(base, "goarch=amd64", "goarch=386", 1),
		"digest":    strings.Replace(base, strings.Repeat("a", 64), "short", 1),
		"equals":    base + "unexpected=a=b\n",
	}
	for name, contents := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, handoffMetadataFile), []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadHandoffMetadata(dir); err == nil {
				t.Fatal("ReadHandoffMetadata() unexpectedly accepted malformed sidecar")
			}
		})
	}
}

func TestHandoffResultRequiresValidStatusAndIdentity(t *testing.T) {
	dir := t.TempDir()
	metadata := testHandoffMetadata()
	for _, result := range []HandoffResult{
		{HandoffMetadata: metadata, Result: "ready", ErrorCode: "candidate_exit"},
		{HandoffMetadata: metadata, Result: "failed-start"},
		{HandoffMetadata: metadata, Result: "unknown"},
	} {
		if err := WriteHandoffResult(dir, result); err == nil {
			t.Fatalf("WriteHandoffResult(%#v) unexpectedly succeeded", result)
		}
	}
	valid := HandoffResult{HandoffMetadata: metadata, Result: "ready"}
	if err := WriteHandoffResult(dir, valid); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(filepath.Join(dir, handoffResultFile))
	if err != nil {
		t.Fatal(err)
	}
	contents = []byte(strings.Replace(string(contents), "handoff_id=123e4567-e89b-12d3-a456-426614174001", "handoff_id=123e4567-e89b-12d3-a456-426614174009", 1))
	if err := os.WriteFile(filepath.Join(dir, handoffResultFile), contents, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadHandoffResult(dir)
	if err != nil || got.HandoffID == metadata.HandoffID {
		t.Fatalf("reader should preserve mismatched identity for caller comparison: %#v, %v", got, err)
	}
}

func testHandoffMetadata() HandoffMetadata {
	return HandoffMetadata{
		TaskID: "123e4567-e89b-12d3-a456-426614174000", HandoffID: "123e4567-e89b-12d3-a456-426614174001",
		TargetVersion: "2.2.2", GOARCH: "amd64", BinarySHA256: strings.Repeat("a", 64),
	}
}
