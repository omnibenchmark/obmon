package cache

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/google/uuid"
)

func cacheRoot() (string, error) {
	base, err := os.UserCacheDir() // ~/.cache on Linux, ~/Library/Caches on macOS
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "obmon", "runs"), nil
}

// Run represents an ongoing or completed streaming session.
type Run struct {
	ID         string     `json:"id"`
	Host       string     `json:"host"`
	RemoteFile string     `json:"remote_file"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Lines      int64      `json:"lines"`

	dir string // not serialized
}

// TelemetryPath returns the path to the cached telemetry.jsonl file.
func (r *Run) TelemetryPath() string {
	return filepath.Join(r.dir, "telemetry.jsonl")
}

// Writer returns an *os.File opened O_APPEND|O_CREATE for telemetry.jsonl.
func (r *Run) Writer() (*os.File, error) {
	return os.OpenFile(r.TelemetryPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
}

// Remove deletes the run directory entirely. Used to clean up on failed receives.
func (r *Run) Remove() error {
	return os.RemoveAll(r.dir)
}

// Finish writes finished_at to meta.json.
func (r *Run) Finish() error {
	now := time.Now()
	r.FinishedAt = &now
	return r.saveMeta()
}

func (r *Run) saveMeta() error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(r.dir, "meta.json"), data, 0o644)
}

// New creates a fresh run directory, writes meta.json, and returns the Run.
func New(host, remoteFile string) (*Run, error) {
	root, err := cacheRoot()
	if err != nil {
		return nil, fmt.Errorf("cache root: %w", err)
	}

	id := uuid.New().String()
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", dir, err)
	}

	run := &Run{
		ID:         id,
		Host:       host,
		RemoteFile: remoteFile,
		StartedAt:  time.Now(),
		dir:        dir,
	}
	if err := run.saveMeta(); err != nil {
		return nil, fmt.Errorf("save meta: %w", err)
	}
	return run, nil
}

// Resume returns the latest in-progress run for host+remoteFile.
// run.Lines is set to the number of complete lines already in telemetry.jsonl.
func Resume(host, remoteFile string) (*Run, error) {
	runs, err := List()
	if err != nil {
		return nil, err
	}

	for i := range runs {
		r := &runs[i]
		if r.Host == host && r.RemoteFile == remoteFile && r.FinishedAt == nil {
			lines, err := countLines(r.TelemetryPath())
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("count lines: %w", err)
			}
			r.Lines = lines
			return r, nil
		}
	}

	return nil, fmt.Errorf("no in-progress run for %s:%s", host, remoteFile)
}

// Get returns the run with the given ID or unique prefix.
func Get(id string) (*Run, error) {
	root, err := cacheRoot()
	if err != nil {
		return nil, err
	}

	// Try exact match first.
	dir := filepath.Join(root, id)
	data, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err == nil {
		var r Run
		if err := json.Unmarshal(data, &r); err != nil {
			return nil, err
		}
		r.dir = dir
		return &r, nil
	}

	// Try prefix match.
	runs, lerr := List()
	if lerr != nil {
		return nil, lerr
	}
	var matches []*Run
	for i := range runs {
		r := &runs[i]
		if len(r.ID) >= len(id) && r.ID[:len(id)] == id {
			matches = append(matches, r)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("run %q not found", id)
	case 1:
		return matches[0], nil
	default:
		return nil, fmt.Errorf("ambiguous run prefix %q matches %d runs", id, len(matches))
	}
}

// List returns all runs from the cache directory, newest first.
func List() ([]Run, error) {
	root, err := cacheRoot()
	if err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	var runs []Run
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		data, err := os.ReadFile(filepath.Join(dir, "meta.json"))
		if err != nil {
			continue
		}
		var r Run
		if err := json.Unmarshal(data, &r); err != nil {
			continue
		}
		r.dir = dir
		runs = append(runs, r)
	}

	sort.Slice(runs, func(i, j int) bool {
		return runs[i].StartedAt.After(runs[j].StartedAt)
	})

	return runs, nil
}

func countLines(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	var count int64
	buf := make([]byte, 32*1024)
	for {
		n, err := f.Read(buf)
		for i := 0; i < n; i++ {
			if buf[i] == '\n' {
				count++
			}
		}
		if err != nil {
			break
		}
	}
	return count, nil
}
