package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/overspecific/blorb/internal/config"
	"github.com/overspecific/blorb/internal/llm"
	"github.com/overspecific/blorb/internal/logging"
)

// commandTool runs a tool as a subprocess: the JSON args become the
// process's stdin, stdout is captured as the result output.
type commandTool struct {
	toolName    string
	description string
	argsSchema  json.RawMessage
	command     []string
}

// newCommandTool validates a command tool entry and builds its
// implementation.
func newCommandTool(e config.ToolEntry) (tool, error) {
	if len(e.Command) == 0 {
		return nil, fmt.Errorf("tool %q: command is required", e.Name)
	}
	for _, cmd := range e.Command {
		if cmd == "" {
			return nil, fmt.Errorf("tool %q: command must not contain empty strings", e.Name)
		}
	}
	return &commandTool{
		toolName:    e.Name,
		description: e.Description,
		argsSchema:  e.ArgsSchema,
		command:     e.Command,
	}, nil
}

func (t *commandTool) name() string { return t.toolName }

// close releases per-instance resources; a command tool holds none.
func (t *commandTool) close() {}

func (t *commandTool) definition() llm.Tool {
	schema := t.argsSchema
	if len(schema) == 0 {
		schema = json.RawMessage("{}")
	}
	return llm.Tool{
		Name:        t.toolName,
		Description: t.description,
		Parameters:  schema,
	}
}

// run executes the subprocess. The ctx is already derived with the
// registry's per-call timeout. Tool-reported failures (non-zero exit) return
// ToolResult{Err: true} with no error; infrastructure failures (start error,
// timeout, unusable stdin) return an error.
func (t *commandTool) run(ctx context.Context, args json.RawMessage, sink logging.Sink) (ToolResult, error) {
	// Remaining time until the timeout fires, captured before the process
	// starts so the timeout error can report the configured duration.
	remaining := time.Duration(0)
	if deadline, ok := ctx.Deadline(); ok {
		remaining = time.Until(deadline)
	}

	cmd := exec.CommandContext(ctx, t.command[0], t.command[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	cmd.Stdin = bytes.NewReader(args)
	var stdout, stderr limitedBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		err = fmt.Errorf("tool %q: start %s: %w", t.toolName, t.command[0], err)
		writeResultRecord(sink, t.toolName, "error: "+err.Error())
		return ToolResult{}, err
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- cmd.Wait()
	}()

	var runErr error
	select {
	case runErr = <-errCh:
	case <-ctx.Done():
		killProcessGroup(cmd.Process)
		<-errCh // reap the child; the outcome no longer matters
		var err error
		if errors.Is(ctx.Err(), context.Canceled) {
			err = fmt.Errorf("tool %q: %w", t.toolName, ctx.Err())
		} else {
			err = fmt.Errorf("tool %q: timed out after %s", t.toolName, remaining)
		}
		writeResultRecord(sink, t.toolName, "error: "+err.Error())
		return ToolResult{}, err
	}

	if runErr == nil {
		res := ToolResult{Output: trimSingleTrailingNewline(stdout.String())}
		writeResultRecord(sink, t.toolName, res.Output)
		return res, nil
	}

	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		res := ToolResult{
			Output: fmt.Sprintf("Tool %s failed with exit code %d.\nStderr: %s",
				t.toolName, exitErr.ExitCode(), truncateStderr(stderr.String())),
			Err: true,
		}
		writeResultRecord(sink, t.toolName, res.Output)
		return res, nil
	}
	err := fmt.Errorf("tool %q: %w", t.toolName, runErr)
	writeResultRecord(sink, t.toolName, "error: "+err.Error())
	return ToolResult{}, err
}

// killProcessGroup kills the tool and anything it spawned. The process was
// started with Setpgid, so its pid is its process group id.
func killProcessGroup(p *os.Process) {
	if p == nil {
		return
	}
	_ = syscall.Kill(-p.Pid, syscall.SIGKILL)
	_ = p.Kill()
}
