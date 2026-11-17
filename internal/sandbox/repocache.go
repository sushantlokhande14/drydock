package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"
)

// RepoCache keeps bare mirrors of repositories on a shared volume.
//
// Two different sandboxes touch a repository, with opposite privileges. The
// fetcher has network access and a writable cache, and runs only the fixed
// git commands in this file. An agent sandbox has no network and sees the
// cache read-only; it clones from the mirror with --shared, which borrows the
// mirror's objects instead of copying them and so takes milliseconds. The
// component that talks to the internet never runs a model-chosen command, and
// the component that runs model-chosen commands cannot talk to the internet.
type RepoCache struct {
	driver Driver
	spec   Spec
	// maxAge is how long a mirror is trusted before it is fetched again.
	maxAge time.Duration

	mu      sync.Mutex
	fetcher Sandbox
	fresh   map[string]time.Time
	flights map[string]*flight
}

type flight struct {
	done chan struct{}
	err  error
}

// NewRepoCache returns a cache that stores mirrors on volume, using image for the fetcher.
func NewRepoCache(driver Driver, image, volume string, maxAge time.Duration) *RepoCache {
	return &RepoCache{
		driver: driver,
		spec: Spec{
			Image:       image,
			Purpose:     PurposeFetcher,
			CacheVolume: volume,
			Limits:      Limits{MemoryMB: 1024, CPUs: 2, Pids: 256, WorkspaceMB: 1024, TmpMB: 1024},
		},
		maxAge:  maxAge,
		fresh:   map[string]time.Time{},
		flights: map[string]*flight{},
	}
}

// Only plain https clone URLs. This rejects option injection ("--upload-pack=..."),
// git's command-running transports ("ext::sh -c ..."), local paths and embedded credentials.
var cloneURL = regexp.MustCompile(`^https://[A-Za-z0-9.-]+(:[0-9]+)?/[A-Za-z0-9._~/-]+$`)

// ValidateURL reports whether url is a repository address the fetcher may be given.
func ValidateURL(url string) error {
	if !cloneURL.MatchString(url) || strings.Contains(url, "..") {
		return fmt.Errorf("sandbox: %q is not an allowed repository URL (https only, no credentials or options)", url)
	}
	return nil
}

// MirrorPath is where a repository's mirror lives inside every sandbox.
func MirrorPath(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "/cache/" + hex.EncodeToString(sum[:8]) + ".git"
}

func (c *RepoCache) fetcherSandbox(ctx context.Context) (Sandbox, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fetcher != nil {
		return c.fetcher, nil
	}
	sb, err := c.driver.Create(ctx, c.spec)
	if err != nil {
		return nil, err
	}
	c.fetcher = sb
	return sb, nil
}

// single runs fn once per key at a time: callers that arrive while a fetch of the
// same repository is in flight wait for it instead of starting another.
func (c *RepoCache) single(ctx context.Context, key string, fn func() error) error {
	c.mu.Lock()
	if running, ok := c.flights[key]; ok {
		c.mu.Unlock()
		select {
		case <-running.done:
			return running.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	mine := &flight{done: make(chan struct{})}
	c.flights[key] = mine
	c.mu.Unlock()

	mine.err = fn()

	c.mu.Lock()
	delete(c.flights, key)
	c.mu.Unlock()
	close(mine.done)
	return mine.err
}

// Ensure makes sure a mirror of url exists and is no older than the cache's
// maximum age, and returns its path. fetched reports whether the network was used.
func (c *RepoCache) Ensure(ctx context.Context, url string) (mirror string, fetched bool, err error) {
	if err := ValidateURL(url); err != nil {
		return "", false, err
	}
	mirror = MirrorPath(url)

	c.mu.Lock()
	last, known := c.fresh[mirror]
	c.mu.Unlock()
	if known && time.Since(last) < c.maxAge {
		return mirror, false, nil
	}

	err = c.single(ctx, mirror, func() error {
		sb, err := c.fetcherSandbox(ctx)
		if err != nil {
			return err
		}
		// Clone into a temporary name and rename, so a sandbox never sees a half-written mirror.
		script := `set -e
if [ -d "$1" ]; then
  git -C "$1" fetch --quiet --prune origin '+refs/heads/*:refs/heads/*' '+refs/tags/*:refs/tags/*'
else
  rm -rf "$1.partial"
  git clone --quiet --mirror -- "$2" "$1.partial"
  mv "$1.partial" "$1"
fi`
		result, err := sb.Exec(ctx, Command{
			Argv:    []string{"bash", "-c", script, "bash", mirror, url},
			Timeout: 10 * time.Minute,
		})
		if err != nil {
			return err
		}
		if !result.OK() {
			return fmt.Errorf("sandbox: mirroring %s: %s", url, strings.TrimSpace(result.Combined()))
		}
		fetched = true
		c.mu.Lock()
		c.fresh[mirror] = time.Now()
		c.mu.Unlock()
		return nil
	})
	return mirror, fetched, err
}

// Import turns a tar archive of a plain directory into a mirror named by key
// and returns its path. It is how local task repositories enter the cache.
func (c *RepoCache) Import(ctx context.Context, key string, archive io.Reader) (string, error) {
	mirror := MirrorPath("local:" + key)
	err := c.single(ctx, mirror, func() error {
		sb, err := c.fetcherSandbox(ctx)
		if err != nil {
			return err
		}
		script := `set -e
work="$(mktemp -d)"
tar -x -C "$work"
git -C "$work" init --quiet --initial-branch=main
git -C "$work" add -A
git -C "$work" commit --quiet -m "import"
rm -rf "$1" "$1.partial"
git clone --quiet --bare "$work" "$1.partial"
mv "$1.partial" "$1"
rm -rf "$work"`
		result, err := sb.Exec(ctx, Command{
			Argv:    []string{"bash", "-c", script, "bash", mirror},
			Stdin:   archive,
			Timeout: 2 * time.Minute,
		})
		if err != nil {
			return err
		}
		if !result.OK() {
			return fmt.Errorf("sandbox: importing %s: %s", key, strings.TrimSpace(result.Combined()))
		}
		c.mu.Lock()
		c.fresh[mirror] = time.Now()
		c.mu.Unlock()
		return nil
	})
	return mirror, err
}

// Close removes the fetcher. Mirrors stay on the volume for the next process.
func (c *RepoCache) Close(ctx context.Context) {
	c.mu.Lock()
	fetcher := c.fetcher
	c.fetcher = nil
	c.mu.Unlock()
	if fetcher != nil {
		_ = fetcher.Close(ctx)
	}
}

// WorkspaceDir is where the repository is checked out in a sandbox.
const WorkspaceDir = "/workspace/repo"

var refPattern = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,200}$`)

// Checkout puts a working tree of the mirror at ref into the sandbox.
//
// --shared makes the clone borrow the mirror's object store through git's
// alternates mechanism, so no objects are copied: the cost is writing the
// working tree, not transferring history.
func Checkout(ctx context.Context, sb Sandbox, mirror, ref string) (Result, error) {
	if ref == "" {
		ref = "HEAD"
	}
	if !refPattern.MatchString(ref) || strings.HasPrefix(ref, "-") {
		return Result{}, fmt.Errorf("sandbox: %q is not a valid ref", ref)
	}
	script := `set -e
git clone --quiet --shared --no-checkout -- "$1" "$3"
git -C "$3" checkout --quiet --detach "$2"
git -C "$3" rev-parse HEAD`
	return sb.Exec(ctx, Command{
		Argv:    []string{"bash", "-c", script, "bash", mirror, ref, WorkspaceDir},
		Timeout: 2 * time.Minute,
	})
}

// RepoPath resolves a path relative to the repository root to its location in
// the sandbox. The sandbox already confines an agent; this keeps it inside the
// checkout, so a patch never contains a file from somewhere else, and keeps it
// out of .git, which belongs to the harness.
func RepoPath(given string) (string, error) {
	given = strings.TrimSpace(given)
	if given == "" || given == "." {
		return WorkspaceDir, nil
	}
	given = strings.TrimPrefix(given, WorkspaceDir+"/")
	if strings.HasPrefix(given, "/") || strings.Contains(given, "\x00") {
		return "", fmt.Errorf("use a path relative to the repository root, not %q", given)
	}
	cleaned := path.Clean("/" + given)
	if cleaned == "/.git" || strings.HasPrefix(cleaned, "/.git/") {
		return "", fmt.Errorf("the .git directory is managed by the harness")
	}
	if cleaned == "/" {
		return WorkspaceDir, nil
	}
	return WorkspaceDir + cleaned, nil
}
