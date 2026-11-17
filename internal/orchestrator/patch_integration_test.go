//go:build integration

package orchestrator

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sushantlokhande14/drydock/internal/engine"
	"github.com/sushantlokhande14/drydock/internal/sandbox"
)

// A patch is the harness's statement of what an agent changed. These tests
// check that the agent cannot influence that statement through anything it
// can write to: its own .git directory, its commits, or its git configuration.

const patchVolume = "drydock-test-cache"

func patchImage() string {
	if image := os.Getenv("DRYDOCK_IMAGE"); image != "" {
		return image
	}
	return "drydock-sandbox:python"
}

func checkedOut(t *testing.T) (sandbox.Sandbox, string, string) {
	t.Helper()
	client, err := engine.New("")
	if err != nil {
		t.Fatal(err)
	}
	driver := sandbox.NewDockerDriver(client, fmt.Sprintf("patch-test-%d", time.Now().UnixNano()))
	cache := sandbox.NewRepoCache(driver, patchImage(), patchVolume, time.Hour)

	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	for _, file := range [][2]string{{"calc.py", "def pages(n):\n    return n // 10\n"}, {"notes.txt", "keep\n"}} {
		_ = writer.WriteHeader(&tar.Header{Name: file[0], Mode: 0o644, Size: int64(len(file[1]))})
		_, _ = writer.Write([]byte(file[1]))
	}
	_ = writer.Close()
	mirror, err := cache.Import(context.Background(), "patch-test", archive.Bytes())
	if err != nil {
		t.Fatal(err)
	}

	sb, err := driver.Create(context.Background(), sandbox.Spec{Image: patchImage(), Purpose: sandbox.PurposeAgent, Limits: sandbox.DefaultLimits, CacheVolume: patchVolume})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = sb.Close(ctx)
		cache.Close(ctx)
	})
	checkout, err := sandbox.Checkout(context.Background(), sb, mirror, "")
	if err != nil || !checkout.OK() {
		t.Fatalf("checkout: %+v %v", checkout, err)
	}
	return sb, mirror, strings.TrimSpace(checkout.Stdout)
}

func agentRuns(t *testing.T, sb sandbox.Sandbox, script string) {
	t.Helper()
	result, err := sb.Exec(context.Background(), sandbox.Command{Argv: sandbox.Shell(script), Dir: sandbox.WorkspaceDir})
	if err != nil || !result.OK() {
		t.Fatalf("`%s`: %+v %v", script, result, err)
	}
}

func assertRealChange(t *testing.T, sb sandbox.Sandbox, mirror, commit string) {
	t.Helper()
	diff, stats, err := collectPatch(context.Background(), sb, mirror, commit)
	if err != nil {
		t.Fatalf("collecting the patch failed: %v", err)
	}
	for _, want := range []string{"-    return n // 10", "+    return (n + 9) // 10", "+++ b/new_module.py", "+VALUE = 1"} {
		if !strings.Contains(diff, want) {
			t.Errorf("the patch is missing %q:\n%s", want, diff)
		}
	}
	if stats != (PatchStats{Files: 2, Additions: 2, Deletions: 1}) {
		t.Errorf("stats: %+v", stats)
	}
}

const realChange = `sed -i 's|n // 10|(n + 9) // 10|' calc.py && echo 'VALUE = 1' > new_module.py`

func TestAPatchIsCollectedWhenTheAgentDeletedItsGitDirectory(t *testing.T) {
	sb, mirror, commit := checkedOut(t)
	agentRuns(t, sb, realChange+" && rm -rf .git")

	assertRealChange(t, sb, mirror, commit)
}

func TestCommittingDoesNotHideAChange(t *testing.T) {
	sb, mirror, commit := checkedOut(t)
	// A diff taken against the workspace's own HEAD would now be empty.
	agentRuns(t, sb, realChange+" && git add -A && git commit -qm 'nothing to see' && test -z \"$(git diff HEAD)\"")

	assertRealChange(t, sb, mirror, commit)
}

func TestTheAgentsGitConfigurationCannotHideFiles(t *testing.T) {
	sb, mirror, commit := checkedOut(t)
	agentRuns(t, sb, realChange+` && echo 'new_module.py' > /home/agent/ignore && git config --global core.excludesFile /home/agent/ignore && git config --global diff.external /bin/true`)

	assertRealChange(t, sb, mirror, commit)
}

func TestAnUntouchedRepositoryProducesNoPatch(t *testing.T) {
	sb, mirror, commit := checkedOut(t)

	diff, stats, err := collectPatch(context.Background(), sb, mirror, commit)

	if err != nil || strings.TrimSpace(diff) != "" || stats != (PatchStats{}) {
		t.Fatalf("diff %q stats %+v err %v", diff, stats, err)
	}
}
