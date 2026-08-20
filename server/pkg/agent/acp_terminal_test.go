package agent

import (
	"context"
	"encoding/json"
	"os"
	"runtime"
	"strings"
	"testing"
)

func TestACPManagedTerminalRunsAndReturnsOutput(t *testing.T) {
	t.Parallel()

	command := "printf 'hello from terminal'"
	if runtime.GOOS == "windows" {
		command = "echo hello from terminal"
	}
	c := &hermesClient{
		terminalCtx: cxtBackground(),
		terminalCwd: t.TempDir(),
		terminalEnv: os.Environ(),
		terminals:   make(map[string]*acpTerminal),
	}

	created, err := c.acpTerminalCreate(json.RawMessage(`{"command":` + jsonString(command) + `}`))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	id, ok := created["terminalId"].(string)
	if !ok || id == "" {
		t.Fatalf("create result terminalId = %#v", created["terminalId"])
	}

	waited, err := c.acpTerminalResponse("terminal/wait_for_exit", json.RawMessage(`{"terminalId":`+jsonString(id)+`}`))
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if got := waited["exitCode"]; got != float64(0) && got != 0 {
		t.Fatalf("exitCode = %#v, want 0", got)
	}

	output, err := c.acpTerminalResponse("terminal/output", json.RawMessage(`{"terminalId":`+jsonString(id)+`}`))
	if err != nil {
		t.Fatalf("output: %v", err)
	}
	if !strings.Contains(output["output"].(string), "hello from terminal") {
		t.Fatalf("output = %#v", output["output"])
	}
	if output["truncated"] != false {
		t.Fatalf("truncated = %#v, want false", output["truncated"])
	}

	if err := c.acpTerminalRelease(id); err != nil {
		t.Fatalf("release: %v", err)
	}
}

func TestACPManagedTerminalBoundsOutputToTail(t *testing.T) {
	t.Parallel()

	command := "printf '0123456789'"
	if runtime.GOOS == "windows" {
		command = "echo 0123456789"
	}
	c := &hermesClient{
		terminalCtx: cxtBackground(),
		terminalCwd: t.TempDir(),
		terminalEnv: os.Environ(),
		terminals:   make(map[string]*acpTerminal),
	}
	created, err := c.acpTerminalCreate(json.RawMessage(`{"command":` + jsonString(command) + `,"outputByteLimit":4}`))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	id := created["terminalId"].(string)
	if _, err := c.acpTerminalResponse("terminal/wait_for_exit", json.RawMessage(`{"terminalId":`+jsonString(id)+`}`)); err != nil {
		t.Fatalf("wait: %v", err)
	}
	output, err := c.acpTerminalResponse("terminal/output", json.RawMessage(`{"terminalId":`+jsonString(id)+`}`))
	if err != nil {
		t.Fatalf("output: %v", err)
	}
	if output["truncated"] != true {
		t.Fatalf("truncated = %#v, want true", output["truncated"])
	}
	if len(output["output"].(string)) != 4 {
		t.Fatalf("output length = %d, want 4", len(output["output"].(string)))
	}
	_ = c.acpTerminalRelease(id)
}

// These tiny helpers keep the test JSON readable without introducing a
// second JSON encoder abstraction into the production ACP transport.
func cxtBackground() context.Context { return context.Background() }

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
