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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testProviderFull = "gpt-4o"
	testProviderMini = "gpt-4o-mini"
)

// fakeMetaProviderConfig builds a Config with two providers (whose names
// deliberately collide on substring: gpt-4o ⊂ gpt-4o-mini) pointed at the
// given LiteLLM-compatible test server.
func fakeMetaProviderConfig(baseURL, apiKeyFile string, order int) *Config {
	a, b := testProviderFull, testProviderMini
	if order == 1 {
		a, b = testProviderMini, testProviderFull
	}
	return &Config{
		Providers: []ProviderConfig{
			{Name: a, LiteLLMModel: a, BaseURL: baseURL, APIKeyFile: apiKeyFile},
			{Name: b, LiteLLMModel: b, BaseURL: baseURL, APIKeyFile: apiKeyFile},
		},
		MetaRouterProviderName: a,
	}
}

// testMetaForward returns the forwarder a Router would bind to its
// meta-router: the real provider forwarders, bounded like production.
func testMetaForward(cfg *Config) chatForwarder {
	r := &Router{cfg: cfg}
	return func(ctx context.Context, p *ProviderConfig, req ChatCompletionRequest) ([]byte, error) {
		return r.forwardToProviderWithin(ctx, p, req, metaRouterTimeout)
	}
}

func writeTestAPIKeyFile(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "key")
	if err := os.WriteFile(path, []byte("test-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// fakeMetaLLMServer returns an httptest server that responds to any
// chat/completions request with the configured provider name text.
func fakeMetaLLMServer(t *testing.T, response string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"content": response}},
			},
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
		})
	}))
}

func TestResolveProviderName(t *testing.T) {
	tests := []struct {
		name      string
		providers []string
		raw       string
		want      string
		wantOK    bool
	}{
		{
			name:      "exact match",
			providers: []string{testProviderFull, testProviderMini},
			raw:       testProviderFull,
			want:      testProviderFull,
			wantOK:    true,
		},
		{
			name:      "mini response resolves to mini even when gpt-4o is first",
			providers: []string{testProviderFull, testProviderMini},
			raw:       testProviderMini,
			want:      testProviderMini,
			wantOK:    true,
		},
		{
			name:      "mini response resolves to mini when mini is first",
			providers: []string{testProviderMini, testProviderFull},
			raw:       testProviderMini,
			want:      testProviderMini,
			wantOK:    true,
		},
		{
			name:      "case-insensitive exact",
			providers: []string{"GPT-4o-Mini"},
			raw:       testProviderMini,
			want:      "GPT-4o-Mini",
			wantOK:    true,
		},
		{
			name:      "fuzzy with surrounding text",
			providers: []string{"claude-sonnet-4-6"},
			raw:       "The best provider is claude-sonnet-4-6.",
			want:      "claude-sonnet-4-6",
			wantOK:    true,
		},
		{
			name:      "blank response rejected",
			providers: []string{testProviderFull},
			raw:       "   ",
			want:      "",
			wantOK:    false,
		},
		{
			name:      "empty response rejected",
			providers: []string{testProviderFull},
			raw:       "",
			want:      "",
			wantOK:    false,
		},
		{
			name:      "unknown provider rejected",
			providers: []string{testProviderFull},
			raw:       "gpt-5",
			want:      "",
			wantOK:    false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{Providers: make([]ProviderConfig, 0, len(tt.providers))}
			for _, n := range tt.providers {
				cfg.Providers = append(cfg.Providers, ProviderConfig{Name: n})
			}
			m := NewMetaRouter(cfg, testMetaForward(cfg))
			got, ok := m.resolveProviderName(tt.raw)
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("resolveProviderName(%q) = (%q, %v), want (%q, %v)", tt.raw, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestRouteResolvesMiniRegardlessOfProviderOrder(t *testing.T) {
	for order := range 2 {
		t.Run(fmt.Sprintf("order%d", order), func(t *testing.T) {
			apiKeyFile := writeTestAPIKeyFile(t)
			srv := fakeMetaLLMServer(t, testProviderMini)
			defer srv.Close()
			cfg := fakeMetaProviderConfig(srv.URL, apiKeyFile, order)
			m := NewMetaRouter(cfg, testMetaForward(cfg))
			name, _, err := m.Route(context.Background(), []Message{{Role: "user", Content: "hi"}})
			if err != nil {
				t.Fatalf("Route: %v", err)
			}
			if name != testProviderMini {
				t.Errorf("got %q, want %q (order %d)", name, testProviderMini, order)
			}
		})
	}
}

func TestRouteCacheKeyRespectsStateBuckets(t *testing.T) {
	apiKeyFile := writeTestAPIKeyFile(t)
	srv := fakeMetaLLMServer(t, testProviderMini)
	defer srv.Close()
	cfg := fakeMetaProviderConfig(srv.URL, apiKeyFile, 0)
	m := NewMetaRouter(cfg, testMetaForward(cfg))
	msgs := []Message{{Role: "user", Content: "same task"}}

	// First call caches under the high-budget bucket.
	first := RoutingState{BudgetUSD: 10, SpentUSD: 1, TurnCount: 1}
	if _, _, err := m.Route(context.Background(), msgs, first); err != nil {
		t.Fatalf("first Route: %v", err)
	}

	// Same bucket (different raw values, same buckets) → same cache key.
	sameBucket := RoutingState{BudgetUSD: 10, SpentUSD: 5, TurnCount: 5}
	m.cacheMu.Lock()
	_, cachedSame := m.cache[m.cacheKey(msgs, sameBucket)]
	m.cacheMu.Unlock()
	if !cachedSame {
		t.Errorf("expected cache hit within the same state bucket")
	}

	// Budget crossing the 20% boundary → cache MISS (different key).
	lowBudget := RoutingState{BudgetUSD: 10, SpentUSD: 8.5, TurnCount: 5}
	m.cacheMu.Lock()
	_, cachedLow := m.cache[m.cacheKey(msgs, lowBudget)]
	m.cacheMu.Unlock()
	if cachedLow {
		t.Errorf("expected cache miss when budget crosses the 20%% boundary")
	}

	// Timeout crossing the 70% boundary → cache MISS.
	urgent := RoutingState{BudgetUSD: 10, SpentUSD: 5, TimeoutTotalSec: 100, TimeoutElapsedSec: 80, TurnCount: 5}
	m.cacheMu.Lock()
	_, cachedUrgent := m.cache[m.cacheKey(msgs, urgent)]
	m.cacheMu.Unlock()
	if cachedUrgent {
		t.Errorf("expected cache miss when timeout goes urgent")
	}

	// Turn bucket change → cache MISS.
	deep := RoutingState{BudgetUSD: 10, SpentUSD: 5, TurnCount: 50}
	m.cacheMu.Lock()
	_, cachedDeep := m.cache[m.cacheKey(msgs, deep)]
	m.cacheMu.Unlock()
	if cachedDeep {
		t.Errorf("expected cache miss when the turn bucket changes")
	}
}

func TestRouteCacheHitReturnsCachedProvider(t *testing.T) {
	apiKeyFile := writeTestAPIKeyFile(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"content": testProviderMini}},
			},
		})
	}))
	defer srv.Close()
	cfg := fakeMetaProviderConfig(srv.URL, apiKeyFile, 0)
	m := NewMetaRouter(cfg, testMetaForward(cfg))
	msgs := []Message{{Role: "user", Content: "cache me"}}
	state := RoutingState{BudgetUSD: 5, SpentUSD: 1}

	if _, _, err := m.Route(context.Background(), msgs, state); err != nil {
		t.Fatalf("first Route: %v", err)
	}
	if _, _, err := m.Route(context.Background(), msgs, state); err != nil {
		t.Fatalf("second Route: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("expected 1 LLM call (cache hit on second), got %d", got)
	}
}

func TestRouteBlankResponseErrors(t *testing.T) {
	apiKeyFile := writeTestAPIKeyFile(t)
	srv := fakeMetaLLMServer(t, "   ")
	defer srv.Close()
	cfg := fakeMetaProviderConfig(srv.URL, apiKeyFile, 0)
	m := NewMetaRouter(cfg, testMetaForward(cfg))
	_, _, err := m.Route(context.Background(), []Message{{Role: "user", Content: "hi"}})
	if err == nil {
		t.Fatal("expected error for blank meta-router response")
	}
	if !strings.Contains(err.Error(), "unknown provider") {
		t.Errorf("expected unknown-provider error, got: %v", err)
	}
}

// Regression: the meta-router sent an OpenAI-shaped body with Bearer auth to
// Anthropic's Messages endpoint, so routing through an Anthropic provider
// always failed. It now speaks each provider's own wire format.
func TestMetaRouter_AnthropicProviderUsesMessagesAPI(t *testing.T) {
	captured := captureHTTP(t, `{"id":"msg_1","content":[{"type":"text","text":"gpt-4o"}],"stop_reason":"end_turn","usage":{"input_tokens":7,"output_tokens":1}}`)
	keyFile := filepath.Join(t.TempDir(), "api-key")
	if err := os.WriteFile(keyFile, []byte("sk-ant-test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		LLMRequestTimeout: time.Minute,
		Providers: []ProviderConfig{
			{Name: "claude", LiteLLMModel: "anthropic/claude-sonnet-5-5", APIKeyFile: keyFile},
			{Name: testProviderFull, LiteLLMModel: "openai/gpt-4o", APIKeyFile: keyFile},
		},
		MetaRouterProviderName: "claude",
	}
	m := NewMetaRouter(cfg, testMetaForward(cfg))

	name, usage, err := m.Route(context.Background(), []Message{{Role: "user", Content: "route me"}})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if name != testProviderFull {
		t.Errorf("provider = %q, want %q", name, testProviderFull)
	}
	if usage.PromptTokens != 7 {
		t.Errorf("usage = %+v, want the Anthropic usage mapped through", usage)
	}
	if captured.URL.String() != anthropicMessagesURL {
		t.Errorf("url = %s, want %s", captured.URL, anthropicMessagesURL)
	}
	if captured.Header.Get("x-api-key") != "sk-ant-test" || captured.Header.Get("Authorization") != "" {
		t.Errorf("auth headers = %v, want x-api-key and no Bearer", captured.Header)
	}
	var body map[string]any
	if err := json.Unmarshal(captured.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body["model"] != "claude-sonnet-5-5" || body["max_tokens"] != float64(50) || body["temperature"] != float64(0) {
		t.Errorf("body = %v, want bare model, max_tokens 50, temperature 0", body)
	}
}
