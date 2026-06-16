-- ge-data schema. See docs/database-setup.md (section 6) for the reasoning.
-- Runs ONCE, on first container boot (empty volume). It will NOT re-run later.
-- Column names / keys follow docs/GOAL.md exactly — do not drift casually.

CREATE EXTENSION IF NOT EXISTS timescaledb;

-- ---------------------------------------------------------------------------
-- items: static metadata, from the Wiki /mapping endpoint. One row per item.
--   item_id is the canonical key everywhere; name is for display/search only
--   (names aren't guaranteed unique/stable across game updates). Loaded by a
--   "mapping loader" that re-runs periodically to pick up newly added items.
-- ---------------------------------------------------------------------------
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

-- Case-insensitive lookups for name -> id (search/UI). Non-unique on purpose:
-- names are effectively unique but not guaranteed, so don't let a dup break the
-- mapping load. Make it UNIQUE only after confirming /mapping has no collisions.
CREATE INDEX items_name_lower_idx ON items (lower(name));

-- ---------------------------------------------------------------------------
-- prices_5m: 5-minute series, from the Wiki /5m endpoint. Intraday/flip layer.
--   - avg_* prices are NULLABLE on purpose: null + zero volume = "no trade
--     cleared this block". Keep the nulls; never zero-fill prices.
--   - bigint: high-value items / cumulative volume exceed int4.
--   - PK (ts, item_id) also enables idempotent INSERT ... ON CONFLICT.
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
-- prices_1m: 1-minute instantaneous prices, from the Wiki /latest endpoint.
-- FORWARD-ONLY (no 1m history exists to backfill). Intraday/flip detail layer.
--   - No volume: /latest returns only {high, highTime, low, lowTime}.
--   - high/low are NULLABLE: null = "never seen an instant buy/sell".
--   - high_time/low_time are when the actual transaction happened (the API
--     returns last-known prices, so these let us dedup "on change" — only
--     insert a row when high_time or low_time advanced since the last poll).
--   - ts is the poll minute. PK (ts, item_id).
--   - Denser than 5m, so 1-week chunks (keeps chunk row counts sane).
-- ---------------------------------------------------------------------------
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

-- ---------------------------------------------------------------------------
-- events: news & game-update timeline. Plain table (NOT a hypertable) — low
-- volume, queried by joining time windows against prices_5m.
-- ---------------------------------------------------------------------------
CREATE TABLE events (
  id        bigserial PRIMARY KEY,
  occurred  timestamptz NOT NULL,
  type      text,                 -- update | news | nerf | new_content | ...
  items     integer[],            -- affected item_ids, if known
  source    text,
  notes     text
);
CREATE INDEX ON events (occurred);

-- ---------------------------------------------------------------------------
-- Continuous aggregates: auto-refreshing rollups of recent 5m data.
-- Research runs on raw 5m; zoomed-out queries run cheap off these.
-- NOTE: coalesce volume to 0 for summing, but NEVER coalesce prices.
-- ---------------------------------------------------------------------------
CREATE MATERIALIZED VIEW prices_1h
WITH (timescaledb.continuous) AS
SELECT time_bucket('1 hour', ts) AS hour,
       item_id,
       max(avg_high_price)         AS high,
       min(avg_low_price)          AS low,
       last(avg_low_price, ts)     AS close,
       sum(coalesce(high_volume,0)
         + coalesce(low_volume,0)) AS volume
FROM prices_5m
GROUP BY hour, item_id
WITH NO DATA;

SELECT add_continuous_aggregate_policy('prices_1h',
  start_offset      => INTERVAL '3 days',
  end_offset        => INTERVAL '1 hour',
  schedule_interval => INTERVAL '1 hour');

CREATE MATERIALIZED VIEW prices_5m_daily
WITH (timescaledb.continuous) AS
SELECT time_bucket('1 day', ts) AS day,
       item_id,
       max(avg_high_price)         AS high,
       min(avg_low_price)          AS low,
       last(avg_low_price, ts)     AS close,
       sum(coalesce(high_volume,0)
         + coalesce(low_volume,0)) AS volume
FROM prices_5m
GROUP BY day, item_id
WITH NO DATA;

SELECT add_continuous_aggregate_policy('prices_5m_daily',
  start_offset      => INTERVAL '3 days',
  end_offset        => INTERVAL '1 hour',
  schedule_interval => INTERVAL '1 hour');
