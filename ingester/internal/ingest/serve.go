package ingest

import (
	"context"
	"flag"
	"log"
	"strconv"
	"sync"
	"time"

	"github.com/osrs-ge/ge-data/ingester/internal/store"
	"github.com/osrs-ge/ge-data/ingester/internal/wiki"
)

// Serve runs the always-on forward ingester: mapping loader + 5m live collector
// + 1m collector, until the context is cancelled (SIGINT/SIGTERM).
//
// IMPORTANT: run exactly one instance. Multiple pollers duplicate work and risk
// being rate-limited by the Wiki API. In k3s this is a replicas:1 Deployment.
func Serve(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	st, err := store.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()
	client := wiki.New(cfg.UserAgent)

	// Load item metadata once up front so it's available immediately.
	if err := loadMapping(ctx, client, st); err != nil {
		log.Printf("serve: initial mapping load failed (will retry on schedule): %v", err)
	}

	var wg sync.WaitGroup
	for _, job := range []func(context.Context, *wiki.Client, *store.Store){
		mappingLoader, collect5m, collect1m,
	} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			job(ctx, client, st)
		}()
	}
	wg.Wait()
	return ctx.Err()
}

// mappingLoader refreshes items daily to pick up newly added items.
func mappingLoader(ctx context.Context, c *wiki.Client, st *store.Store) {
	tick := time.NewTicker(24 * time.Hour)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := loadMapping(ctx, c, st); err != nil {
				log.Printf("mapping loader: %v", err)
			}
		}
	}
}

func loadMapping(ctx context.Context, c *wiki.Client, st *store.Store) error {
	items, err := c.Mapping(ctx)
	if err != nil {
		return err
	}
	if err := st.UpsertItems(ctx, itemsToStore(items)); err != nil {
		return err
	}
	log.Printf("mapping loader: upserted %d items", len(items))
	return nil
}

// collect5m polls the latest 5m block every minute and upserts it with DO UPDATE,
// so the row converges to the block's final average as it settles.
func collect5m(ctx context.Context, c *wiki.Client, st *store.Store) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			resp, err := c.FiveMinLatest(ctx)
			if err != nil {
				log.Printf("5m collector: %v", err)
				continue
			}
			rows := rowsFrom5m(resp)
			if err := st.UpsertPrices5m(ctx, rows, store.DoUpdate); err != nil {
				log.Printf("5m collector: write: %v", err)
			}
		}
	}
}

// lastSeen tracks the last transaction times we stored per item, so collect1m
// only writes rows when something actually changed (dedup-on-change).
type lastSeen struct {
	highTime *int64
	lowTime  *int64
}

// collect1m polls /latest every minute and writes only changed rows to prices_1m.
func collect1m(ctx context.Context, c *wiki.Client, st *store.Store) {
	seen := make(map[int]lastSeen)
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			resp, err := c.Latest(ctx)
			if err != nil {
				log.Printf("1m collector: %v", err)
				continue
			}
			now := time.Now().UTC().Truncate(time.Minute)
			rows := changedRows(resp, now, seen)
			if err := st.InsertPrices1m(ctx, rows); err != nil {
				log.Printf("1m collector: write: %v", err)
			}
		}
	}
}

// changedRows returns rows whose high/low transaction time advanced since last
// poll, updating seen as a side effect.
func changedRows(resp *wiki.LatestResponse, now time.Time, seen map[int]lastSeen) []store.Price1m {
	rows := make([]store.Price1m, 0)
	for k, p := range resp.Data {
		id, err := strconv.Atoi(k)
		if err != nil {
			continue
		}
		prev, ok := seen[id]
		if ok && eqInt64Ptr(prev.highTime, p.HighTime) && eqInt64Ptr(prev.lowTime, p.LowTime) {
			continue
		}
		seen[id] = lastSeen{highTime: p.HighTime, lowTime: p.LowTime}
		rows = append(rows, store.Price1m{
			TS: now, ItemID: id,
			High: p.High, HighTime: unixPtr(p.HighTime),
			Low: p.Low, LowTime: unixPtr(p.LowTime),
		})
	}
	return rows
}

func eqInt64Ptr(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
