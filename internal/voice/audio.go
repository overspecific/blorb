// Package voice runs an agent as a voice session through AssemblyAI's Voice
// Agent API: microphone capture, speaker playback, the WebSocket client, and
// the command frontend.
package voice

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// playbackDrainWait bounds how long Close waits for a playback subprocess to
// finish after its stdin is closed before the process group is killed.
const playbackDrainWait = 2 * time.Second

// captureExitWait bounds how long Close waits to see whether a capture
// subprocess reached a non-zero exit on its own before killing it. A capture
// runs until the session ends, so reaching this bound is the normal case.
const captureExitWait = 250 * time.Millisecond

// audioReader reads raw 24 kHz 16-bit little-endian mono PCM from a capture
// subprocess's stdout. Close kills the process: a capture runs until the
// session ends, not until it runs dry.
type audioReader struct {
	cmd     *exec.Cmd
	r       io.ReadCloser
	waitErr chan error

	closeOnce sync.Once
	closeErr  error
}

// startCapture runs command and returns a reader over its stdout. The command
// must produce raw 24 kHz 16-bit little-endian mono PCM, which the config's
// defaults do.
func startCapture(ctx context.Context, command []string) (*audioReader, error) {
	if len(command) == 0 {
		return nil, errors.New("capture command is empty")
	}
	cmd := commandContext(ctx, command)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("capture %s: %w", command[0], err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("capture %s: %w", command[0], err)
	}
	a := &audioReader{cmd: cmd, r: stdout, waitErr: make(chan error, 1)}
	go func() { a.waitErr <- cmd.Wait() }()
	return a, nil
}

// Read returns the next chunk of captured PCM. A capture process that exits
// while the session runs shows up as an error (typically io.EOF) here.
func (a *audioReader) Read(p []byte) (int, error) {
	return a.r.Read(p)
}

// Close kills the capture process group and waits for it. A non-zero exit the
// process reached on its own is reported; the exit caused by Close's own kill
// is not. Closing twice is safe.
func (a *audioReader) Close() error {
	a.closeOnce.Do(func() {
		select {
		case err := <-a.waitErr:
			a.closeErr = exitStatus("capture", a.cmd, err)
		case <-time.After(captureExitWait):
			killProcessGroup(a.cmd.Process)
			<-a.waitErr
		}
	})
	return a.closeErr
}

// audioWriter writes raw 24 kHz 16-bit little-endian mono PCM to a playback
// subprocess's stdin. Close closes stdin and lets the process drain before
// waiting, so a player that flushes on EOF is not cut off.
type audioWriter struct {
	cmd     *exec.Cmd
	w       io.WriteCloser
	waitErr chan error

	closeOnce sync.Once
	closeErr  error
}

// startPlayback runs command and returns a writer over its stdin. The command
// must consume raw 24 kHz 16-bit little-endian mono PCM, which the config's
// defaults do.
func startPlayback(ctx context.Context, command []string) (*audioWriter, error) {
	if len(command) == 0 {
		return nil, errors.New("playback command is empty")
	}
	cmd := commandContext(ctx, command)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("playback %s: %w", command[0], err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("playback %s: %w", command[0], err)
	}
	a := &audioWriter{cmd: cmd, w: stdin, waitErr: make(chan error, 1)}
	go func() { a.waitErr <- cmd.Wait() }()
	return a, nil
}

// Write plays the next chunk of PCM. A playback process that exits while the
// session runs shows up as an error (typically a broken pipe) here.
func (a *audioWriter) Write(p []byte) (int, error) {
	return a.w.Write(p)
}

// Close closes the playback process's stdin, waits briefly for it to exit,
// and kills it if it does not. A non-zero exit reached on its own is reported.
// Closing twice is safe.
func (a *audioWriter) Close() error {
	a.closeOnce.Do(func() {
		// Closing stdin gives a well-behaved player its end-of-stream. A
		// process that ignores it (or never reads) is killed after a
		// bounded wait.
		_ = a.w.Close()
		select {
		case err := <-a.waitErr:
			a.closeErr = exitStatus("playback", a.cmd, err)
		case <-time.After(playbackDrainWait):
			killProcessGroup(a.cmd.Process)
			<-a.waitErr
		}
	})
	return a.closeErr
}

// commandContext builds a subprocess command with the tools package's
// conventions: the command runs as its own process group, so a kill reaches
// anything it spawned, and ctx cancellation kills it.
func commandContext(ctx context.Context, command []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}

// exitStatus turns a cmd.Wait result into an error naming the subprocess. A
// nil error stays nil.
func exitStatus(kind string, cmd *exec.Cmd, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s %s: %w", kind, cmd.Args[0], err)
}

// killProcessGroup kills the process and anything it spawned. The process was
// started with Setpgid, so its pid is its process group id.
func killProcessGroup(p *os.Process) {
	if p == nil {
		return
	}
	_ = syscall.Kill(-p.Pid, syscall.SIGKILL)
	_ = p.Kill()
}
