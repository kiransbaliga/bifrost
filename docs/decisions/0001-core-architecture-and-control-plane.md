# ADR-001: Core Architecture & Single-Binary Control Plane

## Status
Accepted

## Date
2026-11-22

## Context
Traditional self-hosted PaaS platforms like Coolify, Dokku, and Portainer were designed for servers with 4GB–16GB+ of RAM. On a 1GB RAM VPS (such as the popular Oracle Cloud Always Free tier), standard Coolify fails to run reliably because its control plane comprises:
- PHP-FPM / Laravel API server (~250MB RAM)
- PostgreSQL database engine (~100MB RAM)
- Redis / Laravel Horizon queue (~60MB RAM)
- External Traefik or Caddy reverse proxy (~60MB RAM)
- Docker daemon and containerd shims (~120–200MB RAM)

This total baseline of 590MB–700MB+ leaves fewer than 200MB of RAM for actual user workloads. Furthermore, running any `npm build` or `docker build` immediately triggers the Linux Out-Of-Memory (OOM) killer, taking down the entire VPS.

We need an ultra-lightweight, commercial-grade PaaS orchestrator (named **Bifrost**) written in Go that can operate with an idle memory ceiling under **25MB RAM**, leaving 800MB+ of RAM available for applications on a 1GB host.

## Decision
1. **Single Static Binary in Go**:
   Compile the entire control plane—including the Web UI, REST API, Webhook receiver, embedded database, reverse proxy, and deployment runner—into a single Go binary with `CGO_ENABLED=0`.
2. **Eliminate External Daemons**:
   - No external Redis: Use in-process Go channels and worker pools.
   - No external PostgreSQL: Use embedded pure-Go SQLite with strictly bounded memory buffers.
   - No external Traefik/Caddy: Use Go's native `net/http/httputil.ReverseProxy` with Let's Encrypt `autocert`.
3. **Go Runtime Memory Tuning**:
   Configure the Go runtime memory limiter explicitly:
   - `debug.SetMemoryLimit(32 * 1024 * 1024)` (32MB soft limit)
   - `debug.SetGCPercent(50)` (ensures rapid GC cycles before heap fragmentation occurs)
   - Use pooled byte buffers (`sync.Pool`) for HTTP reverse-proxy copying to prevent heap allocations.

## Alternatives Considered

### Python / FastAPI or Node.js / Fastify Control Plane
- **Pros**: Fast prototyping, rich web ecosystem.
- **Cons**: V8/Node.js base idle memory is 40–80MB; Python with dependencies is 50–90MB. Neither compiles to a single zero-dependency static binary without significant runtime packaging overhead.
- **Rejected**: Go offers sub-15MB idle footprint, predictable GC via `GOMEMLIMIT`, and native high-performance networking primitives.

### Rust Control Plane
- **Pros**: Zero GC pauses, minimal memory footprint (5–10MB).
- **Cons**: Slower development velocity for commercial features (webhooks, templating, ACME TLS, DB migrations), smaller standard web/PaaS library ecosystem compared to Go.
- **Rejected**: Go hits the optimal sweet spot between developer velocity, maintainability, and sub-25MB memory efficiency.

## Consequences
- **Positive**: Total idle control plane memory is reduced from ~600MB down to ~20MB.
- **Positive**: Zero external database or cache installation required; single-command startup.
- **Positive**: Trivially easy distribution as a single static binary for Linux `amd64` and `arm64`.
- **Negative**: High-load multi-node clustering is intentionally out of scope for v1 (focused on single-node edge/VPS optimization).
