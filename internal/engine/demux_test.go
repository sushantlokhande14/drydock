package engine

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

func frame(stream byte, payload string) []byte {
	header := make([]byte, 8)
	header[0] = stream
	binary.BigEndian.PutUint32(header[4:], uint32(len(payload)))
	return append(header, payload...)
}

func TestDemuxSplitsInterleavedStreams(t *testing.T) {
	var wire bytes.Buffer
	wire.Write(frame(streamStdout, "out 1\n"))
	wire.Write(frame(streamStderr, "err 1\n"))
	wire.Write(frame(streamStdout, "out 2\n"))
	wire.Write(frame(streamStdout, ""))
	wire.Write(frame(streamStderr, "err 2\n"))

	var stdout, stderr bytes.Buffer
	if err := Demux(&wire, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "out 1\nout 2\n" || stderr.String() != "err 1\nerr 2\n" {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestDemuxKeepsBinaryPayloadsIntact(t *testing.T) {
	// A payload that itself looks like a frame header must not be re-interpreted.
	payload := string(frame(streamStderr, "nested")) + "\x00\xff\x01"
	var stdout bytes.Buffer
	if err := Demux(bytes.NewReader(frame(streamStdout, payload)), &stdout, nil); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != payload {
		t.Fatalf("payload was altered: %q", stdout.String())
	}
}

func TestDemuxDiscardsAStreamWithNoWriter(t *testing.T) {
	var wire bytes.Buffer
	wire.Write(frame(streamStderr, "ignored"))
	wire.Write(frame(streamStdout, "kept"))
	var stdout bytes.Buffer
	if err := Demux(&wire, &stdout, nil); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "kept" {
		t.Fatalf("stdout=%q", stdout.String())
	}
}

func TestDemuxReportsATruncatedStream(t *testing.T) {
	whole := frame(streamStdout, "complete payload")
	for _, cut := range []int{3, 8, 12} {
		if err := Demux(bytes.NewReader(whole[:cut]), &bytes.Buffer{}, nil); err == nil {
			t.Errorf("a stream cut after %d bytes was accepted", cut)
		}
	}
	if err := Demux(bytes.NewReader(nil), nil, nil); err != nil {
		t.Errorf("an empty stream is a clean end, got %v", err)
	}
}

func TestDemuxRejectsAnImplausibleFrameSize(t *testing.T) {
	header := make([]byte, 8)
	header[0] = streamStdout
	binary.BigEndian.PutUint32(header[4:], 1<<30)
	err := Demux(bytes.NewReader(header), &bytes.Buffer{}, nil)
	if err == nil || !strings.Contains(err.Error(), "exceeds the limit") {
		t.Fatalf("expected a size error, got %v", err)
	}
}

func TestNewRejectsUnsupportedHosts(t *testing.T) {
	for _, host := range []string{"npipe:////./pipe/docker_engine", "ssh://host", "localhost:2375"} {
		if _, err := New(host); err == nil {
			t.Errorf("host %q should be rejected", host)
		}
	}
	for _, host := range []string{"unix:///var/run/docker.sock", "tcp://127.0.0.1:2375"} {
		if _, err := New(host); err != nil {
			t.Errorf("host %q should be accepted: %v", host, err)
		}
	}
}
