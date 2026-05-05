package share

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/omnibenchmark/obmon/internal/cache"
	"github.com/schollz/croc/v10/src/croc"
	"github.com/schollz/croc/v10/src/models"
	"github.com/schollz/croc/v10/src/utils"
)

// Config holds optional relay overrides.
// Zero value uses croc's default public relay.
type Config struct {
	// RelayAddress overrides the default relay address (host:port).
	RelayAddress string
	// RelayPorts specifies the relay transfer ports (port numbers only).
	RelayPorts []string
	// RelayPassword is the relay password. Default: "pass123".
	RelayPassword string
}

var defaultRelayPorts = []string{"9009", "9010", "9011", "9012", "9013"}

func crocOpts(cfg Config, isSender bool, code string) croc.Options {
	relayAddr := cfg.RelayAddress
	if relayAddr == "" {
		relayAddr = models.DEFAULT_RELAY
	}
	relayPass := cfg.RelayPassword
	if relayPass == "" {
		relayPass = models.DEFAULT_PASSPHRASE
	}
	relayPorts := defaultRelayPorts
	if len(cfg.RelayPorts) > 0 {
		relayPorts = cfg.RelayPorts
	}
	return croc.Options{
		IsSender:      isSender,
		SharedSecret:  code,
		NoPrompt:      true,
		Overwrite:     true,
		Curve:         "p256",
		RelayAddress:  relayAddr,
		RelayPassword: relayPass,
		RelayPorts:    relayPorts,
		// Force relay when a custom relay is configured; otherwise allow
		// croc's default local-network discovery to handle same-machine transfers.
		DisableLocal: cfg.RelayAddress != "",
		// NOTE: Quiet:true is intentionally omitted. croc.New(Quiet:true)
		// redirects the global os.Stderr to /dev/null, which is a concurrent
		// write to a shared variable that races with other goroutines reading
		// os.Stderr (e.g. concurrent Send+Receive). Callers that want quiet
		// operation should redirect os.Stderr before invoking Send/Receive.
	}
}

// Send sends run's telemetry file to a croc receiver.
// onCode is called with the transfer code once the sender is ready.
// Send blocks until the transfer completes or ctx is cancelled.
func Send(ctx context.Context, run *cache.Run, cfg Config, onCode func(string)) error {
	code := utils.GetRandomName()
	log.Printf("share: generated code %s", code)

	// Create a temp dir with a symlink named <runID>.jsonl so the receiver
	// sees the run ID in the filename (used for duplicate detection).
	tmpDir, err := os.MkdirTemp("", "obmon-share-*")
	if err != nil {
		return fmt.Errorf("mktemp: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	sendPath := filepath.Join(tmpDir, run.ID+".jsonl")
	if err := copyFile(run.TelemetryPath(), sendPath); err != nil {
		return fmt.Errorf("copy telemetry: %w", err)
	}

	filesInfo, emptyFolders, totalFolders, err := croc.GetFilesInfo(
		[]string{sendPath}, false, false, nil)
	if err != nil {
		return fmt.Errorf("file info: %w", err)
	}

	c, err := croc.New(crocOpts(cfg, true, code))
	if err != nil {
		return fmt.Errorf("croc: %w", err)
	}

	// Start the sender before notifying the caller so the relay room is
	// established before the receiver tries to join.
	errCh := make(chan error, 1)
	go func() {
		errCh <- c.Send(filesInfo, emptyFolders, totalFolders)
	}()

	if onCode != nil {
		onCode(code)
	}

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// DuplicateRunError is returned by Receive when the sender's run ID is
// already present in the local cache.
type DuplicateRunError struct {
	Run *cache.Run
}

func (e *DuplicateRunError) Error() string {
	return fmt.Sprintf("run %s already in cache", e.Run.ID)
}

// Receive downloads a telemetry run via croc code and stores it in the cache.
// Returns the new Run on success. Returns *DuplicateRunError if the run is
// already cached.
func Receive(ctx context.Context, code string, cfg Config) (*cache.Run, error) {
	// croc writes files to CWD; use a temp dir so we control the location.
	tmpDir, err := os.MkdirTemp("", "obmon-recv-*")
	if err != nil {
		return nil, fmt.Errorf("mktemp: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	origDir, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("getwd: %w", err)
	}
	if err := os.Chdir(tmpDir); err != nil {
		return nil, fmt.Errorf("chdir: %w", err)
	}
	defer os.Chdir(origDir) //nolint:errcheck

	// Retry on "room not ready": the sender may not have connected to the relay
	// yet (transient race at startup, or fast user paste).
	// Each attempt has a timeout so a hung relay connection doesn't block forever.
	const maxAttempts = 5
	const retryDelay = 300 * time.Millisecond
	const attemptTimeout = 2 * time.Minute
	var receiveErr error
	var c *croc.Client
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			log.Printf("share: waiting for sender... (%d/%d)", attempt+1, maxAttempts)
			select {
			case <-time.After(retryDelay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		var newErr error
		c, newErr = croc.New(crocOpts(cfg, false, code))
		if newErr != nil {
			return nil, fmt.Errorf("croc: %w", newErr)
		}
		errCh := make(chan error, 1)
		go func() { errCh <- c.Receive() }()
		select {
		case receiveErr = <-errCh:
		case <-time.After(attemptTimeout):
			receiveErr = fmt.Errorf("room (secure channel) not ready, maybe peer disconnected")
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if receiveErr == nil {
			break
		}
		if strings.Contains(receiveErr.Error(), "room") || strings.Contains(receiveErr.Error(), "unexpected end of JSON") {
			continue
		}
		return nil, fmt.Errorf("receive: %w", receiveErr)
	}
	if receiveErr != nil {
		return nil, fmt.Errorf("receive: %w", receiveErr)
	}

	if len(c.FilesToTransfer) == 0 {
		return nil, fmt.Errorf("no files received")
	}
	filename := c.FilesToTransfer[0].Name

	// Duplicate check: reject if the sender's run is already cached locally.
	if srcID := strings.TrimSuffix(filename, ".jsonl"); srcID != filename {
		if existing, err := cache.Get(srcID); err == nil {
			return nil, &DuplicateRunError{Run: existing}
		}
	}

	rcvdPath := filepath.Join(tmpDir, filename)
	f, err := os.Open(rcvdPath)
	if err != nil {
		return nil, fmt.Errorf("open received file: %w", err)
	}
	defer f.Close()

	run, err := cache.New("shared", code)
	if err != nil {
		return nil, fmt.Errorf("new run: %w", err)
	}

	w, err := run.Writer()
	if err != nil {
		run.Remove() //nolint:errcheck
		return nil, fmt.Errorf("open writer: %w", err)
	}

	lcw := &lineCountWriter{w: w, lines: &run.Lines}
	if _, err := io.Copy(lcw, f); err != nil {
		w.Close()
		run.Remove() //nolint:errcheck
		return nil, fmt.Errorf("copy: %w", err)
	}
	w.Close()

	if err := run.Finish(); err != nil {
		return run, fmt.Errorf("finish: %w", err)
	}
	return run, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

type lineCountWriter struct {
	w     io.Writer
	lines *int64
}

func (l *lineCountWriter) Write(p []byte) (int, error) {
	n, err := l.w.Write(p)
	for i := 0; i < n; i++ {
		if p[i] == '\n' {
			*l.lines++
		}
	}
	return n, err
}
