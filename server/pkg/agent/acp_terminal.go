package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"sync"
)

const defaultACPOutputByteLimit = 50_000

// acpTerminal is the daemon-side implementation of ACP's terminal/* methods.
// ACP agents use these calls for shell tools when the client advertises the
// terminal capability. Output is retained as a bounded tail, matching ACP's
// truncation contract without allowing a command to grow memory unboundedly.
type acpTerminal struct {
	cmd       *exec.Cmd
	mu        sync.Mutex
	output    []byte
	limit     int
	truncated bool
	done      chan struct{}
	exitCode  *int
}

type acpTerminalOutputWriter struct {
	terminal *acpTerminal
}

func (w acpTerminalOutputWriter) Write(p []byte) (int, error) {
	w.terminal.mu.Lock()
	defer w.terminal.mu.Unlock()

	w.terminal.output = append(w.terminal.output, p...)
	if w.terminal.limit > 0 && len(w.terminal.output) > w.terminal.limit {
		w.terminal.output = append([]byte(nil), w.terminal.output[len(w.terminal.output)-w.terminal.limit:]...)
		w.terminal.truncated = true
	}
	return len(p), nil
}

func (t *acpTerminal) snapshot() (output string, truncated bool, exitCode *int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(append([]byte(nil), t.output...)), t.truncated, t.exitCode
}

func (t *acpTerminal) wait() {
	<-t.done
}

func (t *acpTerminal) kill() error {
	select {
	case <-t.done:
		return nil
	default:
	}

	t.mu.Lock()
	cmd := t.cmd
	t.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if err := cmd.Process.Kill(); err != nil {
		// A process can exit between the done check and Kill. Treat that race
		// as a successful kill; the terminal already has its final status.
		select {
		case <-t.done:
			return nil
		default:
		}
		return err
	}
	return nil
}

func (c *hermesClient) acpTerminalCreate(params json.RawMessage) (map[string]any, error) {
	var p struct {
		Command string   `json:"command"`
		Args    []string `json:"args"`
		Env     []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"env"`
		Cwd             string `json:"cwd"`
		OutputByteLimit int    `json:"outputByteLimit"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("terminal/create params: %w", err)
	}
	if strings.TrimSpace(p.Command) == "" {
		return nil, fmt.Errorf("terminal/create requires command")
	}

	cwd := p.Cwd
	if cwd == "" {
		cwd = c.terminalCwd
	}
	if cwd == "" {
		cwd = "."
	}

	env := append([]string(nil), c.terminalEnv...)
	for _, item := range p.Env {
		if item.Name == "" {
			continue
		}
		env = append(env, item.Name+"="+item.Value)
	}

	var cmd *exec.Cmd
	if len(p.Args) > 0 {
		cmd = exec.CommandContext(c.terminalContext(), p.Command, p.Args...)
	} else if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(c.terminalContext(), "cmd.exe", "/d", "/s", "/c", p.Command)
	} else {
		cmd = exec.CommandContext(c.terminalContext(), "/bin/sh", "-c", p.Command)
	}
	cmd.Dir = cwd
	cmd.Env = env

	limit := p.OutputByteLimit
	if limit <= 0 {
		limit = defaultACPOutputByteLimit
	}
	t := &acpTerminal{cmd: cmd, limit: limit, done: make(chan struct{})}
	writer := acpTerminalOutputWriter{terminal: t}
	cmd.Stdout = writer
	cmd.Stderr = writer
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("terminal/create start: %w", err)
	}

	id := c.nextACPTerminalID()
	c.terminalMu.Lock()
	if c.terminals == nil {
		c.terminals = make(map[string]*acpTerminal)
	}
	c.terminals[id] = t
	c.terminalMu.Unlock()

	go func() {
		err := cmd.Wait()
		var code int
		if err == nil {
			code = 0
		} else if exitErr, ok := err.(*exec.ExitError); ok {
			code = exitErr.ExitCode()
		} else {
			code = -1
		}
		t.mu.Lock()
		t.exitCode = &code
		t.mu.Unlock()
		close(t.done)
	}()

	return map[string]any{"terminalId": id}, nil
}

func (c *hermesClient) terminalContext() context.Context {
	// The ACP transport is normally attached to a live task context. A nil
	// context is not valid for CommandContext, so keep this defensive fallback
	// for unit tests that construct hermesClient directly.
	if c.terminalCtx != nil {
		return c.terminalCtx
	}
	return context.Background()
}

func (c *hermesClient) nextACPTerminalID() string {
	c.terminalMu.Lock()
	defer c.terminalMu.Unlock()
	c.nextTerminalID++
	return fmt.Sprintf("multica-terminal-%d", c.nextTerminalID)
}

func (c *hermesClient) acpTerminalFor(id string) (*acpTerminal, bool) {
	c.terminalMu.Lock()
	defer c.terminalMu.Unlock()
	t, ok := c.terminals[id]
	return t, ok
}

func (c *hermesClient) acpTerminalRelease(id string) error {
	c.terminalMu.Lock()
	t, ok := c.terminals[id]
	if ok {
		delete(c.terminals, id)
	}
	c.terminalMu.Unlock()
	if !ok {
		return nil
	}
	select {
	case <-t.done:
	default:
		_ = t.kill()
	}
	return nil
}

func (c *hermesClient) acpTerminalResponse(method string, params json.RawMessage) (map[string]any, error) {
	var p struct {
		TerminalID string `json:"terminalId"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("%s params: %w", method, err)
	}
	t, ok := c.acpTerminalFor(p.TerminalID)
	if !ok {
		return nil, fmt.Errorf("unknown terminal %q", p.TerminalID)
	}

	switch method {
	case "terminal/output":
		output, truncated, exitCode := t.snapshot()
		result := map[string]any{"output": output, "truncated": truncated}
		if exitCode != nil {
			result["exitStatus"] = map[string]any{"exitCode": *exitCode, "signal": nil}
		}
		return result, nil
	case "terminal/wait_for_exit":
		t.wait()
		_, _, exitCode := t.snapshot()
		if exitCode == nil {
			return nil, fmt.Errorf("terminal %q exited without status", p.TerminalID)
		}
		return map[string]any{"exitCode": *exitCode, "signal": nil}, nil
	case "terminal/kill":
		if err := t.kill(); err != nil {
			return nil, fmt.Errorf("kill terminal %q: %w", p.TerminalID, err)
		}
		return map[string]any{}, nil
	case "terminal/release":
		return map[string]any{}, c.acpTerminalRelease(p.TerminalID)
	default:
		return nil, fmt.Errorf("unsupported terminal method %q", method)
	}
}
