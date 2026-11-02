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
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	syengine "github.com/heddles/agent-orca/internal/router/switchyard"
)

const (
	// strategySwitchyard is the ModelSelector strategy backed by Switchyard.
	strategySwitchyard = "switchyard"
	// envDisableSwitchyard turns Switchyard off for this sidecar: selection falls
	// back to the rule router. It is the kill switch for a misbehaving algorithm.
	envDisableSwitchyard = "MODEL_ROUTER_DISABLE_SWITCHYARD"

	pathSwitchyard         = "switchyard"
	pathSwitchyardFailOpen = "switchyard-failopen"

	// Decision timeouts: a judge is an LLM call, a signal-only algorithm is not.
	switchyardJudgeTimeout  = 10 * time.Second
	switchyardSignalTimeout = 2 * time.Second

	// maxFailOpenReason bounds the error text copied into a decision reason.
	maxFailOpenReason = 200
)

// SwitchyardConfig is the algorithm a "switchyard" ModelSelector runs.
type SwitchyardConfig struct {
	// Algorithm is the Switchyard algorithm spec. Targets in it are ModelProvider
	// names; the model groups come from the selector's capabilityRouting roles.
	Algorithm json.RawMessage `json:"algorithm"`
	// DecisionTimeoutSec bounds one decision; 0 picks 10s with a judge, 2s without.
	DecisionTimeoutSec int `json:"decisionTimeoutSec,omitempty"`
}

// sessionSelector is implemented by selectors that keep per-session state, so a
// warm pod claimed for another conversation starts that conversation clean.
type sessionSelector interface {
	NewSession(id string)
}

// switchyardSelector routes with a Switchyard algorithm and falls back to the
// legacy selector whenever the algorithm cannot decide: a bad configuration, a
// failing judge, a timeout, an unsupported request, or a kill switch. Routing
// never fails because of it.
type switchyardSelector struct {
	cfg       *Config
	legacy    Selector
	forward   chatForwarder
	algorithm string
	timeout   time.Duration

	mu      sync.Mutex
	engine  *syengine.Engine // nil when it could not be built
	initErr error
	session string
}

func newSwitchyardSelector(cfg *Config, legacy Selector, forward chatForwarder) *switchyardSelector {
	s := &switchyardSelector{cfg: cfg, legacy: legacy, forward: forward, session: cfg.SessionID}
	s.algorithm = algorithmType(cfg.Switchyard.Algorithm)
	s.timeout = switchyardTimeout(cfg)
	s.engine, s.initErr = s.build()
	if s.initErr != nil {
		slog.Error("switchyard engine unavailable, routing with the rule router", "err", s.initErr, "run", cfg.RunName)
	}
	return s
}

// switchyardTimeout is how long one decision may take.
func switchyardTimeout(cfg *Config) time.Duration {
	if cfg.Switchyard.DecisionTimeoutSec > 0 {
		return time.Duration(cfg.Switchyard.DecisionTimeoutSec) * time.Second
	}
	if len(cfg.CapabilityRouting["judge"]) > 0 {
		return switchyardJudgeTimeout
	}
	return switchyardSignalTimeout
}

func algorithmType(spec json.RawMessage) string {
	var t struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(spec, &t)
	return t.Type
}

// build creates the engine for the current configuration. The tool classes the
// operator knows (Tool.spec.sideEffect) are added to the algorithm's
// tool_semantics; if Switchyard refuses the result, the user's spec runs as written.
func (s *switchyardSelector) build() (*syengine.Engine, error) {
	targets := make([]string, 0, len(s.cfg.Providers))
	for _, p := range s.cfg.Providers {
		targets = append(targets, p.Name)
	}
	groups, subagent := switchyardGroups(s.cfg)
	engineCfg := syengine.Config{
		Algorithm:       s.cfg.Switchyard.Algorithm,
		Targets:         targets,
		Groups:          groups,
		SubagentGroups:  subagent,
		DecisionTimeout: s.timeout,
	}
	if filled, err := syengine.FillToolSemantics(engineCfg.Algorithm, toolSemantics(s.cfg.ToolDefinitions)); err == nil && string(filled) != string(engineCfg.Algorithm) {
		withTools := engineCfg
		withTools.Algorithm = filled
		if e, err := syengine.New(withTools, s.modelCaller); err == nil {
			return e, nil
		} else {
			slog.Warn("switchyard rejected the tool classes added to the algorithm, using it as written", "err", err)
		}
	}
	return syengine.New(engineCfg, s.modelCaller)
}

// switchyardGroups derives Switchyard's model groups from the selector: "any" is
// every provider that may answer (weighted), capable / efficient / judge come from
// the capabilityRouting keys of those names, and "subagent.<group>" keys feed the
// groups used for delegated work.
func switchyardGroups(cfg *Config) (groups, subagent map[string][]string) {
	var answerers []string
	for _, p := range cfg.Providers {
		if p.Weight > 0 {
			answerers = append(answerers, p.Name)
		}
	}
	if len(answerers) == 0 {
		for _, p := range cfg.Providers {
			answerers = append(answerers, p.Name)
		}
	}
	has := func(name string) bool {
		return slices.ContainsFunc(cfg.Providers, func(p ProviderConfig) bool { return p.Name == name })
	}
	groups = map[string][]string{"any": answerers}
	for _, role := range []string{"capable", "efficient", "judge"} {
		if name := cfg.CapabilityRouting[role]; name != "" && has(name) {
			groups[role] = []string{name}
		}
	}
	for key, name := range cfg.CapabilityRouting {
		if group, ok := strings.CutPrefix(key, "subagent."); ok && group != "" && has(name) {
			if subagent == nil {
				subagent = map[string][]string{"any": answerers}
			}
			subagent[group] = []string{name}
		}
	}
	return groups, subagent
}

// toolSemantics maps tool side-effect classes to what they say about progress:
// reading is investigation, writing and actuating is production.
func toolSemantics(tools []ToolDefinition) syengine.ToolSemantics {
	var sem syengine.ToolSemantics
	for _, t := range tools {
		switch t.SideEffect {
		case "read":
			sem.Observe = append(sem.Observe, t.Name)
		case "write", "actuate":
			sem.Mutate = append(sem.Mutate, t.Name)
		}
	}
	return sem
}

type auxKey struct{}

// auxCollector gathers the LLM calls a decision makes (the judge) so the router
// can charge them to the run, whether or not the decision succeeded.
type auxCollector struct {
	mu    sync.Mutex
	calls []AuxCall
}

func (c *auxCollector) add(call AuxCall) {
	c.mu.Lock()
	c.calls = append(c.calls, call)
	c.mu.Unlock()
}

// modelCaller serves the engine's judge calls with the router's own forwarders.
func (s *switchyardSelector) modelCaller(ctx context.Context, provider string, body []byte) ([]byte, error) {
	var p *ProviderConfig
	for i := range s.cfg.Providers {
		if s.cfg.Providers[i].Name == provider {
			p = &s.cfg.Providers[i]
			break
		}
	}
	if p == nil {
		return nil, fmt.Errorf("no provider %q", provider)
	}
	var req ChatCompletionRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("judge request: %w", err)
	}
	req.Stream = false
	if req.MaxTokens == 0 {
		req.MaxTokens = req.MaxCompletionTokens
	}
	resp, err := s.forward(ctx, p, req)
	if err != nil {
		return nil, err
	}
	var usage struct {
		Usage TokenUsage `json:"usage"`
	}
	if json.Unmarshal(resp, &usage) == nil {
		if c, ok := ctx.Value(auxKey{}).(*auxCollector); ok {
			c.add(AuxCall{Provider: provider, Purpose: "switchyard-judge", Usage: usage.Usage})
		}
	}
	return resp, nil
}

// Select implements Selector.
func (s *switchyardSelector) Select(ctx context.Context, in SelectionInput) (Selection, error) {
	start := time.Now()
	collector := &auxCollector{}
	ctx = context.WithValue(ctx, auxKey{}, collector)

	s.mu.Lock()
	engine, initErr := s.engine, s.initErr
	s.mu.Unlock()
	if engine == nil {
		if initErr == nil {
			initErr = errors.New("the engine is not running")
		}
		return s.failOpen(ctx, in, collector, start, initErr)
	}

	body, err := switchyardRequest(in.Messages)
	if err != nil {
		return s.failOpen(ctx, in, collector, start, err)
	}
	res, err := engine.Route(ctx, body, syengine.Metadata{
		SessionID: in.Session.ID, AgentID: in.Session.AgentID, IsSubagent: in.Session.IsSubagent,
	})
	if err != nil {
		return s.failOpen(ctx, in, collector, start, err)
	}
	provider := s.provider(res.Selected[0])
	if provider == nil {
		return s.failOpen(ctx, in, collector, start, fmt.Errorf("selected %q, which is not a provider", res.Selected[0]))
	}

	confidence, label, source := parseSignals(res.Metadata)
	category := categorize(s.cfg, in.Messages)
	category.SYLabel = label
	reason := "switchyard " + s.algorithm
	if source != "" {
		reason += " (" + source + ")"
	}
	return Selection{
		DecisionID: newDecisionID(),
		Provider:   provider,
		Candidates: s.candidates(res.Selected),
		Strategy:   s.cfg.Strategy,
		Algorithm:  s.algorithm,
		Path:       pathSwitchyard,
		Reason:     reason + ": " + provider.Name,
		Confidence: confidence,
		Category:   category,
		Signals:    res.Metadata,
		AuxUsage:   collector.calls,
		Latency:    time.Since(start),
	}, nil
}

// failOpen routes with the legacy selector and records why Switchyard did not.
// The judge calls it already made are still charged: they cost money.
func (s *switchyardSelector) failOpen(ctx context.Context, in SelectionInput, aux *auxCollector, start time.Time, cause error) (Selection, error) {
	msg := cause.Error()
	if len(msg) > maxFailOpenReason {
		msg = msg[:maxFailOpenReason] + "..."
	}
	slog.Warn("switchyard could not decide, routing with the rule router", "err", cause, "run", s.cfg.RunName)
	sel, err := s.legacy.Select(ctx, in)
	sel.Algorithm = s.algorithm
	sel.Path = pathSwitchyardFailOpen
	sel.Reason = "switchyard failed (" + msg + "); " + sel.Reason
	sel.Signals, _ = json.Marshal(map[string]string{"error": msg})
	sel.AuxUsage = append(slices.Clone(aux.calls), sel.AuxUsage...)
	sel.Latency = time.Since(start)
	return sel, err
}

func (s *switchyardSelector) provider(name string) *ProviderConfig {
	for i := range s.cfg.Providers {
		if s.cfg.Providers[i].Name == name {
			return &s.cfg.Providers[i]
		}
	}
	return nil
}

// candidates lists Switchyard's choices, best first, then the fallback chain.
func (s *switchyardSelector) candidates(selected []string) []string {
	out := slices.Clone(selected)
	for _, name := range s.cfg.FallbackChain {
		if !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	return out
}

// NewSession implements sessionSelector: a different conversation gets an engine
// with no holds or latches left over from the previous one.
func (s *switchyardSelector) NewSession(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == s.session && s.engine != nil {
		return
	}
	s.session = id
	old := s.engine
	s.engine, s.initErr = s.build()
	old.Close()
}

// Close implements Selector.
func (s *switchyardSelector) Close() error {
	s.mu.Lock()
	s.engine.Close()
	s.engine = nil
	s.mu.Unlock()
	return s.legacy.Close()
}

// switchyardMessage and switchyardToolCall carry only standard OpenAI chat
// fields. The router's own types also hold provider extras (Gemini's
// extra_content, streaming indexes) that are not Switchyard's business.
type switchyardMessage struct {
	Role       string               `json:"role"`
	Content    any                  `json:"content"`
	Name       string               `json:"name,omitempty"`
	ToolCallID string               `json:"tool_call_id,omitempty"`
	ToolCalls  []switchyardToolCall `json:"tool_calls,omitempty"`
}

type switchyardToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// switchyardRequest renders the conversation as the OpenAI chat request
// Switchyard decodes.
func switchyardRequest(messages []Message) ([]byte, error) {
	out := make([]switchyardMessage, len(messages))
	for i, m := range messages {
		out[i] = switchyardMessage{Role: m.Role, Content: m.Content, Name: m.Name, ToolCallID: m.ToolCallID}
		for _, tc := range m.ToolCalls {
			typ := tc.Type
			if typ == "" {
				typ = toolTypeFunction
			}
			out[i].ToolCalls = append(out[i].ToolCalls, switchyardToolCall{ID: tc.ID, Type: typ, Function: tc.Function})
		}
		if out[i].Content == nil {
			out[i].Content = ""
		}
	}
	return json.Marshal(map[string]any{"model": "default", "messages": out})
}

// parseSignals reads the algorithm's decision evidence tolerantly: Switchyard is
// pre-1.0 and its metadata may grow or rename keys, so a missing one is not an
// error. The raw JSON is kept on the Selection regardless.
func parseSignals(raw json.RawMessage) (confidence float64, label, source string) {
	var m struct {
		Evidence map[string]any `json:"evidence"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return 0, "", ""
	}
	for _, k := range []string{"confidence", "score", "p_solve"} {
		if v, ok := m.Evidence[k].(float64); ok {
			confidence = v
			break
		}
	}
	source, _ = m.Evidence["source"].(string)
	label = source
	for _, k := range []string{"primary_rule", "capability_boundary"} {
		if v, ok := m.Evidence[k].(string); ok && v != "" {
			label = v
			break
		}
	}
	return confidence, label, source
}
