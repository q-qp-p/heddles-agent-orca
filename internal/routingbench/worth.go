/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package routingbench

import (
	"math"
	"sort"
)

// Judge verdicts on a replayed call, as the benchmark stores them.
const (
	WinnerServed = "served"
	WinnerShadow = "shadow"
	WinnerTie    = "tie"
)

// Sample is one served call that was replayed on another model and judged.
// Samples the judge could not rule on ("invalid") are left out by the caller.
type Sample struct {
	Agent    string
	Category string

	ServedProvider string
	ShadowProvider string
	// Winner is the judge's verdict on the shadow answer against the served one.
	Winner string
	// Inconsistent marks a verdict that is a tie because the two judgments,
	// with the answers in opposite order, disagreed.
	Inconsistent bool

	// ServedCostUSD and ShadowCostUSD are what each answer cost; the judge's
	// own cost is not part of the comparison.
	ServedCostUSD float64
	ShadowCostUSD float64
}

// Tiers says which of two models is the capable (dearer) one. When the selector
// names capable and efficient roles, a pair of exactly those two models is
// oriented by them; any other pair is ordered by cost per token.
type Tiers struct {
	Capable, Efficient string
	// CostPerToken is each provider's input plus output price, used when the
	// roles do not decide.
	CostPerToken map[string]float64
}

// orient names the capable and efficient model of a served/shadow pair. ok is
// false when they cannot be told apart (same model, or equal price).
func (t Tiers) orient(served, shadow string) (capable, efficient string, ok bool) {
	if served == shadow {
		return "", "", false
	}
	if t.Capable != "" && t.Efficient != "" && t.Capable != t.Efficient &&
		((served == t.Capable && shadow == t.Efficient) || (served == t.Efficient && shadow == t.Capable)) {
		return t.Capable, t.Efficient, true
	}
	cs, ch := t.CostPerToken[served], t.CostPerToken[shadow]
	switch {
	case cs > ch:
		return served, shadow, true
	case ch > cs:
		return shadow, served, true
	}
	return "", "", false
}

// Result of one judged pair, from the capable model's point of view.
type Result int8

const (
	// EfficientBetter means the cheaper model's answer won.
	EfficientBetter Result = -1
	// Equal means the judge saw no difference, or could not decide.
	Equal Result = 0
	// CapableBetter means the dearer model's answer won.
	CapableBetter Result = 1
)

// Pair is a Sample oriented by tier.
type Pair struct {
	Result Result
	// RoutedCapable is true when the capable model served the call.
	RoutedCapable bool
	// CapableCostUSD and EfficientCostUSD are what each tier's answer cost.
	CapableCostUSD   float64
	EfficientCostUSD float64
	Inconsistent     bool
}

// GroupKey identifies a comparison: one Agent, one task category, one pair of
// models. The same Agent can have several groups, one per category.
type GroupKey struct {
	Agent, Category, Capable, Efficient string
}

// Group is every judged pair of one GroupKey.
type Group struct {
	GroupKey
	Pairs []Pair
	// RoutedCapableCalls is how many calls of this Agent and category the
	// capable model served in the window, sampled or not. It scales savings
	// from the sample to the traffic. Zero if unknown.
	RoutedCapableCalls int
	// WindowDays is the length of the window RoutedCapableCalls covers.
	WindowDays float64
}

// Aggregate orients samples and groups them by Agent, category and model pair,
// in a stable order. Samples whose models cannot be told apart, or whose
// verdict is not served, shadow or tie, are skipped.
func Aggregate(samples []Sample, tiers func(agent string) Tiers) []Group {
	idx := map[GroupKey]*Group{}
	for _, s := range samples {
		capable, efficient, ok := tiers(s.Agent).orient(s.ServedProvider, s.ShadowProvider)
		if !ok {
			continue
		}
		servedCapable := s.ServedProvider == capable
		var res Result
		switch s.Winner {
		case WinnerTie:
			res = Equal
		case WinnerServed, WinnerShadow:
			servedWon := s.Winner == WinnerServed
			if servedWon == servedCapable {
				res = CapableBetter
			} else {
				res = EfficientBetter
			}
		default:
			continue
		}
		p := Pair{Result: res, RoutedCapable: servedCapable, Inconsistent: s.Inconsistent}
		if servedCapable {
			p.CapableCostUSD, p.EfficientCostUSD = s.ServedCostUSD, s.ShadowCostUSD
		} else {
			p.CapableCostUSD, p.EfficientCostUSD = s.ShadowCostUSD, s.ServedCostUSD
		}
		key := GroupKey{Agent: s.Agent, Category: s.Category, Capable: capable, Efficient: efficient}
		g := idx[key]
		if g == nil {
			g = &Group{GroupKey: key}
			idx[key] = g
		}
		g.Pairs = append(g.Pairs, p)
	}
	out := make([]Group, 0, len(idx))
	for _, g := range idx {
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].GroupKey, out[j].GroupKey
		if a.Agent != b.Agent {
			return a.Agent < b.Agent
		}
		if a.Category != b.Category {
			return a.Category < b.Category
		}
		if a.Capable != b.Capable {
			return a.Capable < b.Capable
		}
		return a.Efficient < b.Efficient
	})
	return out
}

// N is the number of judged pairs.
func (g Group) N() int { return len(g.Pairs) }

// InconsistentRate is the share of pairs on which the judge disagreed with
// itself. A high rate means it cannot tell these answers apart.
func (g Group) InconsistentRate() float64 {
	if len(g.Pairs) == 0 {
		return 0
	}
	n := 0
	for _, p := range g.Pairs {
		if p.Inconsistent {
			n++
		}
	}
	return float64(n) / float64(len(g.Pairs))
}

// Counts of each result.
func (g Group) counts() (capableBetter, efficientBetter, equal int) {
	for _, p := range g.Pairs {
		switch p.Result {
		case CapableBetter:
			capableBetter++
		case EfficientBetter:
			efficientBetter++
		default:
			equal++
		}
	}
	return
}

func netWinOf(pairs []Pair, idx []int) float64 {
	if len(idx) == 0 {
		return 0
	}
	s := 0
	for _, i := range idx {
		s += int(pairs[i].Result)
	}
	return float64(s) / float64(len(idx))
}

func meanDeltaCost(pairs []Pair, idx []int) float64 {
	if len(idx) == 0 {
		return 0
	}
	s := 0.0
	for _, i := range idx {
		s += pairs[i].CapableCostUSD - pairs[i].EfficientCostUSD
	}
	return s / float64(len(idx))
}

// Worth says whether the capable model earns what it costs, for one group.
type Worth struct {
	N int
	// CapableBetter, EfficientBetter and Equal count the judge's verdicts.
	CapableBetter, EfficientBetter, Equal int

	// NetWin is P(capable better) minus P(efficient better): how much more
	// often the dearer model's answer wins. NetWinCI is a paired bootstrap
	// interval.
	NetWin   float64
	NetWinCI Interval

	// DeltaCostUSD is the mean extra cost of using the capable model on one
	// call, with a paired bootstrap interval.
	DeltaCostUSD   float64
	DeltaCostUSDCI Interval

	// USDPerNetWin is DeltaCostUSD divided by NetWin: what one more judged win
	// costs. It is nil when the capable model does not win more often.
	USDPerNetWin *float64

	// EscalationPrecision is P(capable better | routed to capable) and MissRate
	// is P(capable better | routed to efficient). Their difference, with its
	// interval, says whether routing sends the hard calls to the capable model
	// better than chance. Both are nil when a side has no pairs.
	EscalationPrecision *float64
	MissRate            *float64
	RoutingLift         *float64
	RoutingLiftCI       *Interval

	InconsistentRate float64
}

// Options set the evidence bar and the randomness of the bootstrap.
type Options struct {
	// Confidence of every interval. Default 0.95.
	Confidence float64
	// Resamples is the bootstrap size. Default 2000.
	Resamples int
	// Seed makes the bootstrap reproducible. Default 1.
	Seed uint64
	// MinSamples is the judged pairs a group needs before any finding. Default 30.
	MinSamples int
	// NonInferiorityMargin is how much worse than the capable model the cheaper
	// one may be, as a net-win difference, and still count as equal. Default 0.05.
	NonInferiorityMargin float64
	// MinNetWinDelta is the smallest net win worth acting on. Default 0.
	MinNetWinDelta float64
	// MaxUSDPerNetWin is the most the user would pay for one extra judged win.
	// Nil leaves cost-per-win reported but never acted on.
	MaxUSDPerNetWin *float64
	// JudgeUnreliableRate is the inconsistent-judgment rate above which a group
	// is not acted on, because the judge could not separate the answers.
	// Default 0.5.
	JudgeUnreliableRate float64
	// DeadProviderErrorRate and MinProviderCalls decide when a provider is dead.
	// Defaults 0.5 and 20.
	DeadProviderErrorRate float64
	MinProviderCalls      int
}

func (o Options) withDefaults() Options {
	if o.Confidence <= 0 || o.Confidence >= 1 {
		o.Confidence = 0.95
	}
	if o.Resamples <= 0 {
		o.Resamples = 2000
	}
	if o.Seed == 0 {
		o.Seed = 1
	}
	if o.MinSamples <= 0 {
		o.MinSamples = 30
	}
	if o.NonInferiorityMargin <= 0 {
		o.NonInferiorityMargin = 0.05
	}
	if o.JudgeUnreliableRate <= 0 {
		o.JudgeUnreliableRate = 0.5
	}
	if o.DeadProviderErrorRate <= 0 {
		o.DeadProviderErrorRate = 0.5
	}
	if o.MinProviderCalls <= 0 {
		o.MinProviderCalls = 20
	}
	return o
}

// Evaluate computes the worth-it measures of a group.
func Evaluate(g Group, opts Options) Worth {
	o := opts.withDefaults()
	w := Worth{N: g.N(), InconsistentRate: g.InconsistentRate()}
	w.CapableBetter, w.EfficientBetter, w.Equal = g.counts()
	if w.N == 0 {
		w.NetWinCI = Interval{math.NaN(), math.NaN()}
		w.DeltaCostUSDCI = Interval{math.NaN(), math.NaN()}
		return w
	}
	all := make([]int, w.N)
	for i := range all {
		all[i] = i
	}
	w.NetWin = netWinOf(g.Pairs, all)
	w.DeltaCostUSD = meanDeltaCost(g.Pairs, all)
	w.NetWinCI = bootstrap(w.N, o.Resamples, o.Confidence, o.Seed, func(idx []int) (float64, bool) {
		return netWinOf(g.Pairs, idx), true
	})
	w.DeltaCostUSDCI = bootstrap(w.N, o.Resamples, o.Confidence, o.Seed+1, func(idx []int) (float64, bool) {
		return meanDeltaCost(g.Pairs, idx), true
	})
	if w.NetWin > 0 {
		v := w.DeltaCostUSD / w.NetWin
		w.USDPerNetWin = &v
	}

	var kc, nc, ke, ne int
	for _, p := range g.Pairs {
		if p.RoutedCapable {
			nc++
			if p.Result == CapableBetter {
				kc++
			}
		} else {
			ne++
			if p.Result == CapableBetter {
				ke++
			}
		}
	}
	if nc > 0 {
		v := float64(kc) / float64(nc)
		w.EscalationPrecision = &v
	}
	if ne > 0 {
		v := float64(ke) / float64(ne)
		w.MissRate = &v
	}
	if nc > 0 && ne > 0 {
		d, ci := NewcombeDiff(kc, nc, ke, ne, o.Confidence)
		w.RoutingLift, w.RoutingLiftCI = &d, &ci
	}
	return w
}
