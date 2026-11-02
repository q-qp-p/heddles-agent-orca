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
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
)

// metaRouterCacheEntry caches a meta-router decision for
// (task-hash, available-models, state-buckets).
type metaRouterCacheEntry struct {
	providerName string
	expiresAt    time.Time
}

// RoutingState carries the live run state that can invalidate a cached
// meta-router decision (#60 F2). A decision cached at turn 1 must not be
// served at turn 30 once the budget crosses the rule-router's <20% gate:
// a stale cached provider defeats BudgetCap enforcement in hybrid mode.
// The zero value means "state unknown" — all buckets collapse to their
// neutral form and the cache behaves exactly as before.
type RoutingState struct {
	// BudgetUSD is the total run budget; 0 = unlimited.
	BudgetUSD float64
	// SpentUSD is the USD spent so far on the run.
	SpentUSD float64
	// TimeoutTotalSec is the total run timeout; 0 = none.
	TimeoutTotalSec float64
	// TimeoutElapsedSec is the elapsed portion of the timeout.
	TimeoutElapsedSec float64
	// TurnCount is the number of completed LLM turns.
	TurnCount int
}

// budgetBucket maps the run's budget state to a cache-key bucket.
// The boundary (20% remaining) matches rule-router Rule 3 exactly, so a
// cached decision can never override a state-forced cheapest-provider route.
func (s RoutingState) budgetBucket() string {
	if s.BudgetUSD <= 0 {
		return "unlimited"
	}
	if s.BudgetUSD-s.SpentUSD < s.BudgetUSD*0.20 {
		return "low"
	}
	return "high"
}

// timeoutBucket maps the run's timeout state to a cache-key bucket.
// The boundary (70% elapsed) matches rule-router Rule 4 exactly.
func (s RoutingState) timeoutBucket() string {
	if s.TimeoutTotalSec <= 0 {
		return "none"
	}
	if s.TimeoutElapsedSec/s.TimeoutTotalSec > 0.70 {
		return "urgent"
	}
	return "ok"
}

// turnBucket maps the turn count to a coarse cache-key bucket.
func (s RoutingState) turnBucket() string {
	switch {
	case s.TurnCount == 0:
		return "fresh"
	case s.TurnCount <= 10:
		return "early"
	default:
		return "deep"
	}
}

// MetaRouter uses a cheap/fast LLM to decide which model to use for ambiguous requests.
// It is only invoked when the rule-router's confidence falls below the configured threshold.
type MetaRouter struct {
	cfg *Config
	// forward sends the routing prompt to a provider through the router's own
	// forwarders, so every provider wire format (Anthropic Messages included)
	// is spoken correctly. Bound by New once the Router exists.
	forward  chatForwarder
	cache    map[string]metaRouterCacheEntry
	cacheMu  sync.Mutex
	cacheTTL time.Duration
}

// metaRouterTimeout bounds one meta-router LLM call; a slow routing model must
// not stall the request that is waiting on the decision.
const metaRouterTimeout = 15 * time.Second

// chatForwarder sends one non-streaming chat completion to a provider and
// returns the OpenAI-shaped response body.
type chatForwarder func(ctx context.Context, provider *ProviderConfig, req ChatCompletionRequest) ([]byte, error)

// NewMetaRouter creates a MetaRouter from the given configuration. forward
// carries its LLM calls; it may be nil until bound.
func NewMetaRouter(cfg *Config, forward chatForwarder) *MetaRouter {
	return &MetaRouter{
		cfg:      cfg,
		forward:  forward,
		cache:    make(map[string]metaRouterCacheEntry),
		cacheTTL: 60 * time.Second,
	}
}

// Route asks the meta-router LLM to select the best provider for the given messages.
// The optional state captures the live run state used to bucket the cache key.
// Returns the provider name or an error; callers fall back to rule-based routing on error.
func (m *MetaRouter) Route(ctx context.Context, messages []Message, state ...RoutingState) (string, TokenUsage, error) {
	// Find the meta-router provider.
	var metaProvider *ProviderConfig
	for i := range m.cfg.Providers {
		if m.cfg.Providers[i].Name == m.cfg.MetaRouterProviderName {
			metaProvider = &m.cfg.Providers[i]
			break
		}
	}
	if metaProvider == nil {
		return "", TokenUsage{}, fmt.Errorf("meta-router provider %q not found in provider list", m.cfg.MetaRouterProviderName)
	}

	// Build a cache key from the task content hash + available model names
	// + live state buckets, so a stale decision can never override a
	// budget/timeout-forced route (#60 F2).
	cacheKey := m.cacheKey(messages, routingState(state))
	if entry := m.getCached(cacheKey); entry != "" {
		return entry, TokenUsage{}, nil
	}

	// Build the routing prompt.
	routingPrompt := m.buildRoutingPrompt(messages)
	providerName, usage, err := m.callLLM(ctx, metaProvider, routingPrompt)
	if err != nil {
		return "", usage, err
	}

	resolved, ok := m.resolveProviderName(providerName)
	if !ok {
		return "", usage, fmt.Errorf("meta-router returned unknown provider %q", providerName)
	}
	slog.Info("meta-router LLM response", "raw", providerName, "resolved", resolved)
	m.setCached(cacheKey, resolved)
	return resolved, usage, nil
}

// resolveProviderName maps a raw meta-router LLM response to a configured
// provider name deterministically (#60 F1, F5):
//
//  1. exact match (trimmed)
//  2. case-insensitive exact match (LLMs return "GPT-4o-Mini")
//  3. substring fallback — the LONGEST provider name contained in the
//     response wins, because longer names are the more specific match:
//     "gpt-4o" is a substring of "gpt-4o-mini", so a literal "gpt-4o-mini"
//     response must resolve to gpt-4o-mini regardless of provider list order.
//     Ambiguity (more than one candidate) is logged, never silently resolved.
//
// Blank/whitespace responses are rejected with ok=false so the caller's
// error is distinct from a genuine unknown-provider error.
func (m *MetaRouter) resolveProviderName(raw string) (string, bool) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", false
	}

	// 1. Exact match.
	for _, p := range m.cfg.Providers {
		if p.Name == name {
			return p.Name, true
		}
	}

	// 2. Case-insensitive exact match.
	for _, p := range m.cfg.Providers {
		if strings.EqualFold(p.Name, name) {
			return p.Name, true
		}
	}

	// 3. Substring fallback: longest contained provider name wins.
	lower := strings.ToLower(name)
	best := ""
	candidates := 0
	for _, p := range m.cfg.Providers {
		if p.Name == "" {
			continue
		}
		if strings.Contains(lower, strings.ToLower(p.Name)) {
			candidates++
			if len(p.Name) > len(best) {
				best = p.Name
			}
		}
	}
	if best != "" {
		if candidates > 1 {
			slog.Info("meta-router fuzzy match ambiguous, chose longest", "raw", name, "provider", best, "candidates", candidates)
		} else {
			slog.Info("meta-router fuzzy match", "provider", best, "raw", name)
		}
		return best, true
	}
	return "", false
}

// buildRoutingPrompt constructs a routing decision prompt.
// The routing hint for each provider is the primary decision criterion.
func (m *MetaRouter) buildRoutingPrompt(messages []Message) string {
	// Extract the last user message as the task summary.
	taskSummary := ""
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" { //nolint:goconst
			if text, ok := messages[i].Content.(string); ok {
				if len(text) > 500 {
					text = text[:500] + "..."
				}
				taskSummary = text
				break
			}
		}
	}

	var sb strings.Builder
	sb.WriteString("Match the task below to the correct provider using the routing guidelines.\n\n")
	sb.WriteString("TASK:\n" + taskSummary + "\n\n")
	sb.WriteString("PROVIDERS:\n")
	for _, p := range m.cfg.Providers {
		if p.RoutingHint != "" {
			sb.WriteString(fmt.Sprintf("- %s: %s\n", p.Name, p.RoutingHint))
		} else {
			// No routing hint — fall back to capability/latency metadata.
			sb.WriteString(fmt.Sprintf("- %s: capabilities=%v latency=%s\n", p.Name, p.Capabilities, p.LatencyProfile))
		}
	}
	sb.WriteString("\nProvider name:")
	return sb.String()
}

// callLLM sends a single-turn request to the meta-router provider and returns the response text
// along with token usage for cost tracking.
func (m *MetaRouter) callLLM(ctx context.Context, provider *ProviderConfig, prompt string) (string, TokenUsage, error) {
	if m.forward == nil {
		return "", TokenUsage{}, fmt.Errorf("meta-router has no provider forwarder")
	}
	temperature := 0.0
	body, err := m.forward(ctx, provider, ChatCompletionRequest{
		Messages:    []Message{{Role: "user", Content: prompt}},
		MaxTokens:   50,
		Temperature: &temperature,
	})
	if err != nil {
		return "", TokenUsage{}, fmt.Errorf("meta-router LLM call: %w", err)
	}

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage TokenUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", TokenUsage{}, fmt.Errorf("parsing meta-router response: %w", err)
	}
	if len(result.Choices) == 0 {
		return "", TokenUsage{}, fmt.Errorf("meta-router returned no choices")
	}
	return result.Choices[0].Message.Content, result.Usage, nil
}

func (m *MetaRouter) cacheKey(messages []Message, state RoutingState) string {
	h := sha256.New()
	for _, msg := range messages {
		if text, ok := msg.Content.(string); ok {
			h.Write([]byte(text))
		}
	}
	for _, p := range m.cfg.Providers {
		h.Write([]byte(p.Name))
	}
	// State buckets — coarse enough to keep hit rate high, sharp enough at
	// the rule-router boundaries that a stale decision is always a miss.
	h.Write([]byte("b:" + state.budgetBucket()))
	h.Write([]byte(";t:" + state.timeoutBucket()))
	h.Write([]byte(";turn:" + state.turnBucket()))
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func (m *MetaRouter) getCached(key string) string {
	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()
	entry, ok := m.cache[key]
	if !ok || time.Now().After(entry.expiresAt) {
		return ""
	}
	return entry.providerName
}

func (m *MetaRouter) setCached(key, providerName string) {
	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()
	m.cache[key] = metaRouterCacheEntry{
		providerName: providerName,
		expiresAt:    time.Now().Add(m.cacheTTL),
	}
}

// routingState returns the first RoutingState, or the zero value when none
// was supplied (keeps existing call sites compiling unchanged).
func routingState(states []RoutingState) RoutingState {
	if len(states) > 0 {
		return states[0]
	}
	return RoutingState{}
}

// liteLLMEndpoint returns the LiteLLM proxy endpoint for a provider.
// We use a local LiteLLM instance that the operator deploys as a separate pod,
// or we call the provider directly using litellm library conventions via the OpenAI SDK.
// For simplicity, we translate to the provider's native endpoint here.
func liteLLMEndpoint(provider *ProviderConfig) string {
	// If the provider has an explicit BaseURL, use it directly.
	if provider.BaseURL != "" {
		baseURL := strings.TrimRight(provider.BaseURL, "/")
		// Check if the URL already ends with /v1/chat/completions or /v1 to avoid double-appending.
		// Some providers like OpenRouter have endpoints like https://openrouter.ai/api/v1/chat/completions
		// where the user might set baseURL to https://openrouter.ai/api/v1
		if strings.HasSuffix(baseURL, "/v1/chat/completions") {
			return baseURL
		}
		if strings.HasSuffix(baseURL, "/v1") {
			// BaseURL ends with /v1, append /chat/completions directly
			return baseURL + "/chat/completions"
		}
		return baseURL + "/v1/chat/completions"
	}

	// Otherwise, map from the LiteLLM model prefix to the default endpoint.
	model := provider.LiteLLMModel
	switch {
	case strings.HasPrefix(model, "anthropic/"):
		return "https://api.anthropic.com/v1/messages"
	case strings.HasPrefix(model, "openai/"):
		return "https://api.openai.com/v1/chat/completions"
	case strings.HasPrefix(model, "gemini/"):
		return "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions"
	case strings.HasPrefix(model, "ollama/"):
		return "http://localhost:11434/v1/chat/completions"
	default:
		slog.Warn("unknown provider prefix, using OpenAI endpoint", "model", model)
		return "https://api.openai.com/v1/chat/completions"
	}
}

// readAPIKey reads an API key from a file path. The file contents are
// trimmed of leading/trailing whitespace before being returned. This is
// the single entry point for reading provider API keys across the
// streaming, non-streaming, anthropic, and meta-router code paths.
func readAPIKey(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("no API key file configured")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}
