# Task: Complete the Crossplane ApplicationEnvironment Project

## Objective

Complete the existing Crossplane v2 project that exposes a user-facing `ApplicationEnvironment` API.

The project already contains:

* An XRD for `ApplicationEnvironment`
* An XR example
* An existing Composition
* Project structure under:

  * `apis/applicationenvironments/`
  * `examples/applicationenvironments/`

The goal is to review the entire project and implement the missing functionality so that creating an `ApplicationEnvironment` automatically:

1. Provisions/uses the required ArubaCloud infrastructure through the existing Crossplane composition.
2. Identifies the ArubaCloud `Cloudserver` managed resource associated with the environment.
3. Reads the VM public IP from the `Cloudserver.status.atProvider.publicIp`.
4. Retrieves an SSH private key from a dedicated Kubernetes Secret.
5. Connects to the VM over SSH as the `ubuntu` user.
6. Checks whether Docker is already installed.
7. Installs Docker if it is not installed.
8. Deploys/runs the Docker image specified by `ApplicationEnvironment.spec.image`.
9. Keeps all infrastructure/provider/SSH implementation details hidden from the user-facing `ApplicationEnvironment` API.

The user of the platform should only need to create an `ApplicationEnvironment` and specify the Docker image.

---

# Important Architectural Constraints

## 1. Crossplane version

This must be implemented as a **Crossplane v2 project**.

Do not redesign the project as a standalone Kubernetes operator.

Use Crossplane v2 APIs and Composition Functions appropriately.

---

## 2. ArubaCloud provider

The project depends on:

`xpkg.upbound.io/arubacloud/provider-arubacloud:v0.0.9`

Provider reference:

https://marketplace.upbound.io/providers/arubacloud/provider-arubacloud/v0.0.9

The relevant managed resource is:

```yaml
apiVersion: arubacloud.crossplane.io/v1alpha1
kind: Cloudserver
```

There is **no Container managed resource** available/desired.

Do not introduce a fake `Container` managed resource.

The application deployment must therefore happen through the custom Crossplane Composition Function and SSH into the Cloudserver.

---

# 3. User-facing API must remain simple

The public platform API is:

```yaml
apiVersion: <existing-group>/<existing-version>
kind: ApplicationEnvironment
metadata:
  name: example
spec:
  image: nginx:latest
```

The exact existing API group/version must be preserved from the repository.

The user-facing API should NOT require knowledge of:

* ArubaCloud
* `Cloudserver`
* VM names
* public IP addresses
* private IP addresses
* SSH
* SSH usernames
* SSH keys
* Kubernetes Secrets containing SSH keys
* Docker installation
* Docker commands
* ProviderConfig
* ArubaCloud project IDs
* VPCs
* subnets
* security groups
* volumes
* networking implementation

The `ApplicationEnvironment` is intended to be a **Platform API** hiding these implementation details.

---

# 4. Existing project must be reviewed first

Before modifying anything:

1. Inspect the complete repository.
2. Understand the existing Makefile.
3. Inspect all files under:

   * `apis/applicationenvironments/`
   * `examples/applicationenvironments/`
4. Inspect the existing XRD.
5. Inspect the existing XR example.
6. Inspect the existing Composition.
7. Inspect existing Composition Functions.
8. Inspect `project.yaml`, package configuration, dependencies, and generated artifacts.
9. Inspect tests, if present.
10. Determine what is already implemented and avoid rewriting working parts unnecessarily.

Do not blindly replace the existing implementation.

Prefer incremental changes consistent with the existing project's architecture and conventions.

---

# Existing ArubaCloud Cloudserver

The ArubaCloud provider creates a `Cloudserver` managed resource.

A real resource currently looks like:

```yaml
apiVersion: arubacloud.crossplane.io/v1alpha1
kind: Cloudserver
metadata:
  name: web-01
  namespace: default
spec:
  forProvider:
    location: ITBG-Bergamo
    name: web-01
    ...
  providerConfigRef:
    kind: ProviderConfig
    name: default
```

The important runtime information is available in:

```yaml
status:
  atProvider:
    publicIp: 176.107.147.90
    privateIp: 192.168.0.88
    name: web-01
```

For example:

```yaml
status:
  atProvider:
    id: 6a9fb747c0721d0042e867ed
    name: web-01
    privateIp: 192.168.0.88
    publicIp: 176.107.147.90
```

The Composition Function must use the **observed Cloudserver managed resource status** to obtain the public IP.

Do not attempt to discover the IP by calling the ArubaCloud API directly.

Do not duplicate the ArubaCloud API implementation.

Crossplane/provider-arubacloud is responsible for infrastructure provisioning and status observation.

---

# Cloudserver selection

The existing composition creates a Cloudserver named:

```text
web-01
```

The implementation must select the Cloudserver associated with the current `ApplicationEnvironment`.

The current expected implementation uses `web-01` as the Cloudserver identifier/filter.

However, do not make the design unnecessarily dependent on a globally unique hard-coded Kubernetes object if the existing composition already provides a better relationship between the XR and composed resource.

Prefer, in order:

1. Relationship/ownership through the current Composition.
2. Composition resource name/reference.
3. Existing naming convention.
4. `web-01` as the explicit current filter if that is how the existing project is designed.

The function must never accidentally use an unrelated Cloudserver belonging to another environment.

Because there can be multiple `ApplicationEnvironment` resources, the implementation must account for this.

---

# Composition Function

Implement the functionality as a dedicated **Crossplane Composition Function**.

The function is responsible for the application lifecycle/configuration after the infrastructure has become available.

Conceptually:

```text
ApplicationEnvironment
        |
        v
Crossplane Composition
        |
        +--> ArubaCloud Cloudserver
        |
        v
Application Deployment Function
        |
        +--> observe Cloudserver
        |
        +--> get publicIp
        |
        +--> get SSH private key
        |
        +--> SSH ubuntu@publicIp
        |
        +--> check Docker
        |
        +--> install Docker if required
        |
        +--> deploy requested image
```

Do not implement this as a separate Kubernetes controller unless there is a compelling technical reason demonstrated by the repository.

The preferred architecture is a Crossplane Composition Function.

---

# Function Responsibilities

The function should execute the following lifecycle.

## Step 1 — Observe the Cloudserver

Read the observed composed resources supplied by Crossplane.

Find the expected:

```yaml
apiVersion: arubacloud.crossplane.io/v1alpha1
kind: Cloudserver
```

resource.

Wait until it exists.

If the Cloudserver does not exist yet:

* do not attempt SSH
* do not report a successful deployment
* return an appropriate non-ready result
* allow Crossplane to reconcile again

---

# Step 2 — Wait for Cloudserver readiness

Before connecting:

* Verify that the Cloudserver has been successfully reconciled.
* Verify that the required `status.atProvider.publicIp` exists.
* Prefer checking the Cloudserver Ready condition as well.
* Do not SSH while the infrastructure is still being provisioned.

If:

```yaml
status.atProvider.publicIp
```

is missing, return a requeue/non-ready response rather than failing permanently.

The function should be idempotent and safe to execute repeatedly.

---

# Step 3 — Obtain the public IP

Extract:

```text
status.atProvider.publicIp
```

from the observed Cloudserver.

Example:

```text
176.107.147.90
```

Do not expose the public IP as a required field in `ApplicationEnvironment.spec`.

Do not require the user to provide the IP.

The IP is infrastructure state and must be discovered by the function.

---

# Step 4 — Obtain the SSH private key

The SSH private key must come from a **dedicated Kubernetes Secret**.

Do not embed the private key:

* in YAML
* in the Composition
* in Go source code
* in the Function image
* in the ApplicationEnvironment
* in ConfigMaps

The function must read the key from the dedicated Secret.

The implementation should make the Secret reference configurable through the Composition/function configuration rather than requiring the end user to specify it.

Use the existing project conventions if a Secret name/reference already exists.

If no convention exists, introduce a clear implementation-level configuration for the Secret.

The private key is sensitive and must never be:

* logged
* included in Crossplane status
* returned in Function responses
* written to normal Kubernetes resources unnecessarily
* printed during reconciliation

---

# SSH connection

Connect using:

```text
user: ubuntu
host: <Cloudserver status.atProvider.publicIp>
private key: <key from Kubernetes Secret>
```

Use a proper Go SSH implementation/library.

Do not shell out to the local `ssh` binary from the function container unless the existing project architecture explicitly requires it.

The function must run inside the Crossplane control-plane environment, so its implementation must account for:

* network access from the Function pod to the VM
* SSH TCP port 22
* host key verification strategy
* connection timeout
* command timeout
* retry behavior
* VM startup delay

---

# SSH Host Key Handling

Do not disable security checks by blindly using:

```go
ssh.InsecureIgnoreHostKey()
```

unless there is a documented reason and the project explicitly chooses that tradeoff.

Prefer a reasonable host-key strategy suitable for an automatically provisioned VM.

If the project does not currently have host-key management, design the implementation so this behavior is explicit and configurable rather than hidden.

Document the security implications.

---

# SSH Retry Behavior

A Cloudserver can become `Ready` before SSH is immediately available.

Therefore:

```text
Cloudserver Ready
        !=
SSH immediately available
```

The function must tolerate this.

Implement bounded retries/reconciliation behavior for cases such as:

* connection refused
* connection timeout
* VM still booting
* cloud-init still running
* SSH daemon not yet available

Do not create a tight retry loop inside a single Function invocation.

The function should perform a bounded connection attempt and allow Crossplane reconciliation to retry later.

Use reasonable timeouts.

---

# Step 5 — Check Docker

Once connected through SSH, determine whether Docker is installed.

For example, check using an appropriate command equivalent to:

```bash
command -v docker
```

or:

```bash
docker --version
```

Do not assume Docker is already installed.

---

# Step 6 — Install Docker if necessary

If Docker is not installed, install it on the Ubuntu VM.

The implementation must be suitable for the Ubuntu image used by the existing Cloudserver.

The installation must be:

* idempotent
* non-interactive
* safe to execute more than once
* tolerant of repeated reconciliation

After installation, verify that Docker is available.

If necessary, ensure the Docker service is running.

The implementation must not reinstall Docker on every reconciliation.

---

# Step 7 — Deploy the requested image

The requested image comes from:

```yaml
spec:
  image: <docker-image>
```

For example:

```yaml
spec:
  image: nginx:latest
```

The function must deploy that image to the VM using Docker.

The exact container runtime configuration should be designed so reconciliation is idempotent.

For example, the function should be able to determine whether the desired container is already running and avoid blindly starting duplicate containers.

Use a deterministic container name, such as:

```text
application
```

or another project-appropriate name.

The container name should be implementation-defined and must not need to be specified by the user.

---

# Container reconciliation

The function should reconcile the desired Docker state.

At minimum:

```text
Desired image = ApplicationEnvironment.spec.image
```

The VM should eventually run a container using that image.

Consider these cases:

### Container does not exist

Create and start it.

### Container exists and uses the desired image

Leave it running.

### Container exists but uses a different image

Update/recreate the container so it runs the requested image.

### Container exists but is stopped

Start/reconcile it.

### Docker is unavailable

Install/start Docker and retry later.

This must be idempotent.

---

# Image validation

Do not introduce unnecessary image validation in the Crossplane API unless required.

The function should treat `spec.image` as a Docker image reference.

If Docker reports that the image cannot be pulled or started:

* surface a meaningful failure condition/result
* do not silently report success
* do not hide the underlying deployment error

Avoid exposing sensitive SSH implementation details in the user-facing status.

---

# Status and readiness

The `ApplicationEnvironment` should have meaningful Crossplane conditions.

The desired lifecycle is approximately:

```text
ApplicationEnvironment created
        |
        v
Infrastructure provisioning
        |
        v
Cloudserver available
        |
        v
Public IP discovered
        |
        v
SSH connection established
        |
        v
Docker available
        |
        v
Image deployed
        |
        v
ApplicationEnvironment Ready=True
```

Do not report the `ApplicationEnvironment` as ready merely because the Cloudserver is ready.

The platform API should only become Ready once the requested application image has successfully been deployed.

If the infrastructure is ready but application deployment is still pending, keep the XR non-ready with a useful condition/message.

---

# Multiple ApplicationEnvironments

The design must consider that multiple `ApplicationEnvironment` objects may exist.

The function must not:

* connect to the wrong VM
* deploy an image belonging to another XR
* use a Cloudserver from another environment
* overwrite another environment's container

The relationship between:

```text
ApplicationEnvironment
        |
        v
Cloudserver
```

must be deterministic.

If the existing composition uses generated names, references, or owner references, use those rather than globally searching all Cloudservers by a fixed name.

If the current demo intentionally has one Cloudserver called `web-01`, preserve that behavior while still making the function architecture clean enough to evolve.

---

# Function Configuration

Do not expose implementation details through the ApplicationEnvironment API.

Configuration such as:

* SSH Secret name
* SSH username
* Cloudserver selection
* container name
* SSH port
* connection timeout

should be implementation-level configuration.

The end user should still only need:

```yaml
spec:
  image: ...
```

unless the existing XRD intentionally contains additional public parameters.

Do not unnecessarily expand the public API.

---

# Security Requirements

Treat SSH private keys as highly sensitive.

Requirements:

* Never log private keys.
* Never log Secret contents.
* Never put private keys into Function response objects.
* Never put private keys into XR status.
* Never include private keys in error messages.
* Avoid logging complete SSH commands if they could contain sensitive data.
* Do not store the private key in an ordinary ConfigMap.
* Do not hard-code credentials.
* Do not hard-code the private key.
* Use Kubernetes Secret access through the appropriate Crossplane/function mechanism.

Review RBAC/service-account permissions so the function can read only the Secret(s) it actually needs.

Avoid granting broad cluster-admin permissions.

---

# Provider Dependency

Ensure the Crossplane package/project declares the ArubaCloud provider dependency:

```text
xpkg.upbound.io/arubacloud/provider-arubacloud:v0.0.9
```

Do not replace it with another provider.

Do not create a custom replacement for the ArubaCloud Cloudserver provider.

---

# Generated Resources

After implementation, inspect whether the project requires regeneration of:

* generated Go code
* CRDs
* package metadata
* Crossplane package artifacts
* dependency locks
* function package metadata

Use the project's existing Makefile/generation workflow rather than inventing a parallel build process.

Do not manually edit generated files if they are produced by the repository's generation commands.

---

# Examples

Update:

```text
examples/applicationenvironments/
```

so that the example demonstrates the intended developer experience.

The example should be as simple as possible.

For example:

```yaml
apiVersion: <existing-group>/<existing-version>
kind: ApplicationEnvironment
metadata:
  name: example
spec:
  image: nginx:latest
```

Do not require the example user to specify:

* Cloudserver
* IP
* SSH key
* username
* Docker
* ProviderConfig
* ArubaCloud project
* VM details

unless those are already explicitly part of the existing public API.

---

# Documentation

Update the project documentation to explain the architecture.

Document:

1. What `ApplicationEnvironment` represents.
2. What the user needs to provide.
3. How the Composition provisions the infrastructure.
4. How the Composition Function discovers the Cloudserver.
5. How the function obtains `publicIp`.
6. How SSH authentication works.
7. Where the SSH private key is stored.
8. How Docker is installed.
9. How the requested image is deployed.
10. How reconciliation behaves.
11. What happens when the VM is not ready.
12. Security considerations.
13. Required RBAC.
14. How to configure the SSH Secret.
15. How to run the example.

The documentation should clearly communicate that the Cloudserver and SSH implementation are internal platform details.

---

# Testing Requirements

Add meaningful tests for the Composition Function.

At minimum, cover:

## Test 1 — Cloudserver unavailable

Expected:

* no SSH attempt
* non-ready/pending result
* reconciliation can continue later

## Test 2 — Cloudserver exists but public IP is missing

Expected:

* no SSH attempt
* non-ready/pending result

## Test 3 — Cloudserver is ready and public IP exists

Expected:

* function attempts to establish SSH connection

Use dependency injection/interfaces so SSH behavior can be mocked in unit tests.

Do not make unit tests depend on a real ArubaCloud VM.

---

## Test 4 — Docker already installed

Expected:

* Docker installation is skipped
* deployment proceeds

## Test 5 — Docker missing

Expected:

* Docker installation command is executed
* Docker availability is verified
* deployment proceeds

## Test 6 — Desired container does not exist

Expected:

* container is created and started

## Test 7 — Desired container already runs requested image

Expected:

* no unnecessary recreation

## Test 8 — Container runs a different image

Expected:

* old container is reconciled/replaced with desired image

## Test 9 — SSH failure

Expected:

* no false Ready state
* meaningful retryable result
* no secret leakage

## Test 10 — Docker/image deployment failure

Expected:

* `ApplicationEnvironment` remains non-ready
* useful failure information is surfaced
* no false success

---

# Idempotency

This is a critical requirement.

Crossplane Composition Functions are called repeatedly.

The function must be safe to execute many times.

Repeated reconciliation must NOT:

* reinstall Docker unnecessarily
* create duplicate containers
* create duplicate resources
* continuously restart the application
* continuously pull the image
* leak credentials
* produce uncontrolled side effects

The function should converge toward:

```text
VM
 └── Docker
      └── application container
           └── desired image
```

---

# Error Handling

Classify errors appropriately.

Retryable conditions include:

* Cloudserver not yet available
* public IP not assigned
* SSH temporarily unavailable
* VM still booting
* Docker installation temporarily unavailable

Non-retryable/application errors may include:

* malformed image reference
* invalid configuration
* authentication failure due to an invalid SSH key
* permission denied
* image cannot be pulled

Errors should be surfaced through Crossplane conditions/results in a useful but secure way.

Never expose private key contents.

---

# Implementation Quality

Follow idiomatic Go and the conventions already used in the repository.

Use:

* clear interfaces
* dependency injection where external operations are involved
* structured error handling
* context-aware operations
* timeouts
* unit-testable components

Avoid:

* huge monolithic functions
* shelling out to arbitrary binaries without justification
* global mutable state
* hard-coded credentials
* hard-coded secrets
* unnecessary API changes
* unnecessary new controllers
* provider-specific API calls outside the provider managed resource

---

# Important Design Principle

The architecture should preserve this separation of responsibilities:

```text
                 ApplicationEnvironment
                         |
                  Platform API
                         |
                         v
                  Crossplane XRD/XR
                         |
                         v
                    Composition
                    /          \
                   /            \
                  v              v
        ArubaCloud Cloudserver   Application
              MR                    |
              |                     |
              v                     v
      ArubaCloud Provider      Composition Function
                                    |
                                    v
                              SSH to Cloudserver
                                    |
                                    v
                                  Docker
                                    |
                                    v
                            requested image
```

The ArubaCloud provider owns infrastructure provisioning.

The Composition owns infrastructure composition.

The Composition Function owns VM-level application configuration/deployment.

The `ApplicationEnvironment` API hides all of these implementation details.

---

# Definition of Done

The task is complete only when all of the following are true:

* [ ] Existing repository structure has been reviewed.
* [ ] Existing XRD is preserved and improved only where necessary.
* [ ] Existing ApplicationEnvironment XR example works.
* [ ] Existing Composition works with Crossplane v2.
* [ ] ArubaCloud provider v0.0.9 is declared as a dependency.
* [ ] No Container managed resource is introduced.
* [ ] A dedicated Composition Function implements VM configuration.
* [ ] Function discovers the composed ArubaCloud Cloudserver.
* [ ] Function obtains `status.atProvider.publicIp`.
* [ ] Function waits for the VM to become usable.
* [ ] Function obtains SSH private key from a dedicated Kubernetes Secret.
* [ ] Function connects as `ubuntu`.
* [ ] Function does not expose SSH implementation details through the public API.
* [ ] Function checks whether Docker is installed.
* [ ] Function installs Docker when necessary.
* [ ] Function verifies Docker availability.
* [ ] Function deploys `spec.image`.
* [ ] Deployment is idempotent.
* [ ] Image changes are reconciled.
* [ ] SSH failures are handled correctly.
* [ ] Docker failures are handled correctly.
* [ ] ApplicationEnvironment does not become Ready prematurely.
* [ ] Secrets are never logged or exposed.
* [ ] RBAC is least-privilege.
* [ ] Unit tests cover the important reconciliation paths.
* [ ] Example manifests are updated.
* [ ] Documentation is updated.
* [ ] Code generation/package generation is executed where required.
* [ ] The complete project builds successfully.
* [ ] Tests pass.
* [ ] Package validation passes.
* [ ] The resulting project can be installed into a Crossplane v2 control plane.

---

# Final Agent Instructions

Do not stop after implementing the Function.

Review the **entire project end-to-end** and make all changes required for the project to actually work.

At the end, provide:

1. A summary of the architecture.
2. A list of files changed.
3. A description of the Composition Function implementation.
4. How the Cloudserver is identified.
5. How `publicIp` is obtained.
6. How the SSH Secret is configured.
7. How Docker installation works.
8. How the Docker image is deployed.
9. How idempotency is achieved.
10. Tests added/updated.
11. Commands used to generate/build/test the project.
12. Any remaining limitations or assumptions.

Do not merely provide theoretical YAML or pseudocode.

Implement the changes in the repository and leave the project in a buildable/testable state.
