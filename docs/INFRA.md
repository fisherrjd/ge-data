# ge-data: Infrastructure & Ops

What this system needs to run, how it's packaged per environment, and how the
ingester gets built, released, and deployed. See [`GOAL.md`](./GOAL.md) for what we
collect and [`database-setup.md`](./database-setup.md) for the schema.

The design rule: **the requirements below are identical everywhere.** Only the
*packaging* changes (nix shell → eldo + k3s). Satisfy them locally first, confirm the
system works, then re-satisfy the same list in production.

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
| **Single instance** | exactly 1, always | see below — the rule that's easy to break by accident |
| DB connectivity | reach the DB on 5432 | `DATABASE_URL=postgresql://ge-data:<pw>@<db-host>:5432/ge-data` |
| Outbound HTTPS | to `prices.runescape.wiki:443` | the only external dependency; must not be firewalled off |
| `USER_AGENT` | descriptive: project + contact | **mandatory** — blank/generic UAs get blocked |
| Resources | ~10m CPU / 32–128Mi RAM | trivial workload: a few GETs/min + batched inserts |
| Restart policy | restart on crash | a bad fetch shouldn't end the process, but crashes should self-heal |
| Inbound | none | it's a collector — no Service, LB, or ingress |

Logs are structured (`log/slog`, logfmt) to stderr — `kubectl logs`. Lifecycle:
`ingester started` / `ingester shut down cleanly`; per-loop `tick failed`
(`loop=`, `err=`) on a bad tick. The "it's working" signal is the per-tick writes:
`mapping refreshed`, `5m block written`, `1m tick written` (each with row counts).

### The single-instance rule (why, and how it's enforced)

The ingester is a timed poller that **writes**. The 1m `/latest` path dedups on
change (insert only when `high_time`/`low_time` advanced), which is racy under
concurrency — two pollers would double-poll, risk double-inserts, and invite a
Wiki-API block. So there must never be more than one, even momentarily.

Enforced per environment:
- **local**: you run one `go run`. Just don't start two.
- **k3s**: `replicas: 1` **and** a deploy strategy that never briefly runs two
  (`maxSurge: 0`, or `Recreate`). Never an HPA. For HA later, use a
  leader-election Lease, not more replicas.

## Secrets & config

The ingester needs `DATABASE_URL` and `USER_AGENT`; the DB needs a role password.
Delivery differs per environment:

| Value | Used by | Local | Production |
|---|---|---|---|
| DB role password | DB + (inside `DATABASE_URL`) | `POSTGRES_PASSWORD` in `.env` (gitignored) | set on the `ge-data` role out-of-band; also embedded in the Secret's `DATABASE_URL` |
| `DATABASE_URL` | ingester | composed from `.env` + `localhost:5433` | **whole string** in k8s `Secret ge-data-db` (host = `10.42.0.1`) |
| `USER_AGENT` | ingester | `.env` | k8s `Secret ge-data-db` |

In production the Secret carries the **entire** `DATABASE_URL` (host + password) plus
`USER_AGENT`; the pod gets both via `envFrom`. Keeping the host and password in the
Secret means the ops repo embeds no DB topology or credentials.

## The environments

Same requirements, two packagings. Develop locally, then production.

| | DB packaging | Ingester packaging | DB address (for `DATABASE_URL`) |
|---|---|---|---|
| **local** (`default.nix`) | `pg16 + timescaledb` from the nix shell (`db_reset` + `__pg`), `.db/` PGDATA, port **5433** | `go run ./ingester` from the shell | `localhost:5433` |
| **production** | **eldo**, NixOS `services.postgresql` (pg16 + timescaledb), tailnet | container on **k3s**, namespace `osrs-ge`, deployed via the **ops repo** | `10.42.0.1:5432` (eldo's flannel gateway / `cni0`) |

(`docker-compose.yml` is also present as a single-host container DB option; the nix
shell is the primary local path — see [`database-setup.md`](./database-setup.md).)

### Production specifics

- **DB lives on eldo, not in k8s.** Defined in `~/cfg/hosts/eldo/configuration.nix`
  (`services.postgresql`: pg16, `timescaledb` preloaded, `ge-data` DB + role,
  `enableTCPIP`, firewall 5432, a `pg_hba` rule for the connecting source). The
  NixOS config creates an **empty** `ge-data` database; the role password and the
  schema are applied out-of-band (see "Schema loading" and "First-time bootstrap").
- **Ingester deploys via the ops repo** (`~/github/jade/ops`), not from manifests in
  this repo. A `hex.k8s.services.build` def (`svc/ge-data-ingester.nix`, wired into
  `specs.nix`) renders the Deployment: `replicas: 1`, `maxSurge: 0`, no Service/LB,
  `imagePullSecrets: ghcr-secret`, and `envFrom` the `ge-data-db` Secret (which
  carries the whole `DATABASE_URL` + `USER_AGENT`).
- **Why `DATABASE_URL` uses `10.42.0.1`**: the pod has its own netns (so `localhost`
  is empty) and flannel SNATs pod→off-cluster traffic (so eldo's tailnet IP would
  appear as a new source needing a new `pg_hba` rule). Dialing the flannel gateway
  `10.42.0.1` (eldo's `cni0`, where Postgres also listens) keeps the source inside
  `10.42.0.0/16` — which eldo's **existing** `pg_hba` rule already authorizes, so
  **no new rule and no `nixos-rebuild`** are needed. Verify once live with
  `SELECT client_addr FROM pg_stat_activity WHERE usename = 'ge-data';` (expect a
  `10.42.x.x` address). Single-node assumption: if the cluster grows, pin the pod to
  eldo so `10.42.0.1` is eldo's gateway.

## Schema loading

All environments load the **same** `init/01_schema.sql`, but how/when differs:

- **local (nix)**: `db_reset` runs it explicitly against a preloaded server.
- **production (eldo)**: applied **once, by hand**, into eldo's `ge-data` DB (the
  NixOS config only ensures the empty database exists).

> Drift watch: one file, but each path applies it only once. Past v1, adopt a
> migration tool (golang-migrate/goose) so schema changes apply the same way
> everywhere.

## Build & release

Two repos: **`fisherrjd/ge-data`** (this one) builds and publishes the ingester
image to GHCR via `.github/workflows/docker-publish.yml`; **`fisherrjd/ops`**
consumes a published tag (`svc/ge-data-ingester.nix` pins it) and builds nothing.

The image version comes from the repo-root **`VERSION`** file — the single source of
truth. The workflow runs on every branch push:

| You push… | Image tag | Use for |
|---|---|---|
| merge to `main` | `:X.Y.Z` (immutable) | **releases — pin this in ops** |
| any other branch | `:X.Y.Z-b<short-sha>` | a throwaway build to test on the cluster |

On a `main` build CI also git-tags the commit `vX.Y.Z`, then bumps the patch in
`VERSION` and commits it back (`[skip ci]`), so `main` always sits on the next
unreleased version. Tags are immutable — a `main` build whose `VERSION` already
exists in GHCR **fails the guard step** rather than overwriting it.

**SemVer** (`MAJOR.MINOR.PATCH`, currently `0.x`):
- **PATCH** — bug fixes. Automatic: every merge to `main` ships the current version
  and bumps the patch.
- **MINOR/MAJOR** — features / breaking changes. Bump `VERSION` in the PR; CI ships
  it, then auto-bumps the patch.

**Cut a release:** land work on `main` for a patch; for minor/major bump `VERSION` in
the PR first. There is no manual `git tag` step. Verify the Actions run is green and
the tag appears on the GHCR package page
(`https://github.com/fisherrjd/ge-data/pkgs/container/ge-data`).

## Deploy

Routine deploy — in **`fisherrjd/ops`**, edit `svc/ge-data-ingester.nix`:

```nix
, image ? "ghcr.io/fisherrjd/ge-data:0.1.0"
```

Then from the ops repo preview with `hex --dryrun -t specs.nix` and apply with `hex`.
k8s won't re-pull an identical tag without `imagePullPolicy: Always` + a rollout
restart, so a real release means a new tag.

**Test a feature branch on the cluster** (no release): push the branch → CI builds
`:X.Y.Z-b<sha>`; point `svc/ge-data-ingester.nix` at that tag and apply; revert to
the pinned release tag when done. Mind the single-instance rule — don't leave a
feature-branch pod running alongside the release pod.

> The GHCR package is private; the cluster pulls it with `ghcr-secret`, so make the
> package visible to that token (or public). Branch pushes accumulate `*-b<sha>`
> images — set a retention policy on the package if it gets noisy.

## First-time bootstrap

One-time setup before the first deploy (routine deploys above don't repeat these):

1. **ops** *(done)*: `svc/ge-data-ingester.nix` written and wired into `specs.nix`.
2. **eldo** *(done)*: `ge-data` role password set out-of-band; `init/01_schema.sql`
   loaded into the `ge-data` DB (extension as `postgres`, tables as `ge-data`).
   **No new `pg_hba` rule** — the ingester dials `10.42.0.1`, covered by the existing
   `10.42.0.0/16` rule.
3. **cluster**: ensure the `osrs-ge` namespace + `ghcr-secret`; `kubectl apply` the
   `ge-data-db` Secret with the **whole** `DATABASE_URL`
   (`postgresql://ge-data:<pw>@10.42.0.1:5432/ge-data?sslmode=disable`) + `USER_AGENT`.
4. **deploy + verify**: `hex` the ops stack, watch the pod, confirm rows land in
   `prices_5m`/`prices_1m`, and check `client_addr` shows a `10.42.x.x` source.
