package image

import (
	"context"
	"errors"
	"fmt"

	"github.com/onlyLTY/dockerCopilot/internal/types"
)

type CleanupPolicy string

const (
	CleanupDangling CleanupPolicy = "dangling"
	CleanupUnused   CleanupPolicy = "unused"
)

type CleanupItem struct {
	ImageID string `json:"imageID"`
	Name    string `json:"name,omitempty"`
	Tag     string `json:"tag,omitempty"`
	Status  string `json:"status"`
	Error   string `json:"error,omitempty"`
	TaskID  string `json:"taskID,omitempty"`
}

type CleanupResult struct {
	Items   []CleanupItem `json:"items"`
	Total   int           `json:"total"`
	Deleted int           `json:"deleted"`
	Failed  int           `json:"failed"`
	Skipped int           `json:"skipped"`
}

// Cleanup refreshes Docker's current image/container state before choosing
// candidates, then applies ordinary (non-force) deletion one image at a time.
func (s *Service) Cleanup(ctx context.Context, policy CleanupPolicy) (CleanupResult, error) {
	if policy != CleanupDangling && policy != CleanupUnused {
		return CleanupResult{}, fmt.Errorf("unsupported image cleanup policy %q", policy)
	}
	images, err := s.List(ctx)
	if err != nil {
		return CleanupResult{}, fmt.Errorf("refresh images before cleanup: %w", err)
	}
	result := CleanupResult{Items: make([]CleanupItem, 0)}
	for _, item := range images {
		if !cleanupCandidate(item, policy) {
			continue
		}
		result.Total++
		outcome := CleanupItem{ImageID: item.ID, Name: item.ImageName, Tag: item.ImageTag}
		if item.InUsed {
			outcome.Status = "skipped"
			outcome.Error = "image is currently used by a container"
			result.Skipped++
			result.Items = append(result.Items, outcome)
			continue
		}
		if err := s.Remove(ctx, item.ID, false); err != nil {
			outcome.Error = err.Error()
			var conflict *ConflictError
			if errors.As(err, &conflict) {
				outcome.Status, outcome.TaskID = "skipped", conflict.TaskID
				result.Skipped++
			} else {
				outcome.Status = "failed"
				result.Failed++
			}
			result.Items = append(result.Items, outcome)
			continue
		}
		outcome.Status = "succeeded"
		result.Deleted++
		result.Items = append(result.Items, outcome)
	}
	return result, nil
}

func cleanupCandidate(item types.Image, policy CleanupPolicy) bool {
	dangling := len(item.RepoTags) == 0 || item.ImageTag == "None" || item.ImageTag == "<none>"
	if policy == CleanupDangling {
		return dangling
	}
	return !item.InUsed
}
