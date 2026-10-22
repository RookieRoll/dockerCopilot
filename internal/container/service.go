package container

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	dockertypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/google/uuid"
	"github.com/onlyLTY/dockerCopilot/internal/module"
	"github.com/onlyLTY/dockerCopilot/internal/operation"
	"github.com/onlyLTY/dockerCopilot/internal/types"
)

// Service owns the direct container operations used by the HTTP application.
// It deliberately depends on the Docker client rather than the application
// composition context so it can be tested with a daemon or a narrow fake.
type Service struct {
	docker     *client.Client
	updates    *module.ImageUpdateData
	progress   ProgressCallbacks
	opMu       sync.Mutex
	operations *operation.Coordinator
}

type ProgressCallbacks struct {
	Update func(string, types.TaskProgress)
	Get    func(string) (types.TaskProgress, bool)
}

func NewService(docker *client.Client, updates ...*module.ImageUpdateData) *Service {
	var checker *module.ImageUpdateData
	if len(updates) > 0 {
		checker = updates[0]
	}
	return &Service{docker: docker, updates: checker, operations: operation.NewCoordinator()}
}

// Operations returns the coordinator owned by this shared container service.
// The service composition should construct one Service and inject it into all
// handlers/adapters so every mutation path shares the same lease table.
func (s *Service) Operations() *operation.Coordinator {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if s.operations == nil {
		s.operations = operation.NewCoordinator()
	}
	return s.operations
}

// ReserveUpdate protects a container before a queued update is submitted.
// Callers must Confirm only after Runner.Submit has durably accepted the task;
// on submit failure they must Release. The equivalence key must encode every
// effective option that can change update behavior.
func (s *Service) ReserveUpdate(ctx context.Context, id, taskID, equivalenceKey string) (*operation.Lease, string, error) {
	inspected, err := s.Inspect(ctx, id)
	if err != nil {
		return nil, "", err
	}
	target := operation.Target{ID: inspected.ID, Name: strings.TrimLeft(strings.TrimSpace(inspected.Name), "/")}
	if target.ID == "" {
		target.ID = id
	}
	reservation, err := s.Operations().ReserveContext(ctx, operation.Request{
		TaskID: taskID, Kind: operation.ContainerUpdate, Target: target, EquivalenceKey: equivalenceKey,
	})
	if err != nil {
		return nil, "", err
	}
	return reservation.Lease, reservation.ExistingTaskID, nil
}

func (s *Service) reserveSync(ctx context.Context, id, kind string) (string, *operation.Lease, error) {
	inspected, err := s.Inspect(ctx, id)
	if err != nil {
		return "", nil, err
	}
	lease, err := s.reserveInspected(ctx, id, kind, inspected)
	if err != nil {
		return "", nil, err
	}
	return canonicalContainerID(id, inspected), lease, nil
}

// ReserveContainerMutation inspects and reserves the same canonical container
// identity before returning. Callers must hold the lease through any operation
// whose meaning depends on the inspected name or ID, then release it.
func (s *Service) ReserveContainerMutation(ctx context.Context, id, kind string) (dockertypes.ContainerJSON, *operation.Lease, error) {
	inspected, err := s.Inspect(ctx, id)
	if err != nil {
		return dockertypes.ContainerJSON{}, nil, err
	}
	lease, err := s.reserveInspected(ctx, id, kind, inspected)
	if err != nil {
		return dockertypes.ContainerJSON{}, nil, err
	}

	// Refresh after acquiring the ID lease. A rename can finish between the
	// initial Inspect and reservation, leaving the first name stale even though
	// the canonical ID is unchanged.
	current, err := s.Inspect(ctx, canonicalContainerID(id, inspected))
	if err != nil {
		lease.Release()
		return dockertypes.ContainerJSON{}, nil, err
	}
	currentID := canonicalContainerID(id, current)
	if currentID != canonicalContainerID(id, inspected) {
		lease.Release()
		return dockertypes.ContainerJSON{}, nil, fmt.Errorf("container identity changed while acquiring %s reservation", kind)
	}
	current.Name = strings.TrimPrefix(strings.TrimSpace(current.Name), "/")
	if err := lease.AddAlias(operation.Target{ID: currentID, Name: current.Name}); err != nil {
		lease.Release()
		return dockertypes.ContainerJSON{}, nil, err
	}
	return current, lease, nil
}

func (s *Service) reserveInspected(ctx context.Context, id, kind string, inspected dockertypes.ContainerJSON) (*operation.Lease, error) {
	target := operation.Target{ID: canonicalContainerID(id, inspected), Name: strings.TrimLeft(strings.TrimSpace(inspected.Name), "/")}
	reservation, err := s.Operations().ReserveContext(ctx, operation.Request{TaskID: uuid.NewString(), Kind: kind, Target: target})
	if err != nil {
		return nil, err
	}
	if reservation.Lease == nil {
		return nil, &operation.ConflictError{TaskID: reservation.ExistingTaskID, Kind: kind}
	}
	if err := reservation.Lease.Confirm(); err != nil {
		reservation.Lease.Release()
		return nil, err
	}
	return reservation.Lease, nil
}

func canonicalContainerID(requestedID string, inspected dockertypes.ContainerJSON) string {
	if inspected.ID != "" {
		return inspected.ID
	}
	return requestedID
}

func (s *Service) SetProgressCallbacks(callbacks ProgressCallbacks) { s.progress = callbacks }

func (s *Service) Update(ctx context.Context, id, name, imageNameAndTag string, deleteOld bool, taskID string) error {
	return s.UpdateWithOptions(ctx, id, name, imageNameAndTag, deleteOld, taskID, UpdateCallbacks{})
}

// UpdateWithCallbacks is retained for source compatibility. Durable update
// execution requires the full UpdateWithOptions callback set; callers that do
// not provide private snapshot and checkpoint storage fail before mutation.
func (s *Service) UpdateWithCallbacks(ctx context.Context, id, name, imageNameAndTag string, deleteOld bool, taskID string, onCreated func(string) error) error {
	return s.UpdateWithOptions(ctx, id, name, imageNameAndTag, deleteOld, taskID, UpdateCallbacks{OnCreated: onCreated})
}
func (s *Service) List(ctx context.Context) ([]types.Container, error) {
	items, err := s.docker.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return nil, err
	}
	result := make([]types.Container, 0, len(items))
	for _, item := range items {
		entry := types.Container{Container: item}
		result = append(result, entry)
	}
	return s.CheckImageUpdates(result), nil
}

func (s *Service) CheckImageUpdates(items []types.Container) []types.Container {
	if s.updates == nil {
		return items
	}
	for i := range items {
		if update, ok := s.updates.Get(items[i].ImageID); ok && update.NeedUpdate {
			items[i].Update = true
		}
	}
	return items
}

func (s *Service) Start(ctx context.Context, id string) error {
	canonicalID, lease, err := s.reserveSync(ctx, id, "container-start")
	if err != nil {
		return err
	}
	defer lease.Release()
	return s.docker.ContainerStart(ctx, canonicalID, container.StartOptions{})
}

func (s *Service) Stop(ctx context.Context, id string) error {
	canonicalID, lease, err := s.reserveSync(ctx, id, "container-stop")
	if err != nil {
		return err
	}
	defer lease.Release()
	timeout := 10
	return s.docker.ContainerStop(ctx, canonicalID, container.StopOptions{Signal: "SIGINT", Timeout: &timeout})
}

func (s *Service) Restart(ctx context.Context, id string) error {
	canonicalID, lease, err := s.reserveSync(ctx, id, "container-restart")
	if err != nil {
		return err
	}
	defer lease.Release()
	timeout := 10
	return s.docker.ContainerRestart(ctx, canonicalID, container.StopOptions{Signal: "SIGINT", Timeout: &timeout})
}

type Action string

const (
	ActionStart   Action = "start"
	ActionStop    Action = "stop"
	ActionRestart Action = "restart"
)

// OperateOwned re-inspects the exact accepted target before applying an
// operation under the caller's already-confirmed lease. It is used by batch
// tasks, whose one parent lease protects multiple targets until OnSettled.
func (s *Service) OperateOwned(ctx context.Context, taskID string, expected operation.Target, action Action) error {
	if taskID == "" || expected.ID == "" || expected.Name == "" {
		return errors.New("owned container operation needs its task ID and canonical target")
	}
	current, err := s.Inspect(ctx, expected.ID)
	if err != nil {
		return err
	}
	currentID := current.ID
	if currentID == "" {
		currentID = expected.ID
	}
	currentName := strings.TrimLeft(strings.TrimSpace(current.Name), "/")
	if currentID != expected.ID || currentName != expected.Name {
		return fmt.Errorf("container identity changed after batch acceptance (expected %s/%s, found %s/%s)", expected.ID, expected.Name, currentID, currentName)
	}
	if !s.Operations().Owns(taskID, operation.Target{ID: currentID, Name: currentName}) {
		return errors.New("batch task no longer owns the container operation lease")
	}
	switch action {
	case ActionStart:
		return s.docker.ContainerStart(ctx, currentID, container.StartOptions{})
	case ActionStop:
		timeout := 10
		return s.docker.ContainerStop(ctx, currentID, container.StopOptions{Signal: "SIGINT", Timeout: &timeout})
	case ActionRestart:
		timeout := 10
		return s.docker.ContainerRestart(ctx, currentID, container.StopOptions{Signal: "SIGINT", Timeout: &timeout})
	default:
		return fmt.Errorf("unsupported batch container action %q", action)
	}
}

func (s *Service) Rename(ctx context.Context, id, name string) error {
	return s.RenameWithCallback(ctx, id, name, nil)
}

// RenameWithCallback keeps the container's mutation lease through the local
// metadata migration callback so list cleanup cannot observe a half-migrated
// name mapping between Docker rename and settings persistence.
func (s *Service) RenameWithCallback(ctx context.Context, id, name string, afterRename func(oldName string) error) error {
	return s.RenameWithLifecycle(ctx, id, name, nil, func(oldName, _ string) error {
		if afterRename != nil {
			return afterRename(oldName)
		}
		return nil
	})
}

// RenameWithLifecycle keeps the operation lease through durable preparation,
// Docker rename, and local metadata migration.
func (s *Service) RenameWithLifecycle(ctx context.Context, id, name string, beforeRename, afterRename func(oldName, containerID string) error) error {
	canonicalID, lease, err := s.reserveSync(ctx, id, "container-rename")
	if err != nil {
		return err
	}
	defer lease.Release()
	inspected, err := s.Inspect(ctx, canonicalID)
	if err != nil {
		return err
	}
	oldName := strings.TrimLeft(strings.TrimSpace(inspected.Name), "/")
	containerID := inspected.ID
	if containerID == "" {
		containerID = canonicalID
	}
	if err := lease.AddAlias(operation.Target{Name: name}); err != nil {
		return err
	}
	if beforeRename != nil {
		if err := beforeRename(oldName, containerID); err != nil {
			return err
		}
	}
	if err := s.docker.ContainerRename(ctx, canonicalID, name); err != nil {
		return err
	}
	if afterRename != nil {
		return afterRename(oldName, containerID)
	}
	return nil
}

// FingerprintUpdate builds the deterministic equivalence key used when
// reserving a queued update. Callers must include every effective option.
func FingerprintUpdate(name, image string, deleteOld bool) string {
	// Use an ordered typed value so the deduplication key is stable.
	encoded, _ := json.Marshal(struct {
		Name      string `json:"name"`
		Image     string `json:"image"`
		DeleteOld bool   `json:"deleteOld"`
	}{Name: strings.TrimLeft(strings.TrimSpace(name), "/"), Image: strings.TrimSpace(image), DeleteOld: deleteOld})
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func (s *Service) Inspect(ctx context.Context, id string) (dockertypes.ContainerJSON, error) {
	return s.docker.ContainerInspect(ctx, id)
}
