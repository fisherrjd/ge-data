// Command ingester collects OSRS Grand Exchange price data from the Wiki API.
//
//	ingester serve      forward ingester: mapping loader + 5m + 1m collectors (daemon)
//	ingester backfill    one-off: page /5m backward to the --since floor, then exit
//
// Config via env: DATABASE_URL, USER_AGENT.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/osrs-ge/ge-data/ingester/internal/ingest"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "serve":
		err = ingest.Serve(ctx, os.Args[2:])
	case "backfill":
		err = ingest.Backfill(ctx, os.Args[2:])
	default:
		usage()
	}
	if err != nil && err != context.Canceled {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: ingester <serve|backfill> [flags]")
	os.Exit(2)
}
