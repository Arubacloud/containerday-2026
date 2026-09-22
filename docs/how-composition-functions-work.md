# How Crossplane Composition Functions Work

A practical guide based on the `function-appenv-deployer` we built together.

---

## The Big Picture

A **Composition Function** is a gRPC server that Crossplane calls during every reconciliation of a Composite Resource (XR). Crossplane sends a `RunFunctionRequest` containing the current state of the world and expects a `RunFunctionResponse` describing what the world should look like.

```
User creates ApplicationEnvironment
        │
        ▼
Crossplane reads Composition → Pipeline
        │
        ▼ (every ~TTL seconds)
┌─────────────────────────────┐
│  RunFunctionRequest         │
│  ├── observed XR            │  ← what exists now
│  ├── observed composed MRs  │
│  ├── desired state (so far) │  ← accumulated by previous steps
│  ├── extra resources        │  ← fetched by requirements
│  └── context                │  ← pipeline scratchpad
└─────────────────────────────┘
        │
        ▼
  Your Function (gRPC server)
        │
        ▼
┌─────────────────────────────┐
│  RunFunctionResponse        │
│  ├── desired composed MRs   │  ← what Crossplane should create/update
│  ├── desired XR status      │  ← what to write back to the XR
│  ├── results                │  ← messages/events surfaced to the user
│  ├── context                │  ← data for downstream pipeline steps
│  └── requirements           │  ← extra resources to fetch next time
└─────────────────────────────┘
```

---

## The Pipeline Model

A Composition in Pipeline mode is a sequence of steps. Each function receives the accumulated desired state from all previous steps and can read or modify it.

```yaml
pipeline:
  - step: provision-infrastructure    # function-patch-and-transform
  - step: fetch-ssh-secret            # function-extra-resources
  - step: deploy-application          # function-appenv-deployer  ← our function
  - step: auto-ready                  # function-auto-ready
```

**Key rule**: functions run in order. The desired state is passed through the pipeline — each function sees what the previous ones wrote. The final desired state is what Crossplane reconciles against.

---

## What a Function Can Read

### 1. The Observed XR

The live state of the Composite Resource as it exists in the cluster.

```go
xr, err := request.GetObservedCompositeResource(req)
image, _ := xr.Resource.GetString("spec.image")   // reads spec.image
port, _  := xr.Resource.GetInteger("spec.port")   // reads spec.port
```

Use this to read user-provided spec fields.

### 2. Observed Composed Resources

The live state of all managed resources the Composition has created.

```go
observed, _ := request.GetObservedComposedResources(req)
cs, ok := observed["cloudserver"]   // keyed by the resource's name in the Composition
publicIP, _ := cs.Resource.GetString("status.atProvider.publicIp")
```

Use this to read output/status values from infrastructure (IPs, IDs, endpoints).

### 3. The Pipeline Context

A JSON scratchpad passed between pipeline steps. Functions upstream write to it; downstream functions read from it.

```go
ctx := req.GetContext()
val := ctx.GetFields()["my-key"]
```

`function-extra-resources` uses this to pass fetched Secrets to downstream functions:
```go
// function-extra-resources writes here:
ctx["apiextensions.crossplane.io/extra-resources"]["ssh-secret"][0].data.privateKey

// our function reads here:
extraVal := ctx.GetFields()["apiextensions.crossplane.io/extra-resources"]
```

### 4. Function Input

Static configuration embedded in the Composition YAML, parsed per-step.

```yaml
- step: deploy-application
  functionRef:
    name: function-appenv-deployer
  input:
    apiVersion: appenv.platform.example.com/v1alpha1
    kind: DeployerInput
    spec:
      cloudserverResourceName: cloudserver
      sshUser: ubuntu
```

```go
input := &v1alpha1.DeployerInput{}
protojson.Marshal(req.GetInput())     // parse input struct
json.Unmarshal(b, input)
```

This is platform-team configuration — invisible to end users.

---

## What a Function Can Write

### 1. Desired Composed Resources

Tell Crossplane to create/update managed resources.

```go
// function-patch-and-transform does this for you via YAML patches.
// You can also do it in Go:
dcds, _ := request.GetDesiredComposedResources(req)
cs := resource.NewDesiredComposed()
cs.Resource.SetString("spec.forProvider.name", "my-server")
dcds["cloudserver"] = cs
response.SetDesiredComposedResources(rsp, dcds)
```

**Important**: you are describing desired state, not executing anything. Crossplane applies the diff.

### 2. XR Status

Write back to the Composite Resource's status fields — visible to the user.

```go
dxr, _ := request.GetDesiredCompositeResource(req)
dxr.Resource.SetString("status.endpoint", "http://1.2.3.4:9898")
response.SetDesiredCompositeResource(rsp, dxr)
```

We used this to expose the application endpoint.

### 3. Results (Events)

Surface messages as Kubernetes Events on the XR. These appear in `kubectl describe`.

```go
response.Normalf(rsp, "Waiting for Cloudserver publicIp to be assigned")
response.Fatal(rsp, errors.New("spec.image is required"))
```

| Severity | Effect |
|---|---|
| `Normal` | Non-fatal. XR stays non-ready but Crossplane retries. |
| `Warning` | Non-fatal with warning. |
| `Fatal` | Pipeline stops. XR goes `Synced=False`. Use for real misconfigurations only. |

### 4. Requirements (Extra Resources)

Ask Crossplane to fetch Kubernetes resources and pass them on the next invocation.

```go
rsp.Requirements = &fnv1.Requirements{
    Resources: map[string]*fnv1.ResourceSelector{
        "my-secret": {
            ApiVersion: "v1",
            Kind:       "Secret",
            Match:      &fnv1.ResourceSelector_MatchName{MatchName: "my-secret-name"},
            Namespace:  ptr("default"),
        },
    },
}
```

This is what `function-extra-resources` does internally. On the next reconcile, the fetched resource lands in the pipeline context.

---

## Reconciliation Lifecycle

A function is called **on every reconcile** (controlled by the TTL in the response). The pattern is always:

```
observe → decide → declare
```

Never `observe → execute → mutate`. Crossplane owns the execution.

```go
rsp := response.To(req, response.DefaultTTL)  // DefaultTTL = 1 minute

// 1. Observe
cs, ok := observed["cloudserver"]
if !ok {
    response.Normalf(rsp, "Waiting for Cloudserver")  // non-fatal: retry later
    return rsp, nil
}

// 2. Decide
if !isReady(cs) {
    response.Normalf(rsp, "Cloudserver not ready yet")
    return rsp, nil
}

// 3. Declare / Act (for side-effectful functions like ours)
err := deployer.Deploy(ctx, opts)
if err != nil {
    response.Normalf(rsp, "Deploy failed: %s", err)  // non-fatal: retry
    return rsp, nil
}

response.Normalf(rsp, "Deployed successfully")
```

---

## Fatal vs Non-Fatal: The Critical Distinction

| Condition | Use |
|---|---|
| Transient: resource not ready yet, SSH unreachable, secret not in context yet | `response.Normalf` — Crossplane retries |
| Permanent misconfiguration: missing required field, invalid input | `response.Fatal` — stops the pipeline |

We learned this the hard way: marking "SSH secret not in context yet" as Fatal broke the first-reconcile flow. The context is populated by `function-extra-resources` only after one cycle — so our function must tolerate an empty context gracefully.

---

## Inter-Function Communication

Functions communicate through the **pipeline context**, not through return values.

```
function-extra-resources
    → fetches Secret from Kubernetes
    → writes to context["apiextensions.crossplane.io/extra-resources"]["ssh-secret"]

function-appenv-deployer
    → reads from context["apiextensions.crossplane.io/extra-resources"]["ssh-secret"]
    → decodes base64 Secret data (Kubernetes stores Secret.data base64-encoded)
    → uses the private key for SSH
```

The context is a `*structpb.Struct` — a JSON object. You write/read with:
```go
response.SetContextKey(rsp, "my-key", structpb.NewStringValue("value"))
ctx.GetFields()["my-key"].GetStringValue()
```

---

## Idempotency

Crossplane calls your function repeatedly. Your function must converge to the same state regardless of how many times it runs.

**Our implementation**:
```go
// Instead of blindly running docker run every time:
image, running, hostNetwork, exists := inspectContainer(...)

if exists && image == desired && running && hostNetwork {
    return nil  // already correct, do nothing
}

if exists && (image != desired || !hostNetwork) {
    docker rm -f ...  // remove and recreate
}

if !exists {
    docker run ...    // create
}
```

The key is: **check before acting**.

---

## Project Structure

A Crossplane function is a standard Go gRPC server:

```
functions/appenv-deployer/
├── crossplane.yaml          ← Crossplane package metadata (required for xpkg)
├── Dockerfile               ← builds the runtime image
├── main.go                  ← starts the gRPC server
├── input/v1alpha1/
│   └── input.go             ← DeployerInput type (composition-level config)
└── internal/
    ├── fn/
    │   ├── function.go      ← RunFunction: reads request, writes response
    │   └── function_test.go
    ├── deployer/
    │   ├── deployer.go      ← SSH + Docker business logic
    │   └── deployer_test.go
    └── ssh/
        └── ssh.go           ← SSH Client interface + real implementation
```

The `internal/ssh` interface exists purely to enable unit testing without a real VM.

---

## Packaging and Deployment

A function is **not** a plain Docker image. It must be packaged as a Crossplane `xpkg`:

```bash
# 1. Build the runtime Docker image
docker build -t my-function-runtime:local .

# 2. Wrap it with package metadata into an xpkg OCI artifact
crossplane xpkg build \
  --package-root . \                          # reads crossplane.yaml
  --embed-runtime-image my-function-runtime:local \
  -o my-function.xpkg

# 3. Push to a registry
crossplane xpkg push --package-files my-function.xpkg \
  ghcr.io/my-org/my-function:v1.0.0
```

Then install in the cluster:

```yaml
apiVersion: pkg.crossplane.io/v1
kind: Function
metadata:
  name: my-function
spec:
  package: ghcr.io/my-org/my-function:v1.0.0
```

---

## Key Takeaways

1. **Functions are reconciliation loops**, not one-shot scripts. Design for idempotency.
2. **The pipeline context** is how functions share data — not return values or shared state.
3. **Fatal vs Normal results** control whether Crossplane retries or stops. Default to Normal for transient conditions.
4. **The TTL** controls reconciliation frequency. SSH-heavy functions should use a longer TTL in production.
5. **Functions own side effects**. `function-patch-and-transform` owns resource declarations. Your custom function owns everything else (SSH, Docker, APIs).
6. **Never expose implementation details** (IPs, SSH keys, provider details) through the user-facing XR spec.
