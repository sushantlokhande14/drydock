package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"time"

	"github.com/sushantlokhande14/drydock/internal/engine"
)

const (
	labelManaged  = "drydock.managed"
	labelInstance = "drydock.instance"
	labelPurpose  = "drydock.purpose"

	// The unprivileged user every image provides. Nothing in a sandbox runs as root.
	sandboxUser = "1000:1000"
)

// DockerDriver creates sandboxes as containers on one Docker daemon.
type DockerDriver struct {
	engine   *engine.Client
	instance string
}

// NewDockerDriver returns a driver whose sandboxes are labelled with a fresh
// instance id, which is how Reap tells this process's sandboxes from orphans.
func NewDockerDriver(client *engine.Client) *DockerDriver {
	return &DockerDriver{engine: client, instance: randomID(6)}
}

func randomID(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// containerSpec turns a sandbox spec into the container that enforces it.
// This function is the security boundary of the system, so each line is here for a reason.
func (d *DockerDriver) containerSpec(spec Spec) engine.ContainerSpec {
	limits := spec.Limits
	if limits == (Limits{}) {
		limits = DefaultLimits
	}
	labels := map[string]string{
		labelManaged:  "true",
		labelInstance: d.instance,
		labelPurpose:  string(spec.Purpose),
	}
	for key, value := range spec.Labels {
		labels[key] = value
	}

	host := engine.HostConfig{
		// No network: an agent that reads a hostile repository cannot exfiltrate it or fetch a payload.
		NetworkMode: "none",
		// Nothing written outside the tmpfs mounts below survives or is even possible.
		ReadonlyRootfs: true,
		// No capabilities, and no way to regain them through a setuid binary.
		CapDrop:     []string{"ALL"},
		SecurityOpt: []string{"no-new-privileges"},
		// A fork bomb hits this limit, not the host.
		PidsLimit: limits.Pids,
		// Swap equal to memory means no swap: the limit is the limit.
		Memory:     limits.MemoryMB << 20,
		MemorySwap: limits.MemoryMB << 20,
		NanoCPUs:   int64(limits.CPUs * 1e9),
		Tmpfs: map[string]string{
			"/workspace":  fmt.Sprintf("rw,exec,nosuid,nodev,size=%dm,uid=1000,gid=1000,mode=0755", limits.WorkspaceMB),
			"/tmp":        fmt.Sprintf("rw,exec,nosuid,nodev,size=%dm,mode=1777", limits.TmpMB),
			"/home/agent": "rw,exec,nosuid,nodev,size=64m,uid=1000,gid=1000,mode=0755",
		},
		// An init process reaps what test runners leave behind, so zombies do not eat the pid limit.
		Init: true,
	}
	if spec.CacheVolume != "" {
		mode := "ro"
		if spec.Purpose == PurposeFetcher {
			mode = "rw"
		}
		host.Binds = []string{spec.CacheVolume + ":/cache:" + mode}
	}
	if spec.Purpose == PurposeFetcher {
		// The fetcher talks to git hosts. It runs only fixed git commands on validated URLs.
		host.NetworkMode = "bridge"
	}

	return engine.ContainerSpec{
		Image:      spec.Image,
		Cmd:        []string{"sleep", "infinity"},
		User:       sandboxUser,
		WorkingDir: "/workspace",
		Env: []string{
			"HOME=/home/agent",
			"GIT_TERMINAL_PROMPT=0",
			"PYTHONDONTWRITEBYTECODE=1",
			"PIP_DISABLE_PIP_VERSION_CHECK=1",
			"NO_COLOR=1",
		},
		Labels:     labels,
		HostConfig: host,
	}
}

// Create starts a sandbox. The context bounds creation only.
func (d *DockerDriver) Create(ctx context.Context, spec Spec) (Sandbox, error) {
	name := "drydock-" + string(spec.Purpose) + "-" + randomID(6)
	id, err := d.engine.CreateContainer(ctx, name, d.containerSpec(spec))
	if err != nil {
		return nil, fmt.Errorf("sandbox: create: %w", err)
	}
	if err := d.engine.StartContainer(ctx, id); err != nil {
		// Do not leave a created-but-unstarted container behind. The caller's context may be done.
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = d.engine.RemoveContainer(cleanup, id)
		return nil, fmt.Errorf("sandbox: start: %w", err)
	}
	return &dockerSandbox{engine: d.engine, id: id}, nil
}

// Reap removes containers that carry the drydock label but belong to another
// instance: sandboxes whose process crashed or was killed before closing them.
func (d *DockerDriver) Reap(ctx context.Context) (int, error) {
	containers, err := d.engine.ListContainers(ctx, labelManaged+"=true")
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, container := range containers {
		if container.Labels[labelInstance] == d.instance {
			continue
		}
		if err := d.engine.RemoveContainer(ctx, container.ID); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

type dockerSandbox struct {
	engine *engine.Client
	id     string
}

func (s *dockerSandbox) ID() string { return s.id[:12] }

func (s *dockerSandbox) Exec(ctx context.Context, cmd Command) (Result, error) {
	timeout := cmd.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	limit := cmd.MaxOutput
	if limit <= 0 {
		limit = DefaultMaxOutput
	}

	// The deadline is enforced inside the sandbox by timeout(1): TERM at the deadline, KILL two
	// seconds later. Enforcing it from outside would only stop this process waiting; the daemon
	// leaves an exec running when its client disconnects.
	seconds := strconv.FormatFloat(timeout.Seconds(), 'f', 3, 64)
	argv := append([]string{"timeout", "--signal=TERM", "--kill-after=2", seconds}, cmd.Argv...)

	stdout := &capture{limit: limit, stream: "stdout", onOutput: cmd.OnOutput}
	stderr := &capture{limit: limit, stream: "stderr", onOutput: cmd.OnOutput}

	// A margin beyond the in-sandbox deadline, in case the daemon itself stops answering.
	waitCtx, cancel := context.WithTimeout(ctx, timeout+15*time.Second)
	defer cancel()

	started := time.Now()
	code, err := s.engine.Exec(waitCtx, s.id, engine.ExecOptions{
		Cmd:    argv,
		Dir:    cmd.Dir,
		Env:    cmd.Env,
		Stdin:  cmd.Stdin,
		Stdout: stdout,
		Stderr: stderr,
	})
	result := Result{
		ExitCode:  code,
		Stdout:    stdout.String(),
		Stderr:    stderr.String(),
		Truncated: stdout.truncated || stderr.truncated,
		Duration:  time.Since(started),
	}
	if err != nil {
		return result, err
	}
	// timeout(1) exits 124 when it sent TERM and 137 when it had to send KILL.
	if (code == 124 || code == 137) && result.Duration >= timeout {
		result.TimedOut = true
	}
	return result, nil
}

func (s *dockerSandbox) Close(ctx context.Context) error {
	return s.engine.RemoveContainer(ctx, s.id)
}
