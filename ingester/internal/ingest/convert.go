package ingest

import (
	"strconv"
	"time"

	"github.com/osrs-ge/ge-data/ingester/internal/store"
	"github.com/osrs-ge/ge-data/ingester/internal/wiki"
)

// itemsToStore maps Wiki /mapping items to store rows.
func itemsToStore(items []wiki.Item) []store.Item {
	out := make([]store.Item, 0, len(items))
	for _, it := range items {
		out = append(out, store.Item{
			ID: it.ID, Name: it.Name, Members: it.Members, Value: it.Value,
			Lowalch: it.Lowalch, Highalch: it.Highalch, Limit: it.Limit, Examine: it.Examine,
		})
	}
	return out
}

// rowsFrom5m flattens a /5m response into store rows, stamping every row with
// the block timestamp echoed by the API.
func rowsFrom5m(resp *wiki.FiveMinResponse) []store.Price5m {
	ts := time.Unix(resp.Timestamp, 0).UTC()
	rows := make([]store.Price5m, 0, len(resp.Data))
	for k, p := range resp.Data {
		id, err := strconv.Atoi(k)
		if err != nil {
			continue
		}
		rows = append(rows, store.Price5m{
			TS: ts, ItemID: id,
			AvgHigh: p.AvgHighPrice, AvgLow: p.AvgLowPrice,
			HighVol: p.HighPriceVolume, LowVol: p.LowPriceVolume,
		})
	}
	return rows
}

// unixPtr converts a nullable Unix-seconds pointer to a *time.Time.
func unixPtr(sec *int64) *time.Time {
	if sec == nil {
		return nil
	}
	t := time.Unix(*sec, 0).UTC()
	return &t
}

// snapTo5m floors t to the previous 5-minute boundary (UTC).
func snapTo5m(t time.Time) time.Time {
	return time.Unix(t.Unix()-t.Unix()%300, 0).UTC()
}
