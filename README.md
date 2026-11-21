# Bifrost

> **Minimalist, single-binary cloud orchestrator and edge reverse proxy.**  
> Built in Go with embedded SQLite and automated Let's Encrypt TLS.

---

## Overview

Bifrost is an open-source, self-hosted deployment engine designed to provide an ultra-clean, low-overhead alternative to heavy PaaS stacks like Coolify. While engineered to excel on resource-constrained hardware (such as 1GB RAM clouds), it is a generic, production-ready platform that scales gracefully to any multi-core host.

### Core Philosophy

1. **Single Static Binary**: The web dashboard, REST API, GitHub webhook receivers, embedded SQLite database, build orchestrator, and ACME TLS reverse proxy all live in one static binary with zero external daemons.
2. **Real Kernel Telemetry**: Live host telemetry (RAM, Swap, CPU, Disk, and exact process RSS memory) queried directly via OS primitives (`sysctl`, `/proc`, and `statfs`).
3. **Hybrid Deployment Engine**:
   - **Remote Artifact Deploy (0MB Server Build RAM)**: Build applications in GitHub Actions using free hosted runners; Bifrost streams and extracts the production artifact directly.
   - **Local Capped Builds**: Enforces a strict single-concurrency queue and V8 memory limits (`NODE_OPTIONS=--max-old-space-size=384`) to guard against host Out-Of-Memory (OOM) conditions.
4. **Static Direct-Serve Bypass**: Static Single Page Applications (React, Vite, Vue, Astro, Svelte) are served directly via Go's in-process `http.FileServer` with automatic gzip compression and SPA routing fallback, eliminating container daemon overhead entirely.
5. **Kiri & Docker Compatibility**: Communicates natively over Unix domain sockets (`/var/run/kiri.sock` or `/var/run/docker.sock`) using an internal lightweight HTTP client without bloated third-party SDKs.

---

## System Architecture

```
                  Internet Ingress (:80 / :443)
                                │
                                ▼
 ┌─────────────────────────────────────────────────────────────┐
 │                      BIFROST PROCESS                        │
 │                                                             │
 │   ┌─────────────────────────────────────────────────────┐   │
 │   │              IN-PROCESS REVERSE PROXY               │   │
 │   │  - Let's Encrypt TLS (autocert)                     │   │
 │   │  - Lock-free atomic routing table (sync/atomic)     │   │
 │   │  - 32KB buffer pooling (sync.Pool)                  │   │
 │   │  - WebSockets & SSE log streaming                   │   │
 │   └────────────┬───────────────────────────┬────────────┘   │
 │                │                           │                │
 │                │ (Dynamic App Traffic)     │ (Static Route) │
 │                ▼                           ▼                │
 │   ┌────────────────────────┐  ┌─────────────────────────┐   │
 │   │ Container Driver       │  │ Static File Engine      │   │
 │   │ (Unix Socket API)      │  │ (Zero-Container Mode)   │   │
 │   └────────────┬───────────┘  └─────────────────────────┘   │
 │                │                                            │
 │   ┌────────────┴───────────┐  ┌─────────────────────────┐   │
 │   │ Build Orchestrator     │  │ Embedded SQLite         │   │
 │   │ (Artifacts + Queue)    │  │ (WAL, 2MB Cache Cap)    │   │
 │   └────────────────────────┘  └─────────────────────────┘   │
 └────────────────┬────────────────────────────────────────────┘
                  │ (Unix Socket)
                  ▼
         ┌─────────────────┐
         │  Kiri / Docker  │
         └────────┬────────┘
                  │
         ┌────────┴────────┐
         │ App Containers  │
         └─────────────────┘
```

---

## Architectural Decisions (ADRs)

Key architectural decisions are documented under [`docs/decisions/`](docs/decisions/):

- [ADR-001: Core Architecture & Single-Binary Control Plane](docs/decisions/0001-core-architecture-and-control-plane.md)
- [ADR-002: Container Runtime & Kiri Engine Integration](docs/decisions/0002-container-runtime-and-kiri-integration.md)
- [ADR-003: In-Process Reverse Proxy & Automatic TLS](docs/decisions/0003-in-process-reverse-proxy-and-tls.md)
- [ADR-004: Memory-Constrained Build Pipeline & GitHub Deployments](docs/decisions/0004-memory-constrained-builds-and-github-deployments.md)
- [ADR-005: Embedded Storage Engine & Low-Memory Pragmas](docs/decisions/0005-embedded-storage-and-state-management.md)
- [ADR-006: Static Site Bypass & Process Optimization](docs/decisions/0006-static-site-bypass-and-process-isolation.md)

---

## Quickstart

### Prerequisites
- Go 1.22+
- Docker or [Kiri](https://runkiri.com) container runtime (optional for static-only hosting)

### Running Locally

```bash
# 1. Clone and compile
git clone https://github.com/valkyrie-stack/bifrost.git
cd bifrost
make build

# 2. Start Bifrost on port 8080
./bifrost -port 8080
```

Open `http://localhost:8080` in your browser to access the dashboard.

### Running in Production (Linux VPS)

Run the automated installer:

```bash
sudo bash scripts/install.sh
```

The script will:
1. Detect architecture (`amd64` or `arm64`).
2. Verify host memory and configure a 2GB NVMe swapfile if total swap is under 1.5GB.
3. Verify or set up the container runtime.
4. Install the binary to `/usr/local/bin/bifrost` and launch the `bifrost.service` systemd unit.

---

## Deploying from GitHub Actions

Add this workflow to `.github/workflows/deploy.yml` in your repository:

```yaml
name: Deploy to Bifrost

on:
  push:
    branches: [ main ]

jobs:
  build-and-deploy:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-node@v4
        with:
          node-version: 20
      - run: npm ci && npm run build
      - name: Stream artifact to Bifrost
        run: |
          tar -czf artifact.tar.gz -C dist .
          curl -f -X POST "${{ secrets.BIFROST_SERVER }}/api/v1/projects/${{ secrets.BIFROST_PROJECT_ID }}/deploy/artifact" \
            -H "Authorization: Bearer ${{ secrets.BIFROST_DEPLOY_TOKEN }}" \
            --data-binary @artifact.tar.gz
```

---

## License

MIT License. Free and open source.
