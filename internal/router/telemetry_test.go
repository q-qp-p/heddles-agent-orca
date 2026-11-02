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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// --- sink -------------------------------------------------------------------

type batchRecorder struct {
	mu      sync.Mutex
	batches [][]RoutingEvent
	fail    atomic.Int32 // number of upcoming posts to fail
	calls   atomic.Int32
}

func (b *batchRecorder) post(_ context.Context, ev []RoutingEvent) error {
	b.calls.Add(1)
	if b.fail.Add(-1) >= 0 {
		return errors.New("operator unavailable")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.batches = append(b.batches, append([]RoutingEvent(nil), ev...))
	return nil
}

func (b *batchRecorder) ids() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, 0, len(b.batches)*telemetryBatchSize)
	for _, batch := range b.batches {
		for _, e := range batch {
			out = append(out, e.DecisionID)
		}
	}
	return out
}

func newTestSink(t *testing.T, rec *batchRecorder) *telemetrySink {
	t.Helper()
	s := newTelemetrySink(rec.post, NewMetrics(prometheus.NewRegistry()))
	t.Cleanup(func() { s.Close(context.Background()) })
	return s
}

func ev(id string) RoutingEvent { return RoutingEvent{DecisionID: id, Provider: "p"} }

func TestTelemetrySink_FlushSendsInBatches(t *testing.T) {
	rec := &batchRecorder{}
	s := newTestSink(t, rec)
	for i := range telemetryBatchSize*2 + 5 {
		s.Record(ev(fmt.Sprintf("e%03d", i)))
	}
	s.Flush(context.Background())

	if got := len(rec.ids()); got != telemetryBatchSize*2+5 {
		t.Fatalf("delivered %d events, want %d", got, telemetryBatchSize*2+5)
	}
	for _, b := range rec.batches {
		if len(b) > telemetryBatchSize {
			t.Errorf("batch of %d exceeds the batch size %d", len(b), telemetryBatchSize)
		}
	}
	if s.pending() != 0 {
		t.Errorf("%d events still queued after Flush", s.pending())
	}
}

func TestTelemetrySink_RetriesATransientFailure(t *testing.T) {
	rec := &batchRecorder{}
	rec.fail.Store(1) // first attempt fails, the retry succeeds
	s := newTestSink(t, rec)
	s.Record(ev("a"))
	s.Flush(context.Background())

	if ids := rec.ids(); len(ids) != 1 || ids[0] != "a" {
		t.Errorf("delivered %v, want [a] after one retry", ids)
	}
	if got := rec.calls.Load(); got != 2 {
		t.Errorf("post attempts = %d, want 2", got)
	}
}

func TestTelemetrySink_KeepsEventsWhileOperatorIsDown(t *testing.T) {
	rec := &batchRecorder{}
	rec.fail.Store(1000)
	s := newTestSink(t, rec)
	for _, id := range []string{"a", "b", "c"} {
		s.Record(ev(id))
	}
	s.Flush(context.Background())
	if s.pending() != 3 || len(rec.ids()) != 0 {
		t.Fatalf("pending %d delivered %v, want 3 queued and none delivered", s.pending(), rec.ids())
	}

	rec.fail.Store(0) // operator is back
	s.Flush(context.Background())
	if ids := rec.ids(); strings.Join(ids, "") != "abc" {
		t.Errorf("delivered %v, want [a b c] in order after recovery", ids)
	}
}

func TestTelemetrySink_DropsOldestWhenFullAndCountsThem(t *testing.T) {
	rec := &batchRecorder{}
	rec.fail.Store(1 << 20)
	m := NewMetrics(prometheus.NewRegistry())
	// No delivery loop: it would legitimately take batches while this test counts.
	s := &telemetrySink{post: rec.post, metrics: m, wake: make(chan struct{}, 1)}

	for i := range telemetryQueueMax + 30 {
		s.Record(ev(fmt.Sprintf("e%04d", i)))
	}
	if s.pending() != telemetryQueueMax {
		t.Fatalf("pending = %d, want the cap %d", s.pending(), telemetryQueueMax)
	}
	if got := testutil.ToFloat64(m.telemetryDropped); got != 30 {
		t.Errorf("dropped counter = %v, want 30", got)
	}
	s.mu.Lock()
	oldest := s.queue[0].DecisionID
	s.mu.Unlock()
	if oldest != "e0030" {
		t.Errorf("oldest kept = %q, want e0030 (the 30 oldest dropped)", oldest)
	}
}

func TestTelemetrySink_NilIsSafe(t *testing.T) {
	var s *telemetrySink
	s.Record(ev("a"))
	s.Flush(context.Background())
	s.Close(context.Background())
	s.setMetrics(nil)
}

func TestTelemetrySink_LoopFlushesOnItsOwn(t *testing.T) {
	rec := &batchRecorder{}
	s := newTestSink(t, rec)
	for i := range telemetryBatchSize { // a full batch wakes the loop without waiting for the tick
		s.Record(ev(fmt.Sprintf("e%d", i)))
	}
	deadline := time.Now().Add(3 * time.Second)
	for len(rec.ids()) < telemetryBatchSize && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := len(rec.ids()); got != telemetryBatchSize {
		t.Errorf("loop delivered %d events, want %d", got, telemetryBatchSize)
	}
}

// --- classifiers -----------------------------------------------------------

func TestClassifyProviderError(t *testing.T) {
	tests := []struct {
		err  error
		want string
	}{
		{nil, ""},
		{errors.New("provider x returned 429: slow down"), errClassRateLimited},
		{errors.New("provider x returned 503: overloaded"), errClassProvider5xx},
		{errors.New("provider x returned 400: bad request"), errClassProvider4xx},
		{fmt.Errorf("provider x: %w", context.DeadlineExceeded), errClassTimeout},
		{errors.New("dial tcp: connection refused"), errClassProviderError},
	}
	for _, tt := range tests {
		if got := classifyProviderError(tt.err); got != tt.want {
			t.Errorf("classifyProviderError(%v) = %q, want %q", tt.err, got, tt.want)
		}
	}
}

func TestClassifyToolResult(t *testing.T) {
	tests := []struct {
		result         string
		wantErr, parse bool
	}{
		{`{"status":"ok"}`, false, false},
		{`plain text result`, false, false},
		{`{"error": "unknown tool \"x\""}`, true, false},
		{`  {"error":"invalid arguments: unexpected end of JSON input"}`, true, true},
		{`{"result":"mentions invalid arguments but is not an error"}`, false, false},
	}
	for _, tt := range tests {
		isErr, isParse := classifyToolResult(tt.result)
		if isErr != tt.wantErr || isParse != tt.parse {
			t.Errorf("classifyToolResult(%q) = %v,%v want %v,%v", tt.result, isErr, isParse, tt.wantErr, tt.parse)
		}
	}
}

// --- per-call events through the real handler -------------------------------

// eventRecorder stands in for the operator API: it stores posted routing events.
type eventRecorder struct {
	mu     sync.Mutex
	events []RoutingEvent
	srv    *httptest.Server
}

func newEventRecorder(t *testing.T) *eventRecorder {
	t.Helper()
	rec := &eventRecorder{}
	rec.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
		if !strings.HasSuffix(r.URL.Path, "/routing-events") {
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Events []RoutingEvent `json:"events"`
		}
		_ = json.Unmarshal(body, &req)
		rec.mu.Lock()
		rec.events = append(rec.events, req.Events...)
		rec.mu.Unlock()
	}))
	t.Cleanup(rec.srv.Close)
	return rec
}

func (rec *eventRecorder) all() []RoutingEvent {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return append([]RoutingEvent(nil), rec.events...)
}

// usageTurnSSE is a final text turn that reports token usage.
func usageTurnSSE(prompt, completion int) []byte {
	stop := "stop"
	return []byte(strings.Join([]string{
		sseFrame(streamingChatCompletionChunk{ID: "u", Object: "chat.completion.chunk",
			Choices: []streamChoice{{Delta: streamDelta{Role: "assistant"}}}}),
		sseFrame(streamingChatCompletionChunk{ID: "u", Object: "chat.completion.chunk",
			Choices: []streamChoice{{Delta: streamDelta{Content: "Done."}}}}),
		sseFrame(streamingChatCompletionChunk{ID: "u", Object: "chat.completion.chunk",
			Choices: []streamChoice{{Delta: streamDelta{}, FinishReason: &stop}}}),
		sseFrame(streamingChatCompletionChunk{ID: "u", Object: "chat.completion.chunk",
			Usage: &TokenUsage{PromptTokens: prompt, CompletionTokens: completion}}),
		sseDone(),
	}, ""))
}

// badToolTurnSSE asks for a tool the router does not have, which dispatches to
// an error result.
func badToolTurnSSE() []byte {
	return []byte(strings.Join([]string{
		sseFrame(streamingChatCompletionChunk{ID: "t", Object: "chat.completion.chunk",
			Choices: []streamChoice{{Delta: streamDelta{ToolCalls: []ToolCall{{
				Index: 0, ID: "call_1", Type: "function",
				Function: FunctionCall{Name: "no_such_tool", Arguments: `{}`},
			}}}}}}),
		sseDone(),
	}, ""))
}

func telemetryTestRouter(t *testing.T, rec *eventRecorder, provider *httptest.Server) *Router {
	t.Helper()
	r := newSelectionTestRouter(t, func(c *Config) {
		c.OperatorAPIURL = rec.srv.URL
		c.Strategy = strategyRuleBased
		c.SelectorName = "balanced"
		c.AgentName = "coder"
		c.Providers = c.Providers[:1]
		c.Providers[0].BaseURL = provider.URL
		c.Providers[0].CostPerInputToken = 0.000002
		c.Providers[0].CostPerOutputToken = 0.00001
	})
	return r
}

func TestRoutingTelemetry_TextTurnEvent(t *testing.T) {
	srv, _ := fakeProvider(t, usageTurnSSE(1000, 200))
	rec := newEventRecorder(t)
	r := telemetryTestRouter(t, rec, srv)
	r.SetMetrics(NewMetrics(prometheus.NewRegistry()))

	if code := serve(r, conversation(t, 2)).Code; code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	r.Finalize()

	events := rec.all()
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1: %+v", len(events), events)
	}
	e := events[0]
	if e.DecisionID == "" || e.Provider != "a" || e.Model != "openai/a" || e.Selector != "balanced" || e.Agent != "coder" {
		t.Errorf("identity = %+v", e)
	}
	if e.Strategy != strategyRuleBased || e.Path != pathWeighted || e.Stage == "" || e.CategoryVersion != categoryVersion {
		t.Errorf("decision = strategy %q path %q stage %q cv %d", e.Strategy, e.Path, e.Stage, e.CategoryVersion)
	}
	if e.PromptTokens != 1000 || e.CompletionTokens != 200 || e.FinishReason != "stop" {
		t.Errorf("usage = %d/%d finish %q", e.PromptTokens, e.CompletionTokens, e.FinishReason)
	}
	if want := 1000*0.000002 + 200*0.00001; e.CostUSD < want-1e-9 || e.CostUSD > want+1e-9 {
		t.Errorf("CostUSD = %v, want %v", e.CostUSD, want)
	}
	if e.TTFTMS == nil || *e.TTFTMS < 0 || e.LatencyMS < *e.TTFTMS {
		t.Errorf("latency %d ttft %v, want a first token no later than the end", e.LatencyMS, e.TTFTMS)
	}
	if e.ErrorClass != "" || e.ToolCalls != 0 || e.CreatedAt.IsZero() || len(e.Candidates) == 0 {
		t.Errorf("event = %+v", e)
	}
}

func TestRoutingTelemetry_ToolLoopCountsToolErrors(t *testing.T) {
	srv, calls := fakeProvider(t, badToolTurnSSE(), usageTurnSSE(10, 5))
	rec := newEventRecorder(t)
	r := telemetryTestRouter(t, rec, srv)

	if code := serve(r, conversation(t, 2)).Code; code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	r.Finalize()

	events := rec.all()
	if calls.Load() != 2 || len(events) != 2 {
		t.Fatalf("provider calls %d, events %d; want 2 and 2", calls.Load(), len(events))
	}
	byTurn := map[int]RoutingEvent{}
	for _, e := range events {
		byTurn[e.TurnIndex] = e
	}
	first, second := byTurn[0], byTurn[1]
	if first.ToolCalls != 1 || first.ToolErrors != 1 || first.ToolParseErrors != 0 {
		t.Errorf("tool turn = calls %d errors %d parse %d, want 1/1/0", first.ToolCalls, first.ToolErrors, first.ToolParseErrors)
	}
	if second.ToolCalls != 0 || second.PromptTokens != 10 {
		t.Errorf("final turn = %+v", second)
	}
	if first.DecisionID == second.DecisionID {
		t.Errorf("the two calls share a decision ID")
	}
}

func TestRoutingTelemetry_FallbackRecordsBothAttempts(t *testing.T) {
	failing := newCapturingProvider(t, true)
	working := newCapturingProvider(t, false)
	rec := newEventRecorder(t)
	r := newSelectionTestRouter(t, func(c *Config) {
		c.OperatorAPIURL = rec.srv.URL
		c.Strategy = strategyRuleBased
		c.FallbackChain = []string{providerPrimary, "fallback"}
		c.Providers = []ProviderConfig{
			{Name: providerPrimary, LiteLLMModel: "openai/p", BaseURL: failing.srv.URL, APIKeyFile: c.SATokenFile, Weight: 1},
			{Name: "fallback", LiteLLMModel: "openai/f", BaseURL: working.srv.URL, APIKeyFile: c.SATokenFile, Weight: 0},
		}
	})

	if code := serve(r, conversation(t, 2)).Code; code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	r.Finalize()

	events := rec.all()
	if len(events) != 2 {
		t.Fatalf("got %d events, want the failed primary and the fallback: %+v", len(events), events)
	}
	var primary, fallback RoutingEvent
	for _, e := range events {
		if e.Provider == providerPrimary {
			primary = e
		} else {
			fallback = e
		}
	}
	if primary.ErrorClass != errClassProvider5xx || primary.TTFTMS != nil {
		t.Errorf("primary = error %q ttft %v, want provider_5xx and no first token", primary.ErrorClass, primary.TTFTMS)
	}
	if fallback.Path != pathFallback || fallback.FallbackFrom != providerPrimary || fallback.ParentDecisionID != primary.DecisionID || fallback.ErrorClass != "" {
		t.Errorf("fallback = path %q from %q parent %q (primary %q) error %q", fallback.Path, fallback.FallbackFrom, fallback.ParentDecisionID, primary.DecisionID, fallback.ErrorClass)
	}
	if fallback.AuxCostUSD != 0 {
		t.Errorf("fallback charged the selector's cost again: %v", fallback.AuxCostUSD)
	}
}

func TestRoutingTelemetry_NoOperatorMeansNoSink(t *testing.T) {
	srv, _ := fakeProvider(t, usageTurnSSE(1, 1))
	r := newSelectionTestRouter(t, func(c *Config) {
		c.Providers = c.Providers[:1]
		c.Providers[0].BaseURL = srv.URL
	})
	if r.telemetry != nil {
		t.Fatalf("a router with no operator API built a telemetry sink")
	}
	if code := serve(r, conversation(t, 2)).Code; code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	r.Finalize() // must not panic without a sink
}

func TestRoutingTelemetry_MetricsSeries(t *testing.T) {
	srv, _ := fakeProvider(t, badToolTurnSSE(), usageTurnSSE(300, 40))
	rec := newEventRecorder(t)
	r := telemetryTestRouter(t, rec, srv)
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	r.SetMetrics(m)

	if code := serve(r, conversation(t, 2)).Code; code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	r.Finalize()

	// The tool turn's stream carries no usage chunk, so only the final turn reports.
	if got := testutil.ToFloat64(m.llmTokens.WithLabelValues("prompt", "a")); got != 300 {
		t.Errorf("prompt tokens = %v, want 300", got)
	}
	if got := testutil.ToFloat64(m.llmTokens.WithLabelValues("completion", "a")); got != 40 {
		t.Errorf("completion tokens = %v, want 40", got)
	}
	if got := testutil.ToFloat64(m.toolCalls); got != 1 {
		t.Errorf("tool_calls_total = %v, want 1 (it was never incremented before)", got)
	}
	if got := testutil.ToFloat64(m.routingDecisions.WithLabelValues(strategyRuleBased, pathWeighted, stageChat)); got < 1 {
		t.Errorf("routing_decisions_total = %v, want at least 1", got)
	}
	if n := testutil.CollectAndCount(m.llmLatency); n != 1 {
		t.Errorf("latency series = %d, want one per provider", n)
	}
	if n := testutil.CollectAndCount(m.streamDuration); n != 1 {
		t.Errorf("stream_duration histogram not observed")
	}
}

func TestMetrics_CapabilityPathDoesNotMintSeries(t *testing.T) {
	m := NewMetrics(prometheus.NewRegistry())
	for _, k := range []string{"code", "reasoning", "my-custom-key"} {
		m.IncRoutingDecision("hybrid", pathCapability+k, "chat")
	}
	if n := testutil.CollectAndCount(m.routingDecisions); n != 1 {
		t.Errorf("series = %d, want 1: operator-chosen capability keys must not become labels", n)
	}
}
