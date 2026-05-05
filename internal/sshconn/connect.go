package sshconn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	sshconfig "github.com/kevinburke/ssh_config"
	gossh "golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

// Config holds parameters for connecting to a remote agent.
// Empty string fields fall back to ~/.ssh/config values for the given Host alias.
type Config struct {
	// Host is an SSH host alias or host:port address.
	Host string
	// User defaults to the value in ~/.ssh/config, then $USER.
	User string
	// IdentityFile defaults to the IdentityFile in ~/.ssh/config.
	IdentityFile string
	// RemoteFile is the absolute path to telemetry.jsonl on the remote host.
	RemoteFile string
	// AgentPath is the absolute path to obmon-agent on the remote host.
	AgentPath string
}

// resolved holds fully-resolved connection parameters including ProxyJump.
type resolved struct {
	addr       string // host:port to dial (or jump through)
	user       string
	identity   string
	proxyJump  string // empty if no jump host
	remoteFile string
	agentPath  string
}

// resolve fills in empty Config fields from ~/.ssh/config.
func resolve(cfg Config) resolved {
	alias := cfg.Host

	user := cfg.User
	if user == "" {
		if v := sshconfig.Get(alias, "User"); v != "" {
			user = v
		} else {
			user = os.Getenv("USER")
		}
	}

	identity := cfg.IdentityFile
	if identity == "" {
		if v := sshconfig.Get(alias, "IdentityFile"); v != "" {
			identity = expandHome(v)
		}
	}

	hostname := sshconfig.Get(alias, "Hostname")
	if hostname == "" {
		hostname = alias
	}
	port := sshconfig.Get(alias, "Port")
	if port == "" {
		port = "22"
	}
	bare, explicitPort, _ := strings.Cut(alias, ":")
	var addr string
	if explicitPort != "" {
		addr = bare + ":" + explicitPort
	} else {
		addr = hostname + ":" + port
	}

	proxyJump := sshconfig.Get(alias, "ProxyJump")

	return resolved{
		addr:       addr,
		user:       user,
		identity:   identity,
		proxyJump:  proxyJump,
		remoteFile: cfg.RemoteFile,
		agentPath:  cfg.AgentPath,
	}
}

func expandHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

// Conn is an active tunnel to a remote obmon-agent.
type Conn struct {
	tunnel     net.Conn
	sshClient  *gossh.Client
	session    *gossh.Session
	jumpClient *gossh.Client // non-nil when ProxyJump is in use
}

func (c *Conn) Read(p []byte) (int, error)  { return c.tunnel.Read(p) }
func (c *Conn) Write(p []byte) (int, error) { return c.tunnel.Write(p) }

func (c *Conn) Close() error {
	c.tunnel.Close()
	c.session.Close()
	c.sshClient.Close()
	if c.jumpClient != nil {
		c.jumpClient.Close()
	}
	return nil
}

// Connect dials the remote SSH server, execs obmon-agent, sets up a local
// port forward, and returns a Conn whose Read yields raw JSONL lines.
// The caller must write the resume handshake ({"resume_line":N}) before reading.
// Empty fields in cfg are resolved from ~/.ssh/config before connecting.
func Connect(ctx context.Context, cfg Config) (io.ReadWriteCloser, error) {
	r := resolve(cfg)

	if r.identity == "" {
		return nil, fmt.Errorf("no identity file: set --identity or add IdentityFile to ~/.ssh/config for %q", cfg.Host)
	}

	signer, err := loadKey(ctx, r.identity)
	if err != nil {
		return nil, err
	}

	sshClient, jumpClient, err := openSSHClient(ctx, r, signer)
	if err != nil {
		return nil, err
	}
	go func() {
		<-ctx.Done()
		sshClient.Close()
	}()

	log.Printf("SSH connected, checking agent...")
	agentPath, remoteFile, err := ensureAgent(ctx, sshClient, r.agentPath, r.remoteFile)
	if err != nil {
		sshClient.Close()
		if jumpClient != nil {
			jumpClient.Close()
		}
		return nil, fmt.Errorf("ensure agent: %w", err)
	}
	log.Printf("remote file: %s", remoteFile)

	log.Printf("starting agent: %s --file %s", agentPath, remoteFile)
	session, err := sshClient.NewSession()
	if err != nil {
		sshClient.Close()
		if jumpClient != nil {
			jumpClient.Close()
		}
		return nil, fmt.Errorf("new session: %w", err)
	}

	stdout, err := session.StdoutPipe()
	if err != nil {
		session.Close()
		sshClient.Close()
		if jumpClient != nil {
			jumpClient.Close()
		}
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	cmd := agentPath + " --file " + shellQuote(remoteFile)
	if err := session.Start(cmd); err != nil {
		session.Close()
		sshClient.Close()
		if jumpClient != nil {
			jumpClient.Close()
		}
		return nil, fmt.Errorf("start agent: %w", err)
	}

	var info struct {
		Port int `json:"port"`
	}
	if err := json.NewDecoder(stdout).Decode(&info); err != nil {
		session.Close()
		sshClient.Close()
		if jumpClient != nil {
			jumpClient.Close()
		}
		return nil, fmt.Errorf("read agent port: %w", err)
	}
	if info.Port == 0 {
		session.Close()
		sshClient.Close()
		if jumpClient != nil {
			jumpClient.Close()
		}
		return nil, fmt.Errorf("agent reported port 0")
	}

	log.Printf("opening tunnel to agent port %d...", info.Port)
	tunnel, err := sshClient.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", info.Port))
	if err != nil {
		session.Close()
		sshClient.Close()
		if jumpClient != nil {
			jumpClient.Close()
		}
		return nil, fmt.Errorf("tunnel to agent port %d: %w", info.Port, err)
	}

	return &Conn{
		tunnel:     tunnel,
		sshClient:  sshClient,
		session:    session,
		jumpClient: jumpClient,
	}, nil
}

// openSSHClient dials r.addr, going through r.proxyJump if set.
// Returns (sshClient, jumpClient, err); jumpClient is nil when no jump is used.
// If the jump host shares the same identity file as the target, the provided
// signer is reused — no second passphrase prompt.
func openSSHClient(ctx context.Context, r resolved, signer gossh.Signer) (*gossh.Client, *gossh.Client, error) {
	sshCfg := &gossh.ClientConfig{
		User:            r.user,
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(signer)},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(), // TODO: use known_hosts
	}

	var transport net.Conn
	var jumpClient *gossh.Client

	if r.proxyJump != "" {
		jr := resolve(Config{Host: r.proxyJump})

		// Only load a new key if the jump host uses a different identity file.
		jumpSigner := signer
		if jr.identity != "" && jr.identity != r.identity {
			s, err := loadKey(ctx, jr.identity)
			if err != nil {
				return nil, nil, fmt.Errorf("jump host key: %w", err)
			}
			jumpSigner = s
		}

		jumpCfg := &gossh.ClientConfig{
			User:            jr.user,
			Auth:            []gossh.AuthMethod{gossh.PublicKeys(jumpSigner)},
			HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		}

		log.Printf("dialing jump host %s...", jr.addr)
		jumpConn, err := (&net.Dialer{}).DialContext(ctx, "tcp", jr.addr)
		if err != nil {
			return nil, nil, fmt.Errorf("jump host tcp dial: %w", err)
		}
		jumpSSH, jchans, jreqs, err := gossh.NewClientConn(jumpConn, jr.addr, jumpCfg)
		if err != nil {
			jumpConn.Close()
			return nil, nil, fmt.Errorf("jump host ssh handshake: %w", err)
		}
		jumpClient = gossh.NewClient(jumpSSH, jchans, jreqs)

		log.Printf("dialing %s via jump host...", r.addr)
		var err2 error
		transport, err2 = jumpClient.Dial("tcp", r.addr)
		if err2 != nil {
			jumpClient.Close()
			return nil, nil, fmt.Errorf("dial %s via jump: %w", r.addr, err2)
		}
	} else {
		log.Printf("dialing %s...", r.addr)
		var err error
		transport, err = (&net.Dialer{}).DialContext(ctx, "tcp", r.addr)
		if err != nil {
			return nil, nil, fmt.Errorf("tcp dial %s: %w", r.addr, err)
		}
	}

	sshConn, chans, reqs, err := gossh.NewClientConn(transport, r.addr, sshCfg)
	if err != nil {
		transport.Close()
		if jumpClient != nil {
			jumpClient.Close()
		}
		return nil, nil, fmt.Errorf("ssh handshake %s: %w", r.addr, err)
	}
	return gossh.NewClient(sshConn, chans, reqs), jumpClient, nil
}

// loadKey reads and parses an SSH private key, prompting for passphrase if needed.
func loadKey(ctx context.Context, identityFile string) (gossh.Signer, error) {
	keyBytes, err := os.ReadFile(identityFile)
	if err != nil {
		return nil, fmt.Errorf("read identity file: %w", err)
	}
	signer, err := gossh.ParsePrivateKey(keyBytes)
	if err == nil {
		return signer, nil
	}

	var ppErr *gossh.PassphraseMissingError
	if !errors.As(err, &ppErr) {
		return nil, fmt.Errorf("parse private key: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Enter passphrase for %s: ", identityFile)
	fd := int(syscall.Stdin)
	oldState, _ := term.GetState(fd)

	type ppResult struct {
		pp  []byte
		err error
	}
	ppCh := make(chan ppResult, 1)
	go func() {
		pp, err := term.ReadPassword(fd)
		ppCh <- ppResult{pp, err}
	}()

	var passphrase []byte
	select {
	case res := <-ppCh:
		fmt.Fprintln(os.Stderr)
		if res.err != nil {
			return nil, fmt.Errorf("read passphrase: %w", res.err)
		}
		passphrase = res.pp
	case <-ctx.Done():
		if oldState != nil {
			term.Restore(fd, oldState) //nolint:errcheck
		}
		fmt.Fprintln(os.Stderr)
		return nil, fmt.Errorf("cancelled")
	}

	signer, err = gossh.ParsePrivateKeyWithPassphrase(keyBytes, passphrase)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}
	return signer, nil
}

// shellQuote wraps s in single quotes, escaping any existing single quotes.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
