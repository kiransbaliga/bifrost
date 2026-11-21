# ADR-002: Container Runtime & Kiri Engine Integration

## Status
Accepted

## Date
2026-11-22

## Context
Standard container runtimes create massive memory overhead on low-spec hardware:
- Traditional Docker (`dockerd` + `containerd` + `containerd-shim` per container) consumes 120MB–200MB+ idle RAM.
- In low-resource environments (1GB RAM VPS), this overhead is unacceptable.

**Kiri** is a modern, lightweight container runtime written in Rust that bypasses `containerd` and `runc` in favor of direct Linux kernel primitives (`clone3`, cgroups v2, namespaces). Kiri's idle daemon consumes only **10–15MB of RAM** and implements the standard Docker Engine HTTP API over a Unix domain socket (`/var/run/kiri.sock` or `/var/run/docker.sock`).

We need Bifrost to control containers efficiently without inflating binary size or memory overhead.

## Decision
1. **Lightweight Unix Socket Client (No Bloated SDKs)**:
   Avoid importing the official Docker Go SDK (`github.com/docker/docker`), which brings in dozens of heavy transitive dependencies, expands binary size by 15MB+, and adds unnecessary memory structures. Instead, implement a lean, specialized HTTP client communicating over Unix domain sockets (`net.Dial("unix", socketPath)`).
2. **Kiri-First with Fallback Auto-Detection**:
   Bifrost checks for sockets in the following order:
   - `/var/run/kiri.sock` (Lightweight Kiri daemon — recommended)
   - `/var/run/docker.sock` (Standard Docker daemon if already installed)
   - `/run/podman/podman.sock` (Podman rootless/system socket)
3. **Container Resource Constraints**:
   Every container spawned by Bifrost must have hard cgroup limits injected:
   - `Memory`: Explicit user-defined or default limit (e.g. 256MB).
   - `MemoryReservation`: Soft target (e.g. 192MB).
   - `OomKillDisable`: Set to `false` to guarantee the control plane host remains alive.
   - `RestartPolicy`: `unless-stopped` with exponential backoff to avoid crash-loop thrashing.

## Alternatives Considered

### Direct `runc` or `crun` execution via CLI fork/exec
- **Pros**: Zero background daemon running when idle.
- **Cons**: Managing OCI bundle states, root filesystems, networking, and layer caching from scratch adds immense complexity, fragility, and operational debt.
- **Rejected**: Kiri already provides native Linux performance, OCI compatibility, and an ultra-lean 10MB daemon implementing the standard Docker API.

### Full Docker Engine SDK
- **Pros**: Official API coverage.
- **Cons**: Massive dependency tree (gRPC, OpenTelemetry, containerd dependencies) bloating the Go heap and binary.
- **Rejected**: A clean 400-line Go client using `net/http` over a Unix socket provides 100% of the needed functionality (`ContainerCreate`, `ContainerStart`, `ContainerStop`, `ContainerInspect`, `ContainerLogs`, `ContainerStats`) with zero dependencies and sub-millisecond dispatch.

## Consequences
- **Positive**: Enables running on 1GB VPS with ~15MB runtime daemon overhead instead of 150MB+.
- **Positive**: Drop-in compatible with standard Dockerfiles and OCI images.
- **Positive**: Zero vendor lock-in: if a user prefers standard Docker or Podman, Bifrost connects to their existing `/var/run/docker.sock` transparently.
