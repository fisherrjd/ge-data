// Package collect wires the wiki.Client to the store.Store on three
// independent loops. Each loop is a `tick` over a fixed interval: the wiki
// fetch is the body, the store write is the tail, and a single bad fetch is
// logged and skipped — the loop must keep running across transient errors.
package collect

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"github.com/osrs-ge/ge-data/ingester/internal/config"
	"github.com/osrs-ge/ge-data/ingester/internal/store"
	"github.com/osrs-ge/ge-data/ingester/internal/wiki"
)

// Deps groups the wiki client, the store, and the configured intervals. One
// struct so the three loops share the same plumbing without each having to
// re-plumb config.
type Deps struct {
	Cfg    config.Config
	Client *wiki.Client
	Store  *store.Store
}

// tick runs fn now, then every d, until ctx is cancelled. Errors are logged
// and the loop continues — one bad fetch must not kill the process. We fire
// once immediately so a fresh start doesn't wait a full interval before the
// first write (a /5m poll that waits 5 minutes on cold start is a bug).
func tick(ctx context.Context, name string, d time.Duration, fn func(context.Context) error) {
	run := func() {
		if err := fn(ctx); err != nil {
			slog.Error("tick failed", "loop", name, "err", err)
		}
	}
	run()
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run()
		}
	}
}

// ---------------------------------------------------------------------------
// MappingLoop: refresh /mapping on startup and once a day. The catalog is
// stable for long stretches (new items land with weekly game updates), so
// the daily cadence is plenty. A failure here is recoverable on the next
// tick — the items table is upsert-keyed and a partial refresh is safe.
// ---------------------------------------------------------------------------

func MappingLoop(ctx context.Context, d Deps) {
	tick(ctx, "mapping", d.Cfg.MappingInterval, func(ctx context.Context) error {
		items, err := d.Client.Mapping(ctx)
		if err != nil {
			return err
		}
		if err := d.Store.UpsertItems(ctx, items); err != nil {
			return err
		}
		slog.Info("mapping refreshed", "rows", len(items))
		return nil
	})
}

// ---------------------------------------------------------------------------
// Prices5mLoop: every 5 minutes, fetch the latest block and upsert at the
// BLOCK's own timestamp (not the wall clock). The Wiki /5m block keeps
// settling for a few minutes after start, so multiple polls in the same
// 5-minute window land on the same row — DO UPDATE converges on the
// final average. The block's `Timestamp` is unix seconds; we convert to
// UTC and let Postgres store it as timestamptz.
//
// One subtlety: on the first poll of a new block, you can also see the
// previous block's stale `Timestamp` if the Wiki hasn't rolled it over
// yet. That's harmless — we'll just re-upsert the old row, which is the
// same value we already have.
// ---------------------------------------------------------------------------

func Prices5mLoop(ctx context.Context, d Deps) {
	tick(ctx, "prices_5m", d.Cfg.Poll5mInterval, func(ctx context.Context) error {
		block, err := d.Client.Prices5m(ctx)
		if err != nil {
			return err
		}
		ts := time.Unix(block.Timestamp, 0).UTC()
		rows := make([]store.Prices5mRow, 0, len(block.Data))
		for idStr, p := range block.Data {
			id, err := strconv.Atoi(idStr)
			if err != nil {
				// Skip junk keys rather than abort the whole batch; one
				// malformed item shouldn't poison a 4.5k-row tick.
				slog.Warn("skipping non-int item id in /5m", "id", idStr)
				continue
			}
			rows = append(rows, store.Prices5mRow{
				ItemID:       id,
				AvgHighPrice: p.AvgHighPrice,
				HighVolume:   p.HighPriceVolume,
				AvgLowPrice:  p.AvgLowPrice,
				LowVolume:    p.LowPriceVolume,
			})
		}
		if err := d.Store.UpsertPrices5m(ctx, ts, rows); err != nil {
			return err
		}
		slog.Info("5m block written", "ts", ts, "rows", len(rows))
		return nil
	})
}

// ---------------------------------------------------------------------------
// Prices1mLoop: every minute, fetch /latest and dedup-on-change. The
// in-memory `last` map is the entire dedup state — it survives as long as
// the process does and is rebuilt from scratch on restart (which is fine:
// restart just means the first tick after restart can emit a duplicate
// row at the same minute; the schema's DO NOTHING handles that).
//
// Dedup rule (per docs/GOAL.md): emit a row only when high_time or low_time
// for an item advanced since the last emit. We track those unix seconds
// in `last`; an item is "first seen" if it's not in the map yet, and we
// always emit in that case.
//
// ts for the row is the POLL minute (now truncated), not the high_time /
// low_time — the time we observed the change. high_time / low_time go in
// their own columns as the time the underlying trade happened.
// ---------------------------------------------------------------------------

type lastSeen struct {
	highTime int64 // unix seconds, 0 if never seen
	lowTime  int64
}

func Prices1mLoop(ctx context.Context, d Deps) {
	last := make(map[int]lastSeen)
	tick(ctx, "prices_1m", d.Cfg.Poll1mInterval, func(ctx context.Context) error {
		latest, err := d.Client.Latest(ctx)
		if err != nil {
			return err
		}
		ts := time.Now().UTC().Truncate(time.Minute)
		rows := make([]store.Prices1mRow, 0, len(latest.Data))
		for idStr, p := range latest.Data {
			id, err := strconv.Atoi(idStr)
			if err != nil {
				slog.Warn("skipping non-int item id in /latest", "id", idStr)
				continue
			}
			ht, lt := int64(0), int64(0)
			if p.HighTime != nil {
				ht = *p.HighTime
			}
			if p.LowTime != nil {
				lt = *p.LowTime
			}
			prev, seen := last[id]
			// Emit if this is the first time we see the item, OR if either
			// timestamp advanced. The 0-sentinel works because real trade
			// times are post-2010 unix seconds — never 0.
			if !seen || ht > prev.highTime || lt > prev.lowTime {
				rows = append(rows, store.Prices1mRow{
					ItemID:   id,
					High:     p.High,
					Low:      p.Low,
					HighTime: unixToTime(p.HighTime),
					LowTime:  unixToTime(p.LowTime),
					Margin:   flipMargin(p.High, p.Low),
				})
				last[id] = lastSeen{highTime: ht, lowTime: lt}
			}
		}
		if err := d.Store.InsertPrices1m(ctx, ts, rows); err != nil {
			return err
		}
		slog.Info("1m tick written", "ts", ts, "candidates", len(latest.Data), "written", len(rows))
		return nil
	})
}

// maxGETax is the per-item cap on Grand Exchange tax: 5,000,000 coins. The cap
// binds at a sale price of 250,000,000 (250M / 50 == 5M).
const maxGETax = 5_000_000

// flipMargin is the post-tax profit of a flip: buy at low, sell at high. GE tax
// (2% of the sale, floored, capped at 5M) hits the SELL leg only and equals
// high/50 exactly — 2% with floor-rounding is integer division by 50, so the
// "no tax under 50 coins / multiples of 50" behavior falls out for free. The
// buy leg is untaxed. Returns nil if either side is missing (can't price a flip
// with one leg); the result can be negative for illiquid items.
//
// The /50 encodes the 2% rate in effect since 2025-05-29; older rates would
// need a date-aware divisor, which only matters if we ever backfill history.
func flipMargin(high, low *int64) *int64 {
	if high == nil || low == nil {
		return nil
	}
	tax := *high / 50
	if tax > maxGETax {
		tax = maxGETax
	}
	m := *high - tax - *low
	return &m
}

// unixToTime converts a nullable unix-seconds pointer (the /latest shape) to
// a nullable time.Time. nil in -> nil out, never the zero time.
func unixToTime(unixSec *int64) any {
	if unixSec == nil {
		return nil
	}
	return time.Unix(*unixSec, 0).UTC()
}
