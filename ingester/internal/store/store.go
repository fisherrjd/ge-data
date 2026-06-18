// Package store wraps pgxpool and exposes the three write paths the ingester
// needs. The shape mirrors init/01_schema.sql: nullable prices stay nullable,
// the PK is (ts, item_id) for both price tables, and item_id is the only
// column the price tables share with `items`. There is NO FK from price to
// item, so a price can land before /mapping is loaded.
package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/osrs-ge/ge-data/ingester/internal/wiki"
)

type Store struct {
	pool *pgxpool.Pool
}

// New dials the pool with a small default config. The pool's default
// MaxConns (4) is fine for one ingester instance — three loops, each
// flushing once a minute or once every five minutes.
func New(ctx context.Context, url string) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close() { s.pool.Close() }

// ---------------------------------------------------------------------------
// UpsertItems
//   - Refresh the /mapping catalog. Never truncate: a failed mid-refresh
//     fetch would leave items empty and the price rows dangling. The ON
//     CONFLICT DO UPDATE means a partial refresh is safe: rows touched by
//     the failed batch keep their old values, rows not yet touched keep
//     theirs too.
//   - Schema: 9 columns in fixed order. Keeping them in the same order as
//     init/01_schema.sql makes the column list easy to cross-check.
// ---------------------------------------------------------------------------

const upsertItemsSQL = `
INSERT INTO items (item_id, name, examine, members, value, lowalch, highalch, buy_limit, icon)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (item_id) DO UPDATE SET
  name      = EXCLUDED.name,
  examine   = EXCLUDED.examine,
  members   = EXCLUDED.members,
  value     = EXCLUDED.value,
  lowalch   = EXCLUDED.lowalch,
  highalch  = EXCLUDED.highalch,
  buy_limit = EXCLUDED.buy_limit,
  icon      = EXCLUDED.icon
`

func (s *Store) UpsertItems(ctx context.Context, items []wiki.Item) error {
	if len(items) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, it := range items {
		batch.Queue(
			upsertItemsSQL,
			it.ID, it.Name, it.Examine, it.Members,
			it.Value, it.LowAlch, it.HighAlch, it.Limit, it.Icon,
		)
	}
	br := s.pool.SendBatch(ctx, batch)
	// Drain the batch so every queued statement actually executes. We don't
	// need the per-row result; a failed Exec returns its error directly. After
	// the loop, br.Close() flushes remaining results and reports any error
	// encountered on the batch as a whole (e.g. a connection failure mid-batch).
	for range items {
		if _, err := br.Exec(); err != nil {
			br.Close()
			return fmt.Errorf("upsert item: %w", err)
		}
	}
	return br.Close()
}

// ---------------------------------------------------------------------------
// UpsertPrices5m
//   - One row per (block-ts, item_id). The /5m endpoint's Timestamp is the
//     BLOCK START and stays the same for the whole 5-minute window, so
//     multiple polls in the same window hit the same row. The block keeps
//     settling (averages converge as more trades land) so we DO UPDATE —
//     the last poll of the window is the canonical value.
//   - All four price/volume fields are *int64. A nil here MUST stay NULL in
//     the row: the schema treats null prices as a liquidity signal (no
//     trade cleared on that side), not as a missing value. pgx handles
//     nil *int64 as SQL NULL natively; do not coalesce to 0.
// ---------------------------------------------------------------------------

const upsertPrices5mSQL = `
INSERT INTO prices_5m (ts, item_id, avg_high_price, avg_low_price, high_volume, low_volume)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (ts, item_id) DO UPDATE SET
  avg_high_price = EXCLUDED.avg_high_price,
  avg_low_price  = EXCLUDED.avg_low_price,
  high_volume    = EXCLUDED.high_volume,
  low_volume     = EXCLUDED.low_volume
`

// Prices5mRow is the row shape the collector builds from a /5m response.
// item_id is parsed from the JSON map key (the API returns string keys),
// the rest is the Avg5m payload verbatim. ts is the block's Timestamp
// converted to a UTC time.Time; we convert in the caller so the collector
// can pick the truncation policy.
type Prices5mRow struct {
	ItemID       int
	AvgHighPrice *int64
	HighVolume   *int64
	AvgLowPrice  *int64
	LowVolume    *int64
}

func (s *Store) UpsertPrices5m(ctx context.Context, ts any, rows []Prices5mRow) error {
	if len(rows) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, r := range rows {
		batch.Queue(
			upsertPrices5mSQL,
			ts, r.ItemID, r.AvgHighPrice, r.AvgLowPrice, r.HighVolume, r.LowVolume,
		)
	}
	br := s.pool.SendBatch(ctx, batch)
	for range rows {
		if _, err := br.Exec(); err != nil {
			br.Close()
			return fmt.Errorf("upsert 5m price: %w", err)
		}
	}
	return br.Close()
}

// ---------------------------------------------------------------------------
// InsertPrices1m
//   - Append-only. The collector has already filtered repeats (dedup on
//     high_time / low_time advance), so DO NOTHING is the right conflict
//     target: if the same (poll-ts, item_id) row already exists (a
//     restart-caught-up replay of the same minute), silently skip it.
//   - high_time / low_time are unix seconds from the API; the collector
//     converts them to time.Time before we get here. high / low are
//     nullable (null = no trade observed on that side yet).
// ---------------------------------------------------------------------------

const insertPrices1mSQL = `
INSERT INTO prices_1m (ts, item_id, high, high_time, low, low_time, margin)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (ts, item_id) DO NOTHING
`

type Prices1mRow struct {
	ItemID   int
	High     *int64
	HighTime any // time.Time; typed as any so the package doesn't import time just for one field
	Low      *int64
	LowTime  any
	// Margin is the post-tax flip margin (high - tax - low), computed by the
	// collector. nil whenever High or Low is nil; never coalesced to 0.
	Margin *int64
}

func (s *Store) InsertPrices1m(ctx context.Context, ts any, rows []Prices1mRow) error {
	if len(rows) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, r := range rows {
		batch.Queue(insertPrices1mSQL, ts, r.ItemID, r.High, r.HighTime, r.Low, r.LowTime, r.Margin)
	}
	br := s.pool.SendBatch(ctx, batch)
	for range rows {
		if _, err := br.Exec(); err != nil {
			br.Close()
			return fmt.Errorf("insert 1m price: %w", err)
		}
	}
	return br.Close()
}
