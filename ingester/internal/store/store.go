// Package store is the Postgres/TimescaleDB persistence layer for the ingester.
// Schema lives in ../../init/01_schema.sql.
package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store wraps a pgx connection pool.
type Store struct {
	pool *pgxpool.Pool
}

// New opens a pool against dsn (a postgres:// URL) and verifies connectivity.
func New(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{pool: pool}, nil
}

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// Item mirrors a row of the items table.
type Item struct {
	ID       int
	Name     string
	Members  bool
	Value    int
	Lowalch  int
	Highalch int
	Limit    int
	Examine  string
}

// UpsertItems inserts/updates item metadata. Idempotent on item_id.
func (s *Store) UpsertItems(ctx context.Context, items []Item) error {
	const q = `
		INSERT INTO items (item_id, name, members, value, lowalch, highalch, buy_limit, examine)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (item_id) DO UPDATE SET
			name=EXCLUDED.name, members=EXCLUDED.members, value=EXCLUDED.value,
			lowalch=EXCLUDED.lowalch, highalch=EXCLUDED.highalch,
			buy_limit=EXCLUDED.buy_limit, examine=EXCLUDED.examine`
	batch := &pgx.Batch{}
	for _, it := range items {
		batch.Queue(q, it.ID, it.Name, it.Members, it.Value, it.Lowalch, it.Highalch, it.Limit, it.Examine)
	}
	return s.sendBatch(ctx, batch, len(items))
}

// ConflictMode selects ON CONFLICT behavior for 5m writes.
type ConflictMode int

const (
	// DoNothing: closed historical blocks are immutable (backfill).
	DoNothing ConflictMode = iota
	// DoUpdate: the live block keeps settling; converge to its latest value.
	DoUpdate
)

// Price5m mirrors a row of prices_5m. Prices/volumes are nullable.
type Price5m struct {
	TS      time.Time
	ItemID  int
	AvgHigh *int64
	AvgLow  *int64
	HighVol *int64
	LowVol  *int64
}

// UpsertPrices5m writes 5m rows with the given conflict behavior.
func (s *Store) UpsertPrices5m(ctx context.Context, rows []Price5m, mode ConflictMode) error {
	conflict := "DO NOTHING"
	if mode == DoUpdate {
		conflict = `DO UPDATE SET
			avg_high_price=EXCLUDED.avg_high_price, avg_low_price=EXCLUDED.avg_low_price,
			high_volume=EXCLUDED.high_volume, low_volume=EXCLUDED.low_volume`
	}
	q := `INSERT INTO prices_5m (ts,item_id,avg_high_price,avg_low_price,high_volume,low_volume)
		VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (ts,item_id) ` + conflict
	batch := &pgx.Batch{}
	for _, r := range rows {
		batch.Queue(q, r.TS, r.ItemID, r.AvgHigh, r.AvgLow, r.HighVol, r.LowVol)
	}
	return s.sendBatch(ctx, batch, len(rows))
}

// Price1m mirrors a row of prices_1m (instantaneous, no volume).
type Price1m struct {
	TS       time.Time
	ItemID   int
	High     *int64
	HighTime *time.Time
	Low      *int64
	LowTime  *time.Time
}

// InsertPrices1m writes 1m rows. Callers pass only changed rows (dedup-on-change);
// DO NOTHING guards against a re-poll within the same minute.
func (s *Store) InsertPrices1m(ctx context.Context, rows []Price1m) error {
	const q = `INSERT INTO prices_1m (ts,item_id,high,high_time,low,low_time)
		VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (ts,item_id) DO NOTHING`
	batch := &pgx.Batch{}
	for _, r := range rows {
		batch.Queue(q, r.TS, r.ItemID, r.High, r.HighTime, r.Low, r.LowTime)
	}
	return s.sendBatch(ctx, batch, len(rows))
}

// EarliestPrice5m returns the oldest ts in prices_5m, used to resume backfill.
// ok is false when the table is empty.
func (s *Store) EarliestPrice5m(ctx context.Context) (t time.Time, ok bool, err error) {
	var ts *time.Time
	if err = s.pool.QueryRow(ctx, `SELECT min(ts) FROM prices_5m`).Scan(&ts); err != nil {
		return time.Time{}, false, err
	}
	if ts == nil {
		return time.Time{}, false, nil
	}
	return *ts, true, nil
}

func (s *Store) sendBatch(ctx context.Context, batch *pgx.Batch, n int) error {
	if n == 0 {
		return nil
	}
	br := s.pool.SendBatch(ctx, batch)
	defer br.Close()
	for range n {
		if _, err := br.Exec(); err != nil {
			return err
		}
	}
	return nil
}
