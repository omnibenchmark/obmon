package aspire

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	containerName = "aspire"
	image         = "mcr.microsoft.com/dotnet/aspire-dashboard:latest"
	readyTimeout  = 30 * time.Second
)

// EnsureRunning starts the Aspire dashboard container if it is not already
// running. It tries docker first, then podman. It waits until the OTLP gRPC
// port at otlpAddr is accepting connections before returning.
func EnsureRunning(ctx context.Context, otlpAddr string) error {
	runtime, err := findRuntime()
	if err != nil {
		return err
	}

	if isRunning(ctx, runtime) {
		log.Printf("aspire: dashboard already running")
		return nil
	}

	mounts, err := ensureBrandingAssets(ctx, runtime)
	if err != nil {
		log.Printf("aspire: warning: could not prepare branding assets: %v", err)
		mounts = nil
	}

	log.Printf("aspire: starting dashboard container via %s...", runtime)
	if err := startDetached(ctx, runtime, mounts); err != nil {
		if len(mounts) == 0 {
			return fmt.Errorf("start aspire container: %w", err)
		}
		log.Printf("aspire: warning: could not start with branding assets (%v) — retrying without", err)
		if err := startDetached(ctx, runtime, nil); err != nil {
			return fmt.Errorf("start aspire container: %w", err)
		}
	}

	return waitReady(ctx, otlpAddr, readyTimeout)
}

// findRuntime returns the first available container runtime (docker or podman).
func findRuntime() (string, error) {
	for _, candidate := range []string{"docker", "podman"} {
		if path, err := exec.LookPath(candidate); err == nil {
			_ = path
			return candidate, nil
		}
	}
	return "", fmt.Errorf("no container runtime found: install docker or podman")
}

func isRunning(ctx context.Context, runtime string) bool {
	// Docker stores container names with a leading slash ("/aspire"); podman
	// does not. Filter by substring, then exact-match each name to be portable.
	out, err := exec.CommandContext(ctx, runtime, "ps",
		"--filter", "name="+containerName,
		"--format", "{{.Names}}",
	).Output()
	if err != nil {
		return false
	}
	for _, name := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimPrefix(name, "/") == containerName {
			return true
		}
	}
	return false
}

func startDetached(ctx context.Context, runtime string, mounts []string) error {
	args := []string{
		"run",
		"--detach",
		"--rm",
		"--name", containerName,
		"-p", "18888:18888", // dashboard UI
		"-p", "18891:18891", // MCP endpoint
		"-p", "4317:18889",  // OTLP gRPC: host 4317 → container 18889
		"-e", "ASPIRE_DASHBOARD_UNSECURED_ALLOW_ANONYMOUS=true",
		"-e", "ASPIRE_ALLOW_UNSECURED_TRANSPORT=true",
		"-e", "ASPIRE_DASHBOARD_MCP_ENDPOINT_URL=http://0.0.0.0:18891",
		"-e", "Dashboard__Mcp__AuthMode=Unsecured",
		"-e", "Dashboard__Frontend__EndpointUrls=http://localhost:18888",
		"-e", "Dashboard__Frontend__PublicUrl=http://localhost:18888",
	}
	for _, m := range mounts {
		args = append(args, "-v", m)
	}
	args = append(args, image)

	log.Printf("aspire: running: %s %s", runtime, strings.Join(args, " "))
	out, err := exec.CommandContext(ctx, runtime, args...).CombinedOutput()
	if err != nil {
		if len(out) > 0 {
			return fmt.Errorf("%w\n%s", err, strings.TrimSpace(string(out)))
		}
		return err
	}
	log.Printf("aspire: container started (%s)", strings.TrimSpace(string(out)))
	return nil
}

// brandingSnippet is appended to the existing FluentUI module JS file.
// Mounting over a file that already exists in the container avoids the Docker
// "missing destination creates a directory" gotcha.
// The IIFE runs when the module is loaded and installs a MutationObserver that
// replaces the logo text once Blazor renders it.
const brandingSnippet = `
// obmon: replace Aspire logo text
;(function(){
  var BRAND='omnibenchmark';
  function patch(){
    document.querySelectorAll('fluent-anchor.logo').forEach(function(logo){
      var w=document.createTreeWalker(logo,NodeFilter.SHOW_TEXT,null,false),n;
      while((n=w.nextNode())){var t=n.textContent.trim();if(t&&t!==BRAND)n.textContent=n.textContent.replace(t,BRAND);}
    });
    document.querySelectorAll('[href*="aka.ms/dotnet/aspire"]').forEach(function(el){
      el.setAttribute('href','https://github.com/omnibenchmark/obmon');
    });
  }
  if(document.readyState==='loading'){document.addEventListener('DOMContentLoaded',patch);}else{patch();}
  new MutationObserver(patch).observe(document.documentElement,{childList:true,subtree:true});
})();
`

const (
	fluentModulePath = "/app/wwwroot/_content/Microsoft.FluentUI.AspNetCore.Components/Microsoft.FluentUI.AspNetCore.Components.lib.module.js"
	cssPath          = "/app/wwwroot/Aspire.Dashboard.styles.css"
)

// cssBrandingAppend is appended to the dashboard CSS.
// font-size:0 on the shadow host propagates to its slotted light-DOM content,
// hiding the "Aspire" text node. ::after injects our brand text instead.
// :not([title]) targets only the text logo; the icon-only logo has title="Aspire".
const cssBrandingAppend = `
/* obmon branding */
fluent-anchor.logo:not([title]) { font-size: 0 !important; }
fluent-anchor.logo:not([title])::after {
  content: 'omnibenchmark';
  font-size: 0.875rem;
  color: var(--neutral-foreground-rest, inherit);
  font-weight: 600;
}
`

// ensureBrandingAssets prepares two patched static files and returns the
// volume mount specs for startDetached. Both files already exist in the image
// so Docker mounts them as files (not directories).
// To force a refresh, delete ~/.cache/obmon/aspire-*.
func ensureBrandingAssets(ctx context.Context, runtime string) ([]string, error) {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return nil, err
	}
	base := filepath.Join(cacheDir, "obmon")
	if err := os.MkdirAll(base, 0o755); err != nil {
		return nil, err
	}

	// Spin up one temp container for all copies.
	idBytes, err := exec.CommandContext(ctx, runtime, "create", image).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(idBytes))
		if msg != "" {
			return nil, fmt.Errorf("create temp container: %w\n%s", err, msg)
		}
		return nil, fmt.Errorf("create temp container: %w", err)
	}
	containerID := strings.TrimSpace(string(idBytes))
	defer exec.Command(runtime, "rm", containerID).Run() //nolint:errcheck

	var mounts []string

	// 1. CSS patch — primary branding mechanism (no JS required).
	cssLocal := filepath.Join(base, "aspire-styles.css")
	if _, err := os.Stat(cssLocal); err != nil {
		log.Printf("aspire: patching CSS for branding...")
		if err := copyAndAppend(ctx, runtime, containerID, cssPath, cssLocal, cssBrandingAppend); err != nil {
			log.Printf("aspire: CSS patch failed: %v", err)
		}
	}
	if _, err := os.Stat(cssLocal); err == nil {
		mounts = append(mounts, cssLocal+":"+cssPath+":ro")
	}

	// 2. JS patch — also patches the FluentUI module for when Blazor runs.
	jsLocal := filepath.Join(base, "aspire-fluent-module.js")
	if _, err := os.Stat(jsLocal); err != nil {
		log.Printf("aspire: patching FluentUI module for branding...")
		if err := copyAndAppend(ctx, runtime, containerID, fluentModulePath, jsLocal, brandingSnippet); err != nil {
			log.Printf("aspire: JS patch failed: %v", err)
		}
	}
	if _, err := os.Stat(jsLocal); err == nil {
		mounts = append(mounts, jsLocal+":"+fluentModulePath+":ro")
	}

	return mounts, nil
}

func copyAndAppend(ctx context.Context, runtime, containerID, srcPath, dest, appendContent string) error {
	if out, err := exec.CommandContext(ctx, runtime, "cp",
		containerID+":"+srcPath, dest,
	).CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(out))
		if msg != "" {
			return fmt.Errorf("copy %s: %w\n%s", srcPath, err, msg)
		}
		return fmt.Errorf("copy %s: %w", srcPath, err)
	}
	f, err := os.OpenFile(dest, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(appendContent)
	return err
}

// OpenBrowser opens url in the system default browser.
func OpenBrowser(url string) error {
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		cmd, args = "open", []string{url}
	default: // linux
		cmd, args = "xdg-open", []string{url}
	}
	return exec.Command(cmd, args...).Start()
}

func waitReady(ctx context.Context, addr string, timeout time.Duration) error {
	log.Printf("aspire: waiting for OTLP endpoint at %s...", addr)
	deadline := time.Now().Add(timeout)
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("aspire not ready at %s after %s", addr, timeout)
		}
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			conn.Close()
			log.Printf("aspire: ready")
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}
