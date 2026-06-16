# ge-data

## Goal

Store OSRS Grand Exchange price data in a TimescaleDB (Postgres) timeseries
database, from **two polls** of the Wiki real-time prices API.

## The two polls

* **5m poll → `prices_5m`.** Hit `/5m` once every 5 minutes. Returns every item
  for the latest 5-minute block in one request: average high/low price plus
  volume. This is the only source with volume.
* **1m poll → `prices_1m`.** Hit `/latest` once a minute. Returns the
  instantaneous last-transaction price per item (`{high, highTime, low,
  lowTime}`) — no volume, no averaging. Forward-only; there is no history to
  backfill.

Both are forward-only: the DB starts empty and grows from when polling begins.

## Notes

* **Keep the nulls.** A price is null exactly when nothing traded that side
  (5m: volume is 0; 1m: never seen). Null is a liquidity signal, not missing
  data — never zero-fill prices.
* **1m dedup on change.** `/latest` returns last-known values every minute, so
  only insert a row when `high_time` or `low_time` advanced since the item's
  last row. Skips repeats, roughly halves storage.
* **Wiki API only.** Set a descriptive `User-Agent` (project + contact) or risk
  being blocked. No hard rate limit; don't hammer it.

See [`database-setup.md`](./database-setup.md) for the schema and how to run it.
