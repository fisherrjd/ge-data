# OSRS Market Research DB

## Goal

Build and maintain a TimescaleDB (Postgres) store of Old School RuneScape Grand Exchange price data, deep enough and granular enough to backtest market ideas and study how prices move.

Two research tracks share one database:

* **Fast flips (intraday).** Profit lives in the spread between instant buy (high) and instant sell (low) inside short windows. Needs 5m data, which carries both prices plus volume. This is the micro trend layer: study how a price absorbs a shock in the minutes and hours after news or an update.
* **Swing trades (up to 3 months).** Hold for weeks, so entry timing to the minute is noise. Daily price is enough. This is the layer that wants deep history.

The bigger ambition: pair the price data with a news and update timeline so we can measure how the market reacts to events, not just chart prices in isolation.

## Data sources

* **Wiki real time API** (`prices.runescape.wiki`). Two endpoints:
  * `/5m` returns every item for a given 5 minute block in one request, so we loop over time, not over items. The **only source with volume**. Supports `?timestamp=` for history back to the March 2021 floor (retention confirmed to reach it).
  * `/latest` returns instantaneous last-transaction prices per item: `{high, highTime, low, lowTime}`. **No volume, no averaging.** This is our forward-only 1-minute detail source (poll every 1m). There is no 1m history to backfill.

Wiki API only — no third-party sources. Daily swing data comes from rolling 5m up to
daily (continuous aggregate), so daily history is capped at the 5m floor (~Mar 2021),
not the deeper history a daily-scrape source could provide. Accepted tradeoff.

## Why these choices

* Calendar years is the wrong unit. What matters is matching data to holding period and to how many independent test windows we get. Flips give thousands of cycles in a year. A 3 month hold only gives about 12 non overlapping windows in 3 years, so the swing layer wants several years of depth. With Wiki-only sources, swing depth is bounded by how far back we backfill 5m — the floor is ~Mar 2021 (~4 yr). **Open: backfill the full ~4 yr to 2021 (favors swing window count) vs. just ~2 yr (lighter).** See open questions.
* OSRS is a nonstationary economy by design. Updates, new content, bot waves, and gold sinks reshape relationships over time, so old regimes are less representative. We weight recent data for testing and treat the deep past as robustness checking — which softens the cost of not having pre-2021 daily history.

## Components to build

1. **Mapping loader.** Fetch Wiki `/mapping`, upsert into `items` (id, name, members, alch, buy limit, etc.). Re-run periodically (e.g. daily) to pick up newly added items. `item_id` is canonical; name is display/search only. Run this first so metadata/buy limits are available, but prices don't depend on it (no FK).
2. **5m backfill.** Page `/5m?timestamp=T` backward from now (last ~2 years), T stepping by 300, snapped to a 5m boundary. Resumable via primary key. Closed blocks are immutable → `ON CONFLICT DO NOTHING`.
3. **5m live collector.** Cron a few minutes past each 5m mark, fetch the last two or three blocks, upsert. The live block settles, so use `ON CONFLICT DO UPDATE` (last-write-wins) → row converges to the final completed average. Banks our own history forward regardless of retention.
4. **1m collector (forward only).** Poll `/latest` every minute → `prices_1m`. Dedup **on change**: only insert when `high_time`/`low_time` advanced since the item's last row (skips last-known repeats; ~halves storage). No volume.
5. **Events table.** News and update timeline: event time, type, affected items, source, notes. Plain table, not a hypertable. Powers before and after window queries against the 5m data.

## Schema and storage notes

* `prices_5m`: hypertable on `ts`, PK `(ts, item_id)`, chunk interval 1 month. Compress segment by `item_id`, order by `ts`, after ~7 days.
* `prices_1m`: hypertable on `ts`, PK `(ts, item_id)`, chunk interval 1 week (denser than 5m). Forward-only, from `/latest`. Columns `high, high_time, low, low_time` — no volume.
* Compression on both: segment by `item_id`, order by `ts`. This is what keeps a multi billion row store small and fast.
* `avg_high_price` and `avg_low_price` (5m) are nullable BIGINT. **Keep the nulls.** Confirmed in real data: volume is never null, a price is null exactly when that side's volume is 0 (no trade cleared). Signal about liquidity, not missing data. Never zero fill prices. (Same for `high`/`low` in `prices_1m`: null = never seen.)
* Write semantics: **backfill** `DO NOTHING` (immutable closed blocks); **5m live** `DO UPDATE` (block settles); **1m** dedup on change.
* Batch backfill inserts (a few thousand rows per transaction) so the multi day backfill does not fight the live collector.
* Continuous aggregates to roll 5m into hourly and daily views. Research runs on raw 5m, zoomed out queries run cheap off the rollups.
* Storage ballpark (~2 yr forward, Timescale-compressed): `prices_5m` ~9 GB, `prices_1m` (dedup-on-change) ~10 GB → ~20 GB total. Provision ~100 GB for headroom (WAL, indexes, aggregates). Uncompressed this would be ~250-400 GB — compression is the whole game.

## Data handling reminders

* Nulls are no trade signal, handle gaps on purpose (forward fill, mark, or skip per analysis).
* Volume coverage is strong on recent data, thin or absent on deep daily history. Liquidity analysis is sharper on recent data. A 3 month position has to clear under GE buy limits without moving price, so volume matters as much as price.

## Resolved

* **Retention depth probe — RESOLVED.** `/5m?timestamp=1617235200` (Apr 1 2021)
  returns a full block: 1,614 items with data, top-level `timestamp` echoed back.
  Retention reaches the March 2021 floor, so we do the **full ~5-year backfill**
  (~450k requests, ~1.5B rows). Disk/chunking/compression are sized for that. The
  community-archive fallback is no longer a concern.
* **Null semantics confirmed in real data.** In the Apr 2021 block, ~28% of items
  have a null `avgHighPrice` (460/1614) or `avgLowPrice` (413/1614). Volume is
  **never** null (it's 0); a price is null **exactly when that side's volume is 0** —
  i.e. no trade cleared that side. Keep the nulls; never zero-fill prices.

## Open questions / next step

* **5m backfill depth.** How far back to page `/5m`? Floor is ~Mar 2021 (~4 yr,
  ~1.5B rows, ~9 GB compressed). Going to the floor maximizes swing-layer window
  count; ~2 yr is lighter. Cheap either way — leaning toward the full ~4 yr since
  it's the only source of daily depth now. Decide before kicking off backfill.
* **Next:** build the ingester (mapping loader → 5m backfill → live 5m + 1m collectors).
