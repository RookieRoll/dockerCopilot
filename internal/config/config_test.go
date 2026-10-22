package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadLegacyConfigAndApplyDefaults(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "legacy.yaml")
	content := []byte("Name: dockerCopilot\nHost: 0.0.0.0\nPort: 12712\nTimeout: 100000000000000\nLog:\n  Encoding: plain\nAuth:\n  AccessSecret: test-secret\n  AccessExpire: 7257600\n")
	if err := os.WriteFile(filename, content, 0o600); err != nil {
		t.Fatal(err)
	}
	var cfg Config
	loaded, err := LoadFile(filename, false)
	if err != nil {
		t.Fatalf("load legacy config: %v", err)
	}
	cfg = loaded
	if *cfg.Tasks.MaxConcurrent != 2 || *cfg.Tasks.QueueCapacity != 100 || *cfg.Tasks.MaxRecords != 1000 {
		t.Fatalf("unexpected task defaults: %#v", cfg.Tasks)
	}
	if cfg.Tasks.Timeout != "30m" || cfg.Tasks.Retention != "168h" || cfg.Tasks.HealthcheckWait != "120s" || cfg.Tasks.RunningObservation != "10s" || cfg.Tasks.RecoveryTimeout != "2m" {
		t.Fatalf("unexpected task durations: %#v", cfg.Tasks)
	}
	if cfg.Logging.Directory != "/data/logs" || *cfg.Logging.RetentionDays != 7 || *cfg.Logging.MaxFileSizeMiB != 20 || *cfg.Logging.MaxTotalSizeMiB != 200 {
		t.Fatalf("unexpected logging defaults: %#v", cfg.Logging)
	}
	if cfg.Backup.Directory != "/data/backups" || cfg.Updates.Repository != "RookieRoll/dockerCopilot" || cfg.Docker.PortOverridesPath != defaultPortPath {
		t.Fatalf("unexpected path or update defaults: %#v", cfg)
	}
}

func TestLoadRepositoryConfigAndRejectExplicitZero(t *testing.T) {
	var cfg Config
	loaded, err := LoadFile(filepath.Join("..", "..", "etc", "dockerCopilot.yaml"), false)
	if err != nil {
		t.Fatalf("load repository config: %v", err)
	}
	cfg = loaded
	if cfg.Tasks.Directory != "/data/tasks" || *cfg.Tasks.MaxConcurrent != 2 || cfg.Updates.Repository != defaultUpdateRepo {
		t.Fatalf("repository config values did not load: %#v", cfg)
	}

	filename := filepath.Join(t.TempDir(), "invalid.yaml")
	content := []byte("Name: dockerCopilot\nHost: 0.0.0.0\nPort: 12712\nTimeout: 100000000000000\nLog:\n  Encoding: plain\nAuth:\n  AccessSecret: test-secret\n  AccessExpire: 7257600\nTasks:\n  QueueCapacity: 0\n")
	if err := os.WriteFile(filename, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(filename, false); err == nil {
		t.Fatal("expected explicit zero queue capacity to fail during config load")
	}
}

func TestEnvironmentOverridesInvalidYAMLBeforeValidation(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "config.yaml")
	content := []byte("Name: dockerCopilot\nHost: 0.0.0.0\nPort: 12712\nTimeout: 100000000000000\nLog:\n  Encoding: plain\nAuth:\n  AccessSecret: test-secret\n  AccessExpire: 7257600\nTasks:\n  MaxConcurrent: -1\n")
	if err := os.WriteFile(filename, content, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKERCOPILOT_TASK_MAX_CONCURRENT", "3")
	loaded, err := LoadFile(filename, true)
	if err != nil {
		t.Fatalf("environment override should replace invalid YAML value before validation: %v", err)
	}
	if *loaded.Tasks.MaxConcurrent != 3 {
		t.Fatalf("got max concurrent %d, want 3", *loaded.Tasks.MaxConcurrent)
	}
}

func TestEnvironmentOverridesAndRequestDeleteOldContainer(t *testing.T) {
	cfg := Config{}
	cfg.ApplyDefaults()
	t.Setenv("BACKUP_DIR", filepath.Join(t.TempDir(), "backup"))
	t.Setenv("DelOldContainer", "false")
	t.Setenv("githubProxy", "https://proxy.example/prefix/")
	t.Setenv("CONTAINER_PORT_OVERRIDES_PATH", filepath.Join(t.TempDir(), "ports.json"))
	t.Setenv("DOCKERCOPILOT_TASK_MAX_CONCURRENT", "3")
	t.Setenv("DOCKERCOPILOT_TASK_TIMEOUT", "45m")
	if err := cfg.ApplyEnvironment(); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.Backup.Directory != os.Getenv("BACKUP_DIR") || cfg.Updates.GitHubProxy != os.Getenv("githubProxy") || cfg.Docker.PortOverridesPath != os.Getenv("CONTAINER_PORT_OVERRIDES_PATH") {
		t.Fatalf("legacy environment overrides not applied: %#v", cfg)
	}
	if *cfg.Tasks.MaxConcurrent != 3 || cfg.Tasks.Timeout != "45m" {
		t.Fatalf("new environment overrides not applied: %#v", cfg.Tasks)
	}
	if cfg.ResolveDeleteOldContainer(nil) {
		t.Fatal("legacy false default should be preserved")
	}
	requestValue := true
	if !cfg.ResolveDeleteOldContainer(&requestValue) {
		t.Fatal("request value should take precedence over configured default")
	}
	options, err := cfg.TaskOptions()
	if err != nil || options.Timeout != 45*time.Minute || options.HealthcheckWait != 120*time.Second || options.RunningObservation != 10*time.Second {
		t.Fatalf("unexpected task options %#v, err=%v", options, err)
	}
}

func TestExplicitEmptyCanonicalProxyClearsLegacyProxy(t *testing.T) {
	cfg := Config{}
	cfg.ApplyDefaults()
	t.Setenv("githubProxy", "https://legacy.proxy.invalid/prefix/")
	t.Setenv("DOCKERCOPILOT_GITHUB_PROXY", "")
	if err := cfg.ApplyEnvironment(); err != nil {
		t.Fatal(err)
	}
	if cfg.Updates.GitHubProxy != "" || cfg.UpdateSource().GitHubProxy != "" {
		t.Fatalf("explicit empty canonical proxy should clear legacy value; loaded=%q source=%q", cfg.Updates.GitHubProxy, cfg.UpdateSource().GitHubProxy)
	}
}

func TestProgramWorkDirAndSelfContainerEnvironmentAreCentralized(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "config.yaml")
	content := []byte("Name: dockerCopilot\nHost: 0.0.0.0\nPort: 12712\nTimeout: 100000000000000\nLog:\n  Encoding: plain\nAuth:\n  AccessSecret: test-secret\n  AccessExpire: 7257600\nUpdates:\n  WorkDir: /yaml/install\nDocker:\n  SelfContainer: named-container\n")
	if err := os.WriteFile(filename, content, 0o600); err != nil {
		t.Fatal(err)
	}
	envWorkDir := filepath.Join(t.TempDir(), "env-install")
	t.Setenv("WORKDIR", envWorkDir)
	t.Setenv("DOCKERCOPILOT_SELF_CONTAINER", "copilot-prod")
	loaded, err := LoadFile(filename, true)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.UpdateSource().WorkDir != envWorkDir || loaded.Docker.SelfContainer != "copilot-prod" {
		t.Fatalf("environment values did not override YAML: updates=%+v docker=%+v", loaded.Updates, loaded.Docker)
	}
	resolved, err := loaded.ProgramWorkDir()
	if err != nil || resolved != envWorkDir {
		t.Fatalf("ProgramWorkDir=%q err=%v", resolved, err)
	}

	loaded.Updates.WorkDir = ""
	resolved, err = loaded.ProgramWorkDir()
	executable, exeErr := os.Executable()
	if err != nil || exeErr != nil || resolved != filepath.Dir(executable) {
		t.Fatalf("executable fallback workdir=%q err=%v exeErr=%v", resolved, err, exeErr)
	}
	loaded.Docker.SelfContainer = "../another-container"
	if err := loaded.Validate(); err == nil {
		t.Fatal("expected unsafe self-container reference to fail validation")
	}
}

func TestValidateRejectsInvalidExplicitOverrides(t *testing.T) {
	cfg := Config{}
	cfg.ApplyDefaults()
	t.Setenv("DOCKERCOPILOT_TASK_MAX_CONCURRENT", "0")
	if err := cfg.ApplyEnvironment(); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected zero worker count to fail validation")
	}

	cfg = Config{}
	cfg.ApplyDefaults()
	zero := 0
	cfg.Tasks.QueueCapacity = &zero
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected explicit zero queue capacity to fail validation")
	}
	cfg = Config{}
	cfg.ApplyDefaults()
	cfg.Updates.Repository = "owner/../repo"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected invalid repository path to fail validation")
	}
	cfg.Updates.Repository = defaultUpdateRepo
	cfg.Updates.GitHubProxy = "https://user:secret@proxy.example"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected proxy credentials to fail validation")
	} else if strings.Contains(err.Error(), "secret") {
		t.Fatalf("validation error leaked proxy credentials: %v", err)
	}
	cfg.Updates.GitHubProxy = ""
	cfg.Tasks.Directory = "   "
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected blank task directory to fail validation")
	}
	cfg.Tasks.Directory = defaultTaskDir
	tooLarge := int64(^uint64(0) >> 1)
	cfg.Logging.MaxTotalSizeMiB = &tooLarge
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected log size that overflows byte conversion to fail validation")
	}
	cfg.Logging.MaxTotalSizeMiB = int64Pointer(200)
	cfg.Logging.Level = "DEBUG"
	if err := cfg.Validate(); err != nil || cfg.Logging.Level != "debug" {
		t.Fatalf("expected log level normalization to debug, got %q, err=%v", cfg.Logging.Level, err)
	}
}
