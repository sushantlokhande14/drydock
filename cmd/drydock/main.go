// Command drydock runs coding agents in isolated sandboxes.
//
//	drydock serve          run the control plane (API and web UI)
//	drydock run            run one bundled task and print what the agent does
//	drydock eval           run every bundled task with a real model and report how many it solves
//	drydock bench startup  measure how long a sandbox takes to become ready, four ways
//	drydock bench load     run many agents at once and report what the system sustained
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/sushantlokhande14/drydock/internal/agent"
	"github.com/sushantlokhande14/drydock/internal/engine"
	"github.com/sushantlokhande14/drydock/internal/events"
	"github.com/sushantlokhande14/drydock/internal/model"
	"github.com/sushantlokhande14/drydock/internal/orchestrator"
	"github.com/sushantlokhande14/drydock/internal/sandbox"
	"github.com/sushantlokhande14/drydock/internal/tasks"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(ctx, os.Args[2:])
	case "run":
		err = runOne(ctx, os.Args[2:])
	case "eval":
		err = evaluate(ctx, os.Args[2:])
	case "bench":
		if len(os.Args) < 3 {
			usage()
		}
		switch os.Args[2] {
		case "startup":
			err = benchStartup(ctx, os.Args[3:])
		case "load":
			err = benchLoad(ctx, os.Args[3:])
		default:
			usage()
		}
	default:
		usage()
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "drydock:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: drydock <serve | run | eval | bench startup | bench load> [flags]\n       drydock <command> -h   for a command's flags")
	os.Exit(2)
}

// options are the settings every command shares.
type options struct {
	dockerHost    string
	image         string
	cacheVolume   string
	dataDir       string
	tasksDir      string
	modelBaseURL  string
	maxConcurrent int
	queueSize     int
	pool          int
	maxSteps      int
	thinkTime     time.Duration
}

func (o *options) register(flags *flag.FlagSet) {
	flags.StringVar(&o.dockerHost, "docker-host", "", "Docker daemon address (default: $DOCKER_HOST, then the local socket)")
	flags.StringVar(&o.image, "image", "drydock-sandbox:python", "sandbox image; build it with images/build.sh")
	flags.StringVar(&o.cacheVolume, "cache-volume", "drydock-cache", "Docker volume that holds repository mirrors")
	flags.StringVar(&o.dataDir, "data", "", "directory for run logs; empty keeps them in memory")
	flags.StringVar(&o.tasksDir, "tasks", "tasks", "directory of bundled tasks")
	flags.StringVar(&o.modelBaseURL, "model-url", "http://localhost:11434/v1", "base URL of an OpenAI-compatible API (the default is a local Ollama)")
	flags.IntVar(&o.maxConcurrent, "max-concurrent", 8, "runs executing at once")
	flags.IntVar(&o.queueSize, "queue", 0, "runs allowed to wait (default: four times max-concurrent)")
	flags.IntVar(&o.pool, "pool", 4, "sandboxes kept started ahead of demand")
	flags.IntVar(&o.maxSteps, "max-steps", 30, "model turns allowed per run")
	flags.DurationVar(&o.thinkTime, "think", 0, "delay before each scripted model turn, to imitate model latency")
}

// system is a booted orchestrator and everything it depends on.
type system struct {
	engine *engine.Client
	driver *sandbox.DockerDriver
	pool   *sandbox.Pool
	cache  *sandbox.RepoCache
	store  *events.Store
	orch   *orchestrator.Orchestrator
	tasks  []tasks.Loaded
}

func (o *options) boot(ctx context.Context) (*system, error) {
	client, err := engine.New(o.dockerHost)
	if err != nil {
		return nil, err
	}
	if err := client.Ping(ctx); err != nil {
		return nil, fmt.Errorf("cannot reach Docker: %w", err)
	}
	if present, err := client.ImageExists(ctx, o.image); err != nil {
		return nil, err
	} else if !present {
		return nil, fmt.Errorf("sandbox image %q not found; build it with images/build.sh", o.image)
	}

	driver := sandbox.NewDockerDriver(client)
	if reaped, err := driver.Reap(ctx); err != nil {
		return nil, fmt.Errorf("reaping orphaned sandboxes: %w", err)
	} else if reaped > 0 {
		fmt.Fprintf(os.Stderr, "removed %d sandbox(es) left by an earlier process\n", reaped)
	}

	store, err := events.Open(o.dataDir)
	if err != nil {
		return nil, err
	}
	cache := sandbox.NewRepoCache(driver, o.image, o.cacheVolume, 10*time.Minute)
	pool := sandbox.NewPool(driver, sandbox.Spec{
		Image:       o.image,
		Purpose:     sandbox.PurposeAgent,
		Limits:      sandbox.DefaultLimits,
		CacheVolume: o.cacheVolume,
	}, o.pool)

	budget := agent.DefaultBudget
	budget.MaxSteps = o.maxSteps
	orch := orchestrator.New(orchestrator.Config{
		MaxConcurrent: o.maxConcurrent,
		QueueSize:     o.queueSize,
		Image:         o.image,
		Budget:        budget,
		NewModel:      o.newModel,
	}, store, pool, cache)
	if restored, err := orch.Restore(); err != nil {
		return nil, fmt.Errorf("restoring run logs: %w", err)
	} else if restored > 0 {
		fmt.Fprintf(os.Stderr, "restored %d earlier run(s) from %s\n", restored, o.dataDir)
	}

	sys := &system{engine: client, driver: driver, pool: pool, cache: cache, store: store, orch: orch}
	if o.tasksDir != "" {
		found, err := tasks.Load(o.tasksDir)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		if sys.tasks, err = tasks.Import(ctx, cache, found); err != nil {
			sys.shutdown()
			return nil, err
		}
	}
	pool.Fill()
	orch.Start()
	return sys, nil
}

func (s *system) shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	s.orch.Shutdown(ctx)
	s.store.CloseAll()
}

// newModel builds the backend a run asked for.
func (o *options) newModel(spec orchestrator.TaskSpec) (model.Backend, error) {
	switch {
	case spec.Model == "scripted":
		if spec.Script == nil {
			return nil, errors.New(`model "scripted" needs a script`)
		}
		backend := model.NewScripted(*spec.Script)
		backend.ThinkTime = o.thinkTime
		return backend, nil
	case strings.HasPrefix(spec.Model, "openai:"):
		return &model.OpenAI{
			BaseURL: o.modelBaseURL,
			Model:   strings.TrimPrefix(spec.Model, "openai:"),
			APIKey:  os.Getenv("DRYDOCK_API_KEY"),
		}, nil
	}
	return nil, fmt.Errorf(`unknown model %q (use "scripted" or "openai:<name>")`, spec.Model)
}
