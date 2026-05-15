package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/omnibenchmark/obmon/internal/agent"
	"github.com/omnibenchmark/obmon/internal/aspire"
	"github.com/omnibenchmark/obmon/internal/cache"
	"github.com/omnibenchmark/obmon/internal/otlp"
	"github.com/omnibenchmark/obmon/internal/share"
	"github.com/omnibenchmark/obmon/internal/sshconn"
)

// Set by -ldflags at build time.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

const defaultAgentPath = "~/.obmon/bin/obmon-agent"

func buildVersion() string {
	v, c, d := version, commit, date
	// Fall back to VCS info embedded by the Go toolchain at build time.
	if v == "dev" {
		if info, ok := debug.ReadBuildInfo(); ok {
			if info.Main.Version != "" && info.Main.Version != "(devel)" {
				v = info.Main.Version
			}
			for _, s := range info.Settings {
				switch s.Key {
				case "vcs.revision":
					c = s.Value
					if len(c) > 12 {
						c = c[:12]
					}
				case "vcs.time":
					d = s.Value
				case "vcs.modified":
					if s.Value == "true" {
						c += "-dirty"
					}
				}
			}
		}
	}
	return fmt.Sprintf("obmon %s (commit %s, built %s)", v, c, d)
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: obmon <command> [flags]")
		fmt.Fprintln(os.Stderr, "commands: stream, runs, replay, share, receive, agent, dashboard, version")
		os.Exit(1)
	}

	switch os.Args[1] {
	case "stream":
		runStream(os.Args[2:])
	case "runs":
		runRuns(os.Args[2:])
	case "replay":
		runReplay(os.Args[2:])
	case "share":
		runShare(os.Args[2:])
	case "receive":
		runReceive(os.Args[2:])
	case "agent":
		runAgent(os.Args[2:])
	case "dashboard":
		runDashboard(os.Args[2:])
	case "version":
		fmt.Println(buildVersion())
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", os.Args[1])
		os.Exit(1)
	}
}

func runDashboard(args []string) {
	fs := flag.NewFlagSet("dashboard", flag.ExitOnError)
	otlpAddr := fs.String("otlp", "localhost:4317", "local OTLP gRPC address to wait on")
	dashURL := fs.String("url", "http://localhost:18888", "dashboard UI URL to open in browser")
	fs.Parse(args) //nolint:errcheck

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := aspire.EnsureRunning(ctx, *otlpAddr); err != nil {
		log.Fatalf("dashboard: %v", err)
	}
	log.Printf("dashboard: UI at %s", *dashURL)
	time.Sleep(1 * time.Second)
	if err := aspire.OpenBrowser(*dashURL); err != nil {
		log.Printf("dashboard: could not open browser: %v", err)
	}
}

func runStream(args []string) {
	fs := flag.NewFlagSet("stream", flag.ExitOnError)
	identity := fs.String("identity", "", "path to SSH private key (default: ~/.ssh/config IdentityFile)")
	otlpAddr := fs.String("aspire", "localhost:4317", "local Aspire OTLP gRPC endpoint")
	agentPath := fs.String("agent-path", defaultAgentPath, "path to obmon-agent on remote")
	fs.Parse(args) //nolint:errcheck

	if fs.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: obmon stream [--identity key] [--aspire addr] [user@]host:path | <local-path>")
		os.Exit(1)
	}

	target := fs.Arg(0)
	local, localPath := isLocalPath(target)
	var user, host, remoteFile string
	if local {
		host = "local"
		remoteFile = localPath
	} else {
		var ok bool
		user, host, remoteFile, ok = parseRemoteSpec(target)
		if !ok {
			fmt.Fprintf(os.Stderr, "error: %q: expected [user@]host:path or a local path\n", target)
			os.Exit(1)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := aspire.EnsureRunning(ctx, *otlpAddr); err != nil {
		log.Fatalf("aspire: %v", err)
	}

	// Determine run: resume existing or start fresh.
	run, err := cache.Resume(host, remoteFile)
	var resumeLine int64
	if err != nil {
		run, err = cache.New(host, remoteFile)
		if err != nil {
			log.Fatalf("cache new: %v", err)
		}
		log.Printf("new run %s", run.ID)
	} else {
		resumeLine = run.Lines
		log.Printf("resuming run %s from line %d", run.ID, resumeLine)
	}

	var conn io.ReadCloser
	if local {
		log.Printf("tailing local file %s", remoteFile)
		pr, pw := io.Pipe()
		go func() {
			err := agent.Tail(ctx, remoteFile, resumeLine, pw)
			pw.CloseWithError(err)
		}()
		conn = pr
	} else {
		cfg := sshconn.Config{
			Host:         host,
			User:         user,
			IdentityFile: *identity,
			RemoteFile:   remoteFile,
			AgentPath:    *agentPath,
		}

		log.Printf("connecting to %s...", cfg.Host)
		sshConn, err := sshconn.Connect(ctx, cfg)
		if err != nil {
			log.Fatalf("connect: %v", err)
		}
		// Send resume handshake before any reads.
		handshake := fmt.Sprintf(`{"resume_line":%d}`, resumeLine) + "\n"
		if _, err := fmt.Fprint(sshConn, handshake); err != nil {
			sshConn.Close()
			log.Fatalf("send resume handshake: %v", err)
		}
		conn = sshConn
	}
	defer conn.Close()

	// Open cache file for appending.
	cacheFile, err := run.Writer()
	if err != nil {
		log.Fatalf("cache writer: %v", err)
	}
	defer cacheFile.Close()

	lcw := &lineCountingWriter{w: cacheFile, lines: &run.Lines}
	tee := io.TeeReader(conn, lcw)

	// If resuming, replay cached lines to OTLP first.
	if resumeLine > 0 {
		cached, err := os.Open(run.TelemetryPath())
		if err != nil {
			log.Fatalf("open cache for replay: %v", err)
		}
		log.Printf("replaying %d cached lines to %s", resumeLine, *otlpAddr)
		if err := otlp.Forward(ctx, cached, *otlpAddr); err != nil {
			cached.Close()
			log.Fatalf("replay: %v", err)
		}
		cached.Close()
	}

	log.Printf("streaming %s → %s", remoteFile, *otlpAddr)
	forwardErr := otlp.Forward(ctx, tee, *otlpAddr)
	log.Printf("stream ended after %d lines (cache: %s)", run.Lines, run.TelemetryPath())
	if forwardErr != nil {
		log.Printf("forward error: %v", forwardErr)
	}

	if err := run.Finish(); err != nil {
		log.Printf("finish run: %v", err)
	}
}

func runRuns(args []string) {
	if len(args) == 0 || args[0] == "list" {
		runs, err := cache.List()
		if err != nil {
			log.Fatalf("list runs: %v", err)
		}
		if len(runs) == 0 {
			fmt.Println("no runs")
			return
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "RUN ID\tHOST\tSTARTED\tLINES\tSTATUS")
		for _, r := range runs {
			status := "in-progress"
			if r.FinishedAt != nil {
				status = "complete"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n",
				r.ID,
				r.Host,
				r.StartedAt.Format(time.DateTime),
				r.Lines,
				status,
			)
		}
		tw.Flush()
		return
	}
	fmt.Fprintf(os.Stderr, "unknown runs subcommand: %s\n", args[0])
	fmt.Fprintln(os.Stderr, "usage: obmon runs list")
	os.Exit(1)
}

func runAgent(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: obmon agent <subcommand>")
		fmt.Fprintln(os.Stderr, "subcommands: deploy")
		os.Exit(1)
	}
	switch args[0] {
	case "deploy":
		runAgentDeploy(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown agent subcommand: %s\n", args[0])
		os.Exit(1)
	}
}

func runAgentDeploy(args []string) {
	fs := flag.NewFlagSet("agent deploy", flag.ExitOnError)
	identity := fs.String("identity", "", "path to SSH private key")
	agentPath := fs.String("agent-path", defaultAgentPath, "destination path on remote")
	binary := fs.String("binary", "./obmon-agent", "local obmon-agent binary to upload")
	fs.Parse(args) //nolint:errcheck

	if fs.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: obmon agent deploy [--identity key] [--binary path] [user@]host")
		os.Exit(1)
	}

	user, host := parseHostSpec(fs.Arg(0))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg := sshconn.Config{
		Host:         host,
		User:         user,
		IdentityFile: *identity,
		AgentPath:    *agentPath,
	}
	log.Printf("deploying %s → %s:%s", *binary, host, *agentPath)
	if err := sshconn.Deploy(ctx, cfg, *binary); err != nil {
		log.Fatalf("deploy: %v", err)
	}
	log.Printf("done")
}

func runReplay(args []string) {
	fs := flag.NewFlagSet("replay", flag.ExitOnError)
	otlpAddr := fs.String("aspire", "localhost:4317", "local Aspire OTLP gRPC endpoint")
	fs.Parse(args) //nolint:errcheck

	if fs.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: obmon replay [--aspire addr] <run-id>")
		os.Exit(1)
	}
	runID := fs.Arg(0)

	run, err := cache.Get(runID)
	if err != nil {
		log.Fatalf("get run: %v", err)
	}

	f, err := os.Open(run.TelemetryPath())
	if err != nil {
		log.Fatalf("open cache: %v", err)
	}
	defer f.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Printf("replaying run %s (%d lines) → %s", run.ID, run.Lines, *otlpAddr)
	if err := otlp.Forward(ctx, f, *otlpAddr); err != nil {
		log.Fatalf("replay: %v", err)
	}
}

func runShare(args []string) {
	fs := flag.NewFlagSet("share", flag.ExitOnError)
	fs.Parse(args) //nolint:errcheck

	if fs.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: obmon share <run-id>")
		os.Exit(1)
	}

	run, err := cache.Get(fs.Arg(0))
	if err != nil {
		log.Fatalf("get run: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() { <-ctx.Done(); stop() }()

	if err := share.Send(ctx, run, share.Config{}, func(code string) {
		fmt.Printf("share code: %s\n", code)
		fmt.Printf("receive with: obmon receive %s\n", code)
		log.Printf("waiting for receiver...")
	}); err != nil {
		log.Fatalf("share: %v", err)
	}
	log.Printf("done: run %s transferred", run.ID[:8])
}

func runReceive(args []string) {
	fs := flag.NewFlagSet("receive", flag.ExitOnError)
	fs.Parse(args) //nolint:errcheck

	if fs.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: obmon receive <croc-code>")
		os.Exit(1)
	}
	code := fs.Arg(0)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() { <-ctx.Done(); stop() }()

	log.Printf("connecting to relay...")
	run, err := share.Receive(ctx, code, share.Config{})
	if err != nil {
		var dupErr *share.DuplicateRunError
		if errors.As(err, &dupErr) {
			log.Fatalf("run %s already in cache; use: obmon replay %s", dupErr.Run.ID[:8], dupErr.Run.ID[:8])
		}
		log.Fatalf("receive: %v", err)
	}
	log.Printf("received %d lines → run %s", run.Lines, run.ID[:8])
	log.Printf("replay with: obmon replay %s", run.ID[:8])
}

// isLocalPath reports whether s should be treated as a local filesystem path
// rather than an [user@]host:path scp-style remote spec.
//
// A path is local if it is absolute, starts with ./, ../, or ~, or contains
// no ':' separator (in which case it cannot be a host:path spec).
func isLocalPath(s string) (bool, string) {
	if strings.HasPrefix(s, "/") || strings.HasPrefix(s, "./") || strings.HasPrefix(s, "../") || s == "." || s == ".." {
		return true, s
	}
	if strings.HasPrefix(s, "~") {
		return true, s
	}
	if !strings.Contains(s, ":") {
		return true, s
	}
	return false, ""
}

// parseRemoteSpec parses [user@]host:path (scp syntax).
// Returns (user, host, path, ok). user is empty when not specified.
func parseRemoteSpec(s string) (user, host, filePath string, ok bool) {
	i := strings.Index(s, ":")
	if i <= 0 {
		return "", "", "", false
	}
	hostPart := s[:i]
	filePath = s[i+1:]
	if j := strings.Index(hostPart, "@"); j >= 0 {
		user, host = hostPart[:j], hostPart[j+1:]
	} else {
		host = hostPart
	}
	return user, host, filePath, true
}

// parseHostSpec parses [user@]host.
// Returns (user, host). user is empty when not specified.
func parseHostSpec(s string) (user, host string) {
	if u, h, ok := strings.Cut(s, "@"); ok {
		return u, h
	}
	return "", s
}

// lineCountingWriter wraps an io.Writer and increments *lines on each '\n'.
type lineCountingWriter struct {
	w     io.Writer
	lines *int64
}

func (l *lineCountingWriter) Write(p []byte) (int, error) {
	n, err := l.w.Write(p)
	for i := 0; i < n; i++ {
		if p[i] == '\n' {
			*l.lines++
		}
	}
	return n, err
}
