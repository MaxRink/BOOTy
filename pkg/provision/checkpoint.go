//go:build linux

package provision

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// ErrNoCheckpoint is returned when no checkpoint file exists.
var ErrNoCheckpoint = errors.New("no checkpoint found")

// Checkpoint records provisioning progress to tmpfs.
//
// NOTE: The checkpoint primarily persists step completion status. Steps that
// derive in-memory state (e.g., detect-disk sets targetDisk, parse-partitions
// sets partition paths) will need to re-run on resume to rebuild that state.
type Checkpoint struct {
	LastCompletedStep string `json:"lastCompletedStep"`
	// NVMeTargetDevice stores the namespace device selected by
	// setup-nvme-namespaces. That step is destructive when repeated, so resume
	// restores this derived disk device instead of rerunning namespace layout.
	NVMeTargetDevice string `json:"nvmeTargetDevice,omitempty"`
	// RAIDTargetDevice stores the md device selected by setup-raid. RAID setup
	// wipes member devices and creates arrays, so resume restores the target
	// instead of rerunning the destructive setup step.
	RAIDTargetDevice string `json:"raidTargetDevice,omitempty"`
	// MirrorTargetDisk and MirrorDisk store the disks that detect-disk
	// selected for DISK_MIRRORING_MODE; MirrorDisk is empty when auto mode
	// fell back to one disk. Resume keeps that selection.
	MirrorTargetDisk string   `json:"mirrorTargetDisk,omitempty"`
	MirrorDisk       string   `json:"mirrorDisk,omitempty"`
	CompletedSteps   []string `json:"completedSteps"`
	// FailureCount is the number of steps that failed at least once
	// (incremented per executeStep error, not per retry attempt).
	FailureCount int      `json:"failureCount"`
	Errors       []string `json:"errors,omitempty"`
	path         string   // configurable checkpoint file path
	persist      bool     // only write to disk when true (BOOTY_RESUME)
}

const defaultCheckpointPath = "/tmp/booty-checkpoint.json"

// Save writes the current checkpoint atomically to tmpfs.
// It is a no-op when the checkpoint was not loaded from disk (persist=false).
func (c *Checkpoint) Save() error {
	if !c.persist {
		return nil
	}
	data, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("marshal checkpoint: %w", err)
	}
	p := c.path
	if p == "" {
		p = defaultCheckpointPath
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write checkpoint tmp: %w", err)
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename checkpoint: %w", err)
	}
	return nil
}

// LoadCheckpoint reads a checkpoint from tmpfs.
// Returns (nil, ErrNoCheckpoint) if no checkpoint file exists.
func LoadCheckpoint() (*Checkpoint, error) {
	return LoadCheckpointFrom(defaultCheckpointPath)
}

// LoadCheckpointFrom reads a checkpoint from the given path.
// Returns (nil, ErrNoCheckpoint) if the file does not exist.
func LoadCheckpointFrom(path string) (*Checkpoint, error) {
	data, err := os.ReadFile(path) //nolint:gosec // checkpoint path is a constant or explicitly provided
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNoCheckpoint
		}
		return nil, fmt.Errorf("read checkpoint: %w", err)
	}
	var cp Checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return nil, fmt.Errorf("unmarshal checkpoint: %w", err)
	}
	cp.path = path
	cp.persist = true
	return &cp, nil
}

// Remove deletes the checkpoint file.
// It is a no-op when the checkpoint was not loaded from disk (persist=false).
func (c *Checkpoint) Remove() error {
	if !c.persist {
		return nil
	}
	p := c.path
	if p == "" {
		p = defaultCheckpointPath
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove checkpoint: %w", err)
	}
	return nil
}

// MarkStep records a completed step in the checkpoint.
func (c *Checkpoint) MarkStep(name string) {
	c.LastCompletedStep = name
	if c.IsCompleted(name) {
		return
	}
	c.CompletedSteps = append(c.CompletedSteps, name)
}

// IsCompleted returns true if the named step was already completed.
func (c *Checkpoint) IsCompleted(name string) bool {
	for _, s := range c.CompletedSteps {
		if s == name {
			return true
		}
	}
	return false
}
