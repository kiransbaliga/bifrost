# Bifrost Architecture Specification

> **Ultra-Lightweight, Single-Binary PaaS for Resource-Constrained Clouds (1GB RAM Targets)**  
> Part of the Valkyrie suite of minimalist cloud tools.

---

## 1. Executive Summary

Bifrost is an open-source, production-grade PaaS (Platform as a Service) written in Go. It provides a Coolify-like self-hosted experience—automated GitHub deployments, reverse proxying, Let's Encrypt SSL/TLS, environment variable management, and multi-project routing—specifically engineered to run reliably on resource-constrained servers with **1GB of RAM** (e.g. Oracle Cloud Always Free tier, Hetzner CX22, DigitalOcean $4 droplets).

### The 1GB RAM Problem & Memory Breakdown

| Component | Standard Coolify Stack | Bifrost Stack | Memory Saved |
| :--- | :--- | :--- | :--- |
| **Control Plane API** | Laravel / PHP-FPM (~250 MB) | Single Go Binary (~15 MB) | **-235 MB** |
| **Database** | PostgreSQL (~100 MB) | Embedded SQLite WAL (~2 MB) | **-98 MB** |
| **Job Queue & Cache** | Redis / Horizon (~60 MB) | In-Process Go Channels (~1 MB) | **-59 MB** |
| **Reverse Proxy & SSL**| Traefik / Caddy (~60 MB) | In-Process Autocert Proxy (~3 MB) | **-57 MB** |
| **Container Engine** | Docker + containerd (~150 MB) | Kiri (~15 MB) or Docker socket | **-135 MB** |
| **Static Frontends** | Node / Nginx containers (20-80 MB each) | In-Process Zero-Copy FileServer (0 MB) | **-80 MB per site** |
| **Total Control Plane**| **~620 MB** (Host OOM risk: Extreme) | **~36 MB** (Host OOM risk: Negligible) | **~584 MB free RAM for apps** |

---

## 2. High-Level Architecture Diagram

```
                 Internet Traffic (:80 / :443)
                             │
                             ▼
 ┌───────────────────────────────────────────────────────────────┐
 │                   BIFROST CONTROL PLANE                       │
 │                   (Single Static Binary)                      │
 │                                                               │
 │   ┌───────────────────────────────────────────────────────┐   │
 │   │               IN-PROCESS REVERSE PROXY                │   │
 │   │  - Let's Encrypt TLS (autocert) on :443               │   │
 │   │  - Lock-free atomic routing table (zero lock waits)   │   │
 │   │  - sync.Pool 32KB buffer pooling (zero GC spike)      │   │
 │   │  - WebSocket & SSE streaming support                  │   │
 │   └──────────┬─────────────────────────────┬──────────────┘   │
 │              │                             │                  │
 │              │ (Dynamic App Traffic)       │ (Static Bypass)  │
 │              ▼                             ▼                  │
 │   ┌──────────────────────┐   ┌────────────────────────────┐   │
 │   │ Container Driver     │   │ Static File Engine         │   │
 │   │ (HTTP / Unix Socket) │   │ (Zero-Container Hosting)   │   │
 │   └──────────┬───────────┘   │ - Direct sendfile(2)       │   │
 │              │               │ - SPA index.html fallback  │   │
 │              │               │ - Brotli & Gzip streaming  │   │
 │              │               └─────────────┬──────────────┘   │
 │              │                             │                  │
 │   ┌──────────┴───────────┐                 │                  │
 │   │ Build Orchestrator   │                 │                  │
 │   │ - GitHub Artifact API│                 │                  │
 │   │ - Capped Git Queue   │                 │                  │
 │   │ - V8 max-old-space   │                 │                  │
 │   └──────────┬───────────┘                 │                  │
 │              │                             │                  │
 │   ┌──────────┴───────────┐                 │                  │
 │   │ Embedded SQLite DB   │                 │                  │
 │   │ (WAL, 2MB Cache Cap) │                 │                  │
 │   └──────────────────────┘                 │                  │
 └──────────────────────┬─────────────────────┼──────────────────┘
                        │                     │
      ┌─────────────────┴─────────┐           │
      │ Unix Socket               │           │
      ▼                           │           │
┌──────────────┐                  │           │
│ Kiri Engine  │ (Rust daemon)    │           │
│ (~15MB RAM)  │                  │           │
└──────┬───────┘                  │           │
       │                          │           │
       ▼                          ▼           ▼
┌──────────────────┐    ┌──────────────────┐ ┌──────────────────┐
│ App 1 (Dynamic)  │    │ App 2 (Dynamic)  │ │ Static Site 3    │
│ Node/Bun/Go/etc. │    │ Python/Rust/etc. │ │ (Direct Disk)    │
│ cgroup: 256MB    │    │ cgroup: 256MB    │ │ RAM: 0 MB        │
└──────────────────┘    └──────────────────┘ └──────────────────┘
```

---

## 3. Core Architectural Subsystems

### 3.1 In-Process Reverse Proxy & TLS (`internal/proxy`)
- **No External Proxy Container**: External Traefik or Caddy containers introduce redundant TCP hops and consume 40MB–80MB of memory.
- **Lock-Free Routing**: Routes are stored in an `atomic.Pointer[map[string]*RouteTarget]`. Route lookups on hot request paths are entirely lock-free.
- **Buffer Recycling**: Proxy copy buffers are acquired from a `sync.Pool` of fixed 32KB slices, preventing high-concurrency requests from triggering GC pressure.
- **Zero-Config TLS**: Integrates with `golang.org/x/crypto/acme/autocert` for automatic Let's Encrypt certificate issuance, renewal, and on-disk caching.

### 3.2 Container Runtime Driver (`internal/runtime`)
- **Unix Socket Protocol**: Communicates with container engines over standard Unix domain sockets (`/var/run/kiri.sock` or `/var/run/docker.sock`).
- **Zero SDK Bloat**: Avoids the massive official Docker Go SDK. Uses a lightweight, specialized HTTP client with sub-1ms request overhead.
- **Strict Cgroup Injection**: Automatically enforces memory ceilings (`Memory`, `MemoryReservation`, `MemorySwap`) and CPU quotas on every managed container.

### 3.3 Hybrid Build Pipeline (`internal/builder`)
- **Priority 1: GitHub Actions Remote Build (0MB Server RAM)**:
  - Developer triggers deployment via `git push`.
  - GitHub Actions runs `npm install && npm run build` on GitHub's free 7GB runners.
  - The workflow packages the build output and streams it as a compressed tarball to Bifrost (`POST /api/v1/projects/:id/deploy/artifact`).
  - Server extracts the artifact in <1 second with <5MB RAM.
- **Priority 2: Capped In-Situ Build (Direct Git Webhook)**:
  - Global FIFO semaphore limits builds to **1 concurrent execution** across the entire host.
  - Node.js memory ceiling: `NODE_OPTIONS="--max-old-space-size=384"` and `NPM_CONFIG_JOBS=1`.
  - Ephemeral build containers with memory caps protect host applications from OOM kills.

### 3.4 Static Site Direct Serve (`internal/runtime/static`)
- For Single Page Applications (React, Vue, Vite, Astro, Svelte):
  - No container is created.
  - Files are served directly from disk via Go's `http.FileServer`.
  - Single-page app fallback routes unknown paths to `index.html`.
  - 0MB container memory consumed.

### 3.5 Embedded Storage (`internal/store`)
- Pure Go SQLite (`modernc.org/sqlite`) compiled without CGO.
- Tuned with aggressive low-memory pragmas:
  - `cache_size = -2000` (limits page cache to 2MB).
  - `journal_mode = WAL` (concurrent read-during-write).
  - `mmap_size = 0` (no virtual memory bloating).
  - `temp_store = FILE` (temporary sort operations kept off RAM).

### 3.6 System Guard & OOM Protection (`internal/sysguard`)
- Inspects `/proc/meminfo` and `/proc/swaps`.
- Verifies at least 2GB of Swap exists before executing local builds.
- Automatically adjusts Go runtime memory parameters:
  - `debug.SetMemoryLimit(32 * 1024 * 1024)`
  - `debug.SetGCPercent(50)`

---

## 4. Security & Isolation Model

1. **Authentication**:
   - Bearer token authentication for REST APIs.
   - Per-project deploy tokens for CI/CD artifact push.
   - HMAC SHA-256 signature verification (`X-Hub-Signature-256`) for GitHub webhook endpoints.
2. **Container Sandboxing**:
   - Containers run as non-root where possible.
   - Read-only root filesystems supported with explicit write volumes.
   - Unprivileged cgroups limits prevent noisy-neighbor memory exhaustion.
3. **Network Security**:
   - Container ports are bound only to `127.0.0.1` or internal bridge networks—never exposed directly to public network interfaces.
   - All external access terminates at Bifrost's in-process TLS proxy.
