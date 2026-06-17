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

We run the official `timescale/timescaledb` Docker image, which preloads the
extension and ships compression enabled.

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

## Container (`docker-compose.yml`)

```yaml
services:
  db:
    image: timescale/timescaledb:2.17.2-pg16   # pin a real tag; never :latest
    environment:
      POSTGRES_DB: ge-data
      POSTGRES_USER: ge-data
      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD:?set POSTGRES_PASSWORD in .env}
    ports:
      - "5000:5432"                              # host:container
    volumes:
      - pgdata:/var/lib/postgresql/data          # data survives rebuilds
      - ./init:/docker-entrypoint-initdb.d:ro    # schema, runs ONCE on first boot
```

- **Named volume `pgdata`** — data lives outside the container lifecycle.
- **`./init` mount** — runs only on first boot (empty volume); it will **not**
  re-run for later migrations.
- **Port 5000**, not 5432, to avoid a system Postgres already on 5432.

## Running it

```bash
# 1. create .env with a password (see .env.example)
# 2. start (first run executes init/*.sql)
docker compose up -d

# 3. connect from the host
psql "postgresql://ge-data:${POSTGRES_PASSWORD}@localhost:5000/ge-data"
#   \dx            -> timescaledb listed
#   \d+ prices_5m  -> hypertable

# wipe and start clean (DESTROYS DATA):
docker compose down -v
```

Apply later schema changes by hand (init scripts won't re-run):

```bash
psql "postgresql://...@localhost:5000/ge-data" -f init/02_something.sql
```

## References

- Wiki real-time prices: https://oldschool.runescape.wiki/w/RuneScape:Real-time_Prices
- Wiki API rules/FAQs: https://prices.runescape.wiki/osrs/faqs
- TimescaleDB docs: https://www.tigerdata.com/docs
- Timescale Docker image: https://hub.docker.com/r/timescale/timescaledb
- `INSERT ... ON CONFLICT`: https://www.postgresql.org/docs/16/sql-insert.html#SQL-ON-CONFLICT
