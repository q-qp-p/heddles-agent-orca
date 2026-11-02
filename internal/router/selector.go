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
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/google/uuid"
)

// Selector picks the provider that serves one LLM call. The legacy rule and
// meta-router pair is one implementation; others plug in behind the same seam.
// Implementations must be safe for concurrent use.
type Selector interface {
	// Select returns the decision for one request. An error means the selector
	// could not decide; callers fall back to the first configured provider.
	Select(ctx context.Context, in SelectionInput) (Selection, error)
	// Close releases any resources the selector holds.
	Close() error
}

// SelectionInput is everything a Selector may use to decide.
type SelectionInput struct {
	// Messages is the full outbound conversation for this call, exactly as it
	// will be sent: preamble, stored history and the turn input. Selectors must
	// not prepend history again.
	Messages []Message
	// EstimatedTokens is the token estimate for Messages.
	EstimatedTokens int
	// State is the live run state (budget, timeout, turn count).
	State RoutingState
	// RemainingTimeoutSec is the time left before the run deadline; 0 = none.
	RemainingTimeoutSec float64
	// Session identifies the conversation for selectors that keep state.
	Session SessionInfo
}

// SessionInfo identifies the conversation a call belongs to.
type SessionInfo struct {
	// ID is the session ID, or the run name when the run has no session.
	ID string
	// AgentID is the Agent the run executes.
	AgentID string
	// IsSubagent marks a run spawned by another run.
	IsSubagent bool
}

// Category describes the kind of work a call performs. Every dimension is
// recorded so decisions can be grouped by any of them later.
type Category struct {
	// Stage is the structured-signal task stage (stage* constants).
	Stage string `json:"stage,omitempty"`
	// StageConf is the stage classification confidence.
	StageConf float64 `json:"stageConf,omitempty"`
	// Version is the classifier version, bumped when stage semantics change.
	Version int `json:"version,omitempty"`
	// SYLabel is the label a Switchyard algorithm assigned, when one did.
	SYLabel string `json:"syLabel,omitempty"`
	// WorkflowStep is the AgentWorkflow step the run belongs to.
	WorkflowStep string `json:"workflowStep,omitempty"`
	// Agent is the Agent the run executes.
	Agent string `json:"agent,omitempty"`
}

// strategyRuleBased is the ModelSelector strategy that uses the rules alone.
const strategyRuleBased = "rule-based"

// categoryVersion is the current stage classifier version.
const categoryVersion = 1

// AuxCall is an LLM call a selector made to reach its decision, such as a
// meta-router prompt. The router charges it to the run.
type AuxCall struct {
	// Provider is the ModelProvider the call went to.
	Provider string
	// Purpose says what the call was for, e.g. "meta-router".
	Purpose string
	// Usage is the token usage of the call.
	Usage TokenUsage
}

// Selection is a Selector's decision.
type Selection struct {
	// DecisionID identifies this decision across traces, status and telemetry.
	DecisionID string
	// Provider is the selected provider (a pointer into Config.Providers).
	Provider *ProviderConfig
	// Candidates is the ordered provider names to try: Provider first, then the
	// fallbacks.
	Candidates []string
	// Strategy is the configured routing strategy.
	Strategy string
	// Algorithm names the algorithm that decided, when the strategy has several
	// (a Switchyard algorithm type); empty for the legacy selector.
	Algorithm string
	// Path names what decided: a rule path, "meta" or "meta-cache".
	Path string
	// Reason is a human-readable explanation.
	Reason string
	// Confidence is the selector's own 0–1 certainty.
	Confidence float64
	// Category is the kind of work this call performs.
	Category Category
	// Signals is selector-specific decision metadata, kept raw.
	Signals json.RawMessage
	// AuxUsage lists the LLM calls the selector made.
	AuxUsage []AuxCall
	// Latency is how long the decision took.
	Latency time.Duration
}

// newDecisionID returns a time-ordered UUIDv7, falling back to a random UUID.
func newDecisionID() string {
	if id, err := uuid.NewV7(); err == nil {
		return id.String()
	}
	return uuid.NewString()
}

// categorize classifies the work a call performs from its messages.
func categorize(cfg *Config, messages []Message) Category {
	st := classifyStage(messages)
	return Category{
		Stage:        st.stage,
		StageConf:    st.confidence,
		Version:      categoryVersion,
		WorkflowStep: cfg.WorkflowStep,
		Agent:        cfg.AgentName,
	}
}

// legacySelector is the original selector: deterministic rules, with the LLM
// meta-router consulted in hybrid mode when the rules are not confident.
type legacySelector struct {
	cfg  *Config
	rule *RuleRouter
	meta *MetaRouter // nil unless the strategy uses the meta-router
}

func newLegacySelector(cfg *Config, rule *RuleRouter, meta *MetaRouter) *legacySelector {
	return &legacySelector{cfg: cfg, rule: rule, meta: meta}
}

// Select implements Selector.
func (s *legacySelector) Select(ctx context.Context, in SelectionInput) (Selection, error) {
	start := time.Now()
	result := s.rule.Route(in.Messages, in.EstimatedTokens, in.RemainingTimeoutSec)
	sel := Selection{
		DecisionID: newDecisionID(),
		Strategy:   s.cfg.Strategy,
		Path:       result.Path,
		Reason:     result.Reason,
		Confidence: result.Confidence,
		Category:   categorize(s.cfg, in.Messages),
	}

	// In hybrid mode, invoke the meta-router for low-confidence decisions.
	if s.meta != nil && result.Confidence < s.cfg.MetaRouterThreshold {
		metaName, usage, err := s.meta.Route(ctx, in.Messages, in.State)
		// Track meta-router LLM cost regardless of routing success.
		if usage.PromptTokens > 0 || usage.CompletionTokens > 0 {
			sel.AuxUsage = append(sel.AuxUsage, AuxCall{
				Provider: s.cfg.MetaRouterProviderName,
				Purpose:  "meta-router",
				Usage:    usage,
			})
		}
		if err != nil {
			slog.Warn("meta-router failed, using rule-based result", "err", err)
		} else {
			sel.Path = "meta"
			if len(sel.AuxUsage) == 0 {
				sel.Path = "meta-cache"
			}
			sel.Reason = "meta-router (confidence was " + fmt.Sprintf("%.2f", result.Confidence) + ")"
			sel.Confidence = confMeta
			result.ProviderName = metaName
		}
	}

	sel.Provider = s.provider(result.ProviderName)
	if sel.Provider == nil {
		sel.Latency = time.Since(start)
		return sel, nil
	}
	sel.Candidates = s.candidates(sel.Provider.Name)
	sel.Latency = time.Since(start)
	return sel, nil
}

// pathFallback marks a decision made after the selected provider failed.
const pathFallback = "fallback"

// confMeta is the confidence assigned to a meta-router decision.
const confMeta = 0.85

// provider resolves a provider name, falling back to the first configured
// provider when the name is unknown. It returns nil when none is configured.
func (s *legacySelector) provider(name string) *ProviderConfig {
	for i := range s.cfg.Providers {
		if s.cfg.Providers[i].Name == name {
			return &s.cfg.Providers[i]
		}
	}
	if len(s.cfg.Providers) > 0 {
		return &s.cfg.Providers[0]
	}
	return nil
}

// candidates lists the chosen provider followed by the fallback chain.
func (s *legacySelector) candidates(chosen string) []string {
	out := []string{chosen}
	for _, name := range s.cfg.FallbackChain {
		if !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	return out
}

// Close implements Selector.
func (s *legacySelector) Close() error { return nil }
