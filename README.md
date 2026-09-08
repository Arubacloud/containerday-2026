# containerday-2026

A Crossplane v2 project showcasing declarative platform APIs for ContainerDay 2026.

## What is ApplicationEnvironment?

`ApplicationEnvironment` is a platform API that provisions a virtual machine and deploys a Docker image onto it. The user only needs to specify a Docker image — all infrastructure details are hidden.

```yaml
apiVersion: platform.example.com/v1alpha1
kind: ApplicationEnvironment
metadata:
  name: my-app
  namespace: default
spec:
  image: nginx:latest
```

## Architecture

```
ApplicationEnvironment (user-facing API)
        |
        v
Crossplane Composition
        |
        +-- ArubaCloud Resources (Project, VPC, Subnet, SG, EIP, Keypair, Blockstorage)
        |
        +-- ArubaCloud Cloudserver MR  (managed by provider-arubacloud)
        |
        +-- function-appenv-deployer
              |
              +-- waits for Cloudserver Ready + publicIp
              |
              +-- reads SSH private key from Kubernetes Secret
              |
              +-- SSH ubuntu@<publicIp>
              |
              +-- checks/installs Docker (idempotent)
              |
              +-- reconciles Docker container running spec.image
```

## Prerequisites

- Crossplane v2 installed
- `provider-arubacloud v0.0.9` configured with a `ProviderConfig` named `default`
- ArubaCloud credentials in a `ProviderConfig`

## SSH Key Setup

The function reads the VM SSH **private key** from a Kubernetes Secret:

```bash
kubectl create secret generic app-ssh-privkey \
  --namespace crossplane-system \
  --from-file=privateKey=/path/to/id_ed25519
```

The corresponding **public key** must be registered in ArubaCloud as a Keypair. The composition expects it in:

```bash
kubectl create secret generic app-ssh-pubkey \
  --namespace crossplane-system \
  --from-literal=value="ssh-ed25519 AAAA... user@host"
```

## Running the example

```bash
kubectl apply -f examples/applicationenvironment/app.yaml
kubectl get applicationenvironment test -o yaml
```

Watch the status progress from provisioning → VM ready → Docker installed → image deployed → `Ready=True`.

## Composition Function: function-appenv-deployer

The function (`functions/appenv-deployer/`) implements the VM configuration lifecycle:

1. **Observe Cloudserver** — reads the composed Cloudserver from the pipeline context.
2. **Wait for Ready** — returns a non-ready result until `status.conditions[type=Ready].status == True`.
3. **Get publicIp** — reads `status.atProvider.publicIp`; requeues if absent.
4. **Read SSH key** — gets the private key from the `ssh-secret` extra resource (a Kubernetes Secret). The key is never logged or put into XR status.
5. **SSH connect** — connects as `ubuntu` on port 22. Host key verification uses TOFU (accept-first-use); network-level security groups provide compensating controls.
6. **Ensure Docker** — runs `command -v docker`; if absent, installs via `get.docker.com` (idempotent). Starts the daemon with `systemctl`.
7. **Reconcile container** — uses `docker inspect` to check container state, then:
   - Container absent → `docker run -d --name application --restart unless-stopped <image>`
   - Container present, correct image, running → no-op
   - Container present, wrong image → `docker rm -f` + `docker run`
   - Container stopped → `docker start`

### Security

- Private keys are never logged, never put into XR status, never in error messages.
- The function requests only the `app-ssh-privkey` Secret via the extra-resources mechanism.
- RBAC should grant the function's ServiceAccount `get` on that specific Secret only.

### RBAC

Apply the following to allow the function to read the SSH secret:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: function-appenv-deployer
  namespace: crossplane-system
rules:
- apiGroups: [""]
  resources: ["secrets"]
  resourceNames: ["app-ssh-privkey"]
  verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: function-appenv-deployer
  namespace: crossplane-system
subjects:
- kind: ServiceAccount
  name: function-appenv-deployer
  namespace: crossplane-system
roleRef:
  kind: Role
  name: function-appenv-deployer
  apiGroup: rbac.authorization.k8s.io
```

## Building the function

```bash
cd functions/appenv-deployer
go build ./...
go test ./...
docker build -t function-appenv-deployer:latest .
```
