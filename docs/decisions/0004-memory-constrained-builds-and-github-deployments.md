# ADR-004: Memory-Constrained Build Pipeline & GitHub Deployments

## Status
Accepted

## Date
2026-11-22

## Context
Building modern JavaScript applications (`npm install`, Vite, Next.js, Rollup, webpack) is notoriously memory-intensive. A single `npm run build` commonly spikes heap usage between 600MB and 1.5GB.

On a 1GB RAM VPS, running an unconstrained local build while user applications are active causes immediate system thrashing, triggering the Linux OOM killer to terminate database processes, the application, or the server itself.

We must support deployment from GitHub while ensuring builds never crash a 1GB server.

## Decision
Implement a **Hybrid Build Pipeline** prioritizing remote artifact deployment with a strictly guarded local fallback:

### 1. Remote Build via GitHub Actions (Zero Server RAM — Recommended)
- Provide an official GitHub Action (`bifrost-deploy-action`) and workflow snippet.
- The build (`npm install && npm run build`) executes on GitHub's free 7GB RAM hosted runners.
- GitHub Actions packages the production output (`dist/` or standalone Node server artifact) into a compressed tarball and streams it directly to Bifrost's authenticated endpoint:
  ```
  POST /api/v1/projects/:id/deploy/artifact
  Authorization: Bearer <DEPLOY_TOKEN>
  ```
- **Server Memory Impact**: < 5MB RAM. The server only performs streaming SHA-256 verification and decompression directly to disk. No Node.js compilation occurs on the VPS.

### 2. In-Situ Capped Local Build (Direct Git Webhook Fallback)
For repositories configured with direct GitHub webhook push triggers:
- **Global Build Semaphore**: Concurrency is strictly clamped to `1` (`chan struct{}` of size 1). Concurrent build requests are placed into a FIFO queue.
- **Node.js Memory Capping**: All Node/JS builds run with:
  ```bash
  NODE_OPTIONS="--max-old-space-size=384"
  NPM_CONFIG_JOBS=1
  UV_THREADPOOL_SIZE=2
  ```
  This forces the V8 garbage collector to collect heap aggressively at 384MB rather than expanding until the OS crashes.
- **Host Swap Guard**:
  At startup and before every local build, Bifrost inspects host memory and `/proc/swaps`. If total swap is below 1.5GB on a host with $\le 1$GB RAM, a warning is raised and the installer script automatically provisions a 2GB NVMe swapfile.
- **Cgroup Enforced Build Container**:
  Local builds are executed inside an ephemeral container bounded to `Memory=512M` and `MemorySwap=1536M`. If an unforeseen memory leak occurs, only the build container is killed by cgroups, preserving host stability and existing running applications.

## Alternatives Considered

### Build Everything Locally inside Docker
- **Rejected**: Causes frequent OOM crashes on 1GB VPS. Single-threaded Vite builds still easily cross 700MB.

### Nixpacks / Cloud Native Buildpacks (Local)
- **Pros**: Automatic language detection.
- **Cons**: `pack` and Nixpacks CLI are heavy binaries (50MB–100MB) with high memory overhead during plan generation and layer creation.
- **Adopted with constraints**: Supported for local builds when swap is present, but GitHub Actions artifact push remains the primary recommended path for low-memory hosts.

## Consequences
- **Positive**: 100% immune to build OOM crashes when using GitHub Actions artifact push.
- **Positive**: 1GB VPS can easily host 4–8 active web apps because zero memory is reserved for build spikes.
- **Positive**: Full GitHub Webhook support for automated continuous deployment (`git push main`).
