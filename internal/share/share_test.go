package share_test

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/omnibenchmark/obmon/internal/cache"
	"github.com/omnibenchmark/obmon/internal/share"
	"github.com/schollz/croc/v10/src/tcp"
)

func TestMain(m *testing.M) {
	// croc writes verbose progress output to os.Stderr. Redirect it to
	// /dev/null for the duration of the test binary. Testing output
	// (t.Log, t.Fatal, etc.) goes through testing.T, not os.Stderr.
	if devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0); err == nil {
		os.Stderr = devNull
	}
	os.Exit(m.Run())
}

// startTestRelay starts a minimal two-port croc relay on random localhost ports
// and returns a Config pointing at it.
func startTestRelay(t *testing.T) share.Config {
	t.Helper()
	// schollz/logger.SetLevel is not thread-safe. Setting LOGGER causes it to
	// return early (env var takes precedence), preventing concurrent writes to
	// the global logger state from tcp.Run goroutines and croc.New() calls.
	t.Setenv("LOGGER", "info")
	coordPort := freePort(t)
	xferPort := freePort(t)

	// Coordination port: banner tells clients which port to use for data.
	go tcp.Run("info", "127.0.0.1", coordPort, "testpass", xferPort) //nolint:errcheck
	// Transfer port: bridges sender and receiver data streams.
	go tcp.Run("info", "127.0.0.1", xferPort, "testpass") //nolint:errcheck

	// Give the relay goroutines a moment to bind.
	time.Sleep(200 * time.Millisecond)

	return share.Config{
		RelayAddress:  "127.0.0.1:" + coordPort,
		RelayPassword: "testpass",
	}
}

// freePort finds an available TCP port on localhost.
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	l.Close()
	return port
}

// makeSrcRun creates a cache run with the given JSONL content.
func makeSrcRun(t *testing.T, content string) *cache.Run {
	t.Helper()
	run, err := cache.New("testhost", "test/telemetry.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	w, err := run.Writer()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, content); err != nil {
		t.Fatal(err)
	}
	w.Close()
	return run
}

func TestSendReceive(t *testing.T) {
	cfg := startTestRelay(t)

	senderCache := t.TempDir()
	receiverCache := t.TempDir()

	t.Setenv("XDG_CACHE_HOME", senderCache)
	const content = `{"type":"log","msg":"hello"}` + "\n" + `{"type":"log","msg":"world"}` + "\n"
	srcRun := makeSrcRun(t, content)

	t.Setenv("XDG_CACHE_HOME", receiverCache)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	codeCh := make(chan string, 1)
	sendErrCh := make(chan error, 1)
	go func() {
		sendErrCh <- share.Send(ctx, srcRun, cfg, func(code string) {
			codeCh <- code
		})
	}()

	var code string
	select {
	case code = <-codeCh:
	case err := <-sendErrCh:
		t.Fatalf("send returned before sending code: %v", err)
	case <-ctx.Done():
		t.Fatal("timeout waiting for share code")
	}

	// Give the sender time to connect to the relay and establish its room
	// before the receiver dials. Without this, the receiver may arrive first,
	// create the room as "first" client, and then fail when the relay sends
	// keep-alive pings while waiting for the sender to join.
	time.Sleep(300 * time.Millisecond)

	dstRun, err := share.Receive(ctx, code, cfg)
	if err != nil {
		t.Fatalf("receive: %v", err)
	}

	if err := <-sendErrCh; err != nil {
		t.Fatalf("send: %v", err)
	}

	got, err := os.ReadFile(dstRun.TelemetryPath())
	if err != nil {
		t.Fatalf("read received file: %v", err)
	}
	if string(got) != content {
		t.Errorf("content mismatch:\ngot:  %q\nwant: %q", got, content)
	}
	if dstRun.Lines != 2 {
		t.Errorf("lines: got %d, want 2", dstRun.Lines)
	}
}

func TestReceiveDuplicate(t *testing.T) {
	cfg := startTestRelay(t)

	dir := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", dir)

	const content = `{"type":"log","msg":"dup"}` + "\n"
	srcRun := makeSrcRun(t, content)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	codeCh := make(chan string, 1)
	sendErrCh := make(chan error, 1)
	go func() {
		sendErrCh <- share.Send(ctx, srcRun, cfg, func(code string) { codeCh <- code })
	}()

	code := <-codeCh
	time.Sleep(300 * time.Millisecond)
	_, err := share.Receive(ctx, code, cfg)
	<-sendErrCh // drain

	var dupErr *share.DuplicateRunError
	if !errors.As(err, &dupErr) {
		t.Fatalf("expected *DuplicateRunError, got: %v", err)
	}
	if dupErr.Run.ID != srcRun.ID {
		t.Errorf("duplicate run ID: got %s, want %s", dupErr.Run.ID, srcRun.ID)
	}
}
