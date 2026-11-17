// Package engine is a small client for the Docker Engine HTTP API.
//
// It implements only what the sandbox driver needs: create, start, list and
// remove containers, and run a command inside one with streamed output and
// optional stdin. Speaking the API directly keeps the dependency list at
// zero and makes the two interesting parts explicit: the hijacked connection
// an exec runs over, and the framing that multiplexes stdout and stderr on it.
package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Client talks to one Docker daemon.
type Client struct {
	http *http.Client
	dial func(ctx context.Context) (net.Conn, error)
}

// New connects to host, which may be "unix:///path/to/docker.sock" or
// "tcp://host:port". An empty host means $DOCKER_HOST, then the default socket.
func New(host string) (*Client, error) {
	if host == "" {
		host = os.Getenv("DOCKER_HOST")
	}
	if host == "" {
		host = "unix:///var/run/docker.sock"
	}
	network, address, ok := strings.Cut(host, "://")
	if !ok || (network != "unix" && network != "tcp") {
		return nil, fmt.Errorf("engine: unsupported docker host %q (want unix:// or tcp://)", host)
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	dial := func(ctx context.Context) (net.Conn, error) { return dialer.DialContext(ctx, network, address) }
	transport := &http.Transport{
		DialContext:         func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx) },
		MaxIdleConns:        256,
		MaxIdleConnsPerHost: 256,
		IdleConnTimeout:     60 * time.Second,
	}
	return &Client{http: &http.Client{Transport: transport}, dial: dial}, nil
}

// APIError is a non-2xx answer from the daemon.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("docker: %d %s", e.Status, e.Message) }

// IsNotFound reports whether err is the daemon saying the object does not exist.
func IsNotFound(err error) bool {
	var api *APIError
	return errors.As(err, &api) && api.Status == http.StatusNotFound
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return apiError(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func apiError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var parsed struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &parsed) == nil && parsed.Message != "" {
		return &APIError{Status: resp.StatusCode, Message: parsed.Message}
	}
	return &APIError{Status: resp.StatusCode, Message: strings.TrimSpace(string(raw))}
}

// Ping checks that the daemon answers.
func (c *Client) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/_ping", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return apiError(resp)
	}
	return nil
}

// HostConfig is the part of a container's configuration that decides how
// isolated it is. The field names are the daemon's.
type HostConfig struct {
	NetworkMode    string            `json:"NetworkMode,omitempty"`
	ReadonlyRootfs bool              `json:"ReadonlyRootfs"`
	CapDrop        []string          `json:"CapDrop,omitempty"`
	SecurityOpt    []string          `json:"SecurityOpt,omitempty"`
	PidsLimit      int64             `json:"PidsLimit,omitempty"`
	Memory         int64             `json:"Memory,omitempty"`
	MemorySwap     int64             `json:"MemorySwap,omitempty"`
	NanoCPUs       int64             `json:"NanoCpus,omitempty"`
	Tmpfs          map[string]string `json:"Tmpfs,omitempty"`
	Binds          []string          `json:"Binds,omitempty"`
	Init           bool              `json:"Init"`
	ExtraHosts     []string          `json:"ExtraHosts,omitempty"`
}

// ContainerSpec describes a container to create.
type ContainerSpec struct {
	Image      string            `json:"Image"`
	Cmd        []string          `json:"Cmd,omitempty"`
	Env        []string          `json:"Env,omitempty"`
	User       string            `json:"User,omitempty"`
	WorkingDir string            `json:"WorkingDir,omitempty"`
	Labels     map[string]string `json:"Labels,omitempty"`
	HostConfig HostConfig        `json:"HostConfig"`
}

// CreateContainer creates a container and returns its id. It is not started.
func (c *Client) CreateContainer(ctx context.Context, name string, spec ContainerSpec) (string, error) {
	path := "/containers/create"
	if name != "" {
		path += "?name=" + url.QueryEscape(name)
	}
	var out struct {
		ID string `json:"Id"`
	}
	if err := c.do(ctx, http.MethodPost, path, spec, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

// StartContainer starts a created container.
func (c *Client) StartContainer(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/containers/"+id+"/start", nil, nil)
}

// RemoveContainer kills and removes a container with its anonymous volumes.
// Removing a container that is already gone is not an error.
func (c *Client) RemoveContainer(ctx context.Context, id string) error {
	err := c.do(ctx, http.MethodDelete, "/containers/"+id+"?force=true&v=true", nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// ContainerSummary is one entry of a container listing.
type ContainerSummary struct {
	ID      string            `json:"Id"`
	Names   []string          `json:"Names"`
	Image   string            `json:"Image"`
	State   string            `json:"State"`
	Labels  map[string]string `json:"Labels"`
	Created int64             `json:"Created"`
}

// ListContainers returns every container, running or not, carrying the label.
// The label is "key" or "key=value".
func (c *Client) ListContainers(ctx context.Context, label string) ([]ContainerSummary, error) {
	filters, _ := json.Marshal(map[string][]string{"label": {label}})
	var out []ContainerSummary
	err := c.do(ctx, http.MethodGet, "/containers/json?all=true&filters="+url.QueryEscape(string(filters)), nil, &out)
	return out, err
}

// ImageExists reports whether the image is present on the daemon.
func (c *Client) ImageExists(ctx context.Context, ref string) (bool, error) {
	err := c.do(ctx, http.MethodGet, "/images/"+url.PathEscape(ref)+"/json", nil, nil)
	if IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

// ExecOptions describes a command to run inside a running container.
type ExecOptions struct {
	Cmd   []string
	Dir   string
	Env   []string
	User  string
	Stdin io.Reader
	// Stdout and Stderr receive output as it is produced. Either may be nil.
	Stdout io.Writer
	Stderr io.Writer
}

// Exec runs a command in the container, streams its output, and returns its
// exit code once it has finished.
//
// Cancelling ctx stops waiting and closes the stream. It does not kill the
// process: the daemon leaves an exec running when its client goes away.
// Callers that need a hard stop bound the command inside the container (the
// sandbox layer wraps commands in timeout(1)) or remove the container.
func (c *Client) Exec(ctx context.Context, containerID string, opts ExecOptions) (int, error) {
	create := map[string]any{
		"AttachStdout": true,
		"AttachStderr": true,
		"AttachStdin":  opts.Stdin != nil,
		"Tty":          false,
		"Cmd":          opts.Cmd,
	}
	if opts.Dir != "" {
		create["WorkingDir"] = opts.Dir
	}
	if len(opts.Env) > 0 {
		create["Env"] = opts.Env
	}
	if opts.User != "" {
		create["User"] = opts.User
	}
	var created struct {
		ID string `json:"Id"`
	}
	if err := c.do(ctx, http.MethodPost, "/containers/"+containerID+"/exec", create, &created); err != nil {
		return -1, err
	}

	conn, reader, err := c.hijack(ctx, "/exec/"+created.ID+"/start", []byte(`{"Detach":false,"Tty":false}`))
	if err != nil {
		return -1, err
	}
	defer conn.Close()

	// Closing the connection is the only way to unblock a read on it.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-done:
		}
	}()

	if opts.Stdin != nil {
		go func() {
			_, _ = io.Copy(conn, opts.Stdin)
			// Half-close: the process sees end of input while its output keeps flowing back.
			if half, ok := conn.(interface{ CloseWrite() error }); ok {
				_ = half.CloseWrite()
			}
		}()
	}

	if err := Demux(reader, opts.Stdout, opts.Stderr); err != nil {
		if ctx.Err() != nil {
			return -1, ctx.Err()
		}
		return -1, fmt.Errorf("engine: reading exec stream: %w", err)
	}

	var inspect struct {
		ExitCode int  `json:"ExitCode"`
		Running  bool `json:"Running"`
	}
	// The stream can end a moment before the daemon records the exit.
	for attempt := 0; ; attempt++ {
		if err := c.do(ctx, http.MethodGet, "/exec/"+created.ID+"/json", nil, &inspect); err != nil {
			return -1, err
		}
		if !inspect.Running || attempt >= 50 {
			break
		}
		select {
		case <-ctx.Done():
			return -1, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	return inspect.ExitCode, nil
}

// hijack sends a request and hands back the raw connection, for endpoints
// that turn the HTTP connection into a byte stream. net/http cannot do this
// for a client, so the request is written by hand.
func (c *Client) hijack(ctx context.Context, path string, body []byte) (net.Conn, *bufio.Reader, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, nil, err
	}
	request := fmt.Sprintf(
		"POST %s HTTP/1.1\r\nHost: docker\r\nContent-Type: application/json\r\nConnection: Upgrade\r\nUpgrade: tcp\r\nContent-Length: %d\r\n\r\n",
		path, len(body),
	)
	if _, err := conn.Write(append([]byte(request), body...)); err != nil {
		conn.Close()
		return nil, nil, err
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols && resp.StatusCode != http.StatusOK {
		defer conn.Close()
		return nil, nil, apiError(resp)
	}
	// Do not touch resp.Body: after the headers the connection is the stream, read through reader.
	return conn, reader, nil
}

// Stream ids in the multiplexed format.
const (
	streamStdout = 1
	streamStderr = 2
)

// maxFrame bounds a single frame so a corrupt header cannot ask for gigabytes.
const maxFrame = 1 << 24

// Demux splits the daemon's multiplexed stream into stdout and stderr.
//
// Without a TTY, the daemon interleaves both streams on one connection as
// frames: an 8-byte header (byte 0 is the stream id, bytes 4..7 the payload
// length, big endian) followed by the payload. A nil writer discards its stream.
func Demux(r io.Reader, stdout, stderr io.Writer) error {
	var header [8]byte
	for {
		if _, err := io.ReadFull(r, header[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		size := binary.BigEndian.Uint32(header[4:])
		if size > maxFrame {
			return fmt.Errorf("engine: frame of %d bytes exceeds the limit", size)
		}
		var target io.Writer
		switch header[0] {
		case streamStdout:
			target = stdout
		case streamStderr:
			target = stderr
		}
		if target == nil {
			target = io.Discard
		}
		if _, err := io.CopyN(target, r, int64(size)); err != nil {
			return err
		}
	}
}
