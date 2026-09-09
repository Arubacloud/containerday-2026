# Distributing the Crossplane Project

How to package, publish and install the `containerday-2026` platform API as a self-contained Crossplane package.

---

## What is a Crossplane Project?

A **Crossplane Project** is a bundle that ships everything a platform API needs:

```
Crossplane Project (Configuration package)
├── XRDs          — the CRDs that define ApplicationEnvironment
├── Compositions  — the pipeline that provisions ArubaCloud + runs the deployer
└── Dependencies  — pinned references to providers and functions
```

When a cluster operator installs the project, Crossplane automatically pulls and installs all declared dependencies (providers, functions) and applies the XRDs and Compositions. The operator gets the `ApplicationEnvironment` API ready to use without manually installing anything else.

**The project is NOT:**
- The actual ArubaCloud VMs (those are created per-user when `ApplicationEnvironment` is applied)
- The SSH secrets (those are cluster-specific, must be created by the operator)
- The ProviderConfig (cluster-specific credentials)

---

## Key files

| File | Purpose |
|---|---|
| `crossplane-project.yaml` | Project manifest — declares dependencies, repository |
| `apis/applicationenvironments/definition.yaml` | XRD bundled into the package |
| `apis/applicationenvironments/composition.yaml` | Composition bundled into the package |
| `functions/appenv-deployer/` | Embedded function — built and pushed alongside the project |
| `schemas/` | Generated language bindings for IDE tooling |

---

## `crossplane-project.yaml` explained

```yaml
apiVersion: dev.crossplane.io/v1alpha1
kind: Project
metadata:
  name: containerday-2026
spec:
  # OCI repository where the Configuration package is pushed.
  # Also controls how embedded function image references are rewritten
  # inside Compositions — always use the same repository for build + push.
  repository: github.com/Arubacloud/containerday-2026

  dependencies:
  # provider-arubacloud: provisions ArubaCloud infrastructure (Cloudserver, VPC, etc.)
  - type: xpkg
    xpkg:
      apiVersion: pkg.crossplane.io/v1
      kind: Provider
      package: xpkg.upbound.io/arubacloud/provider-arubacloud
      version: 'v0.0.9'

  # function-auto-ready: marks XR Ready=True when all composed resources are ready
  - type: xpkg
    xpkg:
      apiVersion: pkg.crossplane.io/v1
      kind: Function
      package: xpkg.upbound.io/crossplane-contrib/function-auto-ready
      version: 'v0.2.1'

  # function-patch-and-transform: provisions infrastructure composed resources
  - type: xpkg
    xpkg:
      apiVersion: pkg.crossplane.io/v1
      kind: Function
      package: xpkg.upbound.io/crossplane-contrib/function-patch-and-transform
      version: 'v0.8.0'

  # function-extra-resources: fetches the SSH private key Secret into the pipeline
  - type: xpkg
    xpkg:
      apiVersion: pkg.crossplane.io/v1
      kind: Function
      package: xpkg.upbound.io/crossplane-contrib/function-extra-resources
      version: 'v0.3.0'

  # NOTE: function-appenv-deployer is NOT listed here as a dependency.
  # It is an *embedded* function — built from source in functions/appenv-deployer/
  # and pushed as part of the project build (not a pre-built external package).
```

**Embedded vs external dependencies:**
- **External** (`type: xpkg`): pre-built packages pulled from a registry. Declared in `dependencies`.
- **Embedded**: built from source in this repo's `functions/` directory. Detected automatically by `crossplane project build`.

---

## Workflow: build → push → install

### 1. Update the dependency cache

Before building, make sure the local cache has the latest resolved versions of all dependencies. This also regenerates `schemas/` (language bindings for IDE tooling).

```bash
crossplane dependency update-cache
```

This reads `crossplane-project.yaml`, resolves version constraints, downloads the packages, and regenerates `schemas/`. Commit any changes to `schemas/` and `schemas/.lock.json`.

### 2. Build the project

```bash
crossplane project build
```

This produces `_output/containerday-2026.xpkg` — a single OCI artifact containing:
- The **Configuration package**: XRDs + Compositions
- The **embedded function package** (`function-appenv-deployer`): built from `functions/appenv-deployer/` source

The build also rewrites the `functionRef` in Compositions so they point to the correct image references under `spec.repository`. This is why `--repository` must be consistent between build and push.

```
_output/
└── containerday-2026.xpkg   ← everything needed to distribute the platform
```

### 3. Push to a registry

```bash
# Push with an explicit version tag
crossplane project push --tag=v1.0.0
```

This pushes two OCI images to `ghcr.io/arubacloud/containerday-2026`:
- `ghcr.io/arubacloud/containerday-2026:v1.0.0` — the Configuration package
- `ghcr.io/arubacloud/containerday-2026/function-appenv-deployer:v1.0.0` — the embedded function

**Important:** the repository in `crossplane-project.yaml` is `github.com/Arubacloud/containerday-2026`. This maps to `ghcr.io/arubacloud/containerday-2026` for pushing. Always use the same repository for build and push.

---

## How a cluster operator installs it

### Prerequisites on the target cluster

1. Crossplane v2.4+ installed
2. `ProviderConfig` named `default` configured with ArubaCloud credentials
3. SSH key secrets created (cluster-specific — not included in the package):

```bash
kubectl create secret generic app-ssh-pubkey \
  --namespace default \
  --from-literal=value="$(cat /path/to/id_ed25519.pub)"

kubectl create secret generic app-ssh-privkey \
  --namespace default \
  --from-file=privateKey=/path/to/id_ed25519
```

4. RBAC for `function-extra-resources` to read the private key (see README).

### Install the Configuration

```bash
kubectl apply -f - <<'EOF'
apiVersion: pkg.crossplane.io/v1
kind: Configuration
metadata:
  name: containerday-2026
spec:
  package: ghcr.io/arubacloud/containerday-2026:v1.0.0
EOF
```

Crossplane automatically:
1. Pulls the Configuration package
2. Reads its dependency declarations
3. Installs `provider-arubacloud`, `function-auto-ready`, `function-patch-and-transform`, `function-extra-resources`
4. Installs `function-appenv-deployer` (the embedded function)
5. Applies the XRD and Composition

Wait for everything to become healthy:

```bash
kubectl wait configuration/containerday-2026 --for=condition=Healthy --timeout=5m
kubectl get providers,functions
```

### Create an ApplicationEnvironment

```bash
kubectl apply -f - <<'EOF'
apiVersion: platform.example.com/v1alpha1
kind: ApplicationEnvironment
metadata:
  name: my-app
  namespace: default
spec:
  image: nginx:latest
  port: 80
EOF
```

---

## Local development with `crossplane project run`

`crossplane project run` spins up a local KIND cluster with Crossplane installed, builds the project, and installs it — all in one command. Useful for iterating without pushing to a registry.

```bash
# Start a local dev control plane
crossplane project run

# Apply a ProviderConfig mock or real credentials
crossplane project run \
  --extra-resources=operations/providerconfig.yaml
```

The local control plane is a KIND cluster. `crossplane project run` updates your `kubeconfig` to point at it so `kubectl` works normally:

```bash
kubectl get applicationenvironments
kubectl apply -f examples/applicationenvironment/app.yaml
```

Stop and clean up:

```bash
crossplane project stop
```

---

## Versioning strategy

The project and its embedded function are versioned together. Follow semver:

```bash
# Patch: bug fixes, schema corrections
crossplane project push --tag=v1.0.1

# Minor: new spec fields, new security rules
crossplane project push --tag=v1.1.0

# Major: breaking API changes
crossplane project push --tag=v2.0.0
```

The embedded function (`function-appenv-deployer`) gets the same tag as the Configuration. They move together because the Composition references the function by image — if they diverged, the Composition might reference a function version that doesn't match the XRD.

The CI pipeline (`function-appenv-deployer.yaml`) also pushes the function independently on every push. This is for the standalone use case (installing the function manually without the project). In practice:
- **Project install**: use `crossplane project push` — single tag, Configuration + function in sync
- **Manual install**: use the function's own tag from the CI pipeline

---

## What gets distributed vs what stays local

| Artifact | Distributed in package | Notes |
|---|---|---|
| XRD (`definition.yaml`) | ✅ | Inside the Configuration xpkg |
| Composition (`composition.yaml`) | ✅ | Inside the Configuration xpkg |
| `function-appenv-deployer` | ✅ | Embedded function, pushed alongside |
| External functions (auto-ready, P&T, extra-resources) | Referenced, not bundled | Pulled by Crossplane from their registries on install |
| provider-arubacloud | Referenced, not bundled | Pulled by Crossplane on install |
| SSH secrets | ❌ Never | Cluster-specific, created by the operator |
| ProviderConfig | ❌ Never | Cluster-specific credentials |
| ArubaCloud VMs | ❌ Never | Created per-user when ApplicationEnvironment is applied |

---

## Updating an installed Configuration

When you push a new version, operators can upgrade with:

```bash
kubectl patch configuration containerday-2026 \
  --type=merge \
  -p '{"spec":{"package":"ghcr.io/arubacloud/containerday-2026:v1.1.0"}}'
```

Crossplane applies the updated XRD and Composition. Existing `ApplicationEnvironment` objects continue to reconcile with the new version. The running VMs and containers are unaffected by a Composition update — they are only changed on the next reconcile if the desired state differs.

---

## Schemas and IDE tooling

After `crossplane dependency update-cache`, the `schemas/` directory contains generated language bindings for the declared dependencies. These enable:
- YAML validation in VS Code, IntelliJ, etc.
- Autocomplete for ArubaCloud resource fields in Compositions
- Type-safe Go/Python/KCL code for custom tools

The `schemas/.lock.json` pins the exact package digests used for generation. Commit both `schemas/` and `schemas/.lock.json` to ensure every contributor gets the same bindings without running the cache command.

```bash
# Regenerate after adding or updating a dependency
crossplane dependency update-cache
git add schemas/
git commit -m "chore: update dependency schemas"
```
