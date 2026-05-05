package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/omnibenchmark/obmon/internal/agent"
)

// Set by -ldflags at build time.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	file := flag.String("file", "", "path to JSONL telemetry file to stream (required)")
	ver := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *ver {
		fmt.Printf("obmon-agent %s (commit %s, built %s)\n", version, commit, date)
		return
	}

	if *file == "" {
		fmt.Fprintln(os.Stderr, "error: --file is required")
		os.Exit(1)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatalf("listen: %v", err)
	}

	port := ln.Addr().(*net.TCPAddr).Port
	// Write port to stdout for the caller (obmon) to read.
	fmt.Printf(`{"port":%d}`, port)
	os.Stdout.Sync()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Printf("agent: streaming %s on port %d", *file, port)
	if err := agent.Serve(ctx, *file, ln); err != nil {
		log.Fatalf("serve: %v", err)
	}
}
