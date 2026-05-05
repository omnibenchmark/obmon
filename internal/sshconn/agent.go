package sshconn

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"github.com/pkg/sftp"
	gossh "golang.org/x/crypto/ssh"
)

const (
	githubRepo = "omnibenchmark/obmon"
	githubTag  = "nightly"
	agentAsset = "obmon-agent_linux_amd64"
)

// GitHubAPIBase is the base URL for GitHub API calls. Override in tests.
var GitHubAPIBase = "https://api.github.com"

// Deploy uploads localBinary to agentPath on the remote host via SFTP.
// cfg must have Host set; User and IdentityFile are resolved from ~/.ssh/config.
func Deploy(ctx context.Context, cfg Config, localBinary string) error {
	r := resolve(cfg)
	if r.identity == "" {
		return fmt.Errorf("no identity file: set --identity or add IdentityFile to ~/.ssh/config for %q", cfg.Host)
	}
	signer, err := loadKey(ctx, r.identity)
	if err != nil {
		return err
	}
	sshClient, jumpClient, err := openSSHClient(ctx, r, signer)
	if err != nil {
		return err
	}
	defer sshClient.Close()
	if jumpClient != nil {
		defer jumpClient.Close()
	}

	sc, err := sftp.NewClient(sshClient)
	if err != nil {
		return fmt.Errorf("sftp: %w", err)
	}
	defer sc.Close()

	rpath, err := expandRemoteTilde(sc, r.agentPath)
	if err != nil {
		return err
	}
	return uploadViaFTP(sc, localBinary, rpath)
}

// ensureAgent checks whether obmon-agent exists at agentPath on the remote
// host. If not, it downloads the latest linux/amd64 release from GitHub and
// uploads via SFTP. Returns the resolved agent path and resolved remoteFile path
// (both with ~ expanded using the remote home directory).
func ensureAgent(ctx context.Context, sshClient *gossh.Client, agentPath, remoteFile string) (agentRPath, fileRPath string, err error) {
	sc, err := sftp.NewClient(sshClient)
	if err != nil {
		return "", "", fmt.Errorf("sftp client: %w", err)
	}
	defer sc.Close()

	rpath, err := expandRemoteTilde(sc, agentPath)
	if err != nil {
		return "", "", err
	}

	fileRPath, err = expandRemoteTilde(sc, remoteFile)
	if err != nil {
		return "", "", err
	}

	if _, statErr := sc.Stat(rpath); statErr != nil {
		log.Printf("agent not found at %s — deploying", rpath)
		rel, err := fetchRelease(ctx)
		if err != nil {
			return "", "", fmt.Errorf("fetch release metadata: %w", err)
		}
		tmpPath, err := downloadAgent(ctx, rel)
		if err != nil {
			return "", "", err
		}
		defer os.Remove(tmpPath)
		if err := uploadViaFTP(sc, tmpPath, rpath); err != nil {
			return "", "", err
		}
		return rpath, fileRPath, nil
	}

	// Binary exists — check if outdated. Use a short timeout so a slow
	// GitHub API or unresponsive remote never blocks stream startup.
	updateCtx, updateCancel := context.WithTimeout(ctx, 5*time.Second)
	defer updateCancel()

	rel, err := fetchRelease(updateCtx)
	if err != nil {
		log.Printf("warning: could not fetch release metadata: %v — skipping update check", err)
		return rpath, fileRPath, nil
	}

	expected, err := fetchExpectedChecksum(updateCtx, rel)
	if err != nil {
		log.Printf("warning: could not fetch expected checksum: %v — skipping update check", err)
		return rpath, fileRPath, nil
	}

	actual, err := remoteChecksum(updateCtx, sshClient, rpath)
	if err != nil {
		log.Printf("warning: could not compute remote checksum: %v — skipping update check", err)
		return rpath, fileRPath, nil
	}

	if actual != expected {
		log.Printf("agent outdated (%s… → %s…) — updating", actual[:12], expected[:12])
		tmpPath, err := downloadAgent(ctx, rel)
		if err != nil {
			return "", "", err
		}
		defer os.Remove(tmpPath)
		if err := uploadViaFTP(sc, tmpPath, rpath); err != nil {
			return "", "", err
		}
	} else {
		log.Printf("agent up to date (%s)", rpath)
	}

	return rpath, fileRPath, nil
}

// expandRemoteTilde replaces a leading ~/ with the SFTP working directory
// (which openssh sets to the user's home directory).
func expandRemoteTilde(sc *sftp.Client, p string) (string, error) {
	if !strings.HasPrefix(p, "~/") {
		return p, nil
	}
	home, err := sc.Getwd()
	if err != nil {
		return "", fmt.Errorf("sftp getwd: %w", err)
	}
	return home + p[1:], nil
}

type githubRelease struct {
	Assets []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

func (r *githubRelease) assetURL(name string) (string, bool) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a.BrowserDownloadURL, true
		}
	}
	return "", false
}

func fetchRelease(ctx context.Context) (*githubRelease, error) {
	apiURL := fmt.Sprintf("%s/repos/%s/releases/tags/%s", GitHubAPIBase, githubRepo, githubTag)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github releases API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github releases API %s (repo: %s, tag: %s)", resp.Status, githubRepo, githubTag)
	}

	var rel githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("decode release: %w", err)
	}
	return &rel, nil
}

func downloadAgent(ctx context.Context, rel *githubRelease) (string, error) {
	dlURL, ok := rel.assetURL(agentAsset)
	if !ok {
		return "", fmt.Errorf("asset %q not found in release", agentAsset)
	}
	log.Printf("downloading %s...", dlURL)
	return downloadToTemp(ctx, dlURL)
}

const checksumsAsset = "checksums.txt"

// fetchExpectedChecksum downloads checksums.txt from the release and returns
// the SHA256 hex string for agentAsset.
func fetchExpectedChecksum(ctx context.Context, rel *githubRelease) (string, error) {
	url, ok := rel.assetURL(checksumsAsset)
	if !ok {
		return "", fmt.Errorf("asset %q not found in release", checksumsAsset)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("download checksums: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download checksums: %s", resp.Status)
	}

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 {
			continue
		}
		if path.Base(fields[1]) == agentAsset {
			return fields[0], nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("scan checksums: %w", err)
	}
	return "", fmt.Errorf("checksum for %q not found in checksums.txt", agentAsset)
}

// remoteChecksum runs sha256sum on the remote path and returns the hex digest.
// Respects ctx cancellation so a slow or hung remote command doesn't block forever.
func remoteChecksum(ctx context.Context, sshClient *gossh.Client, remotePath string) (string, error) {
	sess, err := sshClient.NewSession()
	if err != nil {
		return "", fmt.Errorf("ssh session: %w", err)
	}
	defer sess.Close()

	type result struct {
		data []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		out, err := sess.Output("sha256sum " + remotePath)
		ch <- result{out, err}
	}()

	select {
	case <-ctx.Done():
		return "", fmt.Errorf("sha256sum: %w", ctx.Err())
	case r := <-ch:
		if r.err != nil {
			return "", fmt.Errorf("sha256sum: %w", r.err)
		}
		fields := strings.Fields(string(r.data))
		if len(fields) < 1 {
			return "", fmt.Errorf("unexpected sha256sum output: %q", string(r.data))
		}
		return fields[0], nil
	}
}

func downloadToTemp(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: %s", url, resp.Status)
	}

	f, err := os.CreateTemp("", "obmon-agent-*")
	if err != nil {
		return "", err
	}
	defer f.Close()

	if _, err := io.Copy(f, resp.Body); err != nil {
		os.Remove(f.Name())
		return "", fmt.Errorf("write temp: %w", err)
	}

	return f.Name(), nil
}

func uploadViaFTP(sc *sftp.Client, localPath, remotePath string) error {
	if err := sc.MkdirAll(path.Dir(remotePath)); err != nil {
		return fmt.Errorf("mkdir %s: %w", path.Dir(remotePath), err)
	}

	src, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer src.Close()

	dst, err := sc.OpenFile(remotePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return fmt.Errorf("sftp create %s: %w", remotePath, err)
	}
	defer dst.Close()

	if _, err := io.Copy(dst, src); err != nil {
		return fmt.Errorf("sftp copy: %w", err)
	}

	if err := sc.Chmod(remotePath, 0o755); err != nil {
		return fmt.Errorf("chmod +x: %w", err)
	}

	log.Printf("uploaded obmon-agent → %s", remotePath)
	return nil
}
