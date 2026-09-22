// Package deployer implements the application deployment logic over SSH.
package deployer

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	internalssh "github.com/Arubacloud/containerday-2026/functions/appenv-deployer/internal/ssh"
)

// Deployer handles Docker installation and container lifecycle on a remote VM.
type Deployer interface {
	// Deploy ensures the desired image is running as containerName on the VM
	// reachable at host:port via SSH as user with privateKeyPEM.
	Deploy(ctx context.Context, opts DeployOptions) error
}

// DeployOptions carries all parameters for a single deploy operation.
type DeployOptions struct {
	Host          string
	Port          int
	User          string
	PrivateKeyPEM []byte
	Image         string
	ContainerName string
	// EnvVars are injected as -e KEY=VALUE flags in docker run.
	// Values are shell-quoted so they may contain spaces and special characters.
	// Env var changes do not trigger container recreation; only image changes do.
	EnvVars map[string]string
}

// SSHDeployer is the real deployer that connects over SSH.
type SSHDeployer struct {
	Dialer internalssh.Dialer
	// CmdTimeout is the per-command context timeout.
	CmdTimeout time.Duration
}

// NewSSHDeployer returns an SSHDeployer with production defaults.
func NewSSHDeployer() *SSHDeployer {
	return &SSHDeployer{
		Dialer:     internalssh.NewRealDialer(),
		CmdTimeout: 10 * time.Minute,
	}
}

// Deploy implements Deployer.
func (d *SSHDeployer) Deploy(ctx context.Context, opts DeployOptions) error {
	client, err := d.Dialer.Dial(ctx, opts.Host, opts.Port, opts.User, opts.PrivateKeyPEM)
	if err != nil {
		return fmt.Errorf("ssh connect: %w", err)
	}
	defer client.Close()

	if err := ensureDocker(ctx, client, d.CmdTimeout); err != nil {
		return fmt.Errorf("ensure docker: %w", err)
	}

	if err := reconcileContainer(ctx, client, opts.Image, opts.ContainerName, opts.EnvVars, d.CmdTimeout); err != nil {
		return fmt.Errorf("reconcile container: %w", err)
	}

	return nil
}

// ensureDocker checks whether Docker is installed and installs it if not.
//
// Installation is non-blocking: the install script is launched with nohup in
// the background and the function returns immediately with a retryable error.
// This prevents Crossplane's function deadline from firing during the 1-2 minute
// install. On the next reconcile the flag file /tmp/docker-installing is checked
// and, once gone, Docker is verified and the daemon is started.
func ensureDocker(ctx context.Context, c internalssh.Client, timeout time.Duration) error {
	checkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// Fast path: Docker already installed.
	out, err := c.Run(checkCtx, "command -v docker")
	if err == nil && strings.TrimSpace(out) != "" {
		return startDocker(ctx, c, timeout)
	}

	// Is a background install already running?
	statusCtx, statusCancel := context.WithTimeout(ctx, 10*time.Second)
	defer statusCancel()
	status, _ := c.Run(statusCtx, "test -f /tmp/docker-installing && echo installing || echo not-started")
	if strings.TrimSpace(status) == "installing" {
		return fmt.Errorf("Docker installation in progress, will retry on next reconcile")
	}

	// Launch install in the background — this SSH call returns immediately.
	// /tmp/docker-installing is created before the script starts and removed
	// when it finishes, so subsequent reconciles can detect completion.
	bgCtx, bgCancel := context.WithTimeout(ctx, 15*time.Second)
	defer bgCancel()
	bgCmd := "touch /tmp/docker-installing && " +
		"nohup bash -c '" +
		"export DEBIAN_FRONTEND=noninteractive && " +
		"curl -fsSL https://get.docker.com -o /tmp/get-docker.sh && " +
		"sh /tmp/get-docker.sh && " +
		"rm -f /tmp/get-docker.sh && " +
		"systemctl enable --now docker && " +
		"rm -f /tmp/docker-installing" +
		"' >/tmp/docker-install.log 2>&1 &"
	_, _ = c.Run(bgCtx, bgCmd)

	return fmt.Errorf("Docker installation started in background, will retry on next reconcile")
}

// startDocker ensures the Docker daemon is running.
func startDocker(ctx context.Context, c internalssh.Client, timeout time.Duration) error {
	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// sudo systemctl enable --now docker is idempotent.
	if _, err := c.Run(cmdCtx, "sudo systemctl enable --now docker 2>/dev/null || true"); err != nil {
		return fmt.Errorf("start docker daemon: %w", err)
	}
	return nil
}

// reconcileContainer converges the Docker container state toward the desired image.
// envVars are injected only when creating the container; existing containers are not
// recreated purely due to env var changes (only image or network mode changes trigger recreation).
func reconcileContainer(ctx context.Context, c internalssh.Client, image, containerName string, envVars map[string]string, timeout time.Duration) error {
	// Check current container state.
	inspectCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	currentImage, running, hostNetwork, exists, err := inspectContainer(inspectCtx, c, containerName)
	if err != nil {
		return fmt.Errorf("inspect container: %w", err)
	}

	if exists && currentImage == image && running && hostNetwork {
		// Already in desired state.
		return nil
	}

	if exists && (currentImage != image || !hostNetwork) {
		// Image changed or network mode changed — remove and recreate.
		rmCtx, cancel2 := context.WithTimeout(ctx, timeout)
		defer cancel2()
		if _, err := c.Run(rmCtx, fmt.Sprintf("sudo docker rm -f %s", containerName)); err != nil {
			return fmt.Errorf("remove old container: %w", err)
		}
		exists = false
	}

	if exists && !running {
		// Container exists with correct image but is stopped — start it.
		startCtx, cancel3 := context.WithTimeout(ctx, timeout)
		defer cancel3()
		if _, err := c.Run(startCtx, fmt.Sprintf("sudo docker start %s", containerName)); err != nil {
			return fmt.Errorf("start stopped container: %w", err)
		}
		return nil
	}

	// Container does not exist — create and start it.
	runCtx, cancel4 := context.WithTimeout(ctx, timeout)
	defer cancel4()

	// --network host exposes all container ports directly on the VM's public IP,
	// making the application reachable without explicit port mapping.
	runCmd := fmt.Sprintf(
		"sudo docker run -d --name %s --restart unless-stopped --network host%s %s",
		containerName, buildEnvFlags(envVars), image,
	)
	if _, err := c.Run(runCtx, runCmd); err != nil {
		return fmt.Errorf("docker run %s: %w", image, err)
	}

	return nil
}

// inspectContainer returns currentImage, running, hostNetwork, exists for a named container.
func inspectContainer(ctx context.Context, c internalssh.Client, containerName string) (currentImage string, running bool, hostNetwork bool, exists bool, err error) {
	cmd := fmt.Sprintf(
		`sudo docker inspect --format '{{.Config.Image}}|{{.State.Running}}|{{.HostConfig.NetworkMode}}' %s 2>/dev/null || echo '__not_found__'`,
		containerName,
	)
	out, err := c.Run(ctx, cmd)
	if err != nil {
		return "", false, false, false, err
	}

	out = strings.TrimSpace(out)
	if out == "__not_found__" || out == "" {
		return "", false, false, false, nil
	}

	parts := strings.SplitN(out, "|", 3)
	if len(parts) != 3 {
		return "", false, false, false, fmt.Errorf("unexpected inspect output: %q", out)
	}

	currentImage = strings.TrimSpace(parts[0])
	running = strings.TrimSpace(parts[1]) == "true"
	hostNetwork = strings.TrimSpace(parts[2]) == "host"
	return currentImage, running, hostNetwork, true, nil
}

// buildEnvFlags builds a string of " -e KEY='value'" flags from envVars.
// Keys are sorted for deterministic output. Values are single-quote escaped.
func buildEnvFlags(envVars map[string]string) string {
	if len(envVars) == 0 {
		return ""
	}
	keys := make([]string, 0, len(envVars))
	for k := range envVars {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for _, k := range keys {
		b.WriteString(" -e ")
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(shellQuote(envVars[k]))
	}
	return b.String()
}

// shellQuote wraps s in single quotes, escaping any embedded single quotes.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
