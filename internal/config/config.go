package config

import (
	"fmt"
	"math"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/rest"
)

type Config struct {
	rest.RestConf
	Auth struct { // JWT 认证需要的密钥和过期时间配置
		AccessSecret string
		AccessExpire int64
	}
	Tasks   TaskConfig    `json:",optional"`
	Logging LoggingConfig `json:",optional"`
	Updates UpdateConfig  `json:",optional"`
	Backup  BackupConfig  `json:",optional"`
	Docker  DockerConfig  `json:",optional"`
}

type TaskConfig struct {
	Directory          string `json:",optional,default=/data/tasks"`
	MaxConcurrent      *int   `json:",optional"`
	QueueCapacity      *int   `json:",optional"`
	Timeout            string `json:",optional,default=30m"`
	Retention          string `json:",optional,default=168h"`
	MaxRecords         *int   `json:",optional"`
	HealthcheckWait    string `json:",optional,default=120s"`
	RunningObservation string `json:",optional,default=10s"`
	RecoveryTimeout    string `json:",optional,default=2m"`
}

type LoggingConfig struct {
	Directory       string `json:",optional,default=/data/logs"`
	Level           string `json:",optional,default=info"`
	RetentionDays   *int   `json:",optional"`
	MaxFileSizeMiB  *int64 `json:",optional"`
	MaxTotalSizeMiB *int64 `json:",optional"`
}

type UpdateConfig struct {
	Repository  string `json:",optional,default=RookieRoll/dockerCopilot"`
	GitHubProxy string `json:",optional"`
}

type BackupConfig struct {
	Directory string `json:",optional,default=/data/backups"`
}

type DockerConfig struct {
	DeleteOldContainer *bool  `json:",optional"`
	PortOverridesPath  string `json:",optional,default=/data/config/container-port-overrides.json"`
}

type TaskOptions struct {
	Directory          string
	MaxConcurrent      int
	QueueCapacity      int
	Timeout            time.Duration
	Retention          time.Duration
	MaxRecords         int
	HealthcheckWait    time.Duration
	RunningObservation time.Duration
	RecoveryTimeout    time.Duration
}

type LogOptions struct {
	Directory       string
	Level           string
	RetentionDays   int
	MaxFileSizeMiB  int64
	MaxTotalSizeMiB int64
}

var (
	Version   string
	BuildDate string
)

const (
	defaultTaskDir         = "/data/tasks"
	defaultTaskTimeout     = "30m"
	defaultTaskRetention   = "168h"
	defaultHealthWait      = "120s"
	defaultRunningObserve  = "10s"
	defaultRecoveryTimeout = "2m"
	defaultBackupDir       = "/data/backups"
	defaultLogDir          = "/data/logs"
	defaultPortPath        = "/data/config/container-port-overrides.json"
	defaultUpdateRepo      = "RookieRoll/dockerCopilot"
)

// ApplyDefaults fills configuration values that may be omitted by older
// configuration files. It does not read process environment variables.
func (c *Config) ApplyDefaults() {
	if c.Tasks.Directory == "" {
		c.Tasks.Directory = defaultTaskDir
	}
	if c.Tasks.MaxConcurrent == nil {
		c.Tasks.MaxConcurrent = intPointer(2)
	}
	if c.Tasks.QueueCapacity == nil {
		c.Tasks.QueueCapacity = intPointer(100)
	}
	if c.Tasks.MaxRecords == nil {
		c.Tasks.MaxRecords = intPointer(1000)
	}
	if c.Logging.RetentionDays == nil {
		c.Logging.RetentionDays = intPointer(7)
	}
	if c.Logging.MaxFileSizeMiB == nil {
		c.Logging.MaxFileSizeMiB = int64Pointer(20)
	}
	if c.Logging.MaxTotalSizeMiB == nil {
		c.Logging.MaxTotalSizeMiB = int64Pointer(200)
	}
	if c.Tasks.Timeout == "" {
		c.Tasks.Timeout = defaultTaskTimeout
	}
	if c.Tasks.Retention == "" {
		c.Tasks.Retention = defaultTaskRetention
	}
	if c.Tasks.HealthcheckWait == "" {
		c.Tasks.HealthcheckWait = defaultHealthWait
	}
	if c.Tasks.RunningObservation == "" {
		c.Tasks.RunningObservation = defaultRunningObserve
	}
	if c.Tasks.RecoveryTimeout == "" {
		c.Tasks.RecoveryTimeout = defaultRecoveryTimeout
	}
	if c.Logging.Directory == "" {
		c.Logging.Directory = defaultLogDir
	}
	if c.Logging.Level == "" {
		c.Logging.Level = "info"
	}
	if c.Backup.Directory == "" {
		c.Backup.Directory = defaultBackupDir
	}
	if c.Updates.Repository == "" {
		c.Updates.Repository = defaultUpdateRepo
	}
	if c.Docker.PortOverridesPath == "" {
		c.Docker.PortOverridesPath = defaultPortPath
	}
}

// ApplyEnvironment applies documented environment overrides. Legacy variable
// names remain supported, with environment values taking precedence over YAML.
func (c *Config) ApplyEnvironment() error {
	intOverrides := []struct {
		name string
		dest **int
	}{
		{"DOCKERCOPILOT_TASK_MAX_CONCURRENT", &c.Tasks.MaxConcurrent},
		{"DOCKERCOPILOT_TASK_QUEUE_CAPACITY", &c.Tasks.QueueCapacity},
		{"DOCKERCOPILOT_TASK_MAX_RECORDS", &c.Tasks.MaxRecords},
		{"DOCKERCOPILOT_LOG_RETENTION_DAYS", &c.Logging.RetentionDays},
	}
	for _, override := range intOverrides {
		if override.dest == nil {
			continue
		}
		if raw, ok := os.LookupEnv(override.name); ok {
			value, err := strconv.Atoi(strings.TrimSpace(raw))
			if err != nil {
				return fmt.Errorf("%s must be an integer: %w", override.name, err)
			}
			*override.dest = &value
		}
	}
	int64Overrides := []struct {
		name string
		dest **int64
	}{
		{"DOCKERCOPILOT_LOG_MAX_FILE_MIB", &c.Logging.MaxFileSizeMiB},
		{"DOCKERCOPILOT_LOG_MAX_TOTAL_MIB", &c.Logging.MaxTotalSizeMiB},
	}
	for _, override := range int64Overrides {
		if raw, ok := os.LookupEnv(override.name); ok {
			value, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
			if err != nil {
				return fmt.Errorf("%s must be an integer: %w", override.name, err)
			}
			*override.dest = &value
		}
	}
	stringOverrides := []struct {
		name string
		dest *string
	}{
		{"DOCKERCOPILOT_TASK_TIMEOUT", &c.Tasks.Timeout},
		{"DOCKERCOPILOT_TASK_DIR", &c.Tasks.Directory},
		{"DOCKERCOPILOT_TASK_RETENTION", &c.Tasks.Retention},
		{"DOCKERCOPILOT_TASK_HEALTHCHECK_WAIT", &c.Tasks.HealthcheckWait},
		{"DOCKERCOPILOT_TASK_RUNNING_OBSERVATION", &c.Tasks.RunningObservation},
		{"DOCKERCOPILOT_TASK_RECOVERY_TIMEOUT", &c.Tasks.RecoveryTimeout},
		{"DOCKERCOPILOT_LOG_DIR", &c.Logging.Directory},
		{"DOCKERCOPILOT_LOG_LEVEL", &c.Logging.Level},
		{"DOCKERCOPILOT_UPDATE_REPOSITORY", &c.Updates.Repository},
		{"githubProxy", &c.Updates.GitHubProxy},
		{"BACKUP_DIR", &c.Backup.Directory},
		{"CONTAINER_PORT_OVERRIDES_PATH", &c.Docker.PortOverridesPath},
	}
	for _, override := range stringOverrides {
		if raw, ok := os.LookupEnv(override.name); ok {
			*override.dest = strings.TrimSpace(raw)
		}
	}
	if raw, ok := os.LookupEnv("DelOldContainer"); ok {
		value, err := strconv.ParseBool(strings.TrimSpace(raw))
		if err != nil {
			return fmt.Errorf("DelOldContainer must be a boolean: %w", err)
		}
		c.Docker.DeleteOldContainer = &value
	}
	if raw, ok := os.LookupEnv("DOCKERCOPILOT_GITHUB_PROXY"); ok {
		c.Updates.GitHubProxy = strings.TrimSpace(raw)
	}
	return nil
}

func (c *Config) Validate() error {
	c.ApplyDefaults()
	if *c.Tasks.MaxConcurrent < 1 || *c.Tasks.MaxConcurrent > 64 {
		return fmt.Errorf("Tasks.MaxConcurrent must be between 1 and 64")
	}
	if *c.Tasks.QueueCapacity < 1 || *c.Tasks.QueueCapacity > 10000 {
		return fmt.Errorf("Tasks.QueueCapacity must be between 1 and 10000")
	}
	timeout, err := time.ParseDuration(c.Tasks.Timeout)
	if err != nil || timeout <= 0 {
		return fmt.Errorf("Tasks.Timeout must be a positive duration")
	}
	retention, err := time.ParseDuration(c.Tasks.Retention)
	if err != nil || retention <= 0 {
		return fmt.Errorf("Tasks.Retention must be a positive duration")
	}
	if err := validatePositiveDuration("Tasks.HealthcheckWait", c.Tasks.HealthcheckWait); err != nil {
		return err
	}
	if err := validatePositiveDuration("Tasks.RunningObservation", c.Tasks.RunningObservation); err != nil {
		return err
	}
	if err := validatePositiveDuration("Tasks.RecoveryTimeout", c.Tasks.RecoveryTimeout); err != nil {
		return err
	}
	if *c.Tasks.MaxRecords < 1 {
		return fmt.Errorf("Tasks.MaxRecords must be positive")
	}
	if *c.Logging.RetentionDays < 1 || *c.Logging.MaxFileSizeMiB < 1 || *c.Logging.MaxTotalSizeMiB < 1 {
		return fmt.Errorf("logging retention and size limits must be positive")
	}
	const bytesPerMiB int64 = 1024 * 1024
	const maxInt64 = int64(math.MaxInt64)
	if *c.Logging.MaxFileSizeMiB > maxInt64/bytesPerMiB || *c.Logging.MaxTotalSizeMiB > maxInt64/bytesPerMiB {
		return fmt.Errorf("logging size limits are too large")
	}
	if *c.Logging.MaxFileSizeMiB > *c.Logging.MaxTotalSizeMiB {
		return fmt.Errorf("Logging.MaxFileSizeMiB cannot exceed Logging.MaxTotalSizeMiB")
	}
	c.Logging.Level = strings.ToLower(strings.TrimSpace(c.Logging.Level))
	if level := c.Logging.Level; level != "debug" && level != "info" && level != "error" && level != "severe" {
		return fmt.Errorf("Logging.Level must be debug, info, error, or severe")
	}
	if strings.TrimSpace(c.Tasks.Directory) == "" || strings.TrimSpace(c.Backup.Directory) == "" || strings.TrimSpace(c.Logging.Directory) == "" || strings.TrimSpace(c.Docker.PortOverridesPath) == "" {
		return fmt.Errorf("task, backup, log, and port override paths must not be empty")
	}
	if strings.TrimSpace(c.Updates.Repository) == "" {
		return fmt.Errorf("Updates.Repository must not be empty")
	}
	if !validRepository.MatchString(c.Updates.Repository) || strings.Contains(c.Updates.Repository, "..") {
		return fmt.Errorf("Updates.Repository must use owner/repository format")
	}
	if proxy := strings.TrimSpace(c.Updates.GitHubProxy); proxy != "" {
		parsed, parseErr := url.Parse(proxy)
		if parseErr != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return fmt.Errorf("Updates.GitHubProxy must be an http or https URL without credentials, query, or fragment")
		}
	}
	return nil
}

var validRepository = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

func validatePositiveDuration(name, value string) error {
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return fmt.Errorf("%s must be a positive duration", name)
	}
	return nil
}

func intPointer(value int) *int       { return &value }
func int64Pointer(value int64) *int64 { return &value }

type configLoader Config

// LoadFile loads config without invoking go-zero's automatic Validate hook
// before environment overrides. Defaults and validation run on merged values.
func LoadFile(filename string, useEnvironment bool) (Config, error) {
	var cfg Config
	options := make([]conf.Option, 0, 1)
	if useEnvironment {
		options = append(options, conf.UseEnv())
	}
	if err := conf.Load(filename, (*configLoader)(&cfg), options...); err != nil {
		return Config{}, err
	}
	cfg.ApplyDefaults()
	if useEnvironment {
		if err := cfg.ApplyEnvironment(); err != nil {
			return Config{}, err
		}
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) TaskOptions() (TaskOptions, error) {
	c.ApplyDefaults()
	timeout, err := time.ParseDuration(c.Tasks.Timeout)
	if err != nil {
		return TaskOptions{}, err
	}
	retention, err := time.ParseDuration(c.Tasks.Retention)
	if err != nil {
		return TaskOptions{}, err
	}
	healthcheckWait, err := time.ParseDuration(c.Tasks.HealthcheckWait)
	if err != nil {
		return TaskOptions{}, err
	}
	runningObservation, err := time.ParseDuration(c.Tasks.RunningObservation)
	if err != nil {
		return TaskOptions{}, err
	}
	recoveryTimeout, err := time.ParseDuration(c.Tasks.RecoveryTimeout)
	if err != nil {
		return TaskOptions{}, err
	}
	return TaskOptions{Directory: c.Tasks.Directory, MaxConcurrent: *c.Tasks.MaxConcurrent, QueueCapacity: *c.Tasks.QueueCapacity, Timeout: timeout, Retention: retention, MaxRecords: *c.Tasks.MaxRecords, HealthcheckWait: healthcheckWait, RunningObservation: runningObservation, RecoveryTimeout: recoveryTimeout}, nil
}

func (c Config) LogOptions() LogOptions {
	c.ApplyDefaults()
	return LogOptions{Directory: c.Logging.Directory, Level: c.Logging.Level, RetentionDays: *c.Logging.RetentionDays, MaxFileSizeMiB: *c.Logging.MaxFileSizeMiB, MaxTotalSizeMiB: *c.Logging.MaxTotalSizeMiB}
}

// ResolveDeleteOldContainer applies an optional request override over the
// configured default. nil means the client omitted the option.
func (c Config) ResolveDeleteOldContainer(requestValue *bool) bool {
	if requestValue != nil {
		return *requestValue
	}
	return c.Docker.DeleteOldContainer == nil || *c.Docker.DeleteOldContainer
}

func (c Config) BackupDirectory() string {
	if value := strings.TrimSpace(c.Backup.Directory); value != "" {
		return value
	}
	return LegacyBackupDirectory()
}

func (c Config) PortOverridesFilePath() string {
	if value := strings.TrimSpace(c.Docker.PortOverridesPath); value != "" {
		return value
	}
	return LegacyPortOverridesPath()
}

func (c Config) UpdateSource() UpdateConfig {
	if strings.TrimSpace(c.Updates.Repository) == "" {
		c.Updates.Repository = defaultUpdateRepo
	}
	return c.Updates
}

// LegacyUpdateSource supports callers that have not yet received a loaded
// Config. Normal request paths must use Config.UpdateSource instead.
func LegacyUpdateSource() UpdateConfig {
	cfg := Config{}
	cfg.ApplyDefaults()
	cfg.Updates.GitHubProxy = strings.TrimSpace(os.Getenv("githubProxy"))
	return cfg.Updates
}

func (c UpdateConfig) versionURL() string {
	return c.urlPrefix() + "https://raw.githubusercontent.com/" + c.Repository + "/latest/version"
}

func (c UpdateConfig) releaseBaseURL() string {
	return c.urlPrefix() + "https://github.com/" + c.Repository + "/releases/download"
}

func (c UpdateConfig) urlPrefix() string {
	if proxy := strings.TrimSpace(c.GitHubProxy); proxy != "" {
		return strings.TrimRight(proxy, "/") + "/"
	}
	return ""
}

func (c UpdateConfig) VersionURL() string     { return c.versionURL() }
func (c UpdateConfig) ReleaseBaseURL() string { return c.releaseBaseURL() }

// LegacyBackupDirectory provides compatibility for utility callers that have
// not yet been migrated to receive the loaded Config explicitly.
func LegacyBackupDirectory() string {
	if value := strings.TrimSpace(os.Getenv("BACKUP_DIR")); value != "" {
		return value
	}
	return defaultBackupDir
}

// LegacyPortOverridesPath preserves the old utility API while centralizing its
// environment variable name and default.
func LegacyPortOverridesPath(fallback ...string) string {
	if value := strings.TrimSpace(os.Getenv("CONTAINER_PORT_OVERRIDES_PATH")); value != "" {
		return value
	}
	if len(fallback) > 0 && strings.TrimSpace(fallback[0]) != "" {
		return fallback[0]
	}
	return defaultPortPath
}
