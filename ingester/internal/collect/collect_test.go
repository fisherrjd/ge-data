package collect

import "testing"

func ptr(v int64) *int64 { return &v }

func TestFlipMargin(t *testing.T) {
	cases := []struct {
		name string
		high *int64
		low  *int64
		want *int64
	}{
		{"both nil", nil, nil, nil},
		{"high nil", nil, ptr(100), nil},
		{"low nil", ptr(100), nil, nil},

		// Tax = high/50 (floor). Under 50 coins the floor makes tax zero, so 49
		// and 50 differ by the single coin the blurb describes.
		{"under 50 untaxed", ptr(49), ptr(0), ptr(49)},  // tax 0
		{"exactly 50 taxed 1", ptr(50), ptr(0), ptr(49)}, // tax 1 -> 50-1-0
		{"typical flip", ptr(100), ptr(90), ptr(8)},      // tax 2 -> 100-2-90

		// 5M cap binds at a 250M sale price.
		{"at cap boundary", ptr(250_000_000), ptr(0), ptr(245_000_000)}, // tax exactly 5M
		{"above cap", ptr(300_000_000), ptr(0), ptr(295_000_000)},       // tax capped at 5M, not 6M

		// Illiquid: last insta-sell above last insta-buy -> negative margin.
		{"negative margin", ptr(10), ptr(20), ptr(-10)}, // tax 0 -> 10-0-20
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := flipMargin(c.high, c.low)
			switch {
			case c.want == nil && got != nil:
				t.Fatalf("want nil, got %d", *got)
			case c.want != nil && got == nil:
				t.Fatalf("want %d, got nil", *c.want)
			case c.want != nil && got != nil && *got != *c.want:
				t.Fatalf("want %d, got %d", *c.want, *got)
			}
		})
	}
}
