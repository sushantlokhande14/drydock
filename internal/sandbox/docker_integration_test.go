//go:build integration

// These tests run against a real Docker daemon. They are the evidence for the
// isolation claims: each one starts a sandbox and tries, from the inside, to
// do something the sandbox is supposed to prevent.
//
//	go test -tags integration ./internal/sandbox/
//
// They need the sandbox image (images/build.sh) and access to the Docker socket.
package sandbox_test

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sushantlokhande14/drydock/internal/engine"
	"github.com/sushantlokhande14/drydock/internal/sandbox"
)

func testImage() string {
	if image := os.Getenv("DRYDOCK_IMAGE"); image != "" {
		return image
	}
	return "drydock-sandbox:python"
}

const testVolume = "drydock-test-cache"

func newDriver(t *testing.T) *sandbox.DockerDriver {
	t.Helper()
	client, err := engine.New("")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.Ping(ctx); err != nil {
		t.Fatalf("Docker is not reachable: %v", err)
	}
	if present, err := client.ImageExists(ctx, testImage()); err != nil || !present {
		t.Fatalf("sandbox image %s is missing (err %v); build it with images/build.sh", testImage(), err)
	}
	return sandbox.NewDockerDriver(client)
}

func newSandbox(t *testing.T, driver sandbox.Driver, purpose sandbox.Purpose) sandbox.Sandbox {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	sb, err := driver.Create(ctx, sandbox.Spec{Image: testImage(), Purpose: purpose, Limits: sandbox.DefaultLimits, CacheVolume: testVolume})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = sb.Close(ctx)
	})
	return sb
}

func sh(t *testing.T, sb sandbox.Sandbox, script string) sandbox.Result {
	t.Helper()
	result, err := sb.Exec(context.Background(), sandbox.Command{Argv: sandbox.Shell(script), Timeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("exec %q: %v", script, err)
	}
	return result
}

func TestAnAgentSandboxIsLockedDown(t *testing.T) {
	sb := newSandbox(t, newDriver(t), sandbox.PurposeAgent)

	expect := func(name, script, want string) {
		t.Helper()
		got := strings.TrimSpace(sh(t, sb, script).Stdout)
		if got != want {
			t.Errorf("%s: `%s` printed %q, want %q", name, script, got, want)
		}
	}
	mustFail := func(name, script string) {
		t.Helper()
		if result := sh(t, sb, script); result.OK() {
			t.Errorf("%s: `%s` succeeded; output %q", name, script, result.Combined())
		}
	}

	expect("runs as an unprivileged user", "id -u", "1000")
	expect("holds no capabilities", `awk '/^CapEff/ {print $2}' /proc/self/status`, "0000000000000000")
	expect("cannot acquire any through setuid", `awk '/^CapBnd/ {print $2}' /proc/self/status`, "0000000000000000")
	expect("cannot gain privileges", `awk '/^NoNewPrivs/ {print $2}' /proc/self/status`, "1")
	// The limits are read from the kernel's own accounting. Hosts expose it in one of two layouts
	// (cgroup v2, or v1 with a directory per controller), so each check reads whichever is there.
	const cg = "/sys/fs/cgroup"
	expect("memory is capped at the limit",
		"cat "+cg+"/memory.max 2>/dev/null || cat "+cg+"/memory/memory.limit_in_bytes", "536870912")
	expect("swap is not available",
		"if [ -f "+cg+"/memory.swap.max ]; then cat "+cg+"/memory.swap.max; else echo $(( $(cat "+cg+"/memory/memory.memsw.limit_in_bytes) - $(cat "+cg+"/memory/memory.limit_in_bytes) )); fi", "0")
	expect("process count is capped",
		"cat "+cg+"/pids.max 2>/dev/null || cat "+cg+"/pids/pids.max", "256")
	expect("CPU is capped at one core",
		"cat "+cg+"/cpu.max 2>/dev/null || echo \"$(cat "+cg+"/cpu/cpu.cfs_quota_us) $(cat "+cg+"/cpu/cpu.cfs_period_us)\"", "100000 100000")
	expect("the only network interface is loopback", "ls /sys/class/net", "lo")
	expect("the workspace and /tmp are writable", "touch /workspace/a /tmp/b && echo ok", "ok")

	mustFail("the root file system is read-only", "touch /etc/drydock-probe")
	mustFail("installed code cannot be modified", "touch /usr/local/lib/python3.11/site-packages/probe.py")
	mustFail("the repository cache is read-only", "touch /cache/drydock-probe")
	mustFail("nothing outside is reachable", `python -c "import socket; socket.create_connection(('1.1.1.1', 53), timeout=3)"`)
	mustFail("names do not resolve", `python -c "import socket; socket.getaddrinfo('github.com', 443)"`)
}

func TestAForkBombHitsThePidLimitAndTheSandboxSurvives(t *testing.T) {
	sb := newSandbox(t, newDriver(t), sandbox.PurposeAgent)

	bomb := `python - <<'PY'
import os, sys
children = 0
try:
    while True:
        if os.fork() == 0:
            import time; time.sleep(3); os._exit(0)
        children += 1
except OSError as error:
    print(children, type(error).__name__)
    sys.exit(0)
PY`
	result, err := sb.Exec(context.Background(), sandbox.Command{Argv: sandbox.Shell(bomb), Timeout: 20 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	var children int
	var failure string
	if _, err := fmt.Sscanf(result.Stdout, "%d %s", &children, &failure); err != nil {
		t.Fatalf("the fork loop was not stopped by the limit: %+v", result)
	}
	// 256 is the limit for the whole sandbox; a few slots are taken by init, the shell and python.
	if children < 200 || children > 256 {
		t.Errorf("forked %d children before %s, expected just under 256", children, failure)
	}
	// While those children are alive the sandbox is full: not even a new command can start. That is
	// the limit doing its job. Once they exit, the sandbox is usable again.
	time.Sleep(4 * time.Second)
	if got := strings.TrimSpace(sh(t, sb, "echo alive").Stdout); got != "alive" {
		t.Fatalf("the sandbox did not recover: %q", got)
	}
}

func TestAMemoryHogIsKilledAndTheSandboxSurvives(t *testing.T) {
	sb := newSandbox(t, newDriver(t), sandbox.PurposeAgent)

	result := sh(t, sb, `python -c "x = bytearray(900 * 1024 * 1024); print('survived', len(x))"`)

	if result.OK() || strings.Contains(result.Stdout, "survived") {
		t.Fatalf("allocating 900 MiB inside a 512 MiB sandbox succeeded: %+v", result)
	}
	if got := strings.TrimSpace(sh(t, sb, "echo alive").Stdout); got != "alive" {
		t.Fatalf("the sandbox did not survive the out-of-memory kill: %q", got)
	}
}

func TestACommandPastItsDeadlineIsKilledInsideTheSandbox(t *testing.T) {
	sb := newSandbox(t, newDriver(t), sandbox.PurposeAgent)

	started := time.Now()
	// One child in the foreground and one in the background: both must go.
	result, err := sb.Exec(context.Background(), sandbox.Command{Argv: sandbox.Shell("echo started; sleep 61 & sleep 61; echo finished"), Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}

	if !result.TimedOut || strings.Contains(result.Stdout, "finished") || !strings.Contains(result.Stdout, "started") {
		t.Fatalf("result: %+v", result)
	}
	if elapsed := time.Since(started); elapsed > 8*time.Second {
		t.Fatalf("a 1 s deadline took %s to enforce", elapsed)
	}
	// The processes are gone, not merely abandoned by their caller. (The sandbox's own keep-alive
	// process is also a sleep, which is why this looks for the exact command line.)
	count := `n=0; for f in /proc/[0-9]*/cmdline; do if tr '\0' ' ' < "$f" 2>/dev/null | grep -q '^sleep 61'; then n=$((n+1)); fi; done; echo $n`
	time.Sleep(3 * time.Second) // TERM at the deadline, KILL two seconds later
	if left := strings.TrimSpace(sh(t, sb, count).Stdout); left != "0" {
		t.Fatalf("%s process(es) started by the command are still running after the deadline", left)
	}
}

func TestOutputIsCappedInMemoryButStreamedInFull(t *testing.T) {
	sb := newSandbox(t, newDriver(t), sandbox.PurposeAgent)

	var streamed atomic.Int64
	result, err := sb.Exec(context.Background(), sandbox.Command{
		Argv:      sandbox.Shell("yes drydock | head -c 5000000"),
		MaxOutput: 1000,
		OnOutput:  func(_ string, chunk []byte) { streamed.Add(int64(len(chunk))) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Stdout) != 1000 || !result.Truncated || !result.OK() {
		t.Fatalf("kept %d bytes, truncated=%v, exit %d", len(result.Stdout), result.Truncated, result.ExitCode)
	}
	if streamed.Load() != 5_000_000 {
		t.Fatalf("streamed %d bytes, want all 5000000", streamed.Load())
	}
}

func TestStdinStreamsAndExitCodes(t *testing.T) {
	sb := newSandbox(t, newDriver(t), sandbox.PurposeAgent)

	echoed, err := sb.Exec(context.Background(), sandbox.Command{Argv: []string{"cat"}, Stdin: strings.NewReader("over stdin\n")})
	if err != nil || echoed.Stdout != "over stdin\n" {
		t.Fatalf("stdin round trip: %+v %v", echoed, err)
	}
	split := sh(t, sb, "echo to-stdout; echo to-stderr >&2; exit 7")
	if split.Stdout != "to-stdout\n" || split.Stderr != "to-stderr\n" || split.ExitCode != 7 || split.OK() {
		t.Fatalf("streams and exit code: %+v", split)
	}
}

func TestFilesRoundTripWhateverTheyContain(t *testing.T) {
	sb := newSandbox(t, newDriver(t), sandbox.PurposeAgent)
	content := "plain\n'single' \"double\" `backtick` $HOME $(reboot) \\ \t tabs\nunicode: café ☃\nno trailing newline"

	if err := sandbox.WriteFile(context.Background(), sb, "/workspace/repo/deep/dir/file.txt", []byte(content)); err != nil {
		t.Fatal(err)
	}
	got, truncated, err := sandbox.ReadFile(context.Background(), sb, "/workspace/repo/deep/dir/file.txt", 1<<20)
	if err != nil || truncated || got != content {
		t.Fatalf("read back %q (truncated %v, err %v)", got, truncated, err)
	}
	if _, _, err := sandbox.ReadFile(context.Background(), sb, "/workspace/repo/missing.txt", 100); err == nil {
		t.Fatal("reading a missing file should fail")
	}
}

func TestReapRemovesAnotherInstancesSandboxesOnly(t *testing.T) {
	crashed := newDriver(t)
	orphan := newSandbox(t, crashed, sandbox.PurposeAgent)
	restarted := newDriver(t)
	own := newSandbox(t, restarted, sandbox.PurposeAgent)

	removed, err := restarted.Reap(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if removed < 1 {
		t.Fatalf("reaped %d sandboxes, want at least the orphan", removed)
	}
	if _, err := orphan.Exec(context.Background(), sandbox.Command{Argv: []string{"true"}}); err == nil {
		t.Fatal("the orphaned sandbox is still running")
	}
	if got := strings.TrimSpace(sh(t, own, "echo mine").Stdout); got != "mine" {
		t.Fatalf("the reaper removed a sandbox of its own instance: %q", got)
	}
}

func archive(t *testing.T, files map[string]string) *bytes.Buffer {
	t.Helper()
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	for name, content := range files {
		if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return &buffer
}

func TestARepositoryIsImportedOnceAndCheckedOutWithoutCopyingObjects(t *testing.T) {
	driver := newDriver(t)
	cache := sandbox.NewRepoCache(driver, testImage(), testVolume, time.Hour)
	t.Cleanup(func() { cache.Close(context.Background()) })

	mirror, err := cache.Import(context.Background(), "integration-"+t.Name(), archive(t, map[string]string{
		"calc.py":            "def add(a, b):\n    return a + b\n",
		"tests/test_calc.py": "from calc import add\n\n\ndef test_add():\n    assert add(2, 2) == 4\n",
	}))
	if err != nil {
		t.Fatal(err)
	}

	sb := newSandbox(t, driver, sandbox.PurposeAgent)
	checkout, err := sandbox.Checkout(context.Background(), sb, mirror, "")
	if err != nil || !checkout.OK() {
		t.Fatalf("checkout: %+v %v", checkout, err)
	}
	if commit := strings.TrimSpace(checkout.Stdout); !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(commit) {
		t.Fatalf("checkout did not print a commit id: %q", checkout.Stdout)
	}

	at := func(script string) string {
		return strings.TrimSpace(sh(t, sb, "cd "+sandbox.WorkspaceDir+" && "+script).Stdout)
	}
	if got := at("git status --porcelain | wc -l"); got != "0" {
		t.Errorf("a fresh checkout has %s modified file(s)", got)
	}
	// The clone borrows the mirror's objects instead of copying them.
	if got := at("cat .git/objects/info/alternates"); got != mirror+"/objects" {
		t.Errorf("alternates = %q, want the mirror's object store", got)
	}
	if got := at("find .git/objects -type f -not -path '*/info/*' | wc -l"); got != "0" {
		t.Errorf("the clone copied %s object file(s)", got)
	}
	if result := sh(t, sb, "cd "+sandbox.WorkspaceDir+" && python -m pytest -q -p no:cacheprovider"); !result.OK() {
		t.Errorf("the project's tests do not run in the sandbox: %s", result.Combined())
	}
	// The agent can commit in its own clone, and the shared mirror stays untouched and unwritable.
	if result := sh(t, sb, "cd "+sandbox.WorkspaceDir+" && echo x >> calc.py && git commit -qam change && git log --oneline | wc -l"); strings.TrimSpace(result.Stdout) != "2" {
		t.Errorf("committing in the clone: %+v", result)
	}
	if result := sh(t, sb, "touch "+mirror+"/probe"); result.OK() {
		t.Error("an agent sandbox could write to the mirror")
	}
}

func TestOnlyTheFetcherHasANetworkAndAWritableCache(t *testing.T) {
	fetcher := newSandbox(t, newDriver(t), sandbox.PurposeFetcher)

	if got := strings.TrimSpace(sh(t, fetcher, "ls /sys/class/net | grep -vc '^lo$'").Stdout); got == "0" {
		t.Error("the fetcher has no network interface besides loopback")
	}
	if result := sh(t, fetcher, "touch /cache/.probe && rm /cache/.probe"); !result.OK() {
		t.Errorf("the fetcher cannot write to the cache: %s", result.Combined())
	}
	// Everything else about the fetcher is as strict as an agent sandbox.
	if got := strings.TrimSpace(sh(t, fetcher, `id -u; awk '/^CapEff/ {print $2}' /proc/self/status`).Stdout); got != "1000\n0000000000000000" {
		t.Errorf("fetcher identity and capabilities: %q", got)
	}
	if result := sh(t, fetcher, "touch /etc/drydock-probe"); result.OK() {
		t.Error("the fetcher's root file system is writable")
	}
}
