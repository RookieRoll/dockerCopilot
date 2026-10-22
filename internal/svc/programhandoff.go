package svc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	containertypes "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/onlyLTY/dockerCopilot/internal/config"
	"github.com/onlyLTY/dockerCopilot/internal/task"
	"github.com/onlyLTY/dockerCopilot/internal/updater"
)

var dockerIDReference = regexp.MustCompile(`^[0-9a-fA-F]{12,64}$`)

type ProgramHandoffOptions struct {
	WorkDir       string
	StagingParent string
	SelfContainer string
}

type programInstallValidator struct {
	config         config.Config
	docker         *client.Client
	workDir        string
	executablePath string
}

func newProgramInstallValidator(c config.Config, docker *client.Client, workDir string) *programInstallValidator {
	return &programInstallValidator{config: c, docker: docker, workDir: workDir}
}

func (v *programInstallValidator) VerifyPendingInstall(ctx context.Context, record task.Record) (task.PendingInstallProof, error) {
	marker := record.PendingInstall
	if marker == nil || record.Type != task.TaskTypeProgramUpdate {
		return task.PendingInstallProof{}, errors.New("pending program handoff record is incomplete")
	}
	if err := v.validateRuntime(ctx); err != nil {
		return task.PendingInstallProof{}, err
	}
	metadata, err := updater.ReadHandoffMetadata(v.workDir)
	if err != nil {
		return task.PendingInstallProof{}, err
	}
	if metadata.TaskID != record.TaskID || metadata.HandoffID != marker.HandoffID || metadata.TargetVersion != marker.TargetVersion || metadata.GOARCH != marker.GOARCH || !strings.EqualFold(metadata.BinarySHA256, marker.ArtifactSHA256) {
		return task.PendingInstallProof{}, errors.New("handoff sidecar identity does not match the durable task")
	}
	executable, err := v.currentExecutable()
	if err != nil {
		return task.PendingInstallProof{}, fmt.Errorf("resolve running program: %w", err)
	}
	handoff := updater.InstallHandoff{TaskID: record.TaskID, TargetVersion: marker.TargetVersion, GOARCH: marker.GOARCH, BinarySHA256: marker.ArtifactSHA256}
	if err := updater.VerifyRunningProgram(ctx, executable, config.Version, handoff, 256<<20); err != nil {
		return task.PendingInstallProof{}, err
	}
	return task.PendingInstallProof{
		Verified: true, Ready: false, TaskID: record.TaskID, HandoffID: marker.HandoffID,
		TargetVersion: marker.TargetVersion, GOARCH: marker.GOARCH, ArtifactSHA256: marker.ArtifactSHA256,
	}, nil
}

func (v *programInstallValidator) validateRuntime(ctx context.Context) error {
	if v.docker == nil {
		return errors.New("Docker client is unavailable for program handoff verification")
	}
	if os.Getenv("DOCKERCOPILOT_HANDOFF_PROTOCOL") != "1" {
		return errors.New("program was not started by the handoff-capable launcher; deploy the current start.sh before online updates")
	}
	if strings.TrimSpace(v.workDir) == "" {
		return errors.New("program working directory is not configured")
	}
	workDir, err := filepath.Abs(filepath.Clean(v.workDir))
	if err != nil {
		return fmt.Errorf("resolve program working directory: %w", err)
	}
	workInfo, err := os.Lstat(workDir)
	if err != nil || !workInfo.IsDir() || workInfo.Mode()&os.ModeSymlink != 0 {
		if err == nil {
			err = errors.New("program working directory must be a real directory")
		}
		return fmt.Errorf("program working directory is unavailable: %w", err)
	}
	executable, err := v.currentExecutable()
	if err != nil {
		return fmt.Errorf("resolve running executable: %w", err)
	}
	executable, err = filepath.Abs(executable)
	if err != nil || filepath.Clean(filepath.Dir(executable)) != workDir || filepath.Base(executable) != "dockerCopilot" {
		return errors.New("program working directory must contain the running dockerCopilot executable")
	}
	probe, err := os.CreateTemp(workDir, ".dockerCopilot-write-check-*")
	if err != nil {
		return fmt.Errorf("program working directory is not writable: %w", err)
	}
	probePath := probe.Name()
	closeErr := probe.Close()
	removeErr := os.Remove(probePath)
	if closeErr != nil || removeErr != nil {
		return fmt.Errorf("program working directory write check failed: %w", errors.Join(closeErr, removeErr))
	}
	reference := strings.TrimSpace(v.config.Docker.SelfContainer)
	autoDetect := reference == ""
	if autoDetect {
		reference, err = os.Hostname()
		if err != nil || !dockerIDReference.MatchString(reference) {
			return errors.New("cannot identify the service container; configure Docker.SelfContainer for online updates")
		}
	}
	info, err := v.docker.ContainerInspect(ctx, reference)
	if err != nil {
		return fmt.Errorf("inspect service container for restart readiness: %w", err)
	}
	if info.ContainerJSONBase == nil || info.HostConfig == nil || info.ID == "" {
		return errors.New("Docker did not return complete service container restart settings")
	}
	if !isSupportedProgramLauncher(info) {
		return errors.New("container is not configured to start through the handoff-capable /app/start.sh launcher")
	}
	if autoDetect {
		if !strings.HasPrefix(strings.ToLower(info.ID), strings.ToLower(reference)) {
			return errors.New("container hostname does not match the inspected container ID")
		}
	} else if dockerIDReference.MatchString(reference) {
		if !strings.HasPrefix(strings.ToLower(info.ID), strings.ToLower(reference)) {
			return errors.New("configured service container ID does not match the inspected container")
		}
	} else if strings.TrimPrefix(info.Name, "/") != reference {
		return errors.New("configured service container name does not match the inspected container")
	}
	policy := info.HostConfig.RestartPolicy.Name
	if policy != "always" && policy != "unless-stopped" {
		return fmt.Errorf("online program handoff requires Docker restart policy always or unless-stopped (found %q)", policy)
	}
	if info.HostConfig.ReadonlyRootfs {
		return errors.New("online program handoff requires a writable container root filesystem")
	}
	return nil
}

// ValidateProgramHandoff checks the deployment facts required to perform an
// online update without changing the running process or Docker configuration.
func (ctx *ServiceContext) ValidateProgramHandoff(parent context.Context) error {
	if ctx == nil {
		return errors.New("service context is unavailable")
	}
	workDir, err := ctx.Config.ProgramWorkDir()
	if err != nil {
		return err
	}
	validator := newProgramInstallValidator(ctx.Config, ctx.DockerClient, workDir)
	return validator.validateRuntime(parent)
}

func (v *programInstallValidator) currentExecutable() (string, error) {
	if v.executablePath != "" {
		return v.executablePath, nil
	}
	return os.Executable()
}

func isSupportedProgramLauncher(info containertypes.InspectResponse) bool {
	if info.ContainerJSONBase == nil || info.Config == nil {
		return false
	}
	if filepath.Base(filepath.Clean(info.Path)) != "start.sh" || len(info.Args) != 0 {
		return false
	}
	containsLauncher := func(command []string) bool {
		return len(command) > 0 && filepath.Base(filepath.Clean(command[0])) == "start.sh"
	}
	return containsLauncher(info.Config.Entrypoint) || containsLauncher(info.Config.Cmd)
}

func (ctx *ServiceContext) ProgramHandoffOptions() (ProgramHandoffOptions, error) {
	if ctx == nil {
		return ProgramHandoffOptions{}, errors.New("service context is unavailable")
	}
	workDir, err := ctx.Config.ProgramWorkDir()
	if err != nil {
		return ProgramHandoffOptions{}, err
	}
	tasks, err := ctx.Config.TaskOptions()
	if err != nil {
		return ProgramHandoffOptions{}, err
	}
	return ProgramHandoffOptions{
		WorkDir: workDir, StagingParent: filepath.Join(tasks.Directory, "program-update-stage"),
		SelfContainer: strings.TrimSpace(ctx.Config.Docker.SelfContainer),
	}, nil
}
