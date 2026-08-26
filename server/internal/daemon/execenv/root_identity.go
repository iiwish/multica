package execenv

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	taskRootIndexDir         = ".task_roots"
	taskRootRecordFile       = "root.json"
	taskRootPendingPrefix    = ".pending-"
	taskRootIndexMinPruneAge = time.Hour
)

type taskRootRecord struct {
	WorkspaceID  string `json:"workspace_id"`
	TaskID       string `json:"task_id"`
	RelativePath string `json:"relative_path"`
}

// ResolveRootDir returns the one physical env root assigned to a task. The
// first caller freezes the readable path in an index keyed only by stable IDs;
// later claims keep that path even when display labels are added or renamed.
func ResolveRootDir(params RootDirParams) (string, error) {
	proposed := PredictRootDir(params)
	if proposed == "" {
		return "", nil
	}

	recordDir := taskRootRecordDir(params)
	record, err := readTaskRootRecord(recordDir)
	if err == nil {
		return validateTaskRootRecordAt(recordDir, params, record)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	candidate, err := findOwnedTaskRoot(params)
	if err != nil {
		return "", err
	}
	if candidate == "" {
		candidate = proposed
	}
	relative, err := filepath.Rel(params.WorkspacesRoot, candidate)
	if err != nil {
		return "", fmt.Errorf("execenv: make task root relative: %w", err)
	}
	record = taskRootRecord{
		WorkspaceID:  params.WorkspaceID,
		TaskID:       params.TaskID,
		RelativePath: relative,
	}
	if err := installTaskRootRecord(recordDir, record); err != nil {
		return "", err
	}

	// Another claimant may have won the atomic install with different readable
	// labels. Always re-read the authoritative record instead of returning our
	// proposal.
	record, err = readTaskRootRecord(recordDir)
	if err != nil {
		return "", err
	}
	return validateTaskRootRecordAt(recordDir, params, record)
}

func taskRootRecordDir(params RootDirParams) string {
	return filepath.Join(
		params.WorkspacesRoot,
		taskRootIndexDir,
		stableIdentityKey(params.WorkspaceID+"\x00"+params.TaskID),
	)
}

func stableIdentityKey(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:16])
}

func readTaskRootRecord(recordDir string) (taskRootRecord, error) {
	recordPath := filepath.Join(recordDir, taskRootRecordFile)
	data, err := os.ReadFile(recordPath)
	if err != nil {
		return taskRootRecord{}, err
	}
	var record taskRootRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return taskRootRecord{}, fmt.Errorf("execenv: decode task root record %s: %w", recordPath, err)
	}
	return record, nil
}

func installTaskRootRecord(recordDir string, record taskRootRecord) error {
	parent := filepath.Dir(recordDir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("execenv: create task root index: %w", err)
	}
	tmpDir, err := os.MkdirTemp(parent, taskRootPendingPrefix)
	if err != nil {
		return fmt.Errorf("execenv: create task root record: %w", err)
	}
	defer os.RemoveAll(tmpDir)
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("execenv: encode task root record: %w", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, taskRootRecordFile), data, 0o644); err != nil {
		return fmt.Errorf("execenv: write task root record: %w", err)
	}
	if err := os.Rename(tmpDir, recordDir); err != nil {
		// A complete non-empty directory is installed atomically. If it exists,
		// another claimant won and its record is authoritative.
		if _, readErr := readTaskRootRecord(recordDir); readErr == nil {
			return nil
		}
		return fmt.Errorf("execenv: install task root record: %w", err)
	}
	return nil
}

func validateTaskRootRecord(params RootDirParams, record taskRootRecord) (string, error) {
	if record.WorkspaceID != params.WorkspaceID || record.TaskID != params.TaskID {
		return "", fmt.Errorf("execenv: task root record identity mismatch")
	}
	relative := filepath.Clean(record.RelativePath)
	if relative == "." || filepath.IsAbs(relative) {
		return "", fmt.Errorf("execenv: invalid task root relative path %q", record.RelativePath)
	}
	parts := strings.Split(relative, string(filepath.Separator))
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || parts[0] == ".." || parts[1] == ".." {
		return "", fmt.Errorf("execenv: invalid task root relative path %q", record.RelativePath)
	}
	if !validTaskRootSegment(parts[0], params.WorkspaceID, true) || !validTaskRootSegment(parts[1], params.TaskID, false) {
		return "", fmt.Errorf("execenv: task root relative path %q does not match its stable identity", record.RelativePath)
	}
	return filepath.Join(params.WorkspacesRoot, relative), nil
}

func validateTaskRootRecordAt(recordDir string, params RootDirParams, record taskRootRecord) (string, error) {
	resolved, err := validateTaskRootRecord(params, record)
	if err != nil {
		return "", fmt.Errorf("execenv: validate task root record %s: %w", recordDir, err)
	}
	return resolved, nil
}

func validTaskRootSegment(segment, id string, workspace bool) bool {
	segment = strings.ToLower(segment)
	id = strings.ToLower(id)
	key := strings.ToLower(taskKey(id))
	if workspace && segment == id {
		return true
	}
	if !workspace && segment == key {
		return true
	}
	return strings.HasSuffix(segment, "-"+key)
}

// RemoveRootDirRecord removes the stable index after GC has reclaimed a
// terminal task root. It verifies that the record still points at envRoot so a
// stale cleanup can never remove another task's identity.
func RemoveRootDirRecord(workspacesRoot, envRoot string, owner EnvRootOwner) error {
	if workspacesRoot == "" || envRoot == "" || owner.WorkspaceID == "" || owner.TaskID == "" {
		return nil
	}
	params := RootDirParams{
		WorkspacesRoot: workspacesRoot,
		WorkspaceID:    owner.WorkspaceID,
		TaskID:         owner.TaskID,
	}
	recordDir := taskRootRecordDir(params)
	record, err := readTaskRootRecord(recordDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	resolved, err := validateTaskRootRecordAt(recordDir, params, record)
	if err != nil {
		return err
	}
	if filepath.Clean(resolved) != filepath.Clean(envRoot) {
		return fmt.Errorf("execenv: task root record points to %s, not reclaimed root %s", resolved, envRoot)
	}
	if err := os.RemoveAll(recordDir); err != nil {
		return fmt.Errorf("execenv: remove task root record: %w", err)
	}
	return nil
}

// PruneTaskRootIndex removes abandoned unpublished directories and stable
// records whose physical env root never materialized. Complete records are
// removed only when eligible confirms the task is terminal or inaccessible;
// a live/non-terminal task keeps its frozen path even when the root is absent.
//
// A minimum grace period protects the tiny publication window even when the
// operator configures the general orphan TTL as zero.
func PruneTaskRootIndex(workspacesRoot string, retention time.Duration, now time.Time, eligible func(workspaceID, taskID string) bool) (removed int, err error) {
	indexDir := filepath.Join(workspacesRoot, taskRootIndexDir)
	entries, readErr := os.ReadDir(indexDir)
	if errors.Is(readErr, os.ErrNotExist) {
		return 0, nil
	}
	if readErr != nil {
		return 0, fmt.Errorf("execenv: read task root index: %w", readErr)
	}

	pruneAge := retention
	if pruneAge < taskRootIndexMinPruneAge {
		pruneAge = taskRootIndexMinPruneAge
	}
	var errs []error
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		entryPath := filepath.Join(indexDir, entry.Name())
		info, infoErr := entry.Info()
		if errors.Is(infoErr, os.ErrNotExist) {
			continue
		}
		if infoErr != nil {
			errs = append(errs, fmt.Errorf("inspect task root index entry %s: %w", entryPath, infoErr))
			continue
		}
		if now.Sub(info.ModTime()) <= pruneAge {
			continue
		}

		if strings.HasPrefix(entry.Name(), taskRootPendingPrefix) {
			if removeErr := os.RemoveAll(entryPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				errs = append(errs, fmt.Errorf("remove stale task root pending entry %s: %w", entryPath, removeErr))
				continue
			}
			removed++
			continue
		}

		record, recordErr := readTaskRootRecord(entryPath)
		if errors.Is(recordErr, os.ErrNotExist) {
			continue
		}
		if recordErr != nil {
			errs = append(errs, fmt.Errorf("read stale task root record %s: %w", entryPath, recordErr))
			continue
		}
		params := RootDirParams{
			WorkspacesRoot: workspacesRoot,
			WorkspaceID:    record.WorkspaceID,
			TaskID:         record.TaskID,
		}
		resolved, validateErr := validateTaskRootRecord(params, record)
		if validateErr != nil {
			errs = append(errs, fmt.Errorf("validate stale task root record %s: %w", entryPath, validateErr))
			continue
		}
		if _, statErr := os.Stat(resolved); statErr == nil {
			continue
		} else if !errors.Is(statErr, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("inspect env root for record %s: %w", entryPath, statErr))
			continue
		}
		if eligible == nil || !eligible(record.WorkspaceID, record.TaskID) {
			continue
		}
		if removeErr := os.RemoveAll(entryPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("remove stale task root record %s: %w", entryPath, removeErr))
			continue
		}
		removed++
	}
	return removed, errors.Join(errs...)
}

// findOwnedTaskRoot adopts roots created before the stable index existed. The
// owner marker is authoritative; readable suffixes only narrow the scan.
func findOwnedTaskRoot(params RootDirParams) (string, error) {
	workspaceEntries, err := os.ReadDir(params.WorkspacesRoot)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("execenv: scan existing task roots: %w", err)
	}
	workspaceSuffix := strings.ToLower(taskKey(params.WorkspaceID))
	var found string
	for _, workspaceEntry := range workspaceEntries {
		if !workspaceEntry.IsDir() || strings.HasPrefix(workspaceEntry.Name(), ".") {
			continue
		}
		name := workspaceEntry.Name()
		if name != params.WorkspaceID && !strings.HasSuffix(strings.ToLower(name), "-"+workspaceSuffix) {
			continue
		}
		workspaceDir := filepath.Join(params.WorkspacesRoot, name)
		taskEntries, readErr := os.ReadDir(workspaceDir)
		if readErr != nil {
			return "", fmt.Errorf("execenv: read candidate workspace root %s: %w", workspaceDir, readErr)
		}
		for _, taskEntry := range taskEntries {
			if !taskEntry.IsDir() {
				continue
			}
			taskName := strings.ToLower(taskEntry.Name())
			taskSuffix := strings.ToLower(taskKey(params.TaskID))
			if taskName != taskSuffix && !strings.HasSuffix(taskName, "-"+taskSuffix) {
				continue
			}
			candidate := filepath.Join(workspaceDir, taskEntry.Name())
			owner, readErr := ReadEnvRootOwner(candidate)
			if readErr != nil {
				return "", fmt.Errorf("execenv: read candidate env root owner for %s: %w", candidate, readErr)
			}
			if owner.TaskID == "" {
				hasWork, inspectErr := envRootHoldsWork(candidate)
				if inspectErr != nil {
					return "", fmt.Errorf("execenv: inspect candidate env root %s: %w", candidate, inspectErr)
				}
				if hasWork {
					return "", fmt.Errorf("execenv: candidate env root %s holds files but has no owner", candidate)
				}
			} else if owner.TaskID != params.TaskID {
				continue
			}
			if owner.WorkspaceID != "" && owner.WorkspaceID != params.WorkspaceID {
				continue
			}
			if found != "" && found != candidate {
				return "", fmt.Errorf("execenv: task %s owns multiple env roots", params.TaskID)
			}
			found = candidate
		}
	}
	return found, nil
}
