package cache_test

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/omnibenchmark/obmon/internal/cache"
)

// setCacheDir overrides the OS cache dir for the duration of the test.
func setCacheDir(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", tmp) // Linux
	t.Setenv("HOME", tmp)           // macOS fallback via ~/Library/Caches
	return tmp
}

func TestNew(t *testing.T) {
	setCacheDir(t)

	run, err := cache.New("myhost", "/data/telemetry.jsonl")
	if err != nil {
		t.Fatal(err)
	}

	if run.ID == "" {
		t.Error("expected non-empty ID")
	}
	if run.Host != "myhost" {
		t.Errorf("got Host=%q, want %q", run.Host, "myhost")
	}
	if run.FinishedAt != nil {
		t.Error("FinishedAt should be nil for new run")
	}
	if run.Lines != 0 {
		t.Errorf("Lines should be 0, got %d", run.Lines)
	}

	// TelemetryPath should return a sensible path.
	if run.TelemetryPath() == "" {
		t.Error("TelemetryPath should not be empty")
	}

	// List should return the new run.
	runs, err := cache.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].ID != run.ID {
		t.Errorf("List: got %d runs, expected 1 with ID %s", len(runs), run.ID)
	}
}

func TestResume(t *testing.T) {
	setCacheDir(t)

	run, err := cache.New("myhost", "/data/telemetry.jsonl")
	if err != nil {
		t.Fatal(err)
	}

	// Write 3 lines to the cache file.
	w, err := run.Writer()
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		fmt.Fprintf(w, `{"n":%d}`+"\n", i)
	}
	w.Close()

	// Resume should find the in-progress run with correct line count.
	resumed, err := cache.Resume("myhost", "/data/telemetry.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if resumed.ID != run.ID {
		t.Errorf("got ID %s, want %s", resumed.ID, run.ID)
	}
	if resumed.Lines != 3 {
		t.Errorf("got Lines=%d, want 3", resumed.Lines)
	}
}

func TestResume_NewestFirst(t *testing.T) {
	setCacheDir(t)

	run1, err := cache.New("myhost", "/data/telemetry.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	// Small sleep to ensure different timestamps.
	time.Sleep(2 * time.Millisecond)

	run2, err := cache.New("myhost", "/data/telemetry.jsonl")
	if err != nil {
		t.Fatal(err)
	}

	resumed, err := cache.Resume("myhost", "/data/telemetry.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	// Should return the newest run.
	if resumed.ID != run2.ID {
		t.Errorf("expected newest run %s, got %s", run2.ID, resumed.ID)
	}
	_ = run1
}

func TestFinish(t *testing.T) {
	setCacheDir(t)

	run, err := cache.New("myhost", "/data/telemetry.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Finish(); err != nil {
		t.Fatal(err)
	}
	if run.FinishedAt == nil {
		t.Error("FinishedAt should be set after Finish()")
	}

	// Resume should not find a finished run.
	_, err = cache.Resume("myhost", "/data/telemetry.jsonl")
	if err == nil {
		t.Error("expected error resuming finished run")
	}
}

func TestGet(t *testing.T) {
	setCacheDir(t)

	run, err := cache.New("myhost", "/data/telemetry.jsonl")
	if err != nil {
		t.Fatal(err)
	}

	// Exact ID.
	got, err := cache.Get(run.ID)
	if err != nil {
		t.Fatalf("Get by full ID: %v", err)
	}
	if got.ID != run.ID {
		t.Errorf("got %s, want %s", got.ID, run.ID)
	}

	// Prefix (first 8 chars).
	got2, err := cache.Get(run.ID[:8])
	if err != nil {
		t.Fatalf("Get by prefix: %v", err)
	}
	if got2.ID != run.ID {
		t.Errorf("got %s, want %s", got2.ID, run.ID)
	}
}

func TestWriter_Append(t *testing.T) {
	setCacheDir(t)

	run, err := cache.New("myhost", "/data/telemetry.jsonl")
	if err != nil {
		t.Fatal(err)
	}

	// Write, close, write again — should append.
	w1, _ := run.Writer()
	fmt.Fprintln(w1, `{"n":1}`)
	w1.Close()

	w2, _ := run.Writer()
	fmt.Fprintln(w2, `{"n":2}`)
	w2.Close()

	data, err := os.ReadFile(run.TelemetryPath())
	if err != nil {
		t.Fatal(err)
	}
	want := "{\"n\":1}\n{\"n\":2}\n"
	if string(data) != want {
		t.Errorf("got %q, want %q", string(data), want)
	}
}
