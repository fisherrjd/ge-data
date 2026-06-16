// Package wiki is a thin client for the OSRS Wiki real-time prices API.
// Docs: https://oldschool.runescape.wiki/w/RuneScape:Real-time_Prices
//
// The API requires a descriptive User-Agent identifying the project and a
// contact, or it may block you. Pass one to New.
package wiki

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

const defaultBaseURL = "https://prices.runescape.wiki/api/v1/osrs"

// Client talks to the OSRS Wiki prices API.
type Client struct {
	http      *http.Client
	baseURL   string
	userAgent string
}

// New returns a client. userAgent must identify the project + a contact.
func New(userAgent string) *Client {
	return &Client{
		http:      &http.Client{Timeout: 30 * time.Second},
		baseURL:   defaultBaseURL,
		userAgent: userAgent,
	}
}

// Item is one entry from /mapping. Fields absent for some items decode to zero.
type Item struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	Members  bool   `json:"members"`
	Value    int    `json:"value"`
	Lowalch  int    `json:"lowalch"`
	Highalch int    `json:"highalch"`
	Limit    int    `json:"limit"`
	Examine  string `json:"examine"`
}

// Price is one item's 5-minute average. Prices are nullable: a null price means
// no trade cleared on that side in the block (volume will be 0). Keep the nulls.
type Price struct {
	AvgHighPrice    *int64 `json:"avgHighPrice"`
	AvgLowPrice     *int64 `json:"avgLowPrice"`
	HighPriceVolume *int64 `json:"highPriceVolume"`
	LowPriceVolume  *int64 `json:"lowPriceVolume"`
}

// FiveMinResponse is the /5m payload: data keyed by item id, plus the block's
// Unix timestamp echoed at the top level.
type FiveMinResponse struct {
	Data      map[string]Price `json:"data"`
	Timestamp int64            `json:"timestamp"`
}

// LatestPrice is one item's instantaneous last-transaction prices from /latest.
// No volume. Times are Unix seconds of the actual transaction (nullable).
type LatestPrice struct {
	High     *int64 `json:"high"`
	HighTime *int64 `json:"highTime"`
	Low      *int64 `json:"low"`
	LowTime  *int64 `json:"lowTime"`
}

// LatestResponse is the /latest payload, keyed by item id.
type LatestResponse struct {
	Data map[string]LatestPrice `json:"data"`
}

// Mapping fetches all items and their metadata.
func (c *Client) Mapping(ctx context.Context) ([]Item, error) {
	var items []Item
	if err := c.get(ctx, "/mapping", &items); err != nil {
		return nil, err
	}
	return items, nil
}

// FiveMin fetches the 5-minute block whose start equals the given Unix timestamp
// (must be a 5-minute boundary). Used by the backfill.
func (c *Client) FiveMin(ctx context.Context, ts int64) (*FiveMinResponse, error) {
	var out FiveMinResponse
	if err := c.get(ctx, fmt.Sprintf("/5m?timestamp=%d", ts), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// FiveMinLatest fetches the most recent 5-minute block. Used by the live collector.
func (c *Client) FiveMinLatest(ctx context.Context) (*FiveMinResponse, error) {
	var out FiveMinResponse
	if err := c.get(ctx, "/5m", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Latest fetches instantaneous last-transaction prices for all items.
func (c *Client) Latest(ctx context.Context) (*LatestResponse, error) {
	var out LatestResponse
	if err := c.get(ctx, "/latest", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// get performs a GET with the required User-Agent and retries 429/5xx with
// exponential backoff. 4xx (other than 429) fail fast.
func (c *Client) get(ctx context.Context, path string, out any) error {
	const maxAttempts = 4
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", c.userAgent)

		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = err
		} else {
			retryable, err := decode(resp, out)
			if err == nil {
				return nil
			}
			lastErr = err
			if !retryable {
				return lastErr
			}
		}

		if attempt < maxAttempts {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff(attempt)):
			}
		}
	}
	return fmt.Errorf("wiki: %s: %w", path, lastErr)
}

// decode reads the response and reports whether the error (if any) is retryable.
func decode(resp *http.Response, out any) (retryable bool, err error) {
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
		return false, json.NewDecoder(resp.Body).Decode(out)
	case resp.StatusCode == http.StatusTooManyRequests, resp.StatusCode >= 500:
		return true, fmt.Errorf("status %s", resp.Status)
	default:
		return false, fmt.Errorf("status %s", resp.Status)
	}
}

func backoff(attempt int) time.Duration {
	return time.Duration(attempt*attempt) * 500 * time.Millisecond
}
