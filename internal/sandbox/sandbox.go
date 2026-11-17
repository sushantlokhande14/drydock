// Package sandbox runs untrusted commands in isolated environments.
//
// An agent's commands are chosen by a language model reading a repository it
// did not write, so every command is treated as hostile: no network, no
// capabilities, a read-only root, bounded memory, CPU, processes, time and
// output. The Driver interface keeps that contract independent of what
// provides the isolation.
package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// Limits bounds what one sandbox can consume.
type Limits struct {
	MemoryMB    int64
	CPUs        float64
	Pids        int64
	WorkspaceMB int64
	TmpMB       int64
}

// DefaultLimits is enough to build and test a small repository and nothing more.
var DefaultLimits = Limits{MemoryMB: 512, CPUs: 1, Pids: 256, WorkspaceMB: 512, TmpMB: 256}

// Purpose says what a sandbox is for. It decides the one privilege that varies.
type Purpose string

const (
	// PurposeAgent runs model-chosen commands. It never has network access.
	PurposeAgent Purpose = "agent"
	// PurposeVerify re-runs the tests on a clean checkout with only the patch applied.
	PurposeVerify Purpose = "verify"
	// PurposeFetcher clones and updates repository mirrors. It is the only kind with
	// network access and with a writable cache, and it never runs a model-chosen command.
	PurposeFetcher Purpose = "fetcher"
)

// Spec describes a sandbox to create.
type Spec struct {
	Image   string
	Purpose Purpose
	Limits  Limits
	// CacheVolume, when set, is mounted at /cache: read-only, except for a fetcher.
	CacheVolume string
	Labels      map[string]string
}

// Command is one command to run in a sandbox.
type Command struct {
	Argv  []string
	Dir   string
	Env   []string
	Stdin io.Reader
	// Timeout bounds the command inside the sandbox. Zero means DefaultTimeout.
	Timeout time.Duration
	// MaxOutput caps how much of each stream is kept. Zero means DefaultMaxOutput.
	MaxOutput int
	// OnOutput, if set, is called with each chunk as it arrives, for live streaming.
	OnOutput func(stream string, chunk []byte)
}

const (
	DefaultTimeout   = 120 * time.Second
	DefaultMaxOutput = 64 << 10
)

// Result is what a finished command produced.
type Result struct {
	ExitCode  int
	Stdout    string
	Stderr    string
	Truncated bool
	TimedOut  bool
	Duration  time.Duration
}

// OK reports whether the command exited zero.
func (r Result) OK() bool { return r.ExitCode == 0 && !r.TimedOut }

// Combined is stdout followed by stderr, which is how a person reads a terminal.
func (r Result) Combined() string {
	if r.Stderr == "" {
		return r.Stdout
	}
	if r.Stdout == "" {
		return r.Stderr
	}
	return r.Stdout + "\n" + r.Stderr
}

// Sandbox is one isolated environment. It is used by a single run and then closed;
// sandboxes are never reused across runs.
type Sandbox interface {
	ID() string
	Exec(ctx context.Context, cmd Command) (Result, error)
	Close(ctx context.Context) error
}

// ErrGone is returned by Exec when the sandbox no longer exists. A sandbox that
// waited in the pool can be removed from under it, by an operator or a daemon
// restart; the caller's remedy is to get another one, not to fail the run.
var ErrGone = errors.New("sandbox: no longer exists")

// Driver creates sandboxes.
type Driver interface {
	Create(ctx context.Context, spec Spec) (Sandbox, error)
	// Reap removes sandboxes left behind by earlier processes and returns how many it removed.
	Reap(ctx context.Context) (int, error)
}

// Shell is the argv for running a script through the shell.
func Shell(script string) []string { return []string{"bash", "-c", script} }

// WriteFile writes data to path, creating parent directories. The content goes
// over stdin, so it is never interpreted by a shell.
func WriteFile(ctx context.Context, sb Sandbox, path string, data []byte) error {
	result, err := sb.Exec(ctx, Command{
		Argv:  []string{"sh", "-c", `mkdir -p "$(dirname "$1")" && cat > "$1"`, "sh", path},
		Stdin: bytes.NewReader(data),
	})
	if err != nil {
		return err
	}
	if !result.OK() {
		return fmt.Errorf("sandbox: writing %s: %s", path, strings.TrimSpace(result.Combined()))
	}
	return nil
}

// ReadFile returns up to max bytes of path and whether the file was longer.
func ReadFile(ctx context.Context, sb Sandbox, path string, max int) (string, bool, error) {
	result, err := sb.Exec(ctx, Command{Argv: []string{"cat", "--", path}, MaxOutput: max})
	if err != nil {
		return "", false, err
	}
	if !result.OK() {
		return "", false, fmt.Errorf("sandbox: reading %s: %s", path, strings.TrimSpace(result.Stderr))
	}
	return result.Stdout, result.Truncated, nil
}

// capture keeps the first limit bytes of a stream, forwards every chunk to a
// listener, and remembers whether anything was dropped.
type capture struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	limit     int
	truncated bool
	stream    string
	onOutput  func(stream string, chunk []byte)
}

func (c *capture) Write(p []byte) (int, error) {
	c.mu.Lock()
	room := c.limit - c.buf.Len()
	if room > 0 {
		if len(p) > room {
			c.buf.Write(p[:room])
			c.truncated = true
		} else {
			c.buf.Write(p)
		}
	} else if len(p) > 0 {
		c.truncated = true
	}
	c.mu.Unlock()
	if c.onOutput != nil && len(p) > 0 {
		// The listener gets its own copy: the caller reuses p after Write returns.
		c.onOutput(c.stream, append([]byte(nil), p...))
	}
	return len(p), nil
}

func (c *capture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}
