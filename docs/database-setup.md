# ge-data: Database Setup

How the timeseries DB is built and run. See [`GOAL.md`](./GOAL.md) for *what*
we're collecting; this file covers the schema and the container.

## Why TimescaleDB

TimescaleDB is a PostgreSQL *extension* — same server, same SQL, same drivers.
We use it for two things on append-only, time-ordered, grows-forever data:

1. **Hypertables** — automatic time-based partitioning into "chunks", so inserts
   stay fast and range queries skip irrelevant time.
2. **Columnar compression** — old chunks get compressed hard (OSRS price rows
   are tiny and repetitive, the ideal case). This is the reason to use Timescale.

We run pg16 with the `timescaledb` extension preloaded — locally via the nix dev
shell, in production via eldo's NixOS `services.postgresql` (see
[`INFRA.md`](./INFRA.md)).

## Schema (`init/01_schema.sql`)

A static `items` lookup plus two hypertables, one per poll. The hypertables are
keyed `(ts, item_id)` and compressed (segment by `item_id`, order by `ts`) after
7 days.

- **`items`** — from `/mapping`, one typed column per field: `item_id` (the
  mapping's `id`), `name`, `examine`, `members`, `value`, `lowalch`, `highalch`,
  `buy_limit` (the mapping's `limit`), `icon`. Plain table, not a hypertable. No
  foreign key from the price tables, so a price can reference an item before
  `/mapping` is loaded. The whole feed is ~4.5k items / ~830 KB — one cheap GET.

  **Refresh:** upsert on startup, then again every ~24h while running. Always
  `INSERT ... ON CONFLICT (item_id) DO UPDATE` — never truncate-and-reload (that
  leaves a window where `items` is empty, and a failed fetch would nuke the table
  for nothing). New items land with game updates (~weekly), so a long-running
  poller needs the daily refresh, not just the startup load — but if the new item
  isn't in `items` yet it's harmless: prices still record under its `item_id`,
  it just has no name until the next refresh.

- **`prices_5m`** — from `/5m`. `avg_high_price`, `avg_low_price` (nullable),
  `high_volume`, `low_volume`. 1-month chunks.
- **`prices_1m`** — from `/latest`. `high`, `high_time`, `low`, `low_time`. No
  volume. Nullable prices. 1-week chunks (1m data is ~5× denser).

Write semantics:

- **5m** → `INSERT ... ON CONFLICT (ts, item_id) DO UPDATE`. The current block
  keeps settling while you poll it, so the row converges to the final average.
- **1m** → dedup on change: only insert when `high_time`/`low_time` advanced.

Prices store `item_id` only; join `items` when you want names.

## Running the DB locally (nix dev shell)

Local dev runs a real Postgres from the nix shell — no container. `default.nix`
provides a `pg16 + timescaledb` server plus two scripts: `db_reset` (one-shot
prepare) and `__pg` (serve). PGDATA is `.db/`, port **5433** (set by the shell
hook). **Production runs on eldo**, not here — see [`INFRA.md`](./INFRA.md).

```bash
# 1. create .env with a password (see .env.example)
# 2. enter the shell (exports PGDATA=.db, PGPORT=5433)
nix-shell   # or via direnv (.envrc)

# 3. wipe + bootstrap + load init/01_schema.sql (DESTROYS DATA in .db/)
db_reset

# 4. serve
__pg

# 5. connect from another shell
psql "postgresql://ge-data:${POSTGRES_PASSWORD}@localhost:5433/ge-data"
#   \dx            -> timescaledb listed
#   \d+ prices_5m  -> hypertable
```

`db_reset` loads the schema once against a temporary preloaded server; apply later
schema changes by hand:

```bash
psql "postgresql://...@localhost:5433/ge-data" -f init/02_something.sql
```

## References

- Wiki real-time prices: https://oldschool.runescape.wiki/w/RuneScape:Real-time_Prices
- Wiki API rules/FAQs: https://prices.runescape.wiki/osrs/faqs
- TimescaleDB docs: https://www.tigerdata.com/docs
- Timescale Docker image: https://hub.docker.com/r/timescale/timescaledb
- `INSERT ... ON CONFLICT`: https://www.postgresql.org/docs/16/sql-insert.html#SQL-ON-CONFLICT
