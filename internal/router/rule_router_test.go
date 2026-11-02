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

package router

import (
	"math"
	"testing"
	"time"
)

const (
	providerBig   = "big"
	providerCheap = "cheap"
)

// ruleRouterTestProviders returns a three-provider fixture spanning the
// attributes the decision tree branches on: cost, latency, capability
// and context window.
func ruleRouterTestProviders() []ProviderConfig {
	return []ProviderConfig{
		{
			Name:               providerCheap,
			LiteLLMModel:       "openai/gpt-4o-mini",
			Weight:             5,
			Capabilities:       []string{"code"},
			ContextWindow:      128_000,
			CostPerInputToken:  0.00000015,
			CostPerOutputToken: 0.00000060,
			LatencyProfile:     "fast",
		},
		{
			Name:               providerBig,
			LiteLLMModel:       "anthropic/claude-sonnet-4-6",
			Weight:             3,
			Capabilities:       []string{"vision", "reasoning"},
			ContextWindow:      1_000_000,
			CostPerInputToken:  0.000003,
			CostPerOutputToken: 0.000015,
			LatencyProfile:     "medium",
		},
		{
			Name:               "fastonly",
			LiteLLMModel:       "openai/gpt-4o-mini-2024-07-18",
			Weight:             2,
			Capabilities:       []string{"fast"},
			ContextWindow:      128_000,
			CostPerInputToken:  0.00000025,
			CostPerOutputToken: 0.000001,
			LatencyProfile:     "fast",
		},
	}
}

func newTestRuleRouter(t *testing.T, mutators ...func(*Config)) *RuleRouter {
	t.Helper()
	cfg := &Config{
		Providers:         ruleRouterTestProviders(),
		CapabilityRouting: map[string]string{},
	}
	for _, m := range mutators {
		m(cfg)
	}
	return NewRuleRouter(cfg)
}

// ruleUserTurn is a plain single-user-turn conversation with no stage signals
// (no tool results, no code fences, no summary phrases).
func ruleUserTurn(text string) []Message {
	return []Message{{Role: "user", Content: text}}
}

func TestRuleRouter_NoProviders(t *testing.T) {
	rr := &RuleRouter{cfg: &Config{}}
	res := rr.Route(ruleUserTurn("hi"), 0, 0)
	if res.ProviderName != "" {
		t.Errorf("ProviderName = %q, want empty", res.ProviderName)
	}
	if res.Confidence != 0 {
		t.Errorf("Confidence = %v, want 0", res.Confidence)
	}
	if res.Reason != "no providers configured" {
		t.Errorf("Reason = %q, want %q", res.Reason, "no providers configured")
	}
}

func TestRuleRouter_VisionContent(t *testing.T) {
	rr := newTestRuleRouter(t)
	msgs := []Message{{
		Role: "user",
		Content: []any{map[string]any{
			"type": "image_url",
			"image_url": map[string]string{
				"url": "data:image/png;base64,iVBORw0KGgo=",
			},
		}},
	}}
	res := rr.Route(msgs, 0, 0)
	if res.ProviderName != providerBig {
		t.Errorf("ProviderName = %q, want big (only vision-capable provider)", res.ProviderName)
	}
	if res.Confidence != 0.95 {
		t.Errorf("Confidence = %v, want 0.95", res.Confidence)
	}
	if res.Reason != "vision content detected" {
		t.Errorf("Reason = %q", res.Reason)
	}
}

func TestRuleRouter_LargeContext(t *testing.T) {
	// 110k tokens exceeds 80% of the 128k windows (102,400) but fits the 1M
	// window comfortably. findLargeContext picks the qualifying provider with
	// the most headroom; cheap and fastonly tie at 128k and the first wins.
	rr := newTestRuleRouter(t)
	res := rr.Route(ruleUserTurn("x"), 110_000, 0)
	if res.ProviderName != providerCheap {
		t.Errorf("ProviderName = %q, want cheap (first of the tied 128k windows)", res.ProviderName)
	}
	if res.Confidence != 0.90 {
		t.Errorf("Confidence = %v, want 0.90", res.Confidence)
	}
	if res.Reason != "large context detected" {
		t.Errorf("Reason = %q", res.Reason)
	}
}

func TestRuleRouter_ContextFitsAll_NoLargeContextRule(t *testing.T) {
	// 50k tokens fits every provider with headroom → rule 2 must NOT fire;
	// with no stage mapping or hints the result is weighted random.
	rr := newTestRuleRouter(t)
	res := rr.Route(ruleUserTurn("x"), 50_000, 0)
	if res.Reason != reasonWeightedRandom {
		t.Errorf("Reason = %q, want %q", res.Reason, reasonWeightedRandom)
	}
}

func TestRuleRouter_BudgetApproaching(t *testing.T) {
	rr := newTestRuleRouter(t, func(c *Config) {
		c.BudgetPerRunUSD = 1.00
	})
	rr.spentUSD = 0.85 // 15% remaining, below the 20% rule-3 threshold
	res := rr.Route(ruleUserTurn("hi"), 0, 0)
	if res.ProviderName != providerCheap {
		t.Errorf("ProviderName = %q, want cheap (lowest CostPerInputToken)", res.ProviderName)
	}
	if res.Confidence != 0.85 {
		t.Errorf("Confidence = %v, want 0.85", res.Confidence)
	}
	if res.Reason != "budget < 20% remaining" {
		t.Errorf("Reason = %q", res.Reason)
	}
}

func TestRuleRouter_BudgetNotApproaching(t *testing.T) {
	rr := newTestRuleRouter(t, func(c *Config) {
		c.BudgetPerRunUSD = 1.00
	})
	rr.spentUSD = 0.50 // 50% remaining — rule 3 must not fire
	res := rr.Route(ruleUserTurn("hi"), 0, 0)
	if res.Reason != reasonWeightedRandom {
		t.Errorf("Reason = %q, want %q (no rule should fire)", res.Reason, reasonWeightedRandom)
	}
}

func TestRuleRouter_TimeoutApproaching(t *testing.T) {
	rr := newTestRuleRouter(t)
	// Simulate 8 minutes elapsed against 2 minutes remaining: 480/600 = 0.80
	// > the 0.70 rule-4 threshold. fastest = first "fast" provider = cheap.
	rr.startedAt = time.Now().Add(-8 * time.Minute)
	res := rr.Route(ruleUserTurn("hi"), 0, 120)
	if res.ProviderName != providerCheap {
		t.Errorf("ProviderName = %q, want cheap (first fast-latency provider)", res.ProviderName)
	}
	if res.Confidence != 0.80 {
		t.Errorf("Confidence = %v, want 0.80", res.Confidence)
	}
	if res.Reason != "approaching timeout" {
		t.Errorf("Reason = %q", res.Reason)
	}
}

func TestRuleRouter_TimeoutNotApproaching(t *testing.T) {
	rr := newTestRuleRouter(t)
	// 1 minute elapsed of a 9-minute timeout: 60/540 ≈ 0.11 < 0.70.
	rr.startedAt = time.Now().Add(-1 * time.Minute)
	res := rr.Route(ruleUserTurn("hi"), 0, 480)
	if res.Reason != reasonWeightedRandom {
		t.Errorf("Reason = %q, want %q (rule 4 must not fire)", res.Reason, reasonWeightedRandom)
	}
}

func TestRuleRouter_CapabilityHint(t *testing.T) {
	rr := newTestRuleRouter(t, func(c *Config) {
		c.CapabilityRouting["code"] = providerBig
	})
	// Plain text with a "code" keyword and no code fence → classifyStage
	// returns chat (0.40, below gate), so rule 5 keyword hints fire.
	res := rr.Route(ruleUserTurn("please implement a function to do X"), 0, 0)
	if res.ProviderName != providerBig {
		t.Errorf("ProviderName = %q, want big (capability hint: code keywords)", res.ProviderName)
	}
	if res.Confidence != 0.75 {
		t.Errorf("Confidence = %v, want 0.75", res.Confidence)
	}
	if res.Reason != "capability routing: code" {
		t.Errorf("Reason = %q", res.Reason)
	}
}

func TestRuleRouter_WeightedRandomRespectsWeightZero(t *testing.T) {
	rr := newTestRuleRouter(t, func(c *Config) {
		// fastonly gets weight 0 → reserved for meta-router escalation only.
		c.Providers[2].Weight = 0
	})
	for range 50 {
		res := rr.Route(ruleUserTurn("hi"), 0, 0)
		if res.ProviderName == "fastonly" {
			t.Fatalf("fastonly selected by weighted random with Weight=0")
		}
	}
}

func TestRuleRouter_MaxRequestTokensFilter(t *testing.T) {
	rr := newTestRuleRouter(t, func(c *Config) {
		// Only providerBig (1M window) can carry a 150k-token request.
		c.Providers[0].MaxRequestTokens = 100_000
		c.Providers[2].MaxRequestTokens = 100_000
	})
	// 150k tokens: cheap and fastonly are filtered by MaxRequestTokens, so
	// rule 2 sees only providerBig — 150k < 80% of 1M, so no large-context rule;
	// weighted random over a single provider still selects providerBig.
	res := rr.Route(ruleUserTurn("x"), 150_000, 0)
	if res.ProviderName != providerBig {
		t.Errorf("ProviderName = %q, want big (only unfiltered provider)", res.ProviderName)
	}
	if res.Reason != reasonWeightedRandom {
		t.Errorf("Reason = %q, want %q", res.Reason, reasonWeightedRandom)
	}
	// The scoped-config copy must not have mutated the original providers.
	if len(rr.cfg.Providers) != 3 {
		t.Errorf("cfg.Providers length = %d, want 3 (original config untouched)", len(rr.cfg.Providers))
	}
}

func TestRuleRouter_UpdateSpendAndTurnCount(t *testing.T) {
	rr := newTestRuleRouter(t)
	rr.UpdateSpend(0.10)
	rr.UpdateSpend(0.05)
	if math.Abs(rr.spentUSD-0.15) > 1e-9 {
		t.Errorf("spentUSD = %v, want 0.15", rr.spentUSD)
	}
	rr.IncrementTurn()
	rr.IncrementTurn()
	if rr.TurnCount() != 2 {
		t.Errorf("TurnCount() = %d, want 2", rr.TurnCount())
	}
}

func TestRuleRouter_PathReportedPerRule(t *testing.T) {
	imageMsg := []Message{{Role: "user", Content: []any{map[string]any{"type": "image_url"}}}}
	tests := []struct {
		name     string
		mutate   func(*Config)
		rr       func(*RuleRouter)
		messages []Message
		tokens   int
		timeout  float64
		want     string
	}{
		{name: "vision", messages: imageMsg, want: pathVision},
		{name: "large context", messages: ruleUserTurn("x"), tokens: 110_000, want: pathContext},
		{
			name:     "budget",
			mutate:   func(c *Config) { c.BudgetPerRunUSD = 10 },
			rr:       func(r *RuleRouter) { r.UpdateSpend(9) },
			messages: ruleUserTurn("x"), want: pathBudget,
		},
		{
			name:     "timeout",
			rr:       func(r *RuleRouter) { r.startedAt = time.Now().Add(-90 * time.Second) },
			messages: ruleUserTurn("x"), timeout: 10, want: pathTimeout,
		},
		{
			name:     "capability hint",
			mutate:   func(c *Config) { c.CapabilityRouting["code"] = providerBig },
			messages: ruleUserTurn("please implement a function"), want: pathCapability + "code",
		},
		{name: "weighted", messages: ruleUserTurn("hi"), want: pathWeighted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mutators []func(*Config)
			if tt.mutate != nil {
				mutators = append(mutators, tt.mutate)
			}
			rr := newTestRuleRouter(t, mutators...)
			if tt.rr != nil {
				tt.rr(rr)
			}
			if got := rr.Route(tt.messages, tt.tokens, tt.timeout).Path; got != tt.want {
				t.Errorf("Path = %q, want %q", got, tt.want)
			}
		})
	}

	if got := (&RuleRouter{cfg: &Config{}}).Route(ruleUserTurn("hi"), 0, 0).Path; got != pathNoProviders {
		t.Errorf("no providers: Path = %q, want %q", got, pathNoProviders)
	}
}

// reserveProvider is a weight-0 provider that would win every primary rule
// (cheapest, fastest, largest window, vision-capable) if it were eligible.
func reserveProvider() ProviderConfig {
	return ProviderConfig{
		Name:              "reserve",
		LiteLLMModel:      "openai/reserve",
		Weight:            0,
		Capabilities:      []string{"vision"},
		ContextWindow:     10_000_000,
		CostPerInputToken: 0,
		LatencyProfile:    "fast",
	}
}

func TestRuleRouter_WeightZeroProviderNeverWinsPrimaryRules(t *testing.T) {
	withReserve := func(c *Config) { c.Providers = append(c.Providers, reserveProvider()) }

	t.Run("budget picks the cheapest weighted provider", func(t *testing.T) {
		rr := newTestRuleRouter(t, withReserve, func(c *Config) { c.BudgetPerRunUSD = 10 })
		rr.UpdateSpend(9)
		if got := rr.Route(ruleUserTurn("x"), 0, 0); got.ProviderName != providerCheap {
			t.Errorf("ProviderName = %q, want cheap (reserve is free but weight 0)", got.ProviderName)
		}
	})
	t.Run("large context ignores the reserve window", func(t *testing.T) {
		rr := newTestRuleRouter(t, withReserve)
		if got := rr.Route(ruleUserTurn("x"), 900_000, 0); got.ProviderName != providerBig {
			t.Errorf("ProviderName = %q, want big", got.ProviderName)
		}
	})
	t.Run("timeout picks a weighted fast provider", func(t *testing.T) {
		rr := newTestRuleRouter(t, withReserve)
		rr.startedAt = time.Now().Add(-90 * time.Second)
		if got := rr.Route(ruleUserTurn("x"), 0, 10); got.ProviderName == "reserve" {
			t.Errorf("reserve (weight 0) won the timeout rule")
		}
	})
	t.Run("vision prefers a weighted capable provider", func(t *testing.T) {
		rr := newTestRuleRouter(t, withReserve)
		msgs := []Message{{Role: "user", Content: []any{map[string]any{"type": "image_url"}}}}
		if got := rr.Route(msgs, 0, 0); got.ProviderName != providerBig {
			t.Errorf("ProviderName = %q, want big (weighted vision provider)", got.ProviderName)
		}
	})
	t.Run("vision widens to the reserve when no weighted provider can see", func(t *testing.T) {
		rr := newTestRuleRouter(t, withReserve, func(c *Config) { c.Providers[1].Capabilities = nil })
		msgs := []Message{{Role: "user", Content: []any{map[string]any{"type": "image_url"}}}}
		if got := rr.Route(msgs, 0, 0); got.ProviderName != "reserve" {
			t.Errorf("ProviderName = %q, want reserve (the only vision provider)", got.ProviderName)
		}
	})
	t.Run("all providers weight 0 still routes", func(t *testing.T) {
		rr := newTestRuleRouter(t, func(c *Config) {
			for i := range c.Providers {
				c.Providers[i].Weight = 0
			}
			c.BudgetPerRunUSD = 10
		})
		rr.UpdateSpend(9)
		if got := rr.Route(ruleUserTurn("x"), 0, 0); got.ProviderName != providerCheap {
			t.Errorf("ProviderName = %q, want cheap (no weighted providers: all eligible)", got.ProviderName)
		}
	})
}

func TestRuleRouter_ReservedCapabilityKeysAreNotKeywordHints(t *testing.T) {
	rr := newTestRuleRouter(t, func(c *Config) {
		c.CapabilityRouting = map[string]string{
			"capable":    providerBig,
			"efficient":  providerCheap,
			"judge":      providerBig,
			"any":        providerBig,
			"stage:code": providerBig,
			"subagent.x": providerBig,
		}
	})
	rr.randIntn = func(int) int { return 0 }
	// Every reserved key appears verbatim in the message; none may route it.
	msg := "capable efficient judge any stage:code subagent.x"
	res := rr.Route(ruleUserTurn(msg), 0, 0)
	if res.Path != pathWeighted {
		t.Errorf("Path = %q, want %q: a reserved key was keyword-matched", res.Path, pathWeighted)
	}
}

func TestRuleRouter_CapabilityHintOrderIsDeterministic(t *testing.T) {
	rr := newTestRuleRouter(t, func(c *Config) {
		c.CapabilityRouting = map[string]string{"reasoning": providerBig, "code": providerCheap, "fast": "fastonly"}
	})
	// The message matches all three tags; sorted order makes "code" win every time.
	msgs := ruleUserTurn("quick answer: implement a function and analyze it")
	for range 100 {
		res := rr.Route(msgs, 0, 0)
		if res.Path != pathCapability+"code" || res.ProviderName != providerCheap {
			t.Fatalf("Path=%q provider=%q, want %q/cheap on every call", res.Path, res.ProviderName, pathCapability+"code")
		}
	}
}

func TestRuleRouter_InjectedRandomPicksByWeight(t *testing.T) {
	rr := newTestRuleRouter(t) // weights cheap=5, big=3, fastonly=2 (total 10)
	for pick, want := range map[int]string{0: providerCheap, 4: providerCheap, 5: providerBig, 7: providerBig, 8: "fastonly", 9: "fastonly"} {
		rr.randIntn = func(int) int { return pick }
		if got := rr.Route(ruleUserTurn("hi"), 0, 0).ProviderName; got != want {
			t.Errorf("pick %d: ProviderName = %q, want %q", pick, got, want)
		}
	}
}

func TestReservedCapabilityKey(t *testing.T) {
	for _, k := range []string{"capable", "efficient", "judge", "any", "stage:tool-exec", "subagent.explore"} {
		if !reservedCapabilityKey(k) {
			t.Errorf("%q should be reserved", k)
		}
	}
	for _, k := range []string{"code", "reasoning", "fast", "vision", "stage", "subagent"} {
		if reservedCapabilityKey(k) {
			t.Errorf("%q should not be reserved", k)
		}
	}
}
