//go:build !windows

package daemon

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	// mise resolution runs during runtime discovery. Root prevents a task
	// worktree's config from influencing the selected binary, and WaitDelay
	// keeps inherited output pipes from extending the context deadline.
	miseWhichTimeout   = 2 * time.Second
	miseWhichWaitDelay = 250 * time.Millisecond

	trustedExecutableResolutionDir = "/"
)

func canonicalPath(path string) (string, error) {
	return filepath.EvalSymlinks(path)
}

// discoveredExecutablePath canonicalizes ordinary executable symlinks while
// retaining the launch contract of known version-manager entrypoints. Volta's
// volta-shim and Vite Plus's vp select the managed package from argv[0], so
// their invoked basename must be preserved (#6183, #6702). mise shims also
// dispatch by name, but preserving them would let a task worktree's mise.toml
// select a different version at launch. Resolve those shims to the concrete
// binary selected from a fixed trusted working directory instead (#7764).
//
// For argv[0] dispatchers, the entrypoint's parent is still canonicalized. This
// keeps paths stable when their containing directory is a symlink or an
// ephemeral version-manager prefix while retaining the basename the dispatcher
// needs — the same semantics buildLoginShellResolveScript applies via `pwd -P`.
func discoveredExecutablePath(path string) (string, error) {
	real := canonicalExecutablePath(path)
	if isMiseExecutable(real) {
		return resolveMiseManagedExecutableWithTimeout(real, filepath.Base(path))
	}
	if !isNameDispatchingAgentShim(real) {
		return real, nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return real, nil
	}
	realDir, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return abs, nil
	}
	return filepath.Join(realDir, filepath.Base(abs)), nil
}

func isMiseExecutable(path string) bool {
	return strings.EqualFold(filepath.Base(path), "mise")
}

func resolveMiseDiscoveredPath(path, commandName string) (string, bool, error) {
	real := canonicalExecutablePath(path)
	if !isMiseExecutable(real) {
		return path, false, nil
	}
	target, err := resolveMiseManagedExecutableWithTimeout(real, commandName)
	return target, true, err
}

func resolveMiseManagedExecutableWithTimeout(misePath, commandName string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), miseWhichTimeout)
	defer cancel()
	return resolveMiseManagedExecutable(ctx, misePath, commandName)
}

func resolveMiseManagedExecutable(ctx context.Context, misePath, commandName string) (string, error) {
	cmd := exec.CommandContext(ctx, misePath, "which", commandName)
	cmd.Dir = trustedExecutableResolutionDir
	cmd.WaitDelay = miseWhichWaitDelay
	raw, err := cmd.Output()
	if ctx.Err() != nil {
		return "", fmt.Errorf("resolve mise-managed %s: %w", commandName, ctx.Err())
	}
	if err != nil {
		return "", fmt.Errorf("resolve mise-managed %s: %w", commandName, err)
	}

	target := strings.TrimSpace(string(raw))
	if target == "" || strings.ContainsAny(target, "\r\n") {
		return "", fmt.Errorf("resolve mise-managed %s: mise which returned an invalid path", commandName)
	}
	if !filepath.IsAbs(target) {
		return "", fmt.Errorf("resolve mise-managed %s: mise which returned non-absolute path %q", commandName, target)
	}
	if _, err := exec.LookPath(target); err != nil {
		return "", fmt.Errorf("resolve mise-managed %s target %q: %w", commandName, target, err)
	}
	return canonicalExecutablePath(target), nil
}

func isNameDispatchingAgentShim(path string) bool {
	base := strings.ToLower(filepath.Base(path))
	// "vp" is intentionally admitted despite being a short, generic token:
	// it is Vite Plus's shared dispatcher and, like Volta, selects the managed
	// package from argv[0]. Keep this exact-match list limited to dispatchers
	// confirmed to require the invoked entrypoint name. Do not strip executable
	// extensions: these managers use wrappers or trampolines rather than
	// name-dispatching symlinks, and that is a Windows shape this file never
	// compiles for.
	return base == "volta-shim" || base == "vp"
}

var executablePathForLaunch = executablePathForLaunchDefault

func executablePathForLaunchDefault(string) (string, bool, error) {
	return "", false, nil
}

func canonicalConfiguredExecutablePath(path string) string {
	return path
}
