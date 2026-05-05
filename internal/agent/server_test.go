package agent_test

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/omnibenchmark/obmon/internal/agent"
)

func TestServe_StreamsExistingLines(t *testing.T) {
	f := writeTempJSONL(t, `{"event":"start","ts":1}`, `{"event":"data","ts":2}`)

	ln := mustListen(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go agent.Serve(ctx, f, ln) //nolint:errcheck

	conn := dialAndResume(t, ln.Addr().String(), 0)
	defer conn.Close()

	lines := readLines(t, conn, 2, 3*time.Second)
	want := []string{`{"event":"start","ts":1}`, `{"event":"data","ts":2}`}
	for i, got := range lines {
		if got != want[i] {
			t.Errorf("line %d: got %q want %q", i, got, want[i])
		}
	}
}

func TestServe_StreamsNewLines(t *testing.T) {
	f := writeTempJSONL(t, `{"event":"start","ts":1}`)

	ln := mustListen(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go agent.Serve(ctx, f, ln) //nolint:errcheck

	conn := dialAndResume(t, ln.Addr().String(), 0)
	defer conn.Close()

	// Read the first line.
	_ = readLines(t, conn, 1, 3*time.Second)

	// Append a new line to the file.
	fh, err := os.OpenFile(f, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open for append: %v", err)
	}
	fh.WriteString(`{"event":"end","ts":3}` + "\n") //nolint:errcheck
	fh.Close()

	lines := readLines(t, conn, 1, 3*time.Second)
	if lines[0] != `{"event":"end","ts":3}` {
		t.Errorf("appended line: got %q", lines[0])
	}
}

func TestServe_ResumeFromLine(t *testing.T) {
	f := writeTempJSONL(t,
		`{"n":1}`, `{"n":2}`, `{"n":3}`, `{"n":4}`, `{"n":5}`,
	)

	ln := mustListen(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go agent.Serve(ctx, f, ln) //nolint:errcheck

	// Connect requesting lines after the first 3.
	conn := dialAndResume(t, ln.Addr().String(), 3)
	defer conn.Close()

	lines := readLines(t, conn, 2, 3*time.Second)
	want := []string{`{"n":4}`, `{"n":5}`}
	for i, got := range lines {
		if got != want[i] {
			t.Errorf("line %d: got %q want %q", i, got, want[i])
		}
	}
}

func TestServe_BrokenStream(t *testing.T) {
	f := writeTempJSONL(t,
		`{"n":1}`, `{"n":2}`, `{"n":3}`, `{"n":4}`, `{"n":5}`,
	)

	ln := mustListen(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go agent.Serve(ctx, f, ln) //nolint:errcheck

	// First connection: read 3 lines then disconnect.
	conn1 := dialAndResume(t, ln.Addr().String(), 0)
	got1 := readLines(t, conn1, 3, 3*time.Second)
	conn1.Close()

	// Verify we got the first 3 lines.
	for i, want := range []string{`{"n":1}`, `{"n":2}`, `{"n":3}`} {
		if got1[i] != want {
			t.Errorf("first conn line %d: got %q want %q", i, got1[i], want)
		}
	}

	// Second connection: resume from line 3, expect lines 4 and 5 only.
	conn2 := dialAndResume(t, ln.Addr().String(), 3)
	defer conn2.Close()

	got2 := readLines(t, conn2, 2, 3*time.Second)
	want2 := []string{`{"n":4}`, `{"n":5}`}
	for i, want := range want2 {
		if got2[i] != want {
			t.Errorf("second conn line %d: got %q want %q", i, got2[i], want)
		}
	}
}

// writeTempJSONL creates a temp file with the given lines (each terminated with \n).
func writeTempJSONL(t *testing.T, lines ...string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "telemetry.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	for _, l := range lines {
		f.WriteString(l + "\n") //nolint:errcheck
	}
	f.Close()
	return path
}

func mustListen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return ln
}

// dialAndResume dials the agent and sends the resume handshake.
func dialAndResume(t *testing.T, addr string, resumeLine int64) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	fmt.Fprintf(conn, `{"resume_line":%d}`+"\n", resumeLine)
	return conn
}

// readLines reads exactly n lines from conn within timeout.
func readLines(t *testing.T, conn net.Conn, n int, timeout time.Duration) []string {
	t.Helper()
	conn.SetDeadline(time.Now().Add(timeout)) //nolint:errcheck
	defer conn.SetDeadline(time.Time{})       //nolint:errcheck

	var lines []string
	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
		if len(lines) == n {
			return lines
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read lines: %v", err)
	}
	t.Fatalf("got %d lines, want %d", len(lines), n)
	return nil
}
