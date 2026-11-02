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
	"math/rand"
	"slices"
	"sort"
	"strings"
	"time"
)

// RouteResult is the output of the rule-based router.
type RouteResult struct {
	// ProviderName is the selected ModelProvider CRD name.
	ProviderName string
	// Confidence is a 0.0–1.0 score indicating how certain the rule-based router is.
	// Low confidence triggers the LLM meta-router in hybrid mode.
	Confidence float64
	// Reason is a human-readable explanation for the routing decision.
	Reason string
	// Path names the rule that decided, for telemetry: one of the path*
	// constants, or "rule:capability:<tag>" for a capability hint.
	Path string
}

// Decision paths reported in RouteResult.Path and carried on every routing
// decision record.
const (
	pathNoProviders = "rule:none"
	pathVision      = "rule:vision"
	pathContext     = "rule:context"
	pathBudget      = "rule:budget"
	pathTimeout     = "rule:timeout"
	pathCapability  = "rule:capability:"
	pathWeighted    = "rule:weighted"
)

// reasonWeightedRandom is the Reason of a decision that fell through every
// rule to the weighted draw.
const reasonWeightedRandom = "weighted random"

// Rule confidences. They are hand-assigned, not calibrated; their only
// consumer is the hybrid threshold comparison (Config.MetaRouterThreshold).
const (
	confVision     = 0.95
	confContext    = 0.90
	confBudget     = 0.85
	confTimeout    = 0.80
	confCapability = 0.75
	confWeighted   = 0.50
)

// Thresholds behind rules 2–4.
const (
	// largeContextFraction is the share of a provider's context window above
	// which a request counts as large-context.
	largeContextFraction = 0.80
	// lowBudgetFraction is the share of the run budget below which the
	// cheapest provider is preferred.
	lowBudgetFraction = 0.20
	// urgentTimeoutFraction is the share of the run timeout above which the
	// fastest provider is preferred.
	urgentTimeoutFraction = 0.70
)

// reservedCapabilityKey reports whether a CapabilityRouting key names a role
// or stage rather than a keyword hint. Reserved keys are consumed by other
// selectors and must never be keyword-matched against message text.
func reservedCapabilityKey(key string) bool {
	switch key {
	case "capable", "efficient", "judge", "any":
		return true
	}
	return strings.HasPrefix(key, "stage:") || strings.HasPrefix(key, "subagent.")
}

// RuleRouter selects a model provider using deterministic rules.
// It is the fast path in both "rule-based" and "hybrid" strategies.
type RuleRouter struct {
	cfg       *Config
	startedAt time.Time
	spentUSD  float64
	turnCount int
	// randIntn picks the weighted-random index; tests inject a fixed one.
	randIntn func(n int) int
}

// NewRuleRouter creates a RuleRouter for a given configuration.
func NewRuleRouter(cfg *Config) *RuleRouter {
	return &RuleRouter{cfg: cfg, startedAt: time.Now(), randIntn: rand.Intn} //nolint:gosec // non-cryptographic routing weight
}

// Route selects a provider for the given chat completion request.
// Decision tree (evaluated in priority order):
//  0. Token limit filter: exclude providers whose MaxRequestTokens < estimated tokens
//  1. Vision capability: request contains image attachments → vision-capable model
//  2. Context length: estimated tokens > 80% of model context window → long-context model
//  3. Budget: remaining budget < 20% → cheapest model
//  4. Timeout proximity: elapsed > 70% of run timeout → fastest model
//  5. Capability routing map: check configured tag hints
//  6. Default: weighted random selection
//
// Providers with weight 0 are reserved for meta-router escalation and error
// fallback, so rules 1–4 only consider them when no weighted provider exists
// (rule 1 also widens to them when no weighted provider has the capability).
func (r *RuleRouter) Route(messages []Message, estimatedTokens int, remainingTimeoutSec float64) RouteResult {
	providers := r.cfg.Providers
	if len(providers) == 0 {
		return RouteResult{Confidence: 0, Reason: "no providers configured", Path: pathNoProviders}
	}

	// Rule 0: Filter out providers whose MaxRequestTokens is exceeded.
	// This prevents routing to providers with account-level TPM limits lower
	// than the model's context window (e.g., 30K TPM on a 128K context model).
	// When every provider is filtered out the full list is kept and the
	// provider returns a retryable error (history truncation should kick in).
	candidates := providers
	if estimatedTokens > 0 {
		if filtered := r.filterByRequestTokens(estimatedTokens); len(filtered) > 0 {
			candidates = filtered
		}
	}

	return r.routeInner(candidates, messages, estimatedTokens, remainingTimeoutSec)
}

// routeInner runs the core decision tree (Rules 1–6) over the candidate providers.
func (r *RuleRouter) routeInner(candidates []ProviderConfig, messages []Message, estimatedTokens int, remainingTimeoutSec float64) RouteResult {
	pool := primaryPool(candidates)

	// Rule 1: Vision — request contains image content.
	if hasImageContent(messages) {
		if p := findCapable(pool, "vision"); p != nil {
			return RouteResult{ProviderName: p.Name, Confidence: confVision, Reason: "vision content detected", Path: pathVision}
		}
		if p := findCapable(candidates, "vision"); p != nil {
			return RouteResult{ProviderName: p.Name, Confidence: confVision, Reason: "vision content detected", Path: pathVision}
		}
	}

	// Rule 2: Context length — estimated tokens > 80% of provider's context window.
	if estimatedTokens > 0 {
		if p := findLargeContext(pool, estimatedTokens); p != nil {
			return RouteResult{ProviderName: p.Name, Confidence: confContext, Reason: "large context detected", Path: pathContext}
		}
	}

	// Rule 3: Budget — remaining budget < 20%.
	if r.cfg.BudgetPerRunUSD > 0 {
		remaining := r.cfg.BudgetPerRunUSD - r.spentUSD
		if remaining < r.cfg.BudgetPerRunUSD*lowBudgetFraction {
			if p := findCheapest(pool); p != nil {
				return RouteResult{ProviderName: p.Name, Confidence: confBudget, Reason: "budget < 20% remaining", Path: pathBudget}
			}
		}
	}

	// Rule 4: Timeout proximity — elapsed > 70% of timeout.
	if remainingTimeoutSec > 0 {
		elapsed := time.Since(r.startedAt).Seconds()
		total := elapsed + remainingTimeoutSec
		if total > 0 && elapsed/total > urgentTimeoutFraction {
			if p := findFastest(pool); p != nil {
				return RouteResult{ProviderName: p.Name, Confidence: confTimeout, Reason: "approaching timeout", Path: pathTimeout}
			}
		}
	}

	// Rule 5: Capability routing map — check message content hints. Keys are
	// visited in sorted order so the same conversation always routes the same
	// way; role and stage keys are not keyword hints.
	for _, capability := range r.capabilityHintKeys() {
		if messagesMentionCapability(messages, capability) {
			return RouteResult{ProviderName: r.cfg.CapabilityRouting[capability], Confidence: confCapability,
				Reason: "capability routing: " + capability, Path: pathCapability + capability}
		}
	}

	// Rule 6: Weighted random default.
	p := r.weightedRandom(candidates)
	return RouteResult{ProviderName: p.Name, Confidence: confWeighted, Reason: reasonWeightedRandom, Path: pathWeighted}
}

// capabilityHintKeys returns the CapabilityRouting keys that act as keyword
// hints, sorted.
func (r *RuleRouter) capabilityHintKeys() []string {
	keys := make([]string, 0, len(r.cfg.CapabilityRouting))
	for k := range r.cfg.CapabilityRouting {
		if !reservedCapabilityKey(k) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

// primaryPool returns the providers eligible for rules 1–4: those with
// weight > 0, or all of them when none is weighted.
func primaryPool(providers []ProviderConfig) []ProviderConfig {
	pool := make([]ProviderConfig, 0, len(providers))
	for _, p := range providers {
		if p.Weight > 0 {
			pool = append(pool, p)
		}
	}
	if len(pool) == 0 {
		return providers
	}
	return pool
}

// filterByRequestTokens returns providers that can handle the given token count.
// A provider is excluded when its MaxRequestTokens is set and is less than estimatedTokens.
// Providers with MaxRequestTokens == 0 (unset) are always included.
func (r *RuleRouter) filterByRequestTokens(estimatedTokens int) []ProviderConfig {
	var eligible []ProviderConfig
	for _, p := range r.cfg.Providers {
		if p.MaxRequestTokens > 0 && estimatedTokens > p.MaxRequestTokens {
			continue
		}
		eligible = append(eligible, p)
	}
	return eligible
}

// UpdateSpend records additional spend so budget rules stay accurate.
func (r *RuleRouter) UpdateSpend(usd float64) { r.spentUSD += usd }

// IncrementTurn increments the turn counter.
func (r *RuleRouter) IncrementTurn() { r.turnCount++ }

// TurnCount returns the number of LLM turns completed.
func (r *RuleRouter) TurnCount() int { return r.turnCount }

func findCapable(providers []ProviderConfig, capability string) *ProviderConfig {
	for i := range providers {
		if slices.Contains(providers[i].Capabilities, capability) {
			return &providers[i]
		}
	}
	return nil
}

func findLargeContext(providers []ProviderConfig, estimatedTokens int) *ProviderConfig {
	// Only trigger large-context routing when tokens exceed 80% of a provider's window.
	// Returns nil (no special routing) if all providers have sufficient headroom.
	var best *ProviderConfig
	for i := range providers {
		p := &providers[i]
		if p.ContextWindow <= 0 {
			continue
		}
		if float64(estimatedTokens) < float64(p.ContextWindow)*largeContextFraction {
			continue // fits comfortably — no large-context routing needed
		}
		// Tokens are large relative to this provider; pick the one with most headroom.
		if best == nil || p.ContextWindow > best.ContextWindow {
			best = p
		}
	}
	return best
}

func findCheapest(providers []ProviderConfig) *ProviderConfig {
	var best *ProviderConfig
	for i := range providers {
		p := &providers[i]
		if best == nil || p.CostPerInputToken < best.CostPerInputToken {
			best = p
		}
	}
	return best
}

func findFastest(providers []ProviderConfig) *ProviderConfig {
	// Prefer "fast" latency profile, then "medium", then "slow".
	for _, profile := range []string{"fast", "medium", "slow"} {
		for i := range providers {
			if providers[i].LatencyProfile == profile {
				return &providers[i]
			}
		}
	}
	return &providers[0]
}

func (r *RuleRouter) weightedRandom(providers []ProviderConfig) *ProviderConfig {
	// Providers with weight=0 are reserved for meta-router escalation and error
	// fallback only — they are never selected by random weighted routing.
	total := 0
	for _, p := range providers {
		if p.Weight > 0 {
			total += p.Weight
		}
	}
	if total == 0 {
		return &providers[0]
	}
	intn := r.randIntn
	if intn == nil {
		intn = rand.Intn //nolint:gosec // non-cryptographic routing weight
	}
	pick := intn(total)
	cumulative := 0
	for i := range providers {
		if providers[i].Weight <= 0 {
			continue
		}
		cumulative += providers[i].Weight
		if pick < cumulative {
			return &providers[i]
		}
	}
	return &providers[0]
}

// hasImageContent returns true if any message contains an image URL or base64 image.
func hasImageContent(messages []Message) bool {
	for _, m := range messages {
		switch v := m.Content.(type) {
		case string:
			// Plain string content — no images.
		case []any:
			for _, part := range v {
				if partMap, ok := part.(map[string]any); ok {
					if partMap["type"] == "image_url" {
						return true
					}
				}
			}
		case []map[string]any:
			for _, part := range v {
				if part["type"] == "image_url" {
					return true
				}
			}
		default:
			_ = v
		}
	}
	return false
}

// messagesMentionCapability checks if a capability keyword appears in the conversation.
// This is a lightweight heuristic; the meta-router handles nuanced cases.
func messagesMentionCapability(messages []Message, capability string) bool {
	keywords := capabilityKeywords(capability)
	for _, m := range messages {
		if text, ok := m.Content.(string); ok {
			lower := strings.ToLower(text)
			for _, kw := range keywords {
				if strings.Contains(lower, kw) {
					return true
				}
			}
		}
	}
	return false
}

// capabilityKeywords maps capability tags to hint phrases found in user messages.
func capabilityKeywords(capability string) []string {
	switch capability {
	case "code":
		return []string{"write code", "debug", "function", "class", "implement", "program"}
	case "reasoning":
		return []string{"reason", "think step by step", "analyze", "explain why", "proof"}
	case "fast":
		return []string{"quick", "brief", "short answer", "one sentence"}
	default:
		return []string{capability}
	}
}
