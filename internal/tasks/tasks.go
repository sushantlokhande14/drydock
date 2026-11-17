// Package tasks loads the bundled example tasks: small repositories with a
// seeded defect, an issue describing it, tests the agent cannot see, and a
// recorded solution for the replay backend.
package tasks

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sushantlokhande14/drydock/internal/model"
	"github.com/sushantlokhande14/drydock/internal/orchestrator"
	"github.com/sushantlokhande14/drydock/internal/sandbox"
)

// Task is one example task as it exists on disk.
//
//	tasks/<name>/task.json   title, issue, commands, recorded solution
//	tasks/<name>/repo/       the repository the agent works on
//	tasks/<name>/hidden/     files added only for verification
type Task struct {
	Name          string        `json:"-"`
	Title         string        `json:"title"`
	Issue         string        `json:"issue"`
	TestCommand   string        `json:"testCommand"`
	VerifyCommand string        `json:"verifyCommand"`
	Script        *model.Script `json:"script"`

	dir string
}

// Loaded is a task whose repository is in the cache and can be run.
type Loaded struct {
	Task
	Mirror string
	Hidden map[string]string
}

// Load reads every task under dir, in name order.
func Load(dir string) ([]Task, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []Task
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		raw, err := os.ReadFile(filepath.Join(path, "task.json"))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		task := Task{Name: entry.Name(), dir: path}
		if err := json.Unmarshal(raw, &task); err != nil {
			return nil, fmt.Errorf("tasks: %s: %w", entry.Name(), err)
		}
		if task.Title == "" || task.TestCommand == "" {
			return nil, fmt.Errorf("tasks: %s: title and testCommand are required", entry.Name())
		}
		out = append(out, task)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Archive returns the task's repository as a tar stream.
func (t Task) Archive() (io.Reader, error) {
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	root := filepath.Join(t.dir, "repo")
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if name := entry.Name(); name == "__pycache__" || name == ".pytest_cache" {
				return filepath.SkipDir
			}
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		// Normalise line endings: a checkout on Windows must produce the same repository as one on Linux.
		content = bytes.ReplaceAll(content, []byte("\r\n"), []byte("\n"))
		header := &tar.Header{Name: filepath.ToSlash(relative), Mode: 0o644, Size: int64(len(content))}
		if err := writer.WriteHeader(header); err != nil {
			return err
		}
		_, err = writer.Write(content)
		return err
	})
	if err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return &buffer, nil
}

// HiddenFiles returns the verification-only files, keyed by repository-relative path.
func (t Task) HiddenFiles() (map[string]string, error) {
	root := filepath.Join(t.dir, "hidden")
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return filepath.SkipAll
			}
			return err
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(relative)] = strings.ReplaceAll(string(content), "\r\n", "\n")
		return nil
	})
	return out, err
}

// Import puts every task's repository into the cache.
func Import(ctx context.Context, cache *sandbox.RepoCache, all []Task) ([]Loaded, error) {
	out := make([]Loaded, 0, len(all))
	for _, task := range all {
		archive, err := task.Archive()
		if err != nil {
			return nil, fmt.Errorf("tasks: %s: %w", task.Name, err)
		}
		mirror, err := cache.Import(ctx, task.Name, archive)
		if err != nil {
			return nil, fmt.Errorf("tasks: %s: %w", task.Name, err)
		}
		hidden, err := task.HiddenFiles()
		if err != nil {
			return nil, fmt.Errorf("tasks: %s: %w", task.Name, err)
		}
		out = append(out, Loaded{Task: task, Mirror: mirror, Hidden: hidden})
	}
	return out, nil
}

// Spec builds the run request for this task with the given model.
func (l Loaded) Spec(modelName string) orchestrator.TaskSpec {
	spec := orchestrator.TaskSpec{
		Title:         l.Title,
		Issue:         l.Issue,
		Mirror:        l.Mirror,
		TestCommand:   l.TestCommand,
		VerifyCommand: l.VerifyCommand,
		VerifyFiles:   l.Hidden,
		Model:         modelName,
	}
	if modelName == "scripted" {
		spec.Script = l.Script
	}
	return spec
}
