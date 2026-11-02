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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubSelector records its input and returns a canned selection.
type stubSelector struct {
	got SelectionInput
	sel Selection
}

func (s *stubSelector) Select(_ context.Context, in SelectionInput) (Selection, error) {
	s.got = in
	return s.sel, nil
}
func (s *stubSelector) Close() error { return nil }

func newSelectionTestRouter(t *testing.T, mutate func(*Config)) *Router {
	t.Helper()
	keyFile := writeKeyFile(t)
	cfg := &Config{
		RunName:           "test-run",
		RunNamespace:      "default",
		LLMRequestTimeout: time.Hour,
		SATokenFile:       keyFile,
		Providers: []ProviderConfig{
			{Name: "a", LiteLLMModel: "openai/a", APIKeyFile: keyFile, Weight: 1, CostPerInputToken: 0.001},
			{Name: "b", LiteLLMModel: "openai/b", APIKeyFile: keyFile, Weight: 1},
		},
		Safeguards: RouterSafeguards{ToolExecutionTimeoutSec: 60},
	}
	if mutate != nil {
		mutate(cfg)
	}
	r, err := New(cfg, newTestStore(t), nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { r.cancelFunc() })
	return r
}

func TestSelectProvider_PassesConversationOnceWithRunState(t *testing.T) {
	deadline := time.Now().Add(time.Hour)
	r := newSelectionTestRouter(t, func(c *Config) {
		c.BudgetPerRunUSD = 10
		c.RunDeadline = &deadline
		c.SessionID = "sess-1"
		c.AgentName = "agent-a"
		c.ParentRunName = "parent"
	})
	// History the router already holds must not be prepended to the request:
	// the outbound conversation already contains it.
	r.priorMessages = []Message{{Role: "user", Content: "old"}}
	r.messages = []Message{{Role: "assistant", Content: "older"}}
	r.ruleRouter.UpdateSpend(4)
	r.ruleRouter.IncrementTurn()
	stub := &stubSelector{sel: Selection{Provider: &r.cfg.Providers[1]}}
	r.selector = stub

	outbound := []Message{{Role: "system", Content: "s"}, {Role: "user", Content: "old"}, {Role: "user", Content: "new"}}
	provider, sel := r.selectProvider(context.Background(), outbound)

	if len(stub.got.Messages) != len(outbound) {
		t.Errorf("selector saw %d messages, want exactly the %d outbound ones", len(stub.got.Messages), len(outbound))
	}
	if stub.got.EstimatedTokens <= 0 {
		t.Errorf("EstimatedTokens = %d, want > 0 so the token rules can fire", stub.got.EstimatedTokens)
	}
	st := stub.got.State
	if st.BudgetUSD != 10 || st.SpentUSD != 4 || st.TurnCount != 1 || st.TimeoutTotalSec <= 0 {
		t.Errorf("State = %+v, want budget 10 / spent 4 / turn 1 / a timeout", st)
	}
	if rem := stub.got.RemainingTimeoutSec; rem < 3500 || rem > 3600 {
		t.Errorf("RemainingTimeoutSec = %v, want ~3600", rem)
	}
	if s := stub.got.Session; s.ID != "sess-1" || s.AgentID != "agent-a" || !s.IsSubagent {
		t.Errorf("Session = %+v", s)
	}
	if provider == nil || provider.Name != "b" || sel.DecisionID == "" {
		t.Errorf("provider %v decision %q", provider, sel.DecisionID)
	}
}

func TestSelectProvider_SessionFallsBackToRunName(t *testing.T) {
	r := newSelectionTestRouter(t, nil)
	stub := &stubSelector{sel: Selection{Provider: &r.cfg.Providers[0]}}
	r.selector = stub
	r.selectProvider(context.Background(), ruleUserTurn("x"))
	if got := stub.got.Session.ID; got != "test-run" {
		t.Errorf("Session.ID = %q, want the run name", got)
	}
}

func TestSelectProvider_ChargesSelectorCallsToTheRun(t *testing.T) {
	r := newSelectionTestRouter(t, nil)
	r.selector = &stubSelector{sel: Selection{
		Provider: &r.cfg.Providers[1],
		AuxUsage: []AuxCall{{Provider: "a", Purpose: "meta-router", Usage: TokenUsage{PromptTokens: 1000}}},
	}}
	r.selectProvider(context.Background(), ruleUserTurn("x"))
	if got, want := r.SpendUSD(), 1.0; got < want-1e-9 || got > want+1e-9 {
		t.Errorf("spend = %v, want %v (1000 prompt tokens at 0.001)", got, want)
	}
}

func TestSelectProvider_FallsBackToFirstProviderWhenSelectorYieldsNone(t *testing.T) {
	r := newSelectionTestRouter(t, nil)
	r.selector = &stubSelector{}
	provider, sel := r.selectProvider(context.Background(), ruleUserTurn("x"))
	if provider == nil || provider.Name != "a" || sel.Reason == "" {
		t.Errorf("provider %v reason %q, want the first provider with a reason", provider, sel.Reason)
	}
}

// Regression: selectProvider passed an estimate of 0, so the MaxRequestTokens
// filter (rule 0) never fired in production.
func TestSelectProvider_RequestTokenFilterFiresEndToEnd(t *testing.T) {
	r := newSelectionTestRouter(t, func(c *Config) { c.Providers[0].MaxRequestTokens = 50 })
	big := []Message{{Role: "user", Content: string(make([]byte, 4000))}}
	for range 30 {
		if p, _ := r.selectProvider(context.Background(), big); p.Name != "b" {
			t.Fatalf("provider %q chosen although its MaxRequestTokens is exceeded", p.Name)
		}
	}
}

// Regression: with no timeout reaching the router, the near-timeout rule never fired.
func TestSelectProvider_TimeoutRuleFiresWithRunDeadline(t *testing.T) {
	deadline := time.Now().Add(5 * time.Second)
	r := newSelectionTestRouter(t, func(c *Config) {
		c.RunDeadline = &deadline
		c.Providers[0].LatencyProfile = "slow"
		c.Providers[1].LatencyProfile = "fast"
	})
	r.ruleRouter.startedAt = time.Now().Add(-60 * time.Second) // 92% of the window gone
	_, sel := r.selectProvider(context.Background(), ruleUserTurn("x"))
	if sel.Path != pathTimeout || sel.Provider.Name != "b" {
		t.Errorf("path %q provider %q, want the timeout rule on the fast provider", sel.Path, sel.Provider.Name)
	}
}

// routeRecorder captures the decisions the router posts to the operator API.
type routeRecorder struct {
	mu   sync.Mutex
	got  []map[string]string
	srv  *httptest.Server
	path string
}

func newRouteRecorder(t *testing.T) *routeRecorder {
	t.Helper()
	rec := &routeRecorder{}
	rec.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
		if !strings.HasSuffix(r.URL.Path, "/route") {
			return // other operator calls (context usage) are not routing decisions
		}
		body, _ := io.ReadAll(r.Body)
		var m map[string]string
		_ = json.Unmarshal(body, &m)
		rec.mu.Lock()
		rec.got = append(rec.got, m)
		rec.path = r.URL.Path
		rec.mu.Unlock()
	}))
	t.Cleanup(rec.srv.Close)
	return rec
}

func (rec *routeRecorder) decisions() []map[string]string {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return append([]map[string]string(nil), rec.got...)
}

// waitFor polls until n decisions arrived, then waits a beat to catch extras.
func (rec *routeRecorder) waitFor(t *testing.T, n int) []map[string]string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for len(rec.decisions()) < n && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(150 * time.Millisecond)
	return rec.decisions()
}

func TestHandleChatCompletions_PostsExactlyOneDecisionPerCall(t *testing.T) {
	prov := newCapturingProvider(t, false)
	rec := newRouteRecorder(t)
	r := newSelectionTestRouter(t, func(c *Config) {
		c.OperatorAPIURL = rec.srv.URL
		c.Strategy = strategyRuleBased
		c.Providers = c.Providers[:1]
		c.Providers[0].BaseURL = prov.srv.URL
	})

	if code := serve(r, conversation(t, 2)).Code; code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	got := rec.waitFor(t, 1)
	if len(got) != 1 {
		t.Fatalf("posted %d decisions for one call, want 1: %v", len(got), got)
	}
	d := got[0]
	if d["provider"] != "a" || d["decisionID"] == "" || d["path"] != pathWeighted || d["category"] == "" || d["parentDecisionID"] != "" {
		t.Errorf("decision = %v", d)
	}
	if rec.path != "/agentrun/default/test-run/route" {
		t.Errorf("posted to %q", rec.path)
	}
}

func TestHandleChatCompletions_FallbackPostsLinkedDecision(t *testing.T) {
	failing := newCapturingProvider(t, true)
	working := newCapturingProvider(t, false)
	rec := newRouteRecorder(t)
	r := newSelectionTestRouter(t, func(c *Config) {
		c.OperatorAPIURL = rec.srv.URL
		c.Strategy = strategyRuleBased
		c.FallbackChain = []string{"primary", "fallback"}
		c.Providers = []ProviderConfig{
			{Name: "primary", LiteLLMModel: "openai/p", BaseURL: failing.srv.URL, APIKeyFile: c.SATokenFile, Weight: 1},
			{Name: "fallback", LiteLLMModel: "openai/f", BaseURL: working.srv.URL, APIKeyFile: c.SATokenFile, Weight: 0},
		}
	})

	if code := serve(r, conversation(t, 2)).Code; code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	got := rec.waitFor(t, 2)
	if len(got) != 2 {
		t.Fatalf("posted %d decisions, want the primary and the fallback: %v", len(got), got)
	}
	var primary, fallback map[string]string
	for _, d := range got {
		if d["path"] == pathFallback {
			fallback = d
		} else {
			primary = d
		}
	}
	if primary == nil || fallback == nil {
		t.Fatalf("decisions = %v, want one fallback-path decision and one other", got)
	}
	if primary["provider"] != "primary" || fallback["provider"] != "fallback" {
		t.Errorf("providers = %q / %q", primary["provider"], fallback["provider"])
	}
	if fallback["parentDecisionID"] != primary["decisionID"] || fallback["decisionID"] == primary["decisionID"] {
		t.Errorf("fallback parent %q, primary %q: the fallback must point at the decision it replaced",
			fallback["parentDecisionID"], primary["decisionID"])
	}
}
