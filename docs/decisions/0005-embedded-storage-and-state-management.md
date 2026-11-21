# ADR-005: Embedded Storage Engine & Low-Memory Pragmas

## Status
Accepted

## Date
2026-11-22

## Context
A PaaS must persist state for projects, deployment history, domains, environment variables, user credentials, and routing tables.

External relational databases (PostgreSQL or MySQL) consume 80MB–150MB of resident RAM even when idle. Running a dedicated database container on a 1GB server eliminates roughly 10% of total host memory before any user applications are deployed.

We need an embedded, ACID-compliant storage engine that:
1. Compiles with `CGO_ENABLED=0` for pure static binaries across `amd64` and `arm64`.
2. Consumes less than 4MB of RAM under normal operations.
3. Supports transactions and concurrent reads while writes are occurring.

## Decision
Use pure-Go SQLite (`modernc.org/sqlite`) configured with aggressive memory-capping connection pragmas:

```sql
PRAGMA journal_mode = WAL;
PRAGMA cache_size = -2000;
PRAGMA synchronous = NORMAL;
PRAGMA temp_store = FILE;
PRAGMA mmap_size = 0;
PRAGMA busy_timeout = 5000;
```

### Explanation of Low-Memory Pragmas:
1. `journal_mode = WAL`:
   Write-Ahead Logging permits unlimited concurrent readers while a single writer operates. Reads never block writes and writes never block reads.
2. `cache_size = -2000`:
   A negative value sets an absolute upper memory limit in kibibytes. `-2000` caps the SQLite page cache at **2MB RAM** (down from SQLite's default of up to 32MB).
3. `temp_store = FILE`:
   Forces temporary tables, indices, and sort operations onto disk storage rather than buffering them in memory.
4. `mmap_size = 0`:
   Disables virtual memory memory-mapping (`mmap`), preventing the OS from keeping large portions of the database in host page cache.
5. `synchronous = NORMAL`:
   Sufficient durability in WAL mode with fewer `fsync` calls, reducing I/O wait on low-end VPS spinning/shared disks.

## Alternatives Considered

### BBolt (Key-Value Store)
- **Pros**: Pure Go, extremely lightweight.
- **Cons**: Lacks relational queries, schema migrations, and relational foreign-key integrity. Requires custom indexing logic for querying deployments by project or status.
- **Rejected**: SQLite provides standard SQL, schema migrations, and indexes with identical memory performance when tuned.

### PostgreSQL Container
- **Rejected**: Too heavy for 1GB VPS target.

## Consequences
- **Positive**: Total database memory is strictly capped at ~2MB.
- **Positive**: Single file storage (`/var/lib/bifrost/bifrost.db`) makes backups as simple as copying one file or running `VACUUM INTO`.
- **Positive**: Fully static compilation with zero CGO dependencies.
