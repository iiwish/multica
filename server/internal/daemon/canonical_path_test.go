//go:build !windows

package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestIsNameDispatchingAgentShim_RequiresExactDispatcherName(t *testing.T) {
	tests := []struct {
		name string
		path string
		want bool
	}{
		{name: "Volta", path: filepath.Join("manager", "volta-shim"), want: true},
		{name: "Vite Plus", path: filepath.Join("manager", "vp"), want: true},
		{name: "case insensitive", path: filepath.Join("manager", "VP"), want: true},
		{name: "short name prefix", path: filepath.Join("manager", "vpn"), want: false},
		{name: "short name word", path: filepath.Join("manager", "vproxy"), want: false},
		{name: "extension is not stripped", path: filepath.Join("manager", "volta-shim.exe"), want: false},
		{name: "ordinary version target", path: filepath.Join("manager", "claude-2.1.216"), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isNameDispatchingAgentShim(tt.path); got != tt.want {
				t.Fatalf("isNameDispatchingAgentShim(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestResolveMiseManagedExecutable_TimesOut(t *testing.T) {
	mise := filepath.Join(t.TempDir(), "mise")
	if err := os.WriteFile(mise, []byte("#!/bin/sh\nsleep 5\n"), 0o755); err != nil {
		t.Fatalf("write fake mise: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := resolveMiseManagedExecutable(ctx, mise, "claude")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("resolveMiseManagedExecutable error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("timed-out mise which took %v, want at most 1s", elapsed)
	}
}
