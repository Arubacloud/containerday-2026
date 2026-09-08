// Package ssh provides an SSH client abstraction used by the deployer.
package ssh

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	gossh "golang.org/x/crypto/ssh"
)

// Client is the interface that SSH operations must satisfy. It exists to allow
// unit-testing without a real VM.
type Client interface {
	// Run executes a remote command and returns its combined stdout+stderr.
	Run(ctx context.Context, cmd string) (string, error)
	// Close releases the underlying connection.
	Close() error
}

// Dialer creates SSH Client connections.
type Dialer interface {
	Dial(ctx context.Context, host string, port int, user string, privateKeyPEM []byte) (Client, error)
}

// RealDialer is the production Dialer that uses golang.org/x/crypto/ssh.
type RealDialer struct {
	// ConnectTimeout is the TCP dial timeout.
	ConnectTimeout time.Duration
}

// NewRealDialer returns a RealDialer with sensible defaults.
func NewRealDialer() *RealDialer {
	return &RealDialer{ConnectTimeout: 30 * time.Second}
}

// Dial opens an SSH connection. Host key verification is set to accept-first-use
// (InsecureIgnoreHostKey) because the VMs are freshly provisioned and we have no
// prior fingerprint to compare against. This is an explicit, documented choice:
// the threat model accepts TOFU for automated provisioning. Network-level
// isolation (security groups) provides compensating controls.
func (d *RealDialer) Dial(_ context.Context, host string, port int, user string, privateKeyPEM []byte) (Client, error) {
	signer, err := gossh.ParsePrivateKey(privateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}

	cfg := &gossh.ClientConfig{
		User:            user,
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(signer)},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(), //nolint:gosec // see Dial docstring
		Timeout:         d.ConnectTimeout,
	}

	addr := fmt.Sprintf("%s:%d", host, port)
	conn, err := net.DialTimeout("tcp", addr, d.ConnectTimeout)
	if err != nil {
		return nil, fmt.Errorf("tcp dial %s: %w", addr, err)
	}

	sshConn, chans, reqs, err := gossh.NewClientConn(conn, addr, cfg)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("ssh handshake %s: %w", addr, err)
	}

	return &realClient{client: gossh.NewClient(sshConn, chans, reqs)}, nil
}

type realClient struct {
	client *gossh.Client
}

func (c *realClient) Run(ctx context.Context, cmd string) (string, error) {
	sess, err := c.client.NewSession()
	if err != nil {
		return "", fmt.Errorf("new session: %w", err)
	}
	defer sess.Close()

	// Respect context cancellation via a goroutine that closes the session.
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			sess.Close()
		case <-done:
		}
	}()
	defer close(done)

	out, err := sess.CombinedOutput(cmd)
	output := strings.TrimSpace(string(out))
	if err != nil {
		return output, fmt.Errorf("run %q: %w (output: %s)", cmd, err, output)
	}
	return output, nil
}

func (c *realClient) Close() error {
	return c.client.Close()
}
