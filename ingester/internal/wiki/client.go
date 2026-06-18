// Package wiki is the OSRS Wiki real-time prices client. It only depends on
// the standard library; concrete writers live in internal/store and
// internal/collect. See docs/GOAL.md for the data shape.
package wiki

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// base is the real-time prices API root. The /5m, /latest, and /mapping
// endpoints are appended to this. Pinned in the TODO; the wiki will 404
// anything that doesn't live under v1/osrs.
const base = "https://prices.runescape.wiki/api/v1/osrs"

// Client is a single http.Client wrapped with the User-Agent header the Wiki
// API requires. The header MUST be set on every request — a missing or blank
// UA is grounds for an IP-level block. Reuse one Client across goroutines;
// net/http.Transport is safe for concurrent use.
type Client struct {
	http      *http.Client
	userAgent string
}

// New builds a Client with a 30s per-request timeout. The timeout bounds both
// connect and read; the Wiki response is small (~1 MB for /mapping, ~1 MB for
// /5m) so this is generous.
func New(userAgent string) *Client {
	return &Client{
		http:      &http.Client{Timeout: 30 * time.Second},
		userAgent: userAgent,
	}
}

// get is the single fetch helper. We always set the User-Agent, always check
// the status, and always decode into a caller-supplied target. Non-200 is an
// error: callers (the collect loop) log and skip the tick — they never panic
// or exit the process on a single bad fetch.
func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", c.userAgent)
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

// ---------------------------------------------------------------------------
// /mapping — full item metadata, ~4.5k items, ~830 KB. Refreshed daily via
// UpsertItems. The schema (`init/01_schema.sql`) maps these fields 1:1 to
// the items table; id -> item_id and limit -> buy_limit because `limit` is a
// reserved word in SQL.
// ---------------------------------------------------------------------------

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

// ---------------------------------------------------------------------------
// /5m — latest 5-minute block, one row per traded item. Carries volume on top
// of average high/low prices. The Timestamp is the block start (unix seconds)
// and is what we write into prices_5m.ts: it's stable for the whole 5-minute
// window so repeated polls during the same window all land on the same row,
// which is why UpsertPrices5m uses DO UPDATE (the block keeps settling).
//
// All four price/volume fields are nullable: a null price means nothing
// traded that side (and the matching volume will also be null). We model
// them as *int64 and pass nil straight through to pgx — never coalesce to 0.
// ---------------------------------------------------------------------------

type Avg5m struct {
	AvgHighPrice    *int64 `json:"avgHighPrice"`
	HighPriceVolume *int64 `json:"highPriceVolume"`
	AvgLowPrice     *int64 `json:"avgLowPrice"`
	LowPriceVolume  *int64 `json:"lowPriceVolume"`
}

type Block5m struct {
	Data      map[string]Avg5m `json:"data"`
	Timestamp int64            `json:"timestamp"` // unix seconds, block start
}

func (c *Client) Prices5m(ctx context.Context) (Block5m, error) {
	var out Block5m
	return out, c.get(ctx, "/5m", &out)
}

// ---------------------------------------------------------------------------
// /latest — instantaneous snapshot of every item's last trade. No volume, no
// averaging. /latest repeats the same row minute after minute until a real
// trade happens, so the collector dedups on (high_time, low_time) and only
// inserts when at least one advanced. high/low/highTime/lowTime are all
// nullable in the API for items with no observed trade yet.
// ---------------------------------------------------------------------------

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
