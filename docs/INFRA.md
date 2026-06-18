# ge-data: Infrastructure

What this system needs to run, and how it's packaged in each environment. See
[`GOAL.md`](./GOAL.md) for what we collect and [`database-setup.md`](./database-setup.md)
for the schema.

The design rule: **the requirements below are identical everywhere.** Only the
*packaging* changes (nix shell → Docker Compose → eldo + k3s). Satisfy them locally
first, confirm the system works, then re-satisfy the same list in production.

## Components

Two long-lived components, always **separate processes** — never bundled into one
container/pod (different lifecycles, different base images, the DB is stateful and
the ingester isn't).

| Component | What it is | State | Scales? |
|---|---|---|---|
| **Database** | PostgreSQL 16 + TimescaleDB extension | persistent (the price history) | no — single primary |
| **Ingester** | Go service: 3 poll loops + mapping load | stateless | **no — exactly 1 instance, ever** |

## Requirements (environment-independent)

### Database

| Requirement | Value | Why |
|---|---|---|
| Engine | PostgreSQL **16** | matches schema + drivers |
| Extension | **timescaledb** (preloaded) | hypertables + compression; must be in `shared_preload_libraries` |
| Schema | `init/01_schema.sql` | the one source of truth, loaded once |
| Storage | persistent, grows forever | append-only price history; compresses well but never shrinks to zero |
| Port | 5432 | clients connect here |
| Auth | role `ge-data`, password from secret | `POSTGRES_PASSWORD`; never commit it |

Sizing: start ~10Gi. Rows are tiny and repetitive and old chunks compress hard
(the reason we use Timescale), so growth is slow — but monitor and expand, don't
let it fill.

### Ingester

| Requirement | Value | Why |
|---|---|---|
| **Single instance** | exactly 1, always | see below — this is the rule that's easy to break by accident |
| DB connectivity | reach the DB on 5432 | `DATABASE_URL=postgresql://ge-data:<pw>@<db-host>:5432/ge-data` |
| Outbound HTTPS | to `prices.runescape.wiki:443` | the only external dependency; must not be firewalled off |
| `USER_AGENT` | descriptive: project + contact | **mandatory** — blank/generic UAs get blocked |
| Resources | ~10m CPU / 32–128Mi RAM | trivial workload: a few GETs/min + batched inserts |
| Restart policy | restart on crash | a bad fetch shouldn't end the process, but crashes should self-heal |

### The single-instance rule (why, and how it's enforced)

The ingester is a timed poller that **writes**. The 1m `/latest` path dedups on
change (insert only when `high_time`/`low_time` advanced), which is racy under
concurrency — two pollers would double-poll, risk double-inserts, and invite a
Wiki-API block. So there must never be more than one, even momentarily.

Enforced per environment:
- **local**: you run one `go run` / one compose service. Just don't start two.
- **k3s**: `replicas: 1` **and** a deploy strategy that never briefly runs two
  (`maxSurge: 0`, or `Recreate`). Never an HPA. For HA later, use a
  leader-election Lease, not more replicas.

## Secrets & config

Two secret values, same everywhere; only the delivery differs:

| Key | Used by | Local | Production |
|---|---|---|---|
| `POSTGRES_PASSWORD` | DB + ingester | `.env` (gitignored) | k8s `Secret ge-data-db` |
| `USER_AGENT` | ingester | `.env` | k8s `Secret ge-data-db` |

`DATABASE_URL` is *derived* from the password + the DB's address — the address is
the main thing that changes per environment (table below).

## The environments

Same requirements, three packagings. Develop left-to-right.

| | DB packaging | Ingester packaging | DB address (for `DATABASE_URL`) |
|---|---|---|---|
| **nix dev** (`default.nix`) | `__pg` local server, `.db/` PGDATA, port **5433** | `go run ./ingester` from the shell | `localhost:5433` |
| **docker-compose** | `timescale/timescaledb` service `db`, port **5000** | `go run`, or add an `ingester` service | `localhost:5000` (host) / `db:5432` (in-net) |
| **production** | **eldo**, NixOS `services.postgresql` (pg16 + timescaledb), tailnet | container on **k3s**, namespace `osrs-ge`, deployed via the **ops repo** | `100.66.184.28:5432` (eldo tailnet IP) |

### Production specifics

- **DB lives on eldo, not in k8s.** Defined in `~/cfg/hosts/eldo/configuration.nix`
  (`services.postgresql`: pg16, `timescaledb` preloaded, `ge-data` DB + role,
  `enableTCPIP`, firewall 5432, a `pg_hba` rule for the connecting source). The
  NixOS config creates an **empty** `ge-data` database; the role password and the
  schema are applied out-of-band (see "Schema loading").
- **Ingester deploys via the ops repo** (`~/github/jade/ops`), not from manifests in
  this repo. A `hex.k8s.services.build` def (`svc/ge-data-ingester.nix`, wired into
  `specs.nix`) renders the Deployment: `replicas: 1`, `maxSurge: 0`, no Service/LB
  (it's a collector, nothing inbound), `imagePullSecrets: ghcr-secret`, env from
  `Secret ge-data-db`, `DATABASE_URL` pointing at eldo's tailnet IP.
- **Image**: `ghcr.io/fisherrjd/ge-data:<tag>`, built from `ingester/Dockerfile` by
  `.github/workflows/docker-publish.yml` (tag driven by the repo-root `VERSION`).
- **pg_hba caveat**: k3s/flannel masquerades pod traffic to off-cluster destinations,
  so postgres sees the connection from **eldo's tailnet IP**, not the pod CIDR — the
  `pg_hba` rule must match that source. Verify once live with
  `SELECT client_addr FROM pg_stat_activity WHERE usename = 'ge-data';`.

## Schema loading

All environments load the **same** `init/01_schema.sql`, but how/when differs:

- **nix dev**: `db_reset` runs it explicitly against a preloaded server.
- **docker-compose**: runs via `/docker-entrypoint-initdb.d` **only on first boot of
  an empty volume** — it will NOT re-run for later migrations.
- **production (eldo)**: applied **once, by hand**, into eldo's `ge-data` DB (the
  NixOS config only ensures the empty database exists).

> Drift watch: three paths, one file, but most apply it only once. Past v1, adopt a
> migration tool (golang-migrate/goose) so schema changes apply the same way
> everywhere.

## Going live — remaining steps

The ingester code, `Dockerfile`, and image workflow are done. To deploy:

1. **ops**: write `svc/ge-data-ingester.nix`, wire it into `specs.nix`.
2. **cluster**: ensure the `osrs-ge` namespace + `ghcr-secret`; `kubectl apply` the
   `ge-data-db` Secret (`POSTGRES_PASSWORD`, `USER_AGENT`).
3. **eldo**: add the `pg_hba` rule for the ingester's source IP, set the `ge-data`
   role password (matching the Secret), `nixos-rebuild`.
4. **eldo**: load `init/01_schema.sql` into the `ge-data` DB (one-time).
5. **deploy + verify**: apply the ops stack, watch the pod, confirm rows land in
   `prices_5m`/`prices_1m`, and check `client_addr` validates the `pg_hba` CIDR.
