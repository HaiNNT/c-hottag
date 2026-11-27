// Package usagecost prices token usage at Anthropic list rates and holds the
// list prices of the subscription plans (R203).
package usagecost

import "strings"

// Price is USD per million tokens.
type Price struct{ In, Out, CW5m, CW1h, CR float64 }

// Model is one table entry.
type Model struct {
	Std, Fast Price // Fast zero = no fast mode
	LongIn    int64 // >0: prompt tokens (in+cr+cw5m+cw1h) above this use Long
	Long      Price
}

// Tokens is one response's usage numbers.
type Tokens struct {
	In, Out, CR, CW5m, CW1h, Web int64
	Speed                        string
}

// Quote is a priced Tokens. Priced is the table id used.
type Quote struct {
	USD              float64
	Known, Estimated bool
	Priced           string
}

// WebSearchPerRequest is the list price of one web search request.
const WebSearchPerRequest = 0.01

// lookup finds the table entry for a model id: exact, then with a trailing
// -YYYYMMDD stripped, then the newest model of its family (estimated).
func lookup(model string) (id string, m Model, estimated, ok bool) {
	if m, ok := models[model]; ok {
		return model, m, false, true
	}
	base := stripDate(model)
	if m, ok := models[base]; ok {
		return base, m, false, true
	}
	rest, found := strings.CutPrefix(base, "claude-")
	if !found {
		return "", Model{}, false, false
	}
	family, _, _ := strings.Cut(rest, "-")
	if newest, ok := familyNewest[family]; ok {
		return newest, models[newest], true, true
	}
	return "", Model{}, false, false
}

func stripDate(s string) string {
	i := strings.LastIndexByte(s, '-')
	if i < 0 || len(s)-i-1 != 8 {
		return s
	}
	for _, c := range s[i+1:] {
		if c < '0' || c > '9' {
			return s
		}
	}
	return s[:i]
}

// Cost prices t as one response: a model with a long-prompt tier uses it when
// the prompt is over the limit. An unknown model is not Known and costs 0.
func Cost(model string, t Tokens) Quote { return cost(model, t, true) }

// CostSum prices t as a sum of many responses (a daily row): the long-prompt
// tier never applies, because a sum says nothing about any one prompt. The $
// of a model with such a tier can be understated.
func CostSum(model string, t Tokens) Quote { return cost(model, t, false) }

func cost(model string, t Tokens, tier bool) Quote {
	id, m, est, ok := lookup(model)
	if !ok {
		return Quote{}
	}
	p := m.Std
	if t.Speed == "fast" && m.Fast != (Price{}) {
		p = m.Fast
	}
	if tier && m.LongIn > 0 && t.In+t.CR+t.CW5m+t.CW1h > m.LongIn {
		p = m.Long
	}
	usd := (float64(t.In)*p.In + float64(t.Out)*p.Out + float64(t.CR)*p.CR +
		float64(t.CW5m)*p.CW5m + float64(t.CW1h)*p.CW1h) / 1e6
	usd += float64(t.Web) * WebSearchPerRequest
	return Quote{USD: usd, Known: true, Estimated: est, Priced: id}
}

// PlanPrice is the list price in USD per month of a plan tier. "ultra" is an
// alias of max20x. Team, max and the empty tier have no list price.
func PlanPrice(tier string) (usd float64, ok bool) {
	switch tier {
	case "pro":
		return 20, true
	case "max5x":
		return 100, true
	case "max20x", "ultra":
		return 200, true
	}
	return 0, false
}
