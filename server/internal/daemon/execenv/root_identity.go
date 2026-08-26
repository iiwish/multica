package execenv

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	taskRootIndexDir   = ".task_roots"
	taskRootRecordFile = "root.json"
	// taskRootPendingPrefix names the staging directory installTaskRootRecord
	// renames into place. A directory still carrying it never became
	// authoritative for any task.
	taskRootPendingPrefix = ".pending-"
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
		return validateTaskRootRecord(params, recordDir, record)
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
	return validateTaskRootRecord(params, recordDir, record)
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
	data, err := os.ReadFile(filepath.Join(recordDir, taskRootRecordFile))
	if err != nil {
		return taskRootRecord{}, err
	}
	var record taskRootRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return taskRootRecord{}, fmt.Errorf("execenv: decode task root record: %w", err)
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

// validateTaskRootRecord fails closed on anything it cannot vouch for: a task
// that cannot prove which root is its own must not fall back to proposing a
// fresh one, because the original may still hold live work. That refusal is
// permanent until an operator intervenes, so every error names recordDir —
// the one path they need to inspect or remove.
func validateTaskRootRecord(params RootDirParams, recordDir string, record taskRootRecord) (string, error) {
	if record.WorkspaceID != params.WorkspaceID || record.TaskID != params.TaskID {
		return "", fmt.Errorf("execenv: task root record %s belongs to workspace %s task %s, not workspace %s task %s",
			recordDir, record.WorkspaceID, record.TaskID, params.WorkspaceID, params.TaskID)
	}
	relative := filepath.Clean(record.RelativePath)
	if relative == "." || filepath.IsAbs(relative) {
		return "", fmt.Errorf("execenv: task root record %s holds invalid relative path %q", recordDir, record.RelativePath)
	}
	parts := strings.Split(relative, string(filepath.Separator))
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || parts[0] == ".." || parts[1] == ".." {
		return "", fmt.Errorf("execenv: task root record %s holds invalid relative path %q", recordDir, record.RelativePath)
	}
	if !validTaskRootSegment(parts[0], params.WorkspaceID, true) || !validTaskRootSegment(parts[1], params.TaskID, false) {
		return "", fmt.Errorf("execenv: task root record %s points at %q, which does not match its stable identity", recordDir, record.RelativePath)
	}
	return filepath.Join(params.WorkspacesRoot, relative), nil
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
	resolved, err := validateTaskRootRecord(params, recordDir, record)
	if err != nil {
		return err
	}
	if filepath.Clean(resolved) != filepath.Clean(envRoot) {
		return fmt.Errorf("execenv: task root record points to %s, not reclaimed root %s", resolved, envRoot)
	}
	if err := os.RemoveAll(recordDir); err != nil {
		return fmt.Errorf("execenv: remove task root record: %w", err)
	}
	// The index directory itself is deliberately left in place. Removing it
	// when the last record goes would race installTaskRootRecord, which does
	// MkdirAll(parent) and then MkdirTemp(parent, ...): a removal landing
	// between those two calls hands the claim an ENOENT and fails the task,
	// which is a poor trade for one empty directory.
	return nil
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

// PruneTaskRootRecords reclaims stable-index entries whose env root is gone.
//
// RemoveRootDirRecord is the primary reclaim, but it only fires when GC removes
// a root whose .task_owner still names both ids. A record installed for a task
// that never reached ClaimEnvRoot, or one whose owner marker was lost, has no
// other reader and no other remover — and nothing else walks .task_roots, since
// both the GC task walk and the disk-usage scan skip dot-directories. Left
// alone those entries accumulate silently for the life of the workspaces root.
//
// A missing root is NOT sufficient on its own. ResolveRootDir deliberately
// freezes a task's identity BEFORE Prepare creates the directory, so "record
// with no root" is the correct state for the short window in between; deleting
// there would hand the same task a second physical root and reintroduce exactly
// the orphaning this index exists to prevent. The age gate is what separates
// that window (milliseconds) from a genuine leftover.
//
// Records that exist but cannot be parsed are reported and kept. They are what
// keeps a task failing closed rather than silently re-proposing a different
// root, and validateTaskRootRecord's error already names the directory to
// remove.
func PruneTaskRootRecords(workspacesRoot string, minAge time.Duration, now time.Time, logger *slog.Logger) (removed int) {
	if workspacesRoot == "" || minAge <= 0 {
		return 0
	}
	indexDir := filepath.Join(workspacesRoot, taskRootIndexDir)
	entries, err := os.ReadDir(indexDir)
	if err != nil {
		return 0 // missing or unreadable index — nothing to prune
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		recordDir := filepath.Join(indexDir, entry.Name())
		age, ok := taskRootRecordAge(recordDir, now)
		if !ok || age <= minAge {
			continue
		}

		// A staging directory this old is an install that died between
		// MkdirTemp and the rename. It was never authoritative for any task,
		// so absence of a root proves nothing about it either way.
		if !strings.HasPrefix(entry.Name(), taskRootPendingPrefix) {
			keep, reason := taskRootRecordStillLive(workspacesRoot, recordDir)
			if keep {
				if reason != "" && logger != nil {
					logger.Warn("gc: keeping unreadable task root record", "dir", recordDir, "reason", reason)
				}
				continue
			}
		}

		if err := os.RemoveAll(recordDir); err != nil {
			if logger != nil {
				logger.Warn("gc: remove stale task root record failed", "dir", recordDir, "error", err)
			}
			continue
		}
		removed++
	}
	if removed > 0 && logger != nil {
		logger.Info("gc: reclaimed stale task root records", "count", removed, "min_age", minAge)
	}
	return removed
}

// taskRootRecordStillLive reports whether recordDir must be preserved. The
// record file is the authority; when it cannot be read the answer is always
// "keep", with a reason the caller can surface.
func taskRootRecordStillLive(workspacesRoot, recordDir string) (keep bool, unreadable string) {
	record, err := readTaskRootRecord(recordDir)
	if errors.Is(err, os.ErrNotExist) {
		// A record directory with no record file cannot be read by
		// ResolveRootDir either, so nothing depends on it.
		return false, ""
	}
	if err != nil {
		return true, err.Error()
	}
	relative := filepath.Clean(record.RelativePath)
	if relative == "." || filepath.IsAbs(relative) || !filepath.IsLocal(relative) {
		return true, fmt.Sprintf("invalid relative path %q", record.RelativePath)
	}
	switch _, err := os.Stat(filepath.Join(workspacesRoot, relative)); {
	case err == nil:
		return true, "" // the root is on disk; the record is live
	case errors.Is(err, os.ErrNotExist):
		return false, ""
	default:
		return true, err.Error() // cannot tell — leave it alone
	}
}

// taskRootRecordAge dates a record by its file, which is written once inside a
// staging directory and never rewritten, so its mtime is the install time. A
// staging directory that died before the write has no file; fall back to the
// directory itself so those are still reclaimable.
func taskRootRecordAge(recordDir string, now time.Time) (time.Duration, bool) {
	if info, err := os.Stat(filepath.Join(recordDir, taskRootRecordFile)); err == nil {
		return now.Sub(info.ModTime()), true
	}
	info, err := os.Stat(recordDir)
	if err != nil {
		return 0, false
	}
	return now.Sub(info.ModTime()), true
}
