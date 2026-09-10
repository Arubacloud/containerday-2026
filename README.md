# containerday-2026

A Crossplane v2 project that exposes a **platform API** hiding all ArubaCloud infrastructure details. A developer creates one object and gets a running Docker container on a provisioned VM.

---

## User-facing API

```yaml
apiVersion: platform.example.com/v1alpha1
kind: ApplicationEnvironment
metadata:
  name: my-app
  namespace: default
spec:
  image: nginx:latest   # any Docker image
  port: 80              # port the container listens on (default: 9898)
```

After creation, check status:

```bash
kubectl get applicationenvironment my-app
# NAME     SYNCED   READY   ...
# my-app   True     True

kubectl get applicationenvironment my-app -o jsonpath='{.status.endpoint}'
# http://1.2.3.4:80
```

### Spec fields

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `spec.image` | string | yes | — | Docker image to deploy (e.g. `nginx:latest`, `ghcr.io/org/app:v1`) |
| `spec.port` | integer | no | `9898` | TCP port the container listens on. Used to build `status.endpoint` and open the security group. |

### Status fields

| Field | Description |
|---|---|
| `status.endpoint` | `http://<publicIp>:<port>` — reachable once `Ready=True` |
| `status.conditions` | Standard Crossplane conditions (`Synced`, `Ready`, `Responsive`) |

---

## Architecture

```
ApplicationEnvironment (spec.image, spec.port)
        │
        ▼
Crossplane Composition (Pipeline mode)
        │
        ├── Step 1: provision-infrastructure (function-patch-and-transform)
        │     ├── ArubaCloud Project
        │     ├── VPC
        │     ├── Subnet
        │     ├── Security Group
        │     ├── Security Rules: SSH(22), HTTP(80), HTTPS(443), app(spec.port), ICMP, egress
        │     ├── Elastic IP
        │     ├── Boot Volume (Ubuntu 22.04, 100 GB)
        │     ├── Keypair
        │     └── Cloudserver (flavor CSO4A8, zone ITBG-1)
        │
        ├── Step 2: fetch-ssh-secret (function-extra-resources)
        │     └── reads Secret/app-ssh-privkey from namespace default
        │
        ├── Step 3: deploy-application (function-appenv-deployer)
        │     ├── waits for Cloudserver Ready + publicIp
        │     ├── reads SSH private key from pipeline context
        │     ├── SSH ubuntu@<publicIp>:22
        │     ├── checks/installs Docker (idempotent)
        │     ├── reconciles container named "application"
        │     │     (--network host, --restart unless-stopped)
        │     └── writes status.endpoint = http://<publicIp>:<port>
        │
        └── Step 4: auto-ready (function-auto-ready)
              └── marks XR Ready=True only when all composed resources are ready
```

All resource names are derived from the `ApplicationEnvironment` name — multiple environments never collide.

---

## Prerequisites

**Crossplane v2.4+** must be installed and running before anything below. If you do not have it yet, install it via Helm:

```bash
helm repo add crossplane-stable https://charts.crossplane.io/stable
helm repo update

helm install crossplane \
  crossplane-stable/crossplane \
  --namespace crossplane-system \
  --create-namespace \
  --version 2.4.0

kubectl wait deployment/crossplane \
  --namespace crossplane-system \
  --for=condition=Available --timeout=5m
```

Everything else (provider, functions, XRD, Composition) is installed as part of the steps below.

---

## Installation

### Option A — Configuration package (recommended)

**Step 1 — Install the Configuration**

A Crossplane `Configuration` is a bundle — applying it tells Crossplane to download and install the ArubaCloud provider, all composition functions, the XRD, and the Composition. The provider installation is what registers the `arubacloud.crossplane.io` CRDs; nothing else in these steps will work until the provider is healthy.

```bash
kubectl apply -f - <<'EOF'
apiVersion: pkg.crossplane.io/v1
kind: Configuration
metadata:
  name: containerday-2026
spec:
  package: ghcr.io/arubacloud/containerday-2026:latest
EOF
```

**Step 2 — Wait for the provider to become Healthy**

> **Do not proceed to Step 3 until this command exits successfully.**
> The provider registers the `arubacloud.crossplane.io/v1beta1` CRDs (including `ClusterProviderConfig`). Applying Step 3 before this completes will fail with `no matches for kind "ClusterProviderConfig"`.

```bash
kubectl wait provider/arubacloud-provider-arubacloud \
  --for=condition=Healthy --timeout=5m
```

**Step 3 — Create ArubaCloud credentials and ProviderConfig**

The `ProviderConfig` CRD now exists. The ArubaCloud provider looks for its credentials secret in the **same namespace as the `ProviderConfig` object** (`default`), regardless of `secretRef.namespace`. Create both in `default`:

```bash
kubectl create secret generic arubacloud-credentials \
  --namespace default \
  --from-literal=credentials='{
    "client_id":     "YOUR_CLIENT_ID",
    "client_secret": "YOUR_CLIENT_SECRET",
    "resource_timeout": "30m"
  }'

kubectl apply -f - <<'EOF'
apiVersion: arubacloud.crossplane.io/v1beta1
kind: ProviderConfig
metadata:
  name: default
  namespace: default
spec:
  credentials:
    source: Secret
    secretRef:
      name: arubacloud-credentials
      namespace: default
      key: credentials
EOF
```

**Step 4 — Wait for the full Configuration to be Healthy**

```bash
kubectl wait configuration/containerday-2026 \
  --for=condition=Healthy --timeout=5m
```

**Step 5 — Create SSH key secrets**

Both secrets must be in `default` — the namespace of the XR:

```bash
ssh-keygen -t ed25519 -f /tmp/appenv-key -N "" -C "crossplane-appenv"

kubectl create secret generic app-ssh-pubkey \
  --namespace default \
  --from-literal=value="$(cat /tmp/appenv-key.pub)"

kubectl create secret generic app-ssh-privkey \
  --namespace default \
  --from-file=privateKey=/tmp/appenv-key
```

**Step 6 — Grant RBAC to function-extra-resources**

The hash suffix in the service account name is cluster-specific — the command below detects it automatically:

```bash
SA=$(kubectl get serviceaccount -n crossplane-system \
  --no-headers -o custom-columns=":metadata.name" | grep extra-resources | head -1)

kubectl apply -f - <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: function-extra-resources-ssh-secret
  namespace: default
rules:
- apiGroups: [""]
  resources: ["secrets"]
  resourceNames: ["app-ssh-privkey"]
  verbs: ["get", "list"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: function-extra-resources-ssh-secret
  namespace: default
subjects:
- kind: ServiceAccount
  name: ${SA}
  namespace: crossplane-system
roleRef:
  kind: Role
  name: function-extra-resources-ssh-secret
  apiGroup: rbac.authorization.k8s.io
EOF
```

**Upgrade:**

```bash
# Remove the embedded function first to avoid digest conflicts
kubectl delete function arubacloud-containerday-2026appenv-deployer

kubectl patch configuration containerday-2026 \
  --type=merge -p '{"spec":{"package":"ghcr.io/arubacloud/containerday-2026:v1.0.0"}}'
```

**Uninstall:**

Crossplane does not cascade-delete providers and functions when a Configuration is removed — they are independent objects that could be shared. Delete them explicitly after removing the Configuration:

```bash
kubectl delete configuration containerday-2026

kubectl delete provider arubacloud-provider-arubacloud

kubectl delete function \
  crossplane-contrib-function-patch-and-transform \
  crossplane-contrib-function-extra-resources \
  crossplane-contrib-function-auto-ready \
  arubacloud-containerday-2026appenv-deployer
```

---

### Option B — Manual install

Use this if you need to pin versions independently or the cluster already has some packages installed.

**Step 1 — Install the provider and wait for its CRDs:**

> **The `kubectl wait` at the end of this block is mandatory before Step 2.** The provider registers the `arubacloud.crossplane.io/v1beta1` CRDs (including `ClusterProviderConfig`). Do not run Step 2 until the wait exits successfully.

```bash
kubectl apply -f - <<'EOF'
apiVersion: pkg.crossplane.io/v1
kind: Provider
metadata:
  name: arubacloud-provider-arubacloud
spec:
  package: xpkg.upbound.io/arubacloud/provider-arubacloud:v0.0.10
EOF

kubectl wait provider/arubacloud-provider-arubacloud \
  --for=condition=Healthy --timeout=5m
```

**Step 2 — Create ArubaCloud credentials and ProviderConfig:**

The ArubaCloud provider looks for its credentials secret in the same namespace as the `ProviderConfig` object (`default`). Create both there:

```bash
kubectl create secret generic arubacloud-credentials \
  --namespace default \
  --from-literal=credentials='{
    "client_id":     "YOUR_CLIENT_ID",
    "client_secret": "YOUR_CLIENT_SECRET",
    "resource_timeout": "30m"
  }'

kubectl apply -f - <<'EOF'
apiVersion: arubacloud.crossplane.io/v1beta1
kind: ProviderConfig
metadata:
  name: default
  namespace: default
spec:
  credentials:
    source: Secret
    secretRef:
      name: arubacloud-credentials
      namespace: default
      key: credentials
EOF
```

**Step 3 — Install functions:**

```bash
kubectl apply -f - <<'EOF'
apiVersion: pkg.crossplane.io/v1
kind: Function
metadata:
  name: crossplane-contrib-function-patch-and-transform
spec:
  package: xpkg.upbound.io/crossplane-contrib/function-patch-and-transform:v0.8.0
---
apiVersion: pkg.crossplane.io/v1
kind: Function
metadata:
  name: crossplane-contrib-function-extra-resources
spec:
  package: xpkg.upbound.io/crossplane-contrib/function-extra-resources:v0.3.0
---
apiVersion: pkg.crossplane.io/v1
kind: Function
metadata:
  name: crossplane-contrib-function-auto-ready
spec:
  package: xpkg.upbound.io/crossplane-contrib/function-auto-ready:v0.2.1
---
apiVersion: pkg.crossplane.io/v1
kind: Function
metadata:
  name: arubacloud-containerday-2026appenv-deployer
spec:
  package: ghcr.io/arubacloud/function-appenv-deployer:0.0.8
EOF

kubectl wait \
  function/crossplane-contrib-function-patch-and-transform \
  function/crossplane-contrib-function-extra-resources \
  function/crossplane-contrib-function-auto-ready \
  function/arubacloud-containerday-2026appenv-deployer \
  --for=condition=Healthy --timeout=120s
```

**Step 4 — Apply XRD and Composition:**

```bash
kubectl apply -f apis/applicationenvironments/definition.yaml
kubectl apply -f apis/applicationenvironments/composition.yaml
```

**Steps 5–6 — SSH key secrets and RBAC:** same as Option A Steps 5–6 above.

---

## Usage

### Deploy an application

```bash
kubectl apply -f examples/applicationenvironment/app.yaml
```

Watch progress (infrastructure takes a few minutes to provision):

```bash
kubectl get applicationenvironment test -w
```

Events during reconciliation:

```
Waiting for Cloudserver "cloudserver" to be provisioned
Waiting for Cloudserver to become ready
Waiting for Cloudserver publicIp to be assigned
Waiting for SSH secret: ...          ← first reconcile only
Application "..." deployed successfully as container "application"
```

Once `READY=True`:

```bash
curl $(kubectl get applicationenvironment test -o jsonpath='{.status.endpoint}')
```

### Change the image

```bash
kubectl patch applicationenvironment test \
  --type=merge -p '{"spec":{"image":"nginx:latest","port":80}}'
```

Within ~60 seconds the function detects the image mismatch, removes the old container, and runs the new one. The endpoint updates to `http://<ip>:80`.

### Self-healing

The function reconciles every ~60 seconds. If you manually remove or stop the container on the VM, it will be recreated automatically on the next cycle.

---

## Composition Function: function-appenv-deployer

Source: `functions/appenv-deployer/`  
Package: `ghcr.io/arubacloud/function-appenv-deployer`  
Built and pushed via: `.github/workflows/function-appenv-deployer.yaml`

### Reconciliation steps

1. **Parse input** — reads `cloudserverResourceName`, `sshUser`, `sshPort`, `containerName` from the Composition's static input.
2. **Read XR spec** — reads `spec.image` and `spec.port` from the observed XR.
3. **Observe Cloudserver** — looks up the `cloudserver` composed resource. Returns non-fatal if not yet observed.
4. **Check readiness** — verifies `status.conditions[type=Ready].status == True`.
5. **Get publicIp** — reads `status.atProvider.publicIp`. Returns non-fatal if absent.
6. **Read SSH key** — reads from pipeline context key `apiextensions.crossplane.io/extra-resources` populated by `function-extra-resources`. Base64-decodes the Secret value. Returns non-fatal if context not yet populated (first reconcile).
7. **SSH connect** — dials `ubuntu@<publicIp>:22` using the private key. Timeout: 30s.
8. **Ensure Docker** — runs `command -v docker`; if missing, installs via `get.docker.com`. Starts with `systemctl enable --now docker`.
9. **Reconcile container** — inspects `image`, `running`, `networkMode`. Recreates if any differ from desired. Uses `--network host` so all container ports are accessible directly on the VM's public IP.
10. **Write endpoint** — sets `status.endpoint = http://<publicIp>:<port>` on the desired XR.

### Container reconciliation matrix

| State | Action |
|---|---|
| Does not exist | `docker run -d --name application --restart unless-stopped --network host <image>` |
| Exists, correct image, running, host network | no-op |
| Exists, wrong image | `docker rm -f` → `docker run` |
| Exists, not host network | `docker rm -f` → `docker run` |
| Exists, correct image, stopped | `docker start application` |

### Security

- SSH private key is read from pipeline context and never logged, never written to XR status, never included in error messages.
- Host key verification uses TOFU (accept-first-use). Security groups restrict VM access to SSH and declared application ports.
- RBAC grants `function-extra-resources` `get`+`list` on `app-ssh-privkey` only.

---

## Building and releasing the function

Tests:

```bash
cd functions/appenv-deployer
go test ./... -v -race
```

Build and push (done automatically by CI on every push to `main` and `v*` tags):

```bash
# 1. Build runtime image
docker build -t function-appenv-deployer-runtime:local .

# 2. Build Crossplane xpkg (not a plain Docker image)
crossplane xpkg build \
  --package-root . \
  --embed-runtime-image function-appenv-deployer-runtime:local \
  -o /tmp/function-appenv-deployer.xpkg

# 3. Push
crossplane xpkg push \
  --package-files /tmp/function-appenv-deployer.xpkg \
  ghcr.io/arubacloud/function-appenv-deployer:latest
```

CI workflow: `.github/workflows/function-appenv-deployer.yaml`

---

## Repository structure

```
apis/
  applicationenvironments/
    definition.yaml       ← XRD: ApplicationEnvironment CRD
    composition.yaml      ← Composition: 14 composed resources + 4 pipeline steps

examples/
  applicationenvironment/
    app.yaml              ← Example XR

functions/
  appenv-deployer/
    crossplane.yaml       ← Crossplane package metadata
    Dockerfile            ← Runtime image
    main.go               ← gRPC server entrypoint
    input/v1alpha1/       ← DeployerInput type (composition-level config)
    internal/
      fn/                 ← RunFunction implementation
      deployer/           ← SSH + Docker lifecycle logic
      ssh/                ← SSH Client interface + real implementation

docs/
  requirements.md
  how-composition-functions-work.md

others/                   ← Reference standalone ArubaCloud resource examples
.github/
  workflows/
    function-appenv-deployer.yaml
```
