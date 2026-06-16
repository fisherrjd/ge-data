package ingest

import (
	"context"
	"flag"
	"fmt"
	"log"
	"time"

	"github.com/osrs-ge/ge-data/ingester/internal/store"
	"github.com/osrs-ge/ge-data/ingester/internal/wiki"
)

// Backfill pages /5m backward from the current frontier to the --since floor,
// writing immutable historical blocks. It is resumable: on restart it continues
// from the oldest ts already in prices_5m, so a crash mid-run costs nothing.
func Backfill(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("backfill", flag.ExitOnError)
	since := fs.String("since", "2021-03-01", "earliest date (YYYY-MM-DD, UTC) to backfill to")
	delay := fs.Duration("delay", 75*time.Millisecond, "courtesy pause between API requests")
	if err := fs.Parse(args); err != nil {
		return err
	}

	floor, err := time.Parse("2006-01-02", *since)
	if err != nil {
		return fmt.Errorf("bad --since %q: %w", *since, err)
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

	// Resume point: just below the oldest block we already have, else now.
	start := snapTo5m(time.Now().UTC())
	if earliest, ok, err := st.EarliestPrice5m(ctx); err != nil {
		return fmt.Errorf("resume lookup: %w", err)
	} else if ok {
		start = earliest.Add(-5 * time.Minute)
		log.Printf("backfill: resuming from %s (oldest existing was %s)", start, earliest)
	} else {
		log.Printf("backfill: starting fresh from %s", start)
	}

	const step = 5 * time.Minute
	var blocks, rows int
	for t := start; !t.Before(floor); t = t.Add(-step) {
		select {
		case <-ctx.Done():
			log.Printf("backfill: interrupted at %s (%d blocks, %d rows) — rerun to resume", t, blocks, rows)
			return ctx.Err()
		default:
		}

		resp, err := client.FiveMin(ctx, t.Unix())
		if err != nil {
			return fmt.Errorf("fetch %s: %w", t, err)
		}
		r := rowsFrom5m(resp)
		if err := st.UpsertPrices5m(ctx, r, store.DoNothing); err != nil {
			return fmt.Errorf("write %s: %w", t, err)
		}
		blocks++
		rows += len(r)
		if blocks%500 == 0 {
			log.Printf("backfill: %s — %d blocks, %d rows", t, blocks, rows)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(*delay):
		}
	}
	log.Printf("backfill: done — reached %s (%d blocks, %d rows)", floor, blocks, rows)
	return nil
}
