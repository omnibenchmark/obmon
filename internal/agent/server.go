package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"strings"
	"time"

	"os"
)

// resumeMsg is the handshake the client sends before the agent starts streaming.
type resumeMsg struct {
	ResumeLine int64 `json:"resume_line"`
}

// Serve accepts connections on ln and streams lines from filePath to each client.
// Each client must first send {"resume_line": N}; the agent skips N lines then
// streams from line N+1 onward, following new content as it is written.
func Serve(ctx context.Context, filePath string, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				return err
			}
		}
		go handleConn(ctx, conn, filePath)
	}
}

func handleConn(ctx context.Context, conn net.Conn, filePath string) {
	defer conn.Close()

	// Read the resume handshake from the client.
	var msg resumeMsg
	if err := json.NewDecoder(conn).Decode(&msg); err != nil {
		log.Printf("agent: read resume msg: %v", err)
		return
	}

	if err := Tail(ctx, filePath, msg.ResumeLine, conn); err != nil {
		log.Printf("agent: tail %s: %v", filePath, err)
	}
}

// Tail opens filePath (waiting for it to appear if missing), skips the first
// resumeLine complete lines, then streams remaining and newly-appended lines
// to w. It follows file rotation (replacement by inode) and returns when ctx
// is canceled or a non-recoverable I/O error occurs.
func Tail(ctx context.Context, filePath string, resumeLine int64, w io.Writer) error {
	// Wait for the file to appear — it may not exist yet if the benchmark
	// hasn't started writing.
	var f *os.File
	for {
		var err error
		f, err = os.Open(filePath)
		if err == nil {
			break
		}
		if !os.IsNotExist(err) {
			return err
		}
		log.Printf("agent: waiting for %s...", filePath)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	// Use a closure defer so that reassigning f (on file rotation) still closes
	// the current file on exit.
	defer func() { f.Close() }()

	bw := bufio.NewWriter(w)
	r := bufio.NewReader(f)

	// Skip resumeLine complete lines.
	var skipped int64
	for skipped < resumeLine {
		_, err := r.ReadString('\n')
		if err == io.EOF {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(50 * time.Millisecond):
			}
			continue
		}
		if err != nil {
			return err
		}
		skipped++
	}

	var pending strings.Builder

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		line, err := r.ReadString('\n')
		pending.WriteString(line)

		if err == io.EOF {
			// Check whether the file at filePath has been replaced (new
			// execution). os.SameFile compares device+inode on Unix.
			if newInfo, statErr := os.Stat(filePath); statErr == nil {
				if oldInfo, statErr := f.Stat(); statErr == nil && !os.SameFile(oldInfo, newInfo) {
					log.Printf("agent: new file detected at %s — restarting stream", filePath)
					f.Close()
					newF, openErr := os.Open(filePath)
					if openErr != nil {
						return openErr
					}
					f = newF
					r.Reset(f)
					pending.Reset()
					continue
				}
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if err != nil {
			return err
		}

		full := strings.TrimRight(pending.String(), "\r\n")
		pending.Reset()
		if full == "" {
			continue
		}

		if _, err := bw.WriteString(full + "\n"); err != nil {
			return err
		}
		if err := bw.Flush(); err != nil {
			return err
		}
	}
}
