# ge-data ingester — implementation checklist

The Go service that does the work in [`GOAL.md`](./GOAL.md): two polls + a mapping
load, writing into the TimescaleDB from [`database-setup.md`](./database-setup.md).

One long-running process, three independent loops (mapping / 5m / 1m). Exactly
**one instance** ever — duplicate pollers double the work and risk Wiki-API blocks.

## Suggested layout

```
ingester/
  go.mod                       module github.com/osrs-ge/ge-data/ingester (go 1.26, pgx/v5)
  main.go                      wire config + store + client, start the 3 loops, graceful shutdown
  internal/
    config/config.go           env -> Config
    wiki/client.go             HTTP client + /mapping, /5m, /latest + response types
    store/store.go             pgxpool + UpsertItems / UpsertPrices5m / InsertPrices1m
    collect/collect.go         the 3 loops (mapping daily, 5m, 1m) + 1m dedup state
```

## Checklist

### 0. Bootstrap
- [ ] `cd ingester && go mod init github.com/osrs-ge/ge-data/ingester`
- [ ] `go get github.com/jackc/pgx/v5/pgxpool`
- [ ] DB up (`docker compose up -d`), schema loaded (first boot runs `init/01_schema.sql`)

### 1. Config (`internal/config`)
- [ ] Read `DATABASE_URL`, `USER_AGENT` (both required — fail fast if unset)
- [ ] Intervals with defaults: 1m, 5m, mapping 24h (constants are fine for v1)

### 2. Wiki client (`internal/wiki`)
- [ ] One `*http.Client` with a sane timeout; set `User-Agent` on **every** request
- [ ] `Mapping(ctx) ([]Item, error)` — GET `/mapping`
- [ ] `Prices5m(ctx) (Block5m, error)` — GET `/5m` (latest block; carries volume)
- [ ] `Latest(ctx) (Latest, error)` — GET `/latest` (instantaneous, no volume)
- [ ] Nullable numbers as `*int64` (a null price = nothing traded that side)
- [ ] Treat non-200 as an error; log + skip the tick, never crash the loop

### 3. Store (`internal/store`)
- [ ] `New(ctx, url) (*Store, error)` — `pgxpool.New`
- [ ] `UpsertItems(ctx, []Item)` — `ON CONFLICT (item_id) DO UPDATE` (never truncate)
- [ ] `UpsertPrices5m(ctx, ts, rows)` — `ON CONFLICT (ts,item_id) DO UPDATE` (block settles)
- [ ] `InsertPrices1m(ctx, ts, rows)` — `ON CONFLICT (ts,item_id) DO NOTHING`
- [ ] Batch writes (`pgx.Batch`); ~4.5k rows/tick is one batch

### 4. Collectors (`internal/collect`)
- [ ] Mapping loop: run once at startup, then every 24h
- [ ] 5m loop: tick every 5m, fetch `/5m`, upsert at the block's own `timestamp`
- [ ] 1m loop: tick every 1m, fetch `/latest`, **dedup on change**, insert
  - keep `map[int]struct{ highTime, lowTime int64 }`; only write rows where
    `highTime`/`lowTime` advanced since last seen for that item
- [ ] Each loop logs errors and keeps going (one bad fetch ≠ dead process)

### 5. main + lifecycle
- [ ] `signal.NotifyContext` for SIGINT/SIGTERM; cancel ctx → loops exit, pool closes
- [ ] Start the 3 loops as goroutines; `errgroup` or a `sync.WaitGroup` to join
- [ ] Structured-ish logging (`log/slog`)

### 6. Ship
- [ ] Re-add a small `Dockerfile` (multi-stage, static binary)
- [ ] Re-add the `ingester` service to `docker-compose.yml`
      (`DATABASE_URL=postgres://ge-data:$POSTGRES_PASSWORD@db:5432/ge-data`,
       `USER_AGENT`, `restart: unless-stopped`, `depends_on: db healthy`)

---

## Scaffolding

### `internal/config/config.go`
```go
package config

import (
	"fmt"
	"os"
	"time"
)

type Config struct {
	DatabaseURL string
	UserAgent   string

	MappingInterval time.Duration
	Poll5mInterval  time.Duration
	Poll1mInterval  time.Duration
}

func Load() (Config, error) {
	c := Config{
		DatabaseURL:     os.Getenv("DATABASE_URL"),
		UserAgent:       os.Getenv("USER_AGENT"),
		MappingInterval: 24 * time.Hour,
		Poll5mInterval:  5 * time.Minute,
		Poll1mInterval:  1 * time.Minute,
	}
	if c.DatabaseURL == "" {
		return c, fmt.Errorf("DATABASE_URL is required")
	}
	if c.UserAgent == "" {
		return c, fmt.Errorf("USER_AGENT is required (Wiki API blocks blank UAs)")
	}
	return c, nil
}
```

### `internal/wiki/client.go`
```go
package wiki

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

const base = "https://prices.runescape.wiki/api/v1/osrs"

type Client struct {
	http      *http.Client
	userAgent string
}

func New(userAgent string) *Client {
	return &Client{http: &http.Client{Timeout: 30 * time.Second}, userAgent: userAgent}
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", c.userAgent) // required on every request
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// ---- /mapping ----
// One item per row, all fields fixed. Maps 1:1 to the `items` table
// (id -> item_id, limit -> buy_limit).
type Item struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	Examine  string `json:"examine"`
	Members  bool   `json:"members"`
	Value    int    `json:"value"`
	LowAlch  int    `json:"lowalch"`
	HighAlch int    `json:"highalch"`
	Limit    int    `json:"limit"`
	Icon     string `json:"icon"`
}

func (c *Client) Mapping(ctx context.Context) ([]Item, error) {
	var out []Item
	return out, c.get(ctx, "/mapping", &out)
}

// ---- /5m ----  prices keyed by item id, *int64 because a side can be null.
type Avg5m struct {
	AvgHighPrice    *int64 `json:"avgHighPrice"`
	HighPriceVolume *int64 `json:"highPriceVolume"`
	AvgLowPrice     *int64 `json:"avgLowPrice"`
	LowPriceVolume  *int64 `json:"lowPriceVolume"`
}

type Block5m struct {
	Data      map[string]Avg5m `json:"data"`
	Timestamp int64            `json:"timestamp"` // unix, block start -> ts
}

func (c *Client) Prices5m(ctx context.Context) (Block5m, error) {
	var out Block5m
	return out, c.get(ctx, "/5m", &out)
}

// ---- /latest ----  instantaneous; *Time fields are unix seconds, nullable.
type LatestItem struct {
	High     *int64 `json:"high"`
	HighTime *int64 `json:"highTime"`
	Low      *int64 `json:"low"`
	LowTime  *int64 `json:"lowTime"`
}

type Latest struct {
	Data map[string]LatestItem `json:"data"`
}

func (c *Client) Latest(ctx context.Context) (Latest, error) {
	var out Latest
	return out, c.get(ctx, "/latest", &out)
}
```

### `internal/store/store.go`
```go
package store

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct{ pool *pgxpool.Pool }

func New(ctx context.Context, url string) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close() { s.pool.Close() }

// UpsertItems: refresh the mapping. Never truncate — last-write-wins per id.
//
//	INSERT INTO items (item_id, name, examine, members, value, lowalch, highalch, buy_limit, icon)
//	VALUES ($1,...,$9)
//	ON CONFLICT (item_id) DO UPDATE SET
//	  name=EXCLUDED.name, examine=EXCLUDED.examine, members=EXCLUDED.members,
//	  value=EXCLUDED.value, lowalch=EXCLUDED.lowalch, highalch=EXCLUDED.highalch,
//	  buy_limit=EXCLUDED.buy_limit, icon=EXCLUDED.icon;
//
// TODO: build a pgx.Batch and SendBatch.
func (s *Store) UpsertItems(ctx context.Context /* , items []wiki.Item */) error {
	panic("TODO")
}

// UpsertPrices5m: the live block keeps settling, so DO UPDATE (last-write-wins).
//
//	INSERT INTO prices_5m (ts, item_id, avg_high_price, avg_low_price, high_volume, low_volume)
//	VALUES ($1,...,$6)
//	ON CONFLICT (ts, item_id) DO UPDATE SET
//	  avg_high_price=EXCLUDED.avg_high_price, avg_low_price=EXCLUDED.avg_low_price,
//	  high_volume=EXCLUDED.high_volume, low_volume=EXCLUDED.low_volume;
//
// Keep nulls as nulls — never coalesce a null price to 0.
func (s *Store) UpsertPrices5m(ctx context.Context /* , ts time.Time, rows ... */) error {
	panic("TODO")
}

// InsertPrices1m: dedup is done in the collector; just append.
//
//	INSERT INTO prices_1m (ts, item_id, high, high_time, low, low_time)
//	VALUES ($1,...,$6)
//	ON CONFLICT (ts, item_id) DO NOTHING;
func (s *Store) InsertPrices1m(ctx context.Context /* , ts time.Time, rows ... */) error {
	panic("TODO")
}
```

### `internal/collect/collect.go`
```go
package collect

import (
	"context"
	"log/slog"
	"time"
)

// tick runs fn now, then every d, until ctx is cancelled. Errors are logged,
// never fatal — one bad fetch must not kill the loop.
func tick(ctx context.Context, name string, d time.Duration, fn func(context.Context) error) {
	run := func() {
		if err := fn(ctx); err != nil {
			slog.Error("tick failed", "loop", name, "err", err)
		}
	}
	run() // fire immediately on startup
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

// MappingLoop: startup + every cfg.MappingInterval -> UpsertItems.
// Prices5mLoop: every 5m -> Prices5m, upsert at to_timestamp(block.Timestamp).
// Prices1mLoop: every 1m -> Latest, dedup on change, InsertPrices1m.
//
// Dedup state for 1m (in-memory; survives as long as the process does):
//
//	type seen struct{ highTime, lowTime int64 }
//	last := map[int]seen{}
//	// for each item: emit a row only if highTime or lowTime advanced vs last[id],
//	// then update last[id]. ts = time.Now().UTC().Truncate(time.Minute).
//
// TODO: implement the three loop funcs; each just wires client -> store via tick().
```

### `main.go`
```go
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/osrs-ge/ge-data/ingester/internal/config"
	"github.com/osrs-ge/ge-data/ingester/internal/store"
	"github.com/osrs-ge/ge-data/ingester/internal/wiki"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.New(ctx, cfg.DatabaseURL)
	if err != nil {
		slog.Error("store", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	client := wiki.New(cfg.UserAgent)
	_ = client // TODO: pass cfg, client, st into the three collect loops

	var wg sync.WaitGroup
	// wg.Add(3)
	// go func(){ defer wg.Done(); collect.MappingLoop(ctx, cfg, client, st) }()
	// go func(){ defer wg.Done(); collect.Prices5mLoop(ctx, cfg, client, st) }()
	// go func(){ defer wg.Done(); collect.Prices1mLoop(ctx, cfg, client, st) }()
	wg.Wait()
	slog.Info("shut down cleanly")
}
```

---

## Reference

| endpoint | shape | table | write |
|---|---|---|---|
| `/mapping` | `[]Item` | `items` | `ON CONFLICT (item_id) DO UPDATE` |
| `/5m` | `{data:{id:{avgHighPrice,...}}, timestamp}` | `prices_5m` | `ON CONFLICT (ts,item_id) DO UPDATE` |
| `/latest` | `{data:{id:{high,highTime,low,lowTime}}}` | `prices_1m` | dedup on change, `DO NOTHING` |

- Base URL: `https://prices.runescape.wiki/api/v1/osrs`
- `User-Agent` is mandatory on every call (project + contact).
- Nulls are signal: a null price means nothing traded that side. Never zero-fill.
