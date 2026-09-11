# containerday-2026

A Crossplane v2 project that exposes **platform APIs** hiding all ArubaCloud infrastructure details. A developer creates one object and gets a running Docker container — with or without a managed MySQL database.

---

## Contents

| Section | Description |
|---|---|
| [Platform APIs](#platform-apis) | The two user-facing kinds and their fields |
| [Architecture](#architecture) | How each API composes infrastructure |
| [Prerequisites](#prerequisites) | Crossplane installation |
| [Installation](#installation) | Option A (Configuration package) and Option B (manual) |
| [ApplicationEnvironment usage](#applicationenvironment-usage) | Deploy a container on a VM |
| [Microservice usage](#microservice-usage) | Deploy a container + managed MySQL |
| [Composition Function](#composition-function-function-appenv-deployer) | How the deployer function works |
| [Building and releasing](#building-and-releasing-the-function) | Tests, build, push |
| [Repository structure](#repository-structure) | File layout |

---

## Platform APIs

### ApplicationEnvironment

A VM is provisioned and a Docker image is deployed onto it. No database.

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

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `spec.image` | string | yes | — | Docker image to deploy |
| `spec.port` | integer | no | `9898` | TCP port the container listens on |

| Status field | Description |
|---|---|
| `status.endpoint` | `http://<publicIp>:<port>` — reachable once `Ready=True` |
| `status.conditions` | Standard Crossplane conditions (`Synced`, `Ready`, `Responsive`) |

---

### Microservice

A VM **and** a managed MySQL DBaaS cluster are provisioned. The container is started with MySQL connection env vars pre-injected. Database credentials are written to a Kubernetes Secret.

```yaml
apiVersion: platform.example.com/v1alpha1
kind: Microservice
metadata:
  name: my-svc
  namespace: default
spec:
  image: adminer:4.8.1   # any MySQL-aware Docker image
  port: 8080
  database:
    engine: mysql
  writeConnectionSecretToRef:
    name: my-svc-db-conn   # Secret to write credentials into
    namespace: default
```

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `spec.image` | string | yes | — | Docker image to deploy |
| `spec.port` | integer | no | `8080` | TCP port the container listens on |
| `spec.database.engine` | string | yes | — | Database engine. Only `mysql` supported |
| `spec.writeConnectionSecretToRef.name` | string | no | — | Secret to write DB credentials into |
| `spec.writeConnectionSecretToRef.namespace` | string | no | — | Namespace of that Secret |

| Status field | Description |
|---|---|
| `status.endpoint` | `http://<vmIp>:<port>` — reachable once `Ready=True` |
| `status.databaseHost` | Public IP of the DBaaS cluster |
| `status.databasePort` | `3306` |
| `status.databaseName` | `app` |
| `status.databaseUser` | `appuser` |
| `status.conditions` | Standard Crossplane conditions |

Connection secret keys (written to `writeConnectionSecretToRef`):

| Key | Content |
|---|---|
| `host` | MySQL public IP |
| `port` | `3306` |
| `database` | `app` |
| `username` | `appuser` |
| `password` | plain-text password |
| `endpoint` | `mysql://<host>:3306/app` (full DSN) |

---

## Architecture

### ApplicationEnvironment

```
ApplicationEnvironment (spec.image, spec.port)
        │
        ▼
Crossplane Composition (Pipeline mode)
        │
        ├── Step 1: provision-infrastructure (function-patch-and-transform)
        │     ├── ArubaCloud Project
        │     ├── VPC + Subnet
        │     ├── Security Group + Rules: SSH(22), HTTP(80), app(spec.port), ICMP, egress
        │     ├── Elastic IP
        │     ├── Boot Volume (Ubuntu 22.04, 100 GB)
        │     ├── Keypair
        │     └── Cloudserver (flavor CSO4A8, zone ITBG-1)
        │
        ├── Step 2: fetch-ssh-secret (function-extra-resources)
        │     └── reads Secret/app-ssh-privkey
        │
        ├── Step 3: deploy-application (function-appenv-deployer)
        │     ├── waits for Cloudserver Ready + publicIp
        │     ├── SSH ubuntu@<publicIp>:22
        │     ├── checks/installs Docker (idempotent)
        │     ├── reconciles container (--network host, --restart unless-stopped)
        │     └── writes status.endpoint
        │
        └── Step 4: auto-ready
```

### Microservice

```
Microservice (spec.image, spec.port, spec.database.engine)
        │
        ▼
Crossplane Composition (Pipeline mode)
        │
        ├── Step 1: provision-infrastructure (function-patch-and-transform)
        │     ├── ArubaCloud Project
        │     ├── VPC + Subnet
        │     ├── Security Group + Rules (VM): SSH(22), HTTP(80), app(spec.port), ICMP, egress
        │     ├── Elastic IP (VM)
        │     ├── Boot Volume + Keypair + Cloudserver
        │     ├── Security Group + Rules (DBaaS): MySQL(3306), egress
        │     ├── Elastic IP (DBaaS)
        │     ├── Dbaas (MySQL 8.0, flavor DBO2A4, zone ITBG-1)
        │     ├── Database (name: app)
        │     ├── Dbaasuser (username: appuser)
        │     └── Databasegrant (role: liteadmin)
        │
        ├── Step 2: fetch-secrets (function-extra-resources)
        │     ├── reads Secret/app-ssh-privkey
        │     └── reads Secret/app-db-password
        │
        ├── Step 3: deploy-application (function-appenv-deployer)
        │     ├── waits for Cloudserver Ready + publicIp
        │     ├── waits for Dbaas cluster Ready
        │     ├── waits for DBaaS Elastic IP address
        │     ├── SSH ubuntu@<publicIp>:22
        │     ├── checks/installs Docker (idempotent)
        │     ├── reconciles container with env vars:
        │     │     MYSQL_HOST, MYSQL_PORT, MYSQL_DATABASE, MYSQL_USER,
        │     │     MYSQL_PASSWORD, DB_*, ADMINER_DEFAULT_SERVER
        │     ├── writes status.endpoint + status.database*
        │     └── writes connection details to writeConnectionSecretToRef
        │
        └── Step 4: auto-ready
```

All resource names are derived from the XR name — multiple environments never collide.

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

A Crossplane `Configuration` is a bundle — applying it tells Crossplane to download and install the ArubaCloud provider, all composition functions, the XRDs, and the Compositions. The provider installation is what registers the `arubacloud.crossplane.io` CRDs; nothing else in these steps will work until the provider is healthy.

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
> The provider registers the `arubacloud.crossplane.io/v1beta1` CRDs (including `ProviderConfig`). Applying Step 3 before this completes will fail with `no matches for kind "ProviderConfig"`.

```bash
kubectl wait provider/arubacloud-provider-arubacloud \
  --for=condition=Healthy --timeout=5m
```

**Step 3 — Create ArubaCloud credentials and ProviderConfig**

The ArubaCloud provider looks for its credentials secret in the **same namespace as the `ProviderConfig` object** (`default`). Create both in `default`:

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

**Step 5 — Create platform secrets**

All secrets must be in `default` — the namespace of the XR.

SSH key pair (required by both `ApplicationEnvironment` and `Microservice`):

```bash
ssh-keygen -t ed25519 -f /tmp/appenv-key -N "" -C "crossplane-appenv"

kubectl create secret generic app-ssh-pubkey \
  --namespace default \
  --from-literal=value="$(cat /tmp/appenv-key.pub)"

kubectl create secret generic app-ssh-privkey \
  --namespace default \
  --from-file=privateKey=/tmp/appenv-key
```

DB password (required by `Microservice` only). Must meet ArubaCloud's password policy (minimum 12 characters, mixed case, numbers and symbols):

```bash
kubectl create secret generic app-db-password \
  --namespace default \
  --from-literal=password=<your-db-password>
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
  resourceNames: ["app-ssh-privkey", "app-db-password"]
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

> **The `kubectl wait` at the end of this block is mandatory before Step 2.** The provider registers the `arubacloud.crossplane.io/v1beta1` CRDs. Do not run Step 2 until the wait exits successfully.

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

**Step 4 — Apply XRDs and Compositions:**

```bash
kubectl apply -f apis/applicationenvironments/definition.yaml
kubectl apply -f apis/applicationenvironments/composition.yaml
kubectl apply -f apis/microservices/definition.yaml
kubectl apply -f apis/microservices/composition.yaml
```

**Steps 5–6 — Platform secrets and RBAC:** same as Option A Steps 5–6 above.

---

## ApplicationEnvironment usage

### Deploy an application

```bash
kubectl apply -f examples/applicationenvironment/app.yaml
```

Watch progress (infrastructure takes ~5 minutes to provision):

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

Within ~60 seconds the function detects the image mismatch, removes the old container, and runs the new one.

### Self-healing

The function reconciles every ~60 seconds. If you manually remove or stop the container on the VM, it will be recreated automatically on the next cycle.

---

## Microservice usage

### Deploy a microservice with MySQL

```bash
kubectl apply -f examples/microservice/app.yaml
```

Watch progress (VM + DBaaS cluster both need to provision — allow 10–15 minutes):

```bash
kubectl get microservice my-svc -w
```

Events during reconciliation:

```
Waiting for Cloudserver "cloudserver" to be provisioned
Waiting for Cloudserver to become ready
Waiting for Dbaas cluster "dbaas" to become ready
Waiting for DBaaS Elastic IP address to be assigned
Application "adminer:4.8.1" deployed successfully as container "application"
```

Once `READY=True`, check connection details:

```bash
kubectl get microservice my-svc -o jsonpath='{.status}' | jq .
# {
#   "endpoint":     "http://<vm-ip>:8080",
#   "databaseHost": "<dbaas-ip>",
#   "databasePort": "3306",
#   "databaseName": "app",
#   "databaseUser": "appuser"
# }
```

Read credentials from the connection secret:

```bash
kubectl get secret my-svc-db-conn -n default \
  -o jsonpath='{.data.password}' | base64 -d

# Full MySQL DSN:
kubectl get secret my-svc-db-conn -n default \
  -o jsonpath='{.data.endpoint}' | base64 -d
```

Open adminer in your browser at `http://<status.endpoint>`. The MySQL server field is pre-filled. Log in with:

| Field    | Value                                |
|----------|--------------------------------------|
| Server   | `<status.databaseHost>` (pre-filled) |
| Username | `appuser`                            |
| Password | from `my-svc-db-conn` secret         |
| Database | `app`                                |

### Change the image

```bash
kubectl patch microservice my-svc \
  --type=merge -p '{"spec":{"image":"phpmyadmin:5.2","port":80}}'
```

The function detects the image mismatch on the next reconcile, removes the old container, and starts the new one with the same MySQL env vars injected.

### Injected environment variables

The following env vars are automatically set on the container — any MySQL-aware image can use them without additional configuration:

| Variable | Value |
|---|---|
| `MYSQL_HOST` | DBaaS public IP |
| `MYSQL_PORT` | `3306` |
| `MYSQL_DATABASE` | `app` |
| `MYSQL_USER` | `appuser` |
| `MYSQL_PASSWORD` | from `app-db-password` secret |
| `DB_HOST` / `DB_PORT` / `DB_NAME` / `DB_USER` / `DB_PASSWORD` | same values (common aliases) |
| `ADMINER_DEFAULT_SERVER` | same as `MYSQL_HOST` |

---

## Composition Function: function-appenv-deployer

Source: `functions/appenv-deployer/`
Package: `ghcr.io/arubacloud/function-appenv-deployer`
Built and pushed via: `.github/workflows/function-appenv-deployer.yaml`

### Reconciliation steps

1. **Parse input** — reads `cloudserverResourceName`, `sshUser`, `sshPort`, `containerName`, optional `database` config.
2. **Read XR spec** — reads `spec.image` and `spec.port` from the observed XR.
3. **Observe Cloudserver** — looks up the `cloudserver` composed resource. Returns non-fatal if not yet observed.
4. **Check Cloudserver readiness** — verifies `status.conditions[type=Ready].status == True`.
5. **Get publicIp** — reads `status.atProvider.publicIp`. Returns non-fatal if absent.
6. **Read SSH key** — reads from pipeline context populated by `function-extra-resources`. Returns non-fatal if not yet populated.
7. **[Microservice only] Wait for Dbaas Ready** — checks the `dbaas` composed resource for `Ready=True` before deploying. This ensures MySQL is accessible when the container starts.
8. **[Microservice only] Get DBaaS host** — reads `status.atProvider.address` from the `dbaas-eip` composed resource.
9. **[Microservice only] Read DB password** — reads from pipeline context. Builds MySQL env vars map.
10. **Short-circuit check** — if `status.endpoint` already matches the current IP:port and `status.deployedImage` matches `spec.image`, skip SSH entirely and return in milliseconds. SSH only runs on first deploy or after an image change.
11. **SSH connect** — dials `ubuntu@<publicIp>:22`. Timeout: 30s.
12. **Ensure Docker** — checks `command -v docker`. If missing, launches the install script **in the background** (`nohup`) and returns a retryable result immediately — no blocking wait. The flag file `/tmp/docker-installing` prevents duplicate installs across reconciles.
13. **Reconcile container** — inspects `image`, `running`, `networkMode`. Recreates if any differ from desired. Uses `--network host`.
14. **Write status** — sets `status.endpoint`, `status.deployedImage`, `status.database*`, and connection secret via `writeConnectionSecretToRef`.

### Reconcile lifecycle on a fresh VM

| Cycle | What happens | SSH? | Typical duration |
|---|---|---|---|
| 1 | Infrastructure not ready yet | No | instant |
| 2–N | Waiting for Cloudserver / Dbaas | No | instant |
| N+1 | Docker not found → background install triggered | Yes | ~5 s |
| N+2 to N+M | `/tmp/docker-installing` present → waiting | Yes | ~3 s |
| N+M+1 | Docker ready → pull image + `docker run` | Yes | ~10–30 s |
| N+M+2+ | Short-circuit: endpoint + deployedImage match | No | ~0 s |

To force a re-deploy after the container is stopped or the image changes:

```bash
# Trigger re-deploy by clearing the short-circuit marker
kubectl patch <kind> <name> --type=json \
  -p='[{"op":"remove","path":"/status/deployedImage"}]'
```

### Container reconciliation matrix

| State | Action |
|---|---|
| Does not exist | `docker run -d --name application --restart unless-stopped --network host [-e ...] <image>` |
| Exists, correct image, running, host network | no-op |
| Exists, wrong image | `docker rm -f` → `docker run` |
| Exists, not host network | `docker rm -f` → `docker run` |
| Exists, correct image, stopped | `docker start application` |

### Security

- SSH private key and DB password are read from pipeline context and never logged, never written to XR status, never included in error messages.
- DB password is written only to `writeConnectionSecretToRef` — not to XR status.
- Host key verification uses TOFU (accept-first-use).
- RBAC grants `function-extra-resources` `get`+`list` on `app-ssh-privkey` and `app-db-password` only.

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
    definition.yaml       ← XRD: ApplicationEnvironment
    composition.yaml      ← 14 composed resources + 4 pipeline steps

  microservices/
    definition.yaml       ← XRD: Microservice
    composition.yaml      ← 22 composed resources + 4 pipeline steps

examples/
  applicationenvironment/
    app.yaml              ← ApplicationEnvironment example
  microservice/
    app.yaml              ← Microservice example (adminer:4.8.1 + MySQL)

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
