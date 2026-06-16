# ge-data: Database Setup & Design

A reference for *why* the database is built the way it is, not just how to run it.
Written to be read top-to-bottom while learning; skim the headings if you just need
the commands.

> Status: design doc. The project's intent lives in [`GOAL.md`](./GOAL.md) — that
> file is the source of truth for *what* we're building; this one explains *how* the
> database is set up and *why*. Where they disagree, GOAL.md wins.

---

## 1. Goal

Build and maintain a TimescaleDB store of OSRS Grand Exchange price data deep and
granular enough to **backtest market ideas** and study how prices move. Two research
tracks share one database (see [`GOAL.md`](./GOAL.md) for the full rationale):

- **Fast flips (intraday).** Profit is the spread between instant-buy (high) and
  instant-sell (low) inside short windows. Needs **5-minute** data with volume — the
  micro-trend layer.
- **Swing trades (up to ~3 months).** Entry timing to the minute is noise; **daily**
  price is enough. This is the layer that wants the **deepest history**.

The bigger ambition: pair prices with a **news/update timeline** (an events table) so
we can measure how the market *reacts to events*, not just chart prices in isolation.

This is a **time-series** workload: append-only, time-ordered, grows forever
(potentially **~1.5B rows** at full 5m depth), queried mostly by "item X over date
range Y" and "what happened in the window around event E". That shape drives every
decision below.

---

## 2. Architecture at a glance

We split two concerns that are easy to conflate:

```
┌────────────────────────────┐         ┌─────────────────────────────────────┐
│  nix (default.nix)         │         │  Docker container                    │
│                            │         │                                      │
│  - Go toolchain (ingester) │         │  timescale/timescaledb image         │
│  - dev CLI tools           │  build  │  ├─ PostgreSQL 16                    │
│  - your editor's LSP, etc. │ ──────▶ │  ├─ TimescaleDB extension (preloaded)│
│                            │         │  └─ /var/lib/postgresql/data (volume)│
│  builds the WORK ENV       │         │  RUNS the DATABASE                   │
└────────────────────────────┘         └─────────────────────────────────────┘
        the app you write                   the always-on service it talks to
```

- **nix** builds the *development environment* (the Go ingester, tooling). It is
  local-dev only and never ships.
- **The container image** (Dockerfile) is the artifact that travels.
- **The container** runs the *database* as a long-lived service with its data on a
  persistent volume.

This separation is deliberate. Earlier we used nix-managed local Postgres scripts
(`__pg`, `__pg_bootstrap`) — those are for *ephemeral local dev* and bind to
`localhost` only. They're the wrong tool for a shared, always-on, polled DB. See
[The localhost trap](#9-appendix-the-localhost-trap-a-debugging-story) for the war
story that made this clear.

### Two deployment environments

The same containers run in two places — don't let the descriptions drift:

| | **local dev** | **production** |
|---|---|---|
| orchestrator | `docker-compose.yml` | k3s manifests (server) |
| DB | `db` service + named volume | StatefulSet + PVC + Service |
| forward ingester | `ingester` service (`serve`) | Deployment, **replicas: 1** |
| backfill | `docker compose run --rm backfill` | Job (run-once) |
| secrets | `.env` | k8s Secret |

`docker-compose.yml` is a **local convenience** for the tight edit→run loop, *not* the
deploy target. The forward collectors must run as **exactly one instance** anywhere —
multiple pollers duplicate work and risk Wiki-API rate-limits.

### Two ingest feeds, one database

Everything comes from the **Wiki API only** — no third-party sources (details in
[section 7](#7-data-source-wiki-real-time-prices-api)):

```
  Wiki /mapping ──▶  items     (id <-> name + metadata; re-run to catch new items)
  Wiki /5m      ──▶  prices_5m (hypertable, backfill + forward, avg + VOLUME)
  Wiki /latest  ──▶  prices_1m (hypertable, FORWARD-ONLY, instantaneous, NO volume)
  hand/scrape   ──▶  events    (plain table: news & update timeline)
```

Daily swing data is the `prices_5m_daily` continuous aggregate (a 5m rollup), so daily
history is capped at the 5m floor (~Mar 2021) — accepted tradeoff for staying Wiki-only.

The moving parts to build (from GOAL.md): **mapping loader**, **5m backfill**
(`DO NOTHING`), **5m live collector** (`DO UPDATE`), **1m collector** (`/latest`,
dedup-on-change), and the **events table**.

---

## 3. Why TimescaleDB (and the honest trade-off)

TimescaleDB is **not a separate database** — it's a PostgreSQL *extension*. Same
server, same SQL, same drivers, same `psql`. You get three features that matter
specifically because our data grows forever:

1. **Hypertables** — automatic time-based partitioning. You write/read one logical
   table; under the hood it's split into time "chunks" so inserts stay fast and
   queries skip irrelevant time ranges.
   → https://www.tigerdata.com/docs/api/latest/hypertable

2. **Columnar compression** (a.k.a. "hypercore"/"columnstore" in newer versions) —
   old chunks get compressed up to ~90–98%. OSRS price rows are tiny and highly
   repetitive (same item, slowly-changing prices), which is the ideal case. Over 3
   years this is the difference between tens of GB and a few GB.
   → https://www.tigerdata.com/docs/build/columnar-storage/

3. **Continuous aggregates** — auto-refreshing materialized rollups (e.g. daily
   OHLC bars) that update as new data lands, instead of rescanning raw ticks.
   → https://www.tigerdata.com/docs/use-timescale/latest/continuous-aggregates/about-continuous-aggregates

**Honest trade-off.** The cost is operational: it's another moving part, and the
full-feature build is under the Timescale License (TSL), which nixpkgs marks as
*unfree*. We sidestep that entirely by using the **official Docker image**, which
is purpose-built and ships compression enabled.

**Would plain Postgres do?** For a small fixed dataset, PG16 + a `BRIN` index on
`ts` + a composite index on `(item_id, ts)` is fine. But this targets multi-billion
rows (5m + 1m) with continuous ingest and pre-rolled aggregates — at that scale
compression and chunk exclusion stop being nice-to-haves. Timescale is the right call
here, not a luxury. (BRIN, for reference: https://www.postgresql.org/docs/16/brin.html)

**Storage ballpark** (~2 yr forward, Timescale-compressed at ~12×): `prices_5m`
~9 GB, `prices_1m` (dedup-on-change) ~10 GB → **~20 GB total**. The same data
uncompressed in vanilla Postgres is ~250–400 GB, so
compression is doing ~10× of the work. Provision **~100 GB** for headroom (WAL,
indexes, continuous aggregates). Measure the real compression ratio on the first
month — if it's 8× not 12×, budget ~40% more.

---

## 4. Core Timescale concepts (mental model)

Think of a **hypertable** as a normal table with an automatic filing clerk behind
it:

- Every row has a timestamp. The clerk drops it into a **chunk** — a child table
  covering a fixed time window (we use 7 days).
- When you query a date range, the clerk only opens the relevant folders (chunks)
  and ignores the rest. This is "chunk exclusion" and it's why queries stay fast as
  the table grows. → https://www.tigerdata.com/docs/reference/timescaledb/hypertables
- Once a chunk is old enough (we say 14 days), a background policy rewrites it into
  **columnar** form — squeezing repetitive columns hard. Recent data stays
  row-oriented for fast writes; old data is compressed for cheap storage and fast
  scans. → https://www.tigerdata.com/docs/api/latest/compression/add_compression_policy/

A **continuous aggregate** is a second hypertable holding a *summary* (e.g. one row
per item per day) that the clerk keeps in sync incrementally — only recomputing the
buckets touched by new data, not the whole thing.

### Version note (read before consulting the live docs)

TimescaleDB **2.18+** renamed the compression API to "columnstore":
`add_columnstore_policy()` / `ALTER TABLE ... SET (timescaledb.enable_columnstore)`.
The current tigerdata.com docs use those names. The SQL in this repo targets the
pinned **2.17** image and uses the older, still-supported
`add_compression_policy()` / `timescaledb.compress`. If you bump the image, migrate
to the new API.
→ https://www.tigerdata.com/docs/api/latest/hypercore/add_columnstore_policy

---

## 5. The container setup

### `docker-compose.yml`

```yaml
services:
  db:
    image: timescale/timescaledb:2.17.2-pg16   # pin a real tag; never use :latest
    container_name: ge-data-db
    restart: unless-stopped
    environment:
      POSTGRES_DB: ge-data
      POSTGRES_USER: ge-data
      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD:?set POSTGRES_PASSWORD in .env}
    ports:
      - "5000:5432"        # host:container — see port note below
    volumes:
      - pgdata:/var/lib/postgresql/data        # data survives container rebuilds
      - ./init:/docker-entrypoint-initdb.d:ro  # schema, runs ONCE on first boot
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U ge-data -d ge-data"]
      interval: 10s
      timeout: 5s
      retries: 5

volumes:
  pgdata:
```

Line-by-line reasoning:

- **`image: timescale/timescaledb`** — ships with
  `shared_preload_libraries=timescaledb` already set, so the extension is preloaded
  at startup (a hard requirement Timescale has that plain extensions don't). This is
  the main reason the container path is simpler than wiring it into nix.
  Image + tags: https://hub.docker.com/r/timescale/timescaledb
  Self-hosted Docker guide: https://www.tigerdata.com/docs/self-hosted/latest/install/installation-docker/
- **Pin the tag.** `:latest-pg16` silently jumps versions. Check Docker Hub for the
  current stable tag and pin it. Compose image ref:
  https://docs.docker.com/reference/compose-file/services/#image
- **`restart: unless-stopped`** — the DB comes back after a host reboot/crash.
- **`POSTGRES_*` env vars** — the official Postgres entrypoint reads these to create
  the database, user, and password on first init.
  https://github.com/docker-library/docs/blob/master/postgres/README.md#environment-variables
- **`${POSTGRES_PASSWORD:?...}`** — compose refuses to start if the var is unset, so
  you can't ship a blank password. Interpolation syntax:
  https://docs.docker.com/reference/compose-file/interpolation/
- **Named volume `pgdata`** — the single most important line. Your data lives
  *outside* the container lifecycle: `down`/rebuild/`up` and the history is intact.
  https://docs.docker.com/engine/storage/volumes/
- **`./init` mount** — anything in `/docker-entrypoint-initdb.d` runs **only on the
  first boot** (empty volume). Great for initial schema; it will **not** re-run for
  later migrations. https://github.com/docker-library/docs/blob/master/postgres/README.md#initialization-scripts
- **`healthcheck`** — lets dependents (and you) know when the DB is actually ready,
  not just started. https://docs.docker.com/reference/compose-file/services/#healthcheck

### Port: why 5000, and the caveat

- Format is `host:container`. We publish host **5000** → container **5432**.
- We avoided **5432** because a system PostgreSQL already listens there on this
  machine; reusing it is what caused the auth failure in
  [the appendix](#9-appendix-the-localhost-trap-a-debugging-story).
- **Caveat:** 5000 is a popular default (dev web servers, Flask/werkzeug). Confirm
  it's free before committing:

  ```bash
  ss -ltn 'sport = :5000'   # empty output = free
  ```

- **If your polling apps are also containers**, put them on this compose network and
  let them reach the DB as `db:5432` directly — you may not need to publish a host
  port at all. Networking model:
  https://docs.docker.com/compose/how-tos/networking/

### `.env` (gitignored)

```dotenv
POSTGRES_PASSWORD=change-me-to-something-strong
```

Add `.env` to `.gitignore`. Compose auto-loads it:
https://docs.docker.com/compose/how-tos/environment-variables/set-environment-variables/

---

## 6. Schema design

Column names and key choices below follow GOAL.md exactly — `ts`, nullable price
columns, `(ts, item_id)` keys, 1-month chunks. Don't drift from them casually; the
ingesters depend on them.

### `init/01_schema.sql`

```sql
CREATE EXTENSION IF NOT EXISTS timescaledb;

-- Static metadata, from the Wiki /mapping endpoint. One row per item.
CREATE TABLE items (
  item_id   integer PRIMARY KEY,
  name      text NOT NULL,
  members   boolean,
  value     integer,
  lowalch   integer,
  highalch  integer,
  buy_limit integer,   -- GE buy limit: matters for swing-trade liquidity
  examine   text
);
-- name -> id lookups (search/UI). Case-insensitive, non-unique (see note below).
CREATE INDEX items_name_lower_idx ON items (lower(name));

-- 5-minute series, from the Wiki /5m endpoint. The intraday/flip layer.
CREATE TABLE prices_5m (
  ts             timestamptz NOT NULL,
  item_id        integer     NOT NULL,
  avg_high_price bigint,          -- NULLABLE on purpose — see "Keep the nulls"
  avg_low_price  bigint,          -- NULLABLE on purpose
  high_volume    bigint,
  low_volume     bigint,
  PRIMARY KEY (ts, item_id)
);

SELECT create_hypertable('prices_5m', by_range('ts', INTERVAL '1 month'));
CREATE INDEX ON prices_5m (item_id, ts DESC);

ALTER TABLE prices_5m SET (
  timescaledb.compress,
  timescaledb.compress_segmentby = 'item_id',
  timescaledb.compress_orderby   = 'ts DESC'
);
SELECT add_compression_policy('prices_5m', INTERVAL '7 days');

-- 1-minute instantaneous prices, from /latest. Forward-only, no volume.
-- high/low NULLABLE (null = never seen). high_time/low_time = actual trade time,
-- used to dedup "on change". 1-week chunks (denser than 5m).
CREATE TABLE prices_1m (
  ts        timestamptz NOT NULL,
  item_id   integer     NOT NULL,
  high      bigint,
  high_time timestamptz,
  low       bigint,
  low_time  timestamptz,
  PRIMARY KEY (ts, item_id)
);
SELECT create_hypertable('prices_1m', by_range('ts', INTERVAL '1 week'));
CREATE INDEX ON prices_1m (item_id, ts DESC);
ALTER TABLE prices_1m SET (
  timescaledb.compress,
  timescaledb.compress_segmentby = 'item_id',
  timescaledb.compress_orderby   = 'ts DESC'
);
SELECT add_compression_policy('prices_1m', INTERVAL '7 days');

-- News & game-update timeline. Plain table (NOT a hypertable): low volume,
-- queried by joining time windows against prices_5m.
CREATE TABLE events (
  id        bigserial PRIMARY KEY,
  occurred  timestamptz NOT NULL,
  type      text,                 -- update | news | nerf | new_content | ...
  items     integer[],            -- affected item_ids, if known
  source    text,
  notes     text
);
CREATE INDEX ON events (occurred);
```

Design reasoning (cross-referenced to GOAL.md "Schema and storage notes"):

- **`ts` + PK `(ts, item_id)`.** One row per item per block. The PK drives all write
  semantics via `ON CONFLICT (ts, item_id)`:
  - **5m backfill** → `DO NOTHING`. Closed historical blocks are immutable; re-seeing
    one is a no-op (lets backfill and live overlap safely).
  - **5m live collector** → `DO UPDATE` (last-write-wins). The current block keeps
    settling as you poll it every minute, so the row must converge to the final
    completed average, not freeze a partial snapshot. *Gotcha:* a block isn't truly
    final until its 5-minute window closes — re-fetch by `?timestamp=` after the
    boundary (or mark in-progress rows) so partial averages don't leak into backtests.
  - **1m collector** → dedup **on change**: only insert when `high_time`/`low_time`
    advanced (`/latest` returns last-known prices, so most minutes are repeats).
  https://www.postgresql.org/docs/16/sql-insert.html#SQL-ON-CONFLICT
- **`prices_1m` is forward-only and volume-less.** `/latest` gives instantaneous
  `{high, highTime, low, lowTime}` — no averaging, no volume. There's no 1m history
  to backfill, so this table starts empty at "turn on" and grows from there. Volume
  always stays 5m-resolution (via `prices_5m`). 1-week chunks because 1m data is ~5×
  denser than 5m.
- **Keep the nulls.** `avg_high_price`/`avg_low_price` are nullable `BIGINT`.
  *Verified in the Apr 2021 probe block:* ~28% of items had a null price (460/1614
  high, 413/1614 low), **volume was never null**, and a price was null **exactly when
  that side's volume was 0** — i.e. no trade cleared that side. So a null price is a
  liquidity signal, not missing data. **Never zero-fill prices.** This is why there is
  **no generated `diff`/spread column**: the spread is computed null-aware at query
  time (`avg_high_price - avg_low_price`, correctly NULL when either side didn't
  trade), not stored. (Volume *may* be coalesced to 0 for summing — see the CAGGs.)
- **`bigint`, not `integer`.** Prices and cumulative volumes can exceed the 2.1B
  `int4` ceiling on high-value items. https://www.postgresql.org/docs/16/datatype-numeric.html
- **1-month chunks.** At ~1.5B rows, 1-month chunks keep the chunk count manageable
  while staying query-efficient; this is GOAL.md's chosen interval (vs. the 7-day
  rule-of-thumb for smaller datasets).
  https://www.tigerdata.com/docs/api/latest/hypertable/create_hypertable/
- **Compress after 7 days, segment by `item_id`, order by `ts`.** Segmenting by item
  is what unlocks the high ratio (one item's prices barely change row-to-row) and
  keeps "this multi-billion-row store small and fast." Recent chunks stay
  row-oriented for fast collector writes. GOAL.md says "a few days"; 7 is a concrete
  starting point — tune it.
  https://www.tigerdata.com/docs/api/latest/compression/alter_table_compression/
- **No FK from prices to items.** GOAL.md backfills 5m by *looping over time*, so a
  block can reference an item before `/mapping` is loaded. A foreign key would reject
  those rows. Validate item ids in the application or with a periodic check instead
  of a hard constraint that fights backfill ordering.
- **Daily data is a rollup, not a table.** With Wiki-only sources there's no separate
  daily backbone — the `prices_5m_daily` continuous aggregate (below) *is* the daily
  view, capped at the 5m floor (~Mar 2021).
- **`events` is plain, indexed on `occurred`.** It powers before/after window queries
  against `prices_5m` (e.g. "price path in the 6h after event E").

### Continuous aggregates: hourly & daily rollups (recent data)

Research runs on raw 5m; zoomed-out queries run cheap off rollups that **auto-refresh**
as new 5m data lands.

```sql
CREATE MATERIALIZED VIEW prices_1h
WITH (timescaledb.continuous) AS
SELECT time_bucket('1 hour', ts) AS hour,
       item_id,
       max(avg_high_price)            AS high,
       min(avg_low_price)             AS low,
       last(avg_low_price, ts)        AS close,
       sum(coalesce(high_volume,0)
         + coalesce(low_volume,0))    AS volume
FROM prices_5m
GROUP BY hour, item_id
WITH NO DATA;

SELECT add_continuous_aggregate_policy('prices_1h',
  start_offset      => INTERVAL '3 days',
  end_offset        => INTERVAL '1 hour',
  schedule_interval => INTERVAL '1 hour');
```

(A daily continuous aggregate follows the same pattern with `time_bucket('1 day', ts)`.)
Note `coalesce(volume,0)` is fine for *summing volume* — but never coalesce the
*prices*, per the keep-nulls rule.
`time_bucket`: https://www.tigerdata.com/docs/api/latest/hyperfunctions/time_bucket/ ·
continuous aggregate policy: https://www.tigerdata.com/docs/api/latest/continuous-aggregates/add_continuous_aggregate_policy/

---

## 7. Data source: Wiki Real-time Prices API

One source, RuneLite-partnered. The only source with the high/low spread and volume.
Hard floor: data starts **March 2021** (retention confirmed to reach it). Endpoints:

- **`/mapping`** — all items + metadata → populates `items`.
- **`/5m?timestamp=T`** — returns **every item** for the 5-minute block at `T` in one
  request. Crucial design consequence: **you loop over time, not over items** — page
  `T` backward in 300s steps (snapped to a 5m boundary) to backfill.
  Confirmed response shape (from the Apr 2021 probe):
  ```json
  {
    "data": {
      "2":  { "avgHighPrice": 145, "highPriceVolume": 88554,
              "avgLowPrice": 142, "lowPriceVolume": 17896 },
      "12": { "avgHighPrice": 199800, "highPriceVolume": 1,
              "avgLowPrice": null,    "lowPriceVolume": 0 }
    },
    "timestamp": 1617235200
  }
  ```
  `data` is keyed by `item_id`; the block's `timestamp` (Unix) is **echoed at top
  level** → `to_timestamp()` into `ts` (don't trust only the request value). Map
  `avgHighPrice→avg_high_price`, etc.
- **`/1h`** — same shape, hourly windows.
- **`/latest`** — instantaneous last-transaction prices per item:
  `{high, highTime, low, lowTime}`. **No volume, no averaging.** Polled every minute
  → `prices_1m` (forward-only). Returns *last-known* values even when nothing traded,
  so dedup on `highTime`/`lowTime` to avoid storing identical rows each minute.

**Etiquette / requirement:** set a descriptive `User-Agent` identifying the project
and a contact, or risk being blocked. No hard rate limit, but don't hammer it.

- Overview: https://oldschool.runescape.wiki/w/RuneScape:Real-time_Prices
- FAQs / usage rules: https://prices.runescape.wiki/osrs/faqs

---

## 8. Running it

```bash
# 1. one-time: create .env with a password (see section 5)
# 2. start (first run executes init/*.sql)
docker compose up -d

# 3. watch health / logs
docker compose ps
docker compose logs -f db

# 4. connect from the host
psql "postgresql://ge-data:${POSTGRES_PASSWORD}@localhost:5000/ge-data"
#   then: \conninfo      -> should say port 5000
#         \dx            -> timescaledb listed
#         \d+ prices_5m  -> shows hypertable
```

Re-running schema changes after first boot (init scripts won't re-run):

```bash
psql "postgresql://...@localhost:5000/ge-data" -f init/02_something.sql
```

To wipe and start clean (DESTROYS DATA):

```bash
docker compose down -v   # -v removes the named volume
```

`docker compose` reference: https://docs.docker.com/reference/cli/docker/compose/

---

## 9. Appendix: the localhost trap (a debugging story)

Worth understanding because it explains why we moved to a container.

The nix dev scripts (`__pg_bootstrap`) depend on two env vars — `PGDATA` (data dir)
and `PGPORT` (port) — that nothing in the project set. With both empty:

1. `initdb ""` failed silently (no data dir created).
2. `psql -p ""` fell back to the **default port 5432**.
3. On this machine, 5432 is the **system PostgreSQL** — a totally different server.
4. That server challenged for the OS user's password → the confusing
   `password authentication failed for user "jade"`.

We were talking to the wrong database the whole time. Lessons baked into the
current design: pick a non-default port (5000), make data location explicit (named
volume), and use a service built to be a service (the container) rather than a
local-dev convenience.

PostgreSQL connection defaults & env vars:
https://www.postgresql.org/docs/16/libpq-envars.html ·
`listen_addresses` / `pg_hba.conf` (relevant if you ever expose it):
https://www.postgresql.org/docs/16/runtime-config-connection.html ·
https://www.postgresql.org/docs/16/auth-pg-hba-conf.html

---

## Open decisions

From GOAL.md, plus one that affects this doc's compose:

1. ~~**Retention depth probe.**~~ **RESOLVED** — `/5m?timestamp=1617235200` (Apr 1
   2021) returns a full block (1,614 items). Retention reaches the March 2021 floor.

2. **5m backfill depth — open.** How far back to page `/5m`: the full ~4 yr to the
   ~Mar 2021 floor (~1.5B rows, ~9 GB; maximizes swing-layer depth, now the only
   source of daily history) vs. ~2 yr (lighter). Cheap either way; leaning toward the
   full floor. Decide before kicking off backfill.

3. **Polling clients — host processes or other containers?**
   → publish host port 5000, or keep the DB on the internal compose network only.

---

## References (all primary sources)

**TimescaleDB / Tiger Data**
- Docs home: https://www.tigerdata.com/docs
- Hypertables & chunks: https://www.tigerdata.com/docs/api/latest/hypertable
- `create_hypertable()`: https://www.tigerdata.com/docs/api/latest/hypertable/create_hypertable/
- Compression (columnstore/hypercore): https://www.tigerdata.com/docs/build/columnar-storage/
- `add_compression_policy()` (2.17): https://www.tigerdata.com/docs/api/latest/compression/add_compression_policy/
- `add_columnstore_policy()` (2.18+): https://www.tigerdata.com/docs/api/latest/hypercore/add_columnstore_policy
- Continuous aggregates: https://www.tigerdata.com/docs/use-timescale/latest/continuous-aggregates/about-continuous-aggregates
- Self-hosted Docker install: https://www.tigerdata.com/docs/self-hosted/latest/install/installation-docker/
- Docker image: https://hub.docker.com/r/timescale/timescaledb

**PostgreSQL 16**
- Generated columns: https://www.postgresql.org/docs/16/ddl-generated-columns.html
- Date/time types: https://www.postgresql.org/docs/16/datatype-datetime.html
- Foreign keys: https://www.postgresql.org/docs/16/ddl-constraints.html#DDL-CONSTRAINTS-FK
- BRIN indexes: https://www.postgresql.org/docs/16/brin.html
- Connection env vars: https://www.postgresql.org/docs/16/libpq-envars.html
- `pg_hba.conf`: https://www.postgresql.org/docs/16/auth-pg-hba-conf.html

**Docker / Compose**
- Compose file reference: https://docs.docker.com/reference/compose-file/
- Postgres image docs: https://github.com/docker-library/docs/blob/master/postgres/README.md
- Volumes: https://docs.docker.com/engine/storage/volumes/
- Compose networking: https://docs.docker.com/compose/how-tos/networking/

**OSRS data sources**
- Wiki Real-time Prices overview: https://oldschool.runescape.wiki/w/RuneScape:Real-time_Prices
- Wiki API FAQs / rules: https://prices.runescape.wiki/osrs/faqs
- `INSERT ... ON CONFLICT` (idempotent writes): https://www.postgresql.org/docs/16/sql-insert.html#SQL-ON-CONFLICT

**nix (your dev environment)**
- PostgreSQL + extensions in nixpkgs: https://wiki.nixos.org/wiki/PostgreSQL
- `mkShell` / shell environments: https://nixos.org/manual/nixpkgs/stable/#sec-pkgs-mkShell
- direnv `use nix`: https://github.com/nix-community/nix-direnv
