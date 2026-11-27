package usagecost

// TableDate is when the table below was last checked against the official
// Anthropic pricing page. Refresh the rates from that page and bump this date
// when prices change; the numbers here are list prices in USD per million
// tokens.
const TableDate = "2026-11-03"

// std builds a Price from input, output and cache-read rates; cache writes are
// 1.25x input for the 5 minute tier and 2x input for the 1 hour tier.
func std(in, out, cr float64) Price {
	return Price{In: in, Out: out, CW5m: in * 1.25, CW1h: in * 2, CR: cr}
}

// double is fast mode: every rate doubles.
func double(p Price) Price {
	return Price{In: p.In * 2, Out: p.Out * 2, CW5m: p.CW5m * 2, CW1h: p.CW1h * 2, CR: p.CR * 2}
}

func withFast(in, out, cr float64) Model {
	s := std(in, out, cr)
	return Model{Std: s, Fast: double(s)}
}

func plain(in, out, cr float64) Model { return Model{Std: std(in, out, cr)} }

var models = map[string]Model{
	"claude-fable-5-2":  withFast(8, 40, 0.20),
	"claude-mythos-5-2": withFast(8, 40, 0.20),
	"claude-fable-5-1":  plain(10, 50, 1.00),
	"claude-mythos-5-1": plain(10, 50, 1.00),
	"claude-fable-5":    plain(10, 50, 1.00),
	"claude-opus-5-5":   withFast(4, 20, 0.20),
	"claude-opus-5":     withFast(5, 25, 0.50),
	"claude-opus-4-8":   withFast(5, 25, 0.50),
	"claude-opus-4-7":   plain(5, 25, 0.50),
	"claude-opus-4-6":   plain(5, 25, 0.50),
	"claude-sonnet-5-5": plain(2, 10, 0.10),
	"claude-sonnet-5":   plain(2, 10, 0.20),
	"claude-sonnet-4-6": plain(3, 15, 0.30),
	"claude-haiku-5-5": {
		Std:    std(0.10, 0.50, 0.01),
		LongIn: 100_000,
		Long:   std(0.50, 2.50, 0.05),
	},
	"claude-haiku-4-5": plain(1, 5, 0.10),
}

// familyNewest is the id a model of an unlisted version is priced as, by
// family prefix `claude-<family>-`.
var familyNewest = map[string]string{
	"opus":   "claude-opus-5-5",
	"sonnet": "claude-sonnet-5-5",
	"haiku":  "claude-haiku-5-5",
	"fable":  "claude-fable-5-2",
	"mythos": "claude-fable-5-2",
}
