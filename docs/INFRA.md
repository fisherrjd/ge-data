# ge-data: Infrastructure

What this system needs to run, independent of *which* machine runs it. See
[`GOAL.md`](./GOAL.md) for what we collect, [`database-setup.md`](./database-setup.md)
for the schema, and [`../k8s/README.md`](../k8s/README.md) for the k3s manifests.

The design rule: **the requirements below are identical in every environment.** Only
the packaging changes (nix shell → Docker Compose → k3s pods). You can satisfy them
locally first, confirm the system works, then re-satisfy the same list when you deploy.

## Components

Two long-lived components. They are always **separate processes** — never bundled
into one container/pod (different lifecycles, different base images, the DB is
stateful and the ingester isn't).

| Component | What it is | State | Scales? |
|---|---|---|---|
| **Database** | PostgreSQL 16 + TimescaleDB extension | persistent (the price history) | no — single primary |
| **Ingester** | Go service: 3 poll loops + mapping load | stateless | **no — exactly 1 instance, ever** |

## Requirements

### Database

| Requirement | Value | Why |
|---|---|---|
| Engine | PostgreSQL **16** | matches schema + drivers |
| Extension | **timescaledb** (preloaded) | hypertables + compression; must be in `shared_preload_libraries` |
| Schema | `init/01_schema.sql` | the one source of truth, loaded on first init |
| Storage | persistent volume, grows forever | append-only price history; compresses well but never shrinks to zero |
| Port | 5432 (container-internal) | clients connect here; host/NodePort mapping is environment-specific |
| Auth | role `ge-data`, password from secret | `POSTGRES_PASSWORD`; never commit it |

Sizing: start ~10Gi. Rows are tiny and repetitive and old chunks compress hard
(the reason we use Timescale), so growth is slow — but monitor and expand the volume,
don't let it fill.

### Ingester

| Requirement | Value | Why |
|---|---|---|
| **Single instance** | exactly 1, always | duplicate pollers double-write and the 1m dedup path is racy under concurrency; also risks Wiki-API blocks |
| DB connectivity | reach the DB on 5432 | `DATABASE_URL=postgresql://ge-data:<pw>@<db-host>:5432/ge-data` |
| Outbound HTTPS | to `prices.runescape.wiki:443` | the only external dependency; must not be firewalled off |
| `USER_AGENT` | descriptive: project + contact | **mandatory** — blank/generic UAs get blocked |
| Resources | ~10m CPU / 32–128Mi RAM | trivial workload: a few GETs/min + batched inserts |
| Restart policy | restart on crash | a bad fetch shouldn't end the process, but crashes should self-heal |

The "single instance" rule is the one requirement that's easy to violate by accident
(a rolling deploy briefly running two, an HPA, a stray manual scale). Enforce it in
whatever runs the ingester — see per-environment notes.

## Secrets & config

Two secret values, same everywhere; only the delivery mechanism differs:

| Key | Used by | Local | k3s |
|---|---|---|---|
| `POSTGRES_PASSWORD` | DB + ingester | `.env` (gitignored) | `Secret ge-data-db` |
| `USER_AGENT` | ingester | `.env` | `Secret ge-data-db` |

`DATABASE_URL` is *derived* from the password + the DB's address, which is the only
thing that genuinely changes per environment (see the table below).

## The three environments

Same requirements, three packagings. Test left-to-right.

| | DB packaging | Ingester packaging | DB address (for `DATABASE_URL`) |
|---|---|---|---|
| **nix dev** (`default.nix`) | `__pg` local server, `.db/` PGDATA, port **5433** | `go run ./ingester` from the shell | `localhost:5433` |
| **docker-compose** | `timescale/timescaledb` service `db` | `ingester` service (to add) | inside net: `db:5432`; from host: `localhost:5000` |
| **k3s** (`k8s/`) | StatefulSet + PVC + headless Service | Deployment, `replicas:1`, `Recreate` | `ge-data-db.ge-data.svc.cluster.local:5432` |

Per-environment "single instance" enforcement:
- **nix**: you run one `go run`. Just don't start two.
- **compose**: one service, `restart: unless-stopped`. Don't `--scale`.
- **k3s**: `replicas: 1` + `strategy: Recreate` (never RollingUpdate, never an HPA).

Schema loading is the one thing that behaves slightly differently:
- **nix**: `db_reset` runs `init/01_schema.sql` explicitly against a preloaded server.
- **compose / k3s**: `init/01_schema.sql` runs via `/docker-entrypoint-initdb.d`
  **only on first boot of an empty volume** — it will NOT re-run for later migrations.

> Drift watch: all three load the *same* `init/01_schema.sql`, but two paths only run
> it once. Past v1, adopt a migration tool (golang-migrate/goose) so schema changes
> apply the same way everywhere. Tracked as a future item.

## Test locally, then wire up — the path

**1. Local (Docker Compose) — prove the system end to end:**
```bash
cp .env.example .env && $EDITOR .env        # set POSTGRES_PASSWORD + USER_AGENT
docker compose up -d                         # DB comes up, runs init/01_schema.sql once
psql "postgresql://ge-data:<pw>@localhost:5000/ge-data" -c '\dx'   # timescaledb listed
# run the ingester against it (once it exists):
DATABASE_URL=postgresql://ge-data:<pw>@localhost:5000/ge-data \
USER_AGENT='ge-data (contact: you@example.com)' go run ./ingester
# confirm rows land:
psql "postgresql://ge-data:<pw>@localhost:5000/ge-data" -c 'select count(*) from prices_5m;'
```

**2. Deploy (k3s) — re-satisfy the same requirements:**
```bash
cp k8s/secret.example.yaml k8s/secret.yaml && $EDITOR k8s/secret.yaml
kubectl apply -f k8s/secret.yaml
docker build -t <registry>/ge-data-ingester:<tag> ./ingester && docker push <registry>/ge-data-ingester:<tag>
# set that image in k8s/ingester.yaml, then:
kubectl apply -k k8s/ --load-restrictor LoadRestrictionsNone
kubectl -n ge-data get pods -w
```

If step 1 works, step 2 is just the same two components + two secrets + one
connection string, expressed as pods.

## What's still missing before deploy

- [ ] **Ingester code** — `ingester/` is empty (see [`TODO.md`](./TODO.md)).
- [ ] **`ingester/Dockerfile`** — multi-stage static build; needed by both compose and k3s.
- [ ] **`ingester` service in `docker-compose.yml`** — local end-to-end test.
- [ ] **Ingester image in a registry** k3s can pull, set in `k8s/ingester.yaml`.
- [ ] **Container registry** reachable from the cluster (or `k3s ctr images import` for a local image).

## Cluster assumptions (k3s)

- A **StorageClass** for the PVC. k3s ships `local-path` (default) — fine for a single
  node; note local-path pins the DB to whichever node holds the data.
- Pods can make **outbound HTTPS** to the public internet (Wiki API). No inbound
  ingress is required — nothing serves traffic; this is a collector.
- Optional: a way to reach the DB for ad-hoc queries (`kubectl port-forward
  svc/ge-data-db 5000:5432`), rather than exposing it.
