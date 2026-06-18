-- ge-data schema. See docs/database-setup.md for the reasoning.
-- Runs ONCE, on first container boot (empty volume). It will NOT re-run later.

CREATE EXTENSION IF NOT EXISTS timescaledb;

-- ---------------------------------------------------------------------------
-- items: full item metadata from the Wiki /mapping endpoint, one column per
-- field (the /mapping shape is small and fixed). Plain table, NOT a hypertable.
-- The price tables do not depend on this table (no FK), so a price can reference
-- an item before /mapping is loaded. Re-run /mapping occasionally
-- (ON CONFLICT (item_id) DO UPDATE) to catch new items / changed metadata.
--   buy_limit <- /mapping "limit" (limit is a SQL reserved word).
-- ---------------------------------------------------------------------------
CREATE TABLE items (
  item_id   integer PRIMARY KEY,   -- /mapping "id"
  name      text NOT NULL,
  examine   text,
  members   boolean,
  value     integer,
  lowalch   integer,
  highalch  integer,
  buy_limit integer,               -- /mapping "limit"
  icon      text
);
CREATE INDEX items_name_lower_idx ON items (lower(name));  -- name -> id search

-- ---------------------------------------------------------------------------
-- prices_5m: 5-minute series, from the Wiki /5m endpoint. Carries volume.
--   - avg_* prices are NULLABLE on purpose: a price is null exactly when that
--     side's volume is 0 (no trade cleared). Keep the nulls; never zero-fill.
--   - bigint: high-value items / cumulative volume exceed int4.
--   - PK (ts, item_id) enables idempotent INSERT ... ON CONFLICT.
-- ---------------------------------------------------------------------------
CREATE TABLE prices_5m (
  ts             timestamptz NOT NULL,
  item_id        integer     NOT NULL,
  avg_high_price bigint,
  avg_low_price  bigint,
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

-- ---------------------------------------------------------------------------
-- prices_1m: instantaneous prices, polled every minute from the Wiki /latest
-- endpoint. No volume (/latest returns only {high, highTime, low, lowTime}).
--   - high/low are NULLABLE: null = never seen an instant buy/sell.
--   - high_time/low_time are when the trade actually happened. /latest returns
--     last-known values every minute, so only insert when one of them advanced
--     (dedup on change) to avoid storing identical rows.
--   - ts is the poll minute. PK (ts, item_id). Denser than 5m, so 1-week chunks.
--   - margin is the post-tax flip margin (sell at high, buy at low), computed
--     by the ingester, NOT a generated column: NULL when either side is NULL.
--     GE tax is 2% of the sale floored, capped at 5M, charged to the SELLER
--     only -- exactly high/50 (integer division). So
--       margin = high - LEAST(high/50, 5000000) - low.
--     Can be negative for illiquid items (last insta-sell above last insta-buy)
--     and is "never simultaneously real": high/low can be from different times
--     (see high_time/low_time). The /50 encodes 2% as of the 2025-05-29 rate
--     change; backfill of older rows would need a date-aware rate.
-- ---------------------------------------------------------------------------
CREATE TABLE prices_1m (
  ts        timestamptz NOT NULL,
  item_id   integer     NOT NULL,
  high      bigint,
  high_time timestamptz,
  low       bigint,
  low_time  timestamptz,
  margin    bigint,
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
