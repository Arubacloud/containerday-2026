// Package deployer implements the application deployment logic over SSH.
package deployer

import (
	"context"
	"fmt"
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

	if err := reconcileContainer(ctx, client, opts.Image, opts.ContainerName, d.CmdTimeout); err != nil {
		return fmt.Errorf("reconcile container: %w", err)
	}

	return nil
}

// ensureDocker checks whether Docker is installed and installs it if not.
func ensureDocker(ctx context.Context, c internalssh.Client, timeout time.Duration) error {
	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	out, err := c.Run(cmdCtx, "command -v docker")
	if err == nil && strings.TrimSpace(out) != "" {
		// Docker is present; make sure the daemon is running.
		return startDocker(ctx, c, timeout)
	}

	// Install Docker using the official convenience script (idempotent).
	installScript := strings.Join([]string{
		"export DEBIAN_FRONTEND=noninteractive",
		"curl -fsSL https://get.docker.com -o /tmp/get-docker.sh",
		"sh /tmp/get-docker.sh",
		"rm -f /tmp/get-docker.sh",
	}, " && ")

	installCtx, cancel2 := context.WithTimeout(ctx, timeout)
	defer cancel2()

	if _, err := c.Run(installCtx, installScript); err != nil {
		return fmt.Errorf("docker install: %w", err)
	}

	return startDocker(ctx, c, timeout)
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
func reconcileContainer(ctx context.Context, c internalssh.Client, image, containerName string, timeout time.Duration) error {
	// Check current container state.
	inspectCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	currentImage, running, exists, err := inspectContainer(inspectCtx, c, containerName)
	if err != nil {
		return fmt.Errorf("inspect container: %w", err)
	}

	if exists && currentImage == image && running {
		// Already in desired state.
		return nil
	}

	if exists && currentImage != image {
		// Image has changed — remove and recreate.
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
	runCmd := fmt.Sprintf("sudo docker run -d --name %s --restart unless-stopped --network host %s", containerName, image)
	if _, err := c.Run(runCtx, runCmd); err != nil {
		return fmt.Errorf("docker run %s: %w", image, err)
	}

	return nil
}

// inspectContainer returns currentImage, running, exists for a named container.
func inspectContainer(ctx context.Context, c internalssh.Client, containerName string) (currentImage string, running bool, exists bool, err error) {
	// docker inspect outputs "<image>\n<status>" for the named container.
	cmd := fmt.Sprintf(
		`sudo docker inspect --format '{{.Config.Image}}|{{.State.Running}}' %s 2>/dev/null || echo '__not_found__'`,
		containerName,
	)
	out, err := c.Run(ctx, cmd)
	if err != nil {
		return "", false, false, err
	}

	out = strings.TrimSpace(out)
	if out == "__not_found__" || out == "" {
		return "", false, false, nil
	}

	parts := strings.SplitN(out, "|", 2)
	if len(parts) != 2 {
		return "", false, false, fmt.Errorf("unexpected inspect output: %q", out)
	}

	currentImage = strings.TrimSpace(parts[0])
	running = strings.TrimSpace(parts[1]) == "true"
	return currentImage, running, true, nil
}
