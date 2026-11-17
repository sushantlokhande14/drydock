// Package sandboxtest provides an in-memory sandbox for tests that exercise
// the layers above the driver without a container runtime.
package sandboxtest

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sushantlokhande14/drydock/internal/sandbox"
)

// Fake is a sandbox with an in-memory file system. It understands the two
// commands sandbox.ReadFile and sandbox.WriteFile send, and hands everything
// else to Handler.
type Fake struct {
	Name string
	// Handler answers commands other than file reads and writes. It may be nil.
	Handler func(f *Fake, cmd sandbox.Command, stdin string) (sandbox.Result, bool)

	mu       sync.Mutex
	files    map[string]string
	commands []string
	closed   bool
}

// NewFake returns a sandbox holding the given files, keyed by absolute path.
func NewFake(name string, files map[string]string) *Fake {
	copied := map[string]string{}
	for path, content := range files {
		copied[path] = content
	}
	return &Fake{Name: name, files: copied}
}

func (f *Fake) ID() string { return f.Name }

// File returns a file's content and whether it exists.
func (f *Fake) File(path string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	content, ok := f.files[path]
	return content, ok
}

// SetFile creates or replaces a file.
func (f *Fake) SetFile(path, content string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[path] = content
}

// Commands returns every command run so far, each as its argv joined by spaces.
func (f *Fake) Commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.commands...)
}

// Closed reports whether Close was called.
func (f *Fake) Closed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

func (f *Fake) Exec(ctx context.Context, cmd sandbox.Command) (sandbox.Result, error) {
	if err := ctx.Err(); err != nil {
		return sandbox.Result{}, err
	}
	stdin := ""
	if cmd.Stdin != nil {
		raw, err := io.ReadAll(cmd.Stdin)
		if err != nil {
			return sandbox.Result{}, err
		}
		stdin = string(raw)
	}
	f.mu.Lock()
	f.commands = append(f.commands, strings.Join(cmd.Argv, " "))
	f.mu.Unlock()

	argv := cmd.Argv
	switch {
	case len(argv) == 3 && argv[0] == "cat" && argv[1] == "--":
		content, ok := f.File(argv[2])
		if !ok {
			return sandbox.Result{ExitCode: 1, Stderr: "cat: " + argv[2] + ": No such file or directory"}, nil
		}
		limit := cmd.MaxOutput
		if limit > 0 && len(content) > limit {
			return sandbox.Result{Stdout: content[:limit], Truncated: true}, nil
		}
		return sandbox.Result{Stdout: content}, nil
	case len(argv) == 5 && argv[0] == "sh" && strings.Contains(argv[2], `cat > "$1"`):
		f.SetFile(argv[4], stdin)
		return sandbox.Result{}, nil
	}
	if f.Handler != nil {
		if result, handled := f.Handler(f, cmd, stdin); handled {
			if cmd.OnOutput != nil && result.Stdout != "" {
				cmd.OnOutput("stdout", []byte(result.Stdout))
			}
			return result, nil
		}
	}
	return sandbox.Result{}, nil
}

func (f *Fake) Close(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

// Driver creates Fakes. It counts creations and can be made slow or failing.
type Driver struct {
	// New builds each sandbox. Nil means an empty Fake.
	New func(index int, spec sandbox.Spec) *Fake
	// Delay is how long Create takes.
	Delay time.Duration
	// Fail, if set, is returned by Create.
	Fail error

	mu      sync.Mutex
	created []*Fake
	specs   []sandbox.Spec
	count   atomic.Int64
}

func (d *Driver) Create(ctx context.Context, spec sandbox.Spec) (sandbox.Sandbox, error) {
	if d.Delay > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(d.Delay):
		}
	}
	if d.Fail != nil {
		return nil, d.Fail
	}
	index := int(d.count.Add(1))
	var fake *Fake
	if d.New != nil {
		fake = d.New(index, spec)
	} else {
		fake = NewFake(fmt.Sprintf("fake-%d", index), nil)
	}
	d.mu.Lock()
	d.created = append(d.created, fake)
	d.specs = append(d.specs, spec)
	d.mu.Unlock()
	return fake, nil
}

func (d *Driver) Reap(context.Context) (int, error) { return 0, nil }

// Created returns every sandbox the driver has made, in creation order.
func (d *Driver) Created() []*Fake {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]*Fake(nil), d.created...)
}

// Specs returns the spec of every sandbox the driver has made.
func (d *Driver) Specs() []sandbox.Spec {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]sandbox.Spec(nil), d.specs...)
}
