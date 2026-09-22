# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

---

## [0.1.0] - 2026-09-22

### Added

#### Platform APIs

- **`ApplicationEnvironment`** XRD (`platform.example.com/v1alpha1`) — single-object API that provisions an ArubaCloud VM and deploys a Docker container onto it via SSH. Exposes `spec.image`, `spec.port`, and `status.endpoint`.
- **`Microservice`** XRD (`platform.example.com/v1alpha1`) — extends `ApplicationEnvironment` with a managed MySQL 8.0 DBaaS cluster. Injects connection environment variables into the container at startup and writes credentials to a Kubernetes Secret via `writeConnectionSecretToRef`. Exposes `status.databaseHost`, `status.databasePort`, `status.databaseName`, `status.databaseUser`.

#### Composition Function

- **`function-appenv-deployer`** — custom Go-based Crossplane composition function that handles the full application deployment lifecycle:
  - SSH connectivity check and Docker installation on the target VM
  - Container inspection and recreation when network mode changes
  - Non-blocking Docker installation to avoid Crossplane pipeline deadline timeouts
  - Short-circuit logic when a container is already deployed and healthy
  - Skip deployment when the XR has a `deletionTimestamp` set
  - `sanitizeError` helper to expose full Docker error output in XR conditions
  - Publishes `status.endpoint` as `http://<publicIp>:<port>` once the container is running

#### Infrastructure Compositions

- **`ApplicationEnvironment` Composition** — orchestrates VPC, Subnet, SecurityGroup, VM, and SSH key resources via `provider-arubacloud`. Waits for VM readiness before invoking the deployer function.
- **`Microservice` Composition** — adds DBaaS cluster provisioning (mysql-8.0 engine, DBO2A4 flavor, `liteadmin` role for grant). Waits for DBaaS `Ready` condition before starting the container. Propagates VPC/Subnet URIs through XR status to work around ref resolution limitations.

#### CI/CD

- GitHub Actions workflow for **`function-appenv-deployer`**: runs Go tests with race detection, builds a Crossplane `xpkg`, and pushes to `ghcr.io/arubacloud/function-appenv-deployer` on push to `main` or `v*` tags.
- GitHub Actions workflow for the **Configuration package**: builds the full Crossplane project (XRDs + Compositions + embedded function) and pushes to `ghcr.io/arubacloud/containerday-2026` on push to `main` or `v*` tags.
- Dependency cache keyed on `schemas/.lock.json` to speed up CI runs.

#### Dependencies

- `provider-arubacloud` v0.0.10 — ArubaCloud infrastructure resources (VM, VPC, Subnet, SecurityGroup, DBaaS, SSH key)
- `function-auto-ready` v0.2.1 — automatic `Ready` condition management
- `function-patch-and-transform` v0.8.0 — field patching and transformation
- `function-extra-resources` v0.3.0 — SSH secret retrieval from the pipeline context

#### Documentation

- Full README with platform API reference, architecture overview, installation guides (Option A: Configuration package; Option B: manual), usage examples for both APIs, and a deep-dive into `function-appenv-deployer`.
- Distribution guide covering build, push, install, and versioning of the Configuration package.

---

[0.1.0]: https://github.com/Arubacloud/containerday-2026/releases/tag/v0.1.0
