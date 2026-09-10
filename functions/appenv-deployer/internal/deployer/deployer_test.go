package deployer

import (
	"context"
	"errors"
	"strings"
	"testing"

	internalssh "github.com/Arubacloud/containerday-2026/functions/appenv-deployer/internal/ssh"
)

// --- Mocks ---

type mockDialer struct {
	client internalssh.Client
	err    error
}

func (m *mockDialer) Dial(_ context.Context, _ string, _ int, _ string, _ []byte) (internalssh.Client, error) {
	return m.client, m.err
}

type mockClient struct {
	responses map[string]cmdResult
	calls     []string
}

type cmdResult struct {
	out string
	err error
}

func (m *mockClient) Run(_ context.Context, cmd string) (string, error) {
	m.calls = append(m.calls, cmd)
	// Match by substring of cmd since real commands may have flags.
	for k, v := range m.responses {
		if strings.Contains(cmd, k) {
			return v.out, v.err
		}
	}
	return "", nil
}

func (m *mockClient) Close() error { return nil }

func newDeployer(client internalssh.Client, dialErr error) *SSHDeployer {
	return &SSHDeployer{
		Dialer: &mockDialer{client: client, err: dialErr},
	}
}

// --- Tests ---

// Test 9: SSH failure
func TestDeploy_SSHDialFailure(t *testing.T) {
	d := newDeployer(nil, errors.New("connection refused"))
	err := d.Deploy(context.Background(), DeployOptions{
		Host:  "1.2.3.4",
		Port:  22,
		User:  "ubuntu",
		Image: "nginx:latest",
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "ssh connect") {
		t.Errorf("unexpected error: %v", err)
	}
}

// Test 4: Docker already installed
func TestDeploy_DockerAlreadyInstalled(t *testing.T) {
	client := &mockClient{
		responses: map[string]cmdResult{
			"command -v docker": {out: "/usr/bin/docker"},
			"systemctl":         {out: ""},
			"__not_found__":     {out: "__not_found__"},
		},
	}
	// Container does not exist yet.
	client.responses["docker inspect"] = cmdResult{out: "__not_found__"}
	client.responses["docker run"] = cmdResult{out: "containerid"}

	d := newDeployer(client, nil)
	if err := d.Deploy(context.Background(), DeployOptions{
		Image:         "nginx:latest",
		ContainerName: "application",
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify install script was NOT called.
	for _, c := range client.calls {
		if strings.Contains(c, "get-docker.sh") {
			t.Error("Docker install script should not have been called when Docker is already installed")
		}
	}
}

// Test 5: Docker missing — installation is triggered
func TestDeploy_DockerMissing(t *testing.T) {
	installCalled := false
	client := &mockClient{
		responses: map[string]cmdResult{
			"command -v docker": {out: "", err: errors.New("not found")},
			"get-docker.sh":     {out: ""},
			"systemctl":         {out: ""},
			"docker inspect":    {out: "__not_found__"},
			"docker run":        {out: "containerid"},
		},
	}

	origRun := client.Run
	_ = origRun

	// Override Run to track install call.
	trackClient := &trackingClient{
		inner: client,
		onRun: func(cmd string) {
			if strings.Contains(cmd, "get-docker.sh") {
				installCalled = true
			}
		},
	}

	d := newDeployer(trackClient, nil)
	if err := d.Deploy(context.Background(), DeployOptions{
		Image:         "nginx:latest",
		ContainerName: "application",
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !installCalled {
		t.Error("expected Docker install to be called")
	}
}

// Test 6: Container does not exist — create it
func TestReconcileContainer_Create(t *testing.T) {
	client := &mockClient{
		responses: map[string]cmdResult{
			"docker inspect": {out: "__not_found__"},
			"docker run":     {out: "abc123"},
		},
	}

	err := reconcileContainer(context.Background(), client, "nginx:latest", "application", nil, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var ran bool
	for _, c := range client.calls {
		if strings.Contains(c, "docker run") {
			ran = true
		}
	}
	if !ran {
		t.Error("expected docker run to be called")
	}
}

// Test 7: Container already running with correct image — no recreation
func TestReconcileContainer_AlreadyRunning(t *testing.T) {
	client := &mockClient{
		responses: map[string]cmdResult{
			"docker inspect": {out: "nginx:latest|true|host"},
		},
	}

	err := reconcileContainer(context.Background(), client, "nginx:latest", "application", nil, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, c := range client.calls {
		if strings.Contains(c, "docker run") || strings.Contains(c, "docker rm") {
			t.Errorf("unexpected docker command when container already in desired state: %q", c)
		}
	}
}

// Test 8: Container runs a different image — recreate
func TestReconcileContainer_WrongImage(t *testing.T) {
	client := &mockClient{
		responses: map[string]cmdResult{
			"docker inspect": {out: "oldimage:v1|true|host"},
			"docker rm":      {out: ""},
			"docker run":     {out: "newid"},
		},
	}

	err := reconcileContainer(context.Background(), client, "nginx:latest", "application", nil, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var removed, started bool
	for _, c := range client.calls {
		if strings.Contains(c, "docker rm") {
			removed = true
		}
		if strings.Contains(c, "docker run") {
			started = true
		}
	}
	if !removed {
		t.Error("expected old container to be removed")
	}
	if !started {
		t.Error("expected new container to be started")
	}
}

// Test: Stopped container with correct image — start it
func TestReconcileContainer_StoppedCorrectImage(t *testing.T) {
	client := &mockClient{
		responses: map[string]cmdResult{
			"docker inspect": {out: "nginx:latest|false|host"},
			"docker start":   {out: "application"},
		},
	}

	err := reconcileContainer(context.Background(), client, "nginx:latest", "application", nil, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var started bool
	for _, c := range client.calls {
		if strings.Contains(c, "docker start") {
			started = true
		}
	}
	if !started {
		t.Error("expected docker start to be called for stopped container")
	}
}

// Test 10: Docker run failure
func TestReconcileContainer_RunFailure(t *testing.T) {
	client := &mockClient{
		responses: map[string]cmdResult{
			"docker inspect": {out: "__not_found__"},
			"docker run":     {out: "Error response", err: errors.New("image pull failed")},
		},
	}

	err := reconcileContainer(context.Background(), client, "bad-image:nope", "application", nil, 0)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

// Test: EnvVars are included in docker run command
func TestReconcileContainer_EnvVarsInRunCommand(t *testing.T) {
	client := &mockClient{
		responses: map[string]cmdResult{
			"docker inspect": {out: "__not_found__"},
			"docker run":     {out: "abc123"},
		},
	}

	envVars := map[string]string{
		"MYSQL_HOST":     "1.2.3.4",
		"MYSQL_PASSWORD": "s3cr3t",
	}

	err := reconcileContainer(context.Background(), client, "adminer:4.8.1", "application", envVars, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var runCmd string
	for _, c := range client.calls {
		if strings.Contains(c, "docker run") {
			runCmd = c
			break
		}
	}
	if runCmd == "" {
		t.Fatal("expected docker run to be called")
	}
	if !strings.Contains(runCmd, "MYSQL_HOST") {
		t.Errorf("expected MYSQL_HOST in docker run command, got: %q", runCmd)
	}
	if !strings.Contains(runCmd, "MYSQL_PASSWORD") {
		t.Errorf("expected MYSQL_PASSWORD in docker run command, got: %q", runCmd)
	}
}

// --- helpers ---

type trackingClient struct {
	inner internalssh.Client
	onRun func(string)
}

func (t *trackingClient) Run(ctx context.Context, cmd string) (string, error) {
	t.onRun(cmd)
	return t.inner.Run(ctx, cmd)
}

func (t *trackingClient) Close() error { return t.inner.Close() }
