# ADR-003: In-Process Reverse Proxy & Automatic TLS

## Status
Accepted

## Date
2026-11-22

## Context
A self-hosted PaaS must route incoming HTTP and HTTPS requests from public domains (e.g. `api.example.com`, `app.example.com`) to internal project ports and automatically manage SSL/TLS certificates.

Conventional PaaS stacks deploy Traefik, Caddy, or Nginx as a secondary container. On a 1GB server, running a separate proxy container consumes 40MB–80MB of RAM, adds inter-process networking hops, and requires complex container label polling.

Go's standard library provides a production-tested, high-throughput `net/http/httputil.ReverseProxy` and official ACME TLS automation (`golang.org/x/crypto/acme/autocert`).

## Decision
1. **In-Process Reverse Proxy**:
   Embed the reverse proxy directly inside the Bifrost process.
   - Eliminates 40MB–80MB of external proxy memory.
   - Eliminates IPC/socket latency between proxy and orchestrator.
2. **Lock-Free Routing with Atomic Pointer**:
   Route lookups occur on every incoming HTTP request. To eliminate mutex lock contention under concurrent load, store the active routing table in an `atomic.Pointer[RoutingTable]`. When routes are added or removed, create a copy-on-write routing map and swap the pointer atomically.
3. **Zero-Allocation Buffer Pooling**:
   Provide a custom `BufferPool` to `httputil.ReverseProxy` using `sync.Pool` with fixed 32KB byte buffers. This stops the Go GC from thrashing during high-volume data transfers.
4. **Embedded Let's Encrypt Autocert**:
   Use `autocert.Manager` bound to ports `80` (HTTP-01 challenge) and `443` (TLS-ALPN-01 / HTTPS). Cache issued certificates to a persistent directory on disk (`/var/lib/bifrost/certs`).
5. **Full WebSocket & Streaming Support**:
   Enable streaming reverse proxying out-of-the-box (`FlushInterval: -1`) to support Server-Sent Events (SSE), WebSockets, and chunked transfer encoding natively.

## Alternatives Considered

### Standalone Caddy Container
- **Pros**: Automatic HTTPS out of the box, mature configuration.
- **Cons**: 40MB–60MB RAM footprint; requires dynamic API integration (Caddy admin API) or reloading Caddyfiles.
- **Rejected**: Redundant overhead when Go's standard library already provides all necessary primitives in-process.

### Nginx with Certbot Cron
- **Pros**: Very fast, classic Linux architecture.
- **Cons**: Requires template generation, process signaling (`nginx -s reload`), external cron jobs, and Python/Certbot dependencies.
- **Rejected**: Violates the single-binary zero-dependency goal.

## Consequences
- **Positive**: 0MB additional process memory for reverse proxying.
- **Positive**: Instant, sub-millisecond route reconfiguration upon deployment completion without reloading configuration files.
- **Positive**: Automatic Let's Encrypt issuance for all mapped domains.
- **Negative**: Port 80 and 443 must be available on the host machine for Bifrost to bind directly.
