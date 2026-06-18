// Command ingester is the ge-data collection process. One instance only:
// it runs three independent loops (mapping refresh, /5m poll, /1m poll)
// against the OSRS Wiki real-time prices API and writes the results to the
// TimescaleDB described in docs/database-setup.md.
//
// Process model:
//   - One *wiki.Client, one *store.Store, one Deps shared across the loops.
//   - Each loop runs in its own goroutine under a sync.WaitGroup.
//   - signal.NotifyContext cancels on SIGINT/SIGTERM; the loops exit on the
//     next tick boundary, the pool closes on the deferred Close.
//   - Errors are logged and the loop continues. A dead process is a much
//     worse outcome than a missed tick.
//
// See docs/GOAL.md for the data shape and docs/INFRA.md for how it deploys.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/osrs-ge/ge-data/ingester/internal/collect"
	"github.com/osrs-ge/ge-data/ingester/internal/config"
	"github.com/osrs-ge/ge-data/ingester/internal/store"
	"github.com/osrs-ge/ge-data/ingester/internal/wiki"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	cfg, err := config.Load()
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(1)
	}

	// signal.NotifyContext gives us a context that's cancelled on the first
	// SIGINT/SIGTERM. The defer ensures the signal handler is unregistered
	// and the underlying channel is closed before main returns.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.New(ctx, cfg.DatabaseURL)
	if err != nil {
		slog.Error("store", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	client := wiki.New(cfg.UserAgent)

	deps := collect.Deps{
		Cfg:    cfg,
		Client: client,
		Store:  st,
	}

	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); collect.MappingLoop(ctx, deps) }()
	go func() { defer wg.Done(); collect.Prices5mLoop(ctx, deps) }()
	go func() { defer wg.Done(); collect.Prices1mLoop(ctx, deps) }()

	slog.Info("ingester started",
		"mapping_interval", cfg.MappingInterval,
		"poll_5m_interval", cfg.Poll5mInterval,
		"poll_1m_interval", cfg.Poll1mInterval,
	)

	wg.Wait()
	slog.Info("ingester shut down cleanly")
}
