# ADR-006: Static Site Bypass & Process Optimization

## Status
Accepted

## Date
2026-11-22

## Context
A substantial percentage of frontend JavaScript projects (React, Vue, SvelteKit, Vite, Astro, Next.js static export) produce pure static bundles (`HTML`, `CSS`, `JS`, images).

In standard PaaS engines (Coolify, Render, Heroku), deploying a static site frequently boots an entire Docker container running Nginx, Caddy, or `serve` (Node.js). A single Node.js static server consumes 50MB–100MB of RAM; an Nginx container consumes 15MB–25MB plus container shim overhead.

On a 1GB server hosting multiple personal or commercial client sites, running containers for static assets wastefully squanders scarce memory.

## Decision
1. **Static Direct-Serve Mode (Zero-Container Static Hosting)**:
   When a project is designated as `static` (or builds into a standard `dist/` or `out/` directory):
   - **Do not spin up a container.**
   - Register an in-process static route within Bifrost's routing engine pointing directly to `/var/lib/bifrost/projects/:id/releases/:release_id/dist`.
   - Serve static assets using Go's optimized `http.FileServer` with:
     - Automatic gzip & Brotli compression.
     - Single-Page Application (SPA) fallback routing (rewriting non-asset 404s to `index.html`).
     - Aggressive asset caching (`Cache-Control: public, max-age=31536000, immutable` for hashed files).
2. **Memory Impact**:
   - **0MB container daemon overhead**.
   - **0MB container runtime memory**.
   - Zero-copy file transmission via Linux `sendfile(2)`.
   - On a 1GB server, this allows hosting **dozens of static frontend apps alongside dynamic backends** with zero memory penalty.

## Alternatives Considered

### Spin up a minimal Alpine Nginx container per static site
- **Pros**: Isolated process per project.
- **Cons**: Still consumes ~15MB RAM per site. 10 static sites = 150MB lost on a 1GB VPS.
- **Rejected**: Serving static files is a native capability of Go's HTTP engine; adding an extra container layer is needless bloat.

## Consequences
- **Positive**: Near-infinite scalability for static sites on a low-end VPS.
- **Positive**: Sub-millisecond Time-To-First-Byte (TTFB) bypassing internal Docker bridge virtual interfaces (`veth`).
- **Positive**: Seamless SPA routing support built into project settings.
