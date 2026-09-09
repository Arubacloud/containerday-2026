# function-appenv-deployer — Deep Dive

A file-by-file walkthrough of the function source code with detailed comments explaining every design decision.

---

## Package overview

```
functions/appenv-deployer/
├── crossplane.yaml          Crossplane package metadata
├── Dockerfile               builds the runtime image
├── main.go                  gRPC server entrypoint
├── input/v1alpha1/
│   └── input.go             DeployerInput — composition-level config type
└── internal/
    ├── fn/
    │   ├── function.go      RunFunction — the reconciliation brain
    │   └── function_test.go
    ├── deployer/
    │   ├── deployer.go      SSH + Docker lifecycle
    │   └── deployer_test.go
    └── ssh/
        └── ssh.go           SSH Client/Dialer interfaces + real implementation
```

The `internal/` boundary enforces separation: `fn` owns Crossplane protocol concerns, `deployer` owns SSH/Docker concerns, `ssh` owns the network transport. Each layer can be tested independently.

---

## `main.go` — The entrypoint

```go
func main() {
    debug := os.Getenv("FUNCTION_DEBUG") == "true"

    log, err := logging.NewLogger(debug)
    // ...

    srv := fn.NewFunction(log, deployer.NewSSHDeployer())

    if err := function.Serve(srv,
        function.MTLSCertificates(os.Getenv("TLS_SERVER_CERTS_DIR")),
        function.Insecure(os.Getenv("FUNCTION_INSECURE") == "true"),
    ); err != nil { ... }
}
```

**What it does:**

`function.Serve` starts a gRPC server on `:9443`. Crossplane connects to it over mTLS and calls `RunFunction` on every reconciliation cycle.

**Two TLS modes:**
- **Production** (`TLS_SERVER_CERTS_DIR` set): Crossplane injects a volume with `tls.crt`, `tls.key`, `ca.crt` into the function pod. `MTLSCertificates` reads them and requires mutual TLS — only Crossplane can call the function.
- **Development** (`FUNCTION_INSECURE=true`): no TLS. Used with `crossplane beta render` for local testing.

**Dependency injection at the root:** `deployer.NewSSHDeployer()` is the real SSH implementation. In tests, a mock `Deployer` is passed instead. This is the only place where the real and mock implementations diverge.

---

## `input/v1alpha1/input.go` — Composition-level configuration

```go
type DeployerInput struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`
    Spec DeployerInputSpec `json:"spec"`
}

type DeployerInputSpec struct {
    CloudserverResourceName string    // which composed resource to watch
    SSHSecretRef            SSHSecretRef
    SSHUser                 string    // ubuntu
    SSHPort                 int       // 22
    ContainerName           string    // application
}
```

**Key design principle — two layers of configuration:**

| Layer | Where | Who sets it | Example |
|---|---|---|---|
| **Platform config** | `DeployerInput` in Composition YAML | Platform team | `sshUser: ubuntu`, `containerName: application` |
| **User config** | `ApplicationEnvironment.spec` | Developer | `image: nginx:latest`, `port: 80` |

`DeployerInput` is embedded in the Composition's `input:` field. It is static YAML that the platform team controls. End users never see it, never edit it, never need to know it exists.

The function reads **user config** from the observed XR (`spec.image`, `spec.port`) and **platform config** from `DeployerInput`. Neither bleeds into the other.

**Why embed `TypeMeta` and `ObjectMeta`?** The Crossplane function SDK requires function inputs to be valid Kubernetes objects so they can be decoded via the standard `runtime.Object` interface. The `TypeMeta` carries `apiVersion`/`kind` which identifies the input type in the Composition YAML.

---

## `internal/fn/function.go` — The reconciliation brain

This is where the Crossplane protocol is implemented. Every public method maps to a step in the reconciliation loop.

### The response bootstrap

```go
rsp := response.To(req, response.DefaultTTL)
```

`response.To` copies the incoming desired state into the response. This is critical: in a pipeline, each function receives the desired state accumulated by all previous functions. If you forget this line, you wipe out everything the previous pipeline steps wrote.

`response.DefaultTTL = 1 minute` tells Crossplane how long to cache this response before calling the function again. It controls reconciliation frequency. Lower = faster convergence but more SSH connections.

---

### Parsing function input

```go
func parseInput(req *fnv1.RunFunctionRequest) (*v1alpha1.DeployerInput, error) {
    raw := req.GetInput()         // *structpb.Struct — a JSON object as protobuf
    b, err := protojson.Marshal(raw)  // → JSON bytes
    json.Unmarshal(b, out)            // → DeployerInput struct
}
```

The function input arrives as a `*structpb.Struct` (protobuf's representation of a JSON object). There is no direct way to unmarshal it into a typed Go struct. The two-step dance — `protojson.Marshal` then `json.Unmarshal` — converts it via JSON bytes.

Note: `request.GetInput(req, into)` from the SDK exists but requires `into` to implement `runtime.Object`. Since we use our own simple struct, we do the conversion manually.

---

### Reading from the XR

```go
xr, _ := request.GetObservedCompositeResource(req)
image, _ := xr.Resource.GetString("spec.image")
appPort, _ := xr.Resource.GetInteger("spec.port")
```

`GetObservedCompositeResource` returns the **observed** XR — the live state as it exists in the cluster right now. We use this (not the desired state) to read what the user specified.

`GetString` / `GetInteger` use dot-notation field paths on the unstructured object. They return an error if the field is missing or the wrong type.

`spec.port` defaults to `9898` if absent — this handles XRs created before the field was added to the XRD.

---

### Waiting for the Cloudserver — non-fatal returns

```go
csRes, ok := observed[resource.Name(input.Spec.CloudserverResourceName)]
if !ok {
    response.Normalf(rsp, "Waiting for Cloudserver %q to be provisioned", ...)
    return rsp, nil   // ← return early, not an error
}
```

This is the central pattern for any condition that is **transient** (will resolve itself):

1. Return `rsp, nil` — no Go error. A Go error would crash the pipeline.
2. Add a `Normal` result — surfaces as a Kubernetes Event on the XR.
3. Do NOT call `response.Fatal` — that stops the pipeline and marks the XR `Synced=False`.

Crossplane sees a valid response, notes the Normal result, and retries after the TTL expires.

The three guards — Cloudserver not observed, not ready, no public IP — are ordered from coarse to fine. Each one exits early if its condition is not met, so the code below each guard can assume its precondition holds.

---

### Reading the SSH key from the pipeline context

```go
func getSSHKeyFromContext(req *fnv1.RunFunctionRequest, ref v1alpha1.SSHSecretRef) ([]byte, error) {
    ctx := req.GetContext()
    extraVal := ctx.GetFields()["apiextensions.crossplane.io/extra-resources"]
    secretList := extraVal.GetStructValue().GetFields()["ssh-secret"].GetListValue()
    secretStruct := secretList.GetValues()[0].GetStructValue()
    encoded := secretStruct.GetFields()["data"].GetStructValue().GetFields()[ref.Key].GetStringValue()
    pem, _ := base64.StdEncoding.DecodeString(encoded)
    return pem, nil
}
```

**Why context, not `req.ExtraResources`?**

`function-extra-resources` (the previous pipeline step) uses Crossplane's *requirements* mechanism: it declares what resources it needs in its response, Crossplane fetches them, then on the **next reconcile** they appear in `req.RequiredResources`. After fetching, `function-extra-resources` stores the results in the **pipeline context** under `apiextensions.crossplane.io/extra-resources` so downstream steps in the **same pipeline run** can use them immediately.

Our function reads from the context, not from `req.ExtraResources`. On the very first reconcile the context is empty (function-extra-resources exited early before populating it). We return a non-fatal error so Crossplane retries — on the second reconcile the context is populated.

**Why base64 decode?** Kubernetes stores `Secret.data` values as base64-encoded strings in its API responses. When the secret is serialised through protobuf/structpb and stored in the context, it remains base64-encoded. We must decode it before using it as a PEM key.

---

### Writing back to XR status

```go
dxr, err := request.GetDesiredCompositeResource(req)
dxr.Resource.SetString("status.endpoint", fmt.Sprintf("http://%s:%d", publicIP, appPort))
response.SetDesiredCompositeResource(rsp, dxr)
```

`GetDesiredCompositeResource` returns the **desired** XR — the accumulated desired state from earlier pipeline steps. We modify it and write it back.

**Why desired, not observed?** We are telling Crossplane what the XR *should* look like. Crossplane applies the diff. Writing to `observed` would have no effect.

This only runs after a successful deployment — `status.endpoint` always reflects an actually reachable application, never a partially-provisioned one.

---

### sanitizeError

```go
func sanitizeError(err error) string {
    msg := err.Error()
    if len(msg) > 200 {
        return msg[:200] + "..."
    }
    return msg
}
```

SSH commands can produce verbose output (docker pull logs, apt output) that gets embedded in error messages. Truncating at 200 characters prevents:
- Kubernetes Events from exceeding size limits
- Accidentally leaking sensitive command output (e.g. env vars printed during shell init)

---

## `internal/deployer/deployer.go` — SSH + Docker lifecycle

### The `Deployer` interface

```go
type Deployer interface {
    Deploy(ctx context.Context, opts DeployOptions) error
}
```

A single method. The interface exists purely for testability — in production `SSHDeployer` is used; in tests a `noopDeployer` mock is injected. Without this interface, every test would need a real SSH server.

---

### `ensureDocker`

```go
func ensureDocker(ctx context.Context, c internalssh.Client, timeout time.Duration) error {
    out, err := c.Run(cmdCtx, "command -v docker")
    if err == nil && strings.TrimSpace(out) != "" {
        return startDocker(ctx, c, timeout)  // already installed
    }

    installScript := strings.Join([]string{
        "export DEBIAN_FRONTEND=noninteractive",
        "curl -fsSL https://get.docker.com -o /tmp/get-docker.sh",
        "sh /tmp/get-docker.sh",
        "rm -f /tmp/get-docker.sh",
    }, " && ")

    c.Run(installCtx, installScript)
    return startDocker(ctx, c, timeout)
}
```

**`command -v docker`** is the POSIX way to check if an executable exists. It is preferred over `which docker` (not always available) or `docker --version` (launches the binary, slower).

**`DEBIAN_FRONTEND=noninteractive`** prevents apt from asking interactive questions during Docker installation. Without it the install may hang waiting for a terminal prompt.

**`get.docker.com`** is the official Docker convenience script. It is idempotent — running it on a machine that already has Docker installed is a no-op (it detects the existing installation and exits cleanly).

**`startDocker`** runs `sudo systemctl enable --now docker`. The `enable` part makes Docker start automatically on VM reboot. The `--now` part starts it immediately. The `|| true` at the end prevents failure if systemd is not available.

---

### `reconcileContainer` — the idempotency engine

```go
currentImage, running, hostNetwork, exists, err := inspectContainer(...)

// Case 1: perfect state — nothing to do
if exists && currentImage == image && running && hostNetwork {
    return nil
}

// Case 2: wrong image OR wrong network — remove and fall through to recreate
if exists && (currentImage != image || !hostNetwork) {
    c.Run(rmCtx, "sudo docker rm -f "+containerName)
    exists = false
}

// Case 3: exists, correct, but stopped — just start it
if exists && !running {
    c.Run(startCtx, "sudo docker start "+containerName)
    return nil
}

// Case 4: does not exist — create it
c.Run(runCtx, "sudo docker run -d --name "+containerName+
    " --restart unless-stopped --network host "+image)
```

The four cases cover every possible container state. The logic is linear — each case either returns or falls through to the next. There is no possibility of accidentally running `docker run` when the container already exists.

**`--network host`**: the container shares the VM's network namespace. All ports the container listens on are directly accessible on the VM's public IP without `-p` port mapping. This is necessary because we do not know at compose time which ports the application uses — the user specifies only the image.

**`--restart unless-stopped`**: the container restarts automatically if it crashes or if the VM reboots. It does NOT restart if the container was explicitly stopped with `docker stop` — important for controlled shutdowns.

---

### `inspectContainer`

```go
cmd := `sudo docker inspect --format '{{.Config.Image}}|{{.State.Running}}|{{.HostConfig.NetworkMode}}' ` +
    containerName + ` 2>/dev/null || echo '__not_found__'`
```

`docker inspect --format` uses Go templates to extract specific fields, producing a compact single-line output. Parsing a full JSON blob would work but is fragile.

The `2>/dev/null || echo '__not_found__'` pattern:
- `2>/dev/null` suppresses the error message `"Error: No such container"` that `docker inspect` prints when the container doesn't exist
- `|| echo '__not_found__'` outputs a sentinel value instead of returning a non-zero exit code
- This lets us distinguish "container absent" (sentinel) from a real error

We check three fields and pipe them with `|` as separator:
- `.Config.Image` — what image the container was created with (not necessarily what is running if layers were changed)
- `.State.Running` — boolean, `true` or `false`
- `.HostConfig.NetworkMode` — `"host"`, `"bridge"`, `"none"`, etc.

---

## `internal/ssh/ssh.go` — Transport layer

### The `Client` and `Dialer` interfaces

```go
type Client interface {
    Run(ctx context.Context, cmd string) (string, error)
    Close() error
}

type Dialer interface {
    Dial(ctx context.Context, host string, port int, user string, privateKeyPEM []byte) (Client, error)
}
```

Two separate interfaces:
- `Dialer` is injected into `SSHDeployer`. Tests inject a `mockDialer` that returns a `mockClient`.
- `Client` is what tests control — they pre-load it with canned responses per command substring.

This two-level injection means you can test `SSHDeployer.Deploy` end-to-end without a real SSH server: the mock dialer returns a mock client, the mock client returns pre-configured outputs, and the test verifies which Docker commands were called.

---

### `RealDialer.Dial`

```go
signer, _ := gossh.ParsePrivateKey(privateKeyPEM)

cfg := &gossh.ClientConfig{
    User:            user,
    Auth:            []gossh.AuthMethod{gossh.PublicKeys(signer)},
    HostKeyCallback: gossh.InsecureIgnoreHostKey(), //nolint:gosec
    Timeout:         d.ConnectTimeout,
}

conn, _ := net.DialTimeout("tcp", addr, d.ConnectTimeout)
sshConn, chans, reqs, _ := gossh.NewClientConn(conn, addr, cfg)
return &realClient{client: gossh.NewClient(sshConn, chans, reqs)}, nil
```

**Two-step connection:** we first establish a raw TCP connection (`net.DialTimeout`) then upgrade it to SSH (`gossh.NewClientConn`). This allows us to apply a TCP-level timeout before the SSH handshake, which is important when the VM is booting and the SSH daemon has not started yet — without a TCP timeout, the connection would hang indefinitely.

**`InsecureIgnoreHostKey`**: we cannot verify the host key because:
1. The VM is freshly provisioned — we have never connected to it before
2. There is no infrastructure to distribute or store host key fingerprints
3. The VM's public IP comes from an Elastic IP we control

The security tradeoff is explicit and documented: we accept TOFU (Trust On First Use). A man-in-the-middle attack on the first connection is possible in theory, but the security groups restrict port 22 access to all IPs (`0.0.0.0/0` — this could be tightened to the Crossplane control plane's egress IP in production).

---

### `realClient.Run` — context-aware command execution

```go
func (c *realClient) Run(ctx context.Context, cmd string) (string, error) {
    sess, _ := c.client.NewSession()
    defer sess.Close()

    done := make(chan struct{})
    go func() {
        select {
        case <-ctx.Done():
            sess.Close()   // interrupt the running command
        case <-done:
        }
    }()
    defer close(done)

    out, err := sess.CombinedOutput(cmd)
    return strings.TrimSpace(string(out)), err
}
```

`sess.CombinedOutput` is blocking — it waits for the command to finish. The problem is that if the context is cancelled (e.g., the 12-minute deploy timeout fires), `CombinedOutput` will not unblock on its own.

The goroutine solves this: it watches `ctx.Done()` and calls `sess.Close()` which abruptly terminates the SSH session and causes `CombinedOutput` to return with an error. The `done` channel ensures the goroutine exits cleanly when `Run` returns normally, preventing a goroutine leak.

`strings.TrimSpace` removes trailing newlines from command output, which simplifies comparisons (`"true"` not `"true\n"`).

---

## Data flow summary

```
Crossplane calls RunFunction every ~60 seconds
│
▼
fn.RunFunction
│
├── parseInput()              Composition YAML → DeployerInput struct
├── xr.Resource.GetString()   XR spec → image, port
├── observed["cloudserver"]   observed MRs → Cloudserver status → publicIP
├── getSSHKeyFromContext()     pipeline context → base64 decode → PEM bytes
│
├── deployer.Deploy()
│   │
│   ├── ssh.Dialer.Dial()         TCP + SSH handshake → Client
│   ├── ensureDocker()            Client.Run("command -v docker") → install if missing
│   └── reconcileContainer()
│       ├── inspectContainer()    docker inspect → image|running|networkMode
│       └── docker rm/run/start   converge toward desired state
│
└── response
    ├── Normalf()                 → Kubernetes Event on XR
    └── SetDesiredCompositeResource()  → status.endpoint = http://<ip>:<port>
```

---

## Testing strategy

Each layer is tested in isolation with the layer below mocked:

| Test file | What is mocked | What is tested |
|---|---|---|
| `deployer_test.go` | `ssh.Dialer` → `mockDialer` → `mockClient` | Docker install/start logic, container reconciliation cases |
| `fn/function_test.go` | `deployer.Deployer` → `noopDeployer` | Crossplane protocol: XR parsing, Cloudserver waiting, context reading, status writing |

`mockClient` matches commands by **substring**, not exact string:
```go
responses: map[string]cmdResult{
    "command -v docker": {out: "/usr/bin/docker"},
    "docker inspect":    {out: "nginx:latest|true|host"},
    "docker run":        {out: "abc123"},
}
```

This makes tests resilient to minor flag changes in the real commands while still verifying the intent (checking for Docker, inspecting a container, running a container).

The `fn` tests inject the SSH secret via the pipeline context (not `req.ExtraResources`), mirroring exactly how `function-extra-resources` delivers it in production. The secret value is base64-encoded in the test context, matching the Kubernetes API encoding.
