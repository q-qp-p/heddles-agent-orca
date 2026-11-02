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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// RoutingEvent is one LLM call as the operator stores it: which provider the
// selector chose and why, what the call cost, how long it took and how its tool
// calls went. It carries metadata only, never prompt or response content.
//
// The operator keeps a matching type (postgresql.RoutingEvent) because this
// image must not link the database package; a test in internal/apiserver pins
// the two to one wire format.
type RoutingEvent struct {
	DecisionID       string          `json:"decisionID"`
	ParentDecisionID string          `json:"parentDecisionID,omitempty"`
	Agent            string          `json:"agent,omitempty"`
	Selector         string          `json:"selector,omitempty"`
	Strategy         string          `json:"strategy,omitempty"`
	Algorithm        string          `json:"algorithm,omitempty"`
	Path             string          `json:"path,omitempty"`
	Stage            string          `json:"stage,omitempty"`
	StageConf        float64         `json:"stageConf,omitempty"`
	CategoryVersion  int             `json:"categoryVersion,omitempty"`
	SYLabel          string          `json:"syLabel,omitempty"`
	WorkflowStep     string          `json:"workflowStep,omitempty"`
	Provider         string          `json:"provider"`
	Model            string          `json:"model,omitempty"`
	Candidates       []string        `json:"candidates,omitempty"`
	Signals          json.RawMessage `json:"signals,omitempty"`
	Confidence       float64         `json:"confidence,omitempty"`
	IsShadow         bool            `json:"isShadow,omitempty"`
	PromptTokens     int             `json:"promptTokens,omitempty"`
	CompletionTokens int             `json:"completionTokens,omitempty"`
	CostUSD          float64         `json:"costUSD,omitempty"`
	AuxCostUSD       float64         `json:"auxCostUSD,omitempty"`
	LatencyMS        int             `json:"latencyMS,omitempty"`
	// TTFTMS is nil when no token arrived (a failed call).
	TTFTMS           *int      `json:"ttftMS,omitempty"`
	FinishReason     string    `json:"finishReason,omitempty"`
	FallbackFrom     string    `json:"fallbackFrom,omitempty"`
	ErrorClass       string    `json:"errorClass,omitempty"`
	ToolCalls        int       `json:"toolCalls,omitempty"`
	ToolErrors       int       `json:"toolErrors,omitempty"`
	ToolParseErrors  int       `json:"toolParseErrors,omitempty"`
	GuardrailBlocked bool      `json:"guardrailBlocked,omitempty"`
	SafeguardTrip    string    `json:"safeguardTrip,omitempty"`
	TurnIndex        int       `json:"turnIndex,omitempty"`
	CreatedAt        time.Time `json:"createdAt,omitzero"`
}

// Error classes recorded on a failed call.
const (
	errClassRateLimited    = "rate_limited"
	errClassProvider5xx    = "provider_5xx"
	errClassProvider4xx    = "provider_4xx"
	errClassTimeout        = "timeout"
	errClassProviderError  = "provider_error"
	errClassRequestTooBig  = "request_too_large"
	errClassCancelled      = "cancelled"
	errClassStreamBroken   = "stream_interrupted"
	toolErrorResultPrefix  = `{"error"`
	toolParseErrorFragment = "invalid arguments"
)

// classifyProviderError buckets a provider failure for telemetry. The forwarders
// wrap the status into the message ("provider X returned 503: ...").
func classifyProviderError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	switch {
	case errors.Is(err, context.DeadlineExceeded), strings.Contains(msg, "deadline exceeded"):
		return errClassTimeout
	case strings.Contains(msg, "returned 429"), strings.Contains(msg, "rate limit"):
		return errClassRateLimited
	case strings.Contains(msg, "returned 5"):
		return errClassProvider5xx
	case strings.Contains(msg, "returned 4"):
		return errClassProvider4xx
	}
	return errClassProviderError
}

// classifyToolResult says whether a tool dispatch result is an error and, if
// so, whether the model produced arguments the router could not parse.
func classifyToolResult(result string) (isError, isParseError bool) {
	trimmed := strings.TrimSpace(result)
	if !strings.HasPrefix(trimmed, toolErrorResultPrefix) {
		return false, false
	}
	return true, strings.Contains(trimmed, toolParseErrorFragment)
}

// Telemetry delivery tuning.
const (
	telemetryQueueMax      = 1000
	telemetryBatchSize     = 50
	telemetryFlushInterval = 2 * time.Second
	telemetryPostAttempts  = 3
	telemetryRetryBackoff  = 200 * time.Millisecond
	telemetryPostTimeout   = 10 * time.Second
)

// telemetrySink buffers routing events and posts them to the operator in
// batches. Recording never blocks or fails a request: when the operator is
// unreachable events queue up to telemetryQueueMax and the oldest are dropped
// (and counted) beyond that.
type telemetrySink struct {
	post    func(ctx context.Context, events []RoutingEvent) error
	metrics *Metrics

	mu      sync.Mutex
	queue   []RoutingEvent
	wake    chan struct{}
	stop    chan struct{}
	stopped chan struct{}
	// flushMu serializes sends so a manual Flush never races the loop's batch.
	flushMu sync.Mutex
}

// newTelemetrySink starts the sink's delivery loop. post sends one batch.
func newTelemetrySink(post func(context.Context, []RoutingEvent) error, metrics *Metrics) *telemetrySink {
	s := &telemetrySink{
		post:    post,
		metrics: metrics,
		wake:    make(chan struct{}, 1),
		stop:    make(chan struct{}),
		stopped: make(chan struct{}),
	}
	go s.loop()
	return s
}

// Record queues an event. Nil-safe.
func (s *telemetrySink) Record(ev RoutingEvent) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.queue = append(s.queue, ev)
	if over := len(s.queue) - telemetryQueueMax; over > 0 {
		s.queue = s.queue[over:]
		s.metrics.AddTelemetryDropped(over)
	}
	full := len(s.queue) >= telemetryBatchSize
	s.mu.Unlock()
	if full {
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}
}

func (s *telemetrySink) loop() {
	defer close(s.stopped)
	t := time.NewTicker(telemetryFlushInterval)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
		case <-s.wake:
		}
		ctx, cancel := context.WithTimeout(context.Background(), telemetryPostTimeout)
		s.Flush(ctx)
		cancel()
	}
}

// Flush sends everything queued, a batch at a time, until the queue is empty,
// a batch fails every attempt (it is requeued), or ctx ends. Nil-safe.
func (s *telemetrySink) Flush(ctx context.Context) {
	if s == nil {
		return
	}
	s.flushMu.Lock()
	defer s.flushMu.Unlock()
	for ctx.Err() == nil {
		s.mu.Lock()
		n := min(len(s.queue), telemetryBatchSize)
		if n == 0 {
			s.mu.Unlock()
			return
		}
		batch := append([]RoutingEvent(nil), s.queue[:n]...)
		s.queue = s.queue[n:]
		s.mu.Unlock()

		if err := s.send(ctx, batch); err != nil {
			slog.Warn("posting routing events failed; will retry", "events", len(batch), "err", err)
			s.requeue(batch)
			return
		}
	}
}

// requeue puts a failed batch back at the head of the queue, dropping the
// oldest events if that overflows it.
func (s *telemetrySink) requeue(batch []RoutingEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queue = append(batch, s.queue...)
	if over := len(s.queue) - telemetryQueueMax; over > 0 {
		s.queue = s.queue[over:]
		s.metrics.AddTelemetryDropped(over)
	}
}

func (s *telemetrySink) send(ctx context.Context, batch []RoutingEvent) error {
	var err error
	for attempt := range telemetryPostAttempts {
		if err = s.post(ctx, batch); err == nil {
			return nil
		}
		if attempt == telemetryPostAttempts-1 {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(telemetryRetryBackoff << attempt):
		}
	}
	return err
}

// Close stops the delivery loop and makes a last attempt to send what is
// queued. Nil-safe.
func (s *telemetrySink) Close(ctx context.Context) {
	if s == nil {
		return
	}
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
	<-s.stopped
	s.Flush(ctx)
}

// setMetrics binds the metrics set drops are counted in. Nil-safe.
func (s *telemetrySink) setMetrics(m *Metrics) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.metrics = m
	s.mu.Unlock()
}

// pending is the number of queued events (tests).
func (s *telemetrySink) pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.queue)
}

// postRoutingEvents is the sink's transport: one batch to the operator API.
func (r *Router) postRoutingEvents(ctx context.Context, events []RoutingEvent) error {
	tokenBytes, err := os.ReadFile(r.cfg.SATokenFile)
	if err != nil {
		return fmt.Errorf("reading service account token: %w", err)
	}
	body, err := json.Marshal(map[string]any{"events": events})
	if err != nil {
		return err
	}
	url := fmt.Sprintf("%s/agentrun/%s/%s/routing-events", r.cfg.OperatorAPIURL, r.cfg.RunNamespace, r.cfg.RunName)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tokenBytes)))
	resp, err := routingDecisionClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("operator returned %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

// callTelemetry assembles the event for one LLM call as the call unfolds. It
// is created when the call is routed, carried in the request context, and
// recorded exactly once by finish. All methods are nil-safe so paths that have
// no telemetry (Gemini, tests) need no guards.
type callTelemetry struct {
	r        *Router
	provider *ProviderConfig
	ev       RoutingEvent

	start      time.Time
	firstToken time.Time
	end        time.Time
	recorded   bool
}

type callTelemetryKey struct{}

func callTelemetryFrom(ctx context.Context) *callTelemetry {
	ct, _ := ctx.Value(callTelemetryKey{}).(*callTelemetry)
	return ct
}

// beginCall starts the event for a routed call. fallbackFrom names the
// provider that failed when this call is its fallback.
func (r *Router) beginCall(sel Selection, provider *ProviderConfig, fallbackFrom string) *callTelemetry {
	var aux float64
	for _, a := range sel.AuxUsage {
		for i := range r.cfg.Providers {
			if r.cfg.Providers[i].Name == a.Provider {
				aux += usageCost(a.Usage, &r.cfg.Providers[i])
			}
		}
	}
	now := time.Now()
	return &callTelemetry{
		r:        r,
		provider: provider,
		start:    now,
		ev: RoutingEvent{
			DecisionID:      sel.DecisionID,
			Agent:           r.cfg.AgentName,
			Selector:        r.cfg.SelectorName,
			Strategy:        r.cfg.Strategy,
			Algorithm:       sel.Algorithm,
			Path:            sel.Path,
			Stage:           sel.Category.Stage,
			StageConf:       sel.Category.StageConf,
			CategoryVersion: sel.Category.Version,
			SYLabel:         sel.Category.SYLabel,
			WorkflowStep:    sel.Category.WorkflowStep,
			Provider:        provider.Name,
			Model:           provider.LiteLLMModel,
			Candidates:      sel.Candidates,
			Signals:         sel.Signals,
			Confidence:      sel.Confidence,
			AuxCostUSD:      aux,
			FallbackFrom:    fallbackFrom,
			TurnIndex:       r.ruleRouter.TurnCount(),
			CreatedAt:       now.UTC(),
		},
	}
}

// usageCost prices a call's token usage on provider.
func usageCost(u TokenUsage, p *ProviderConfig) float64 {
	return float64(u.PromptTokens)*p.CostPerInputToken + float64(u.CompletionTokens)*p.CostPerOutputToken
}

// noteFirstToken marks when the first token (content, reasoning or tool call)
// arrived. Only the first call counts.
func (c *callTelemetry) noteFirstToken() {
	if c == nil || !c.firstToken.IsZero() {
		return
	}
	c.firstToken = time.Now()
}

// noteStreamEnd marks the end of the provider stream, so the latency excludes
// the tool dispatch and continuation that follow it.
func (c *callTelemetry) noteStreamEnd(usage TokenUsage, finishReason string) {
	if c == nil {
		return
	}
	c.end = time.Now()
	c.ev.PromptTokens, c.ev.CompletionTokens = usage.PromptTokens, usage.CompletionTokens
	c.ev.CostUSD = usageCost(usage, c.provider)
	c.ev.FinishReason = finishReason
}

// noteToolResult counts one dispatched tool call and whether it failed.
func (c *callTelemetry) noteToolResult(result string) {
	if c == nil {
		return
	}
	c.ev.ToolCalls++
	isErr, isParse := classifyToolResult(result)
	if isErr {
		c.ev.ToolErrors++
	}
	if isParse {
		c.ev.ToolParseErrors++
	}
}

// setError records how the call failed. The first error wins.
func (c *callTelemetry) setError(class string) {
	if c == nil || c.ev.ErrorClass != "" {
		return
	}
	c.ev.ErrorClass = class
}

// setSafeguardTrip records the safeguard that halted the run on this call.
func (c *callTelemetry) setSafeguardTrip(reason string) {
	if c == nil {
		return
	}
	c.ev.SafeguardTrip = reason
}

// finish records the event and its metrics. It is idempotent, so a deferred
// call covers every return path.
func (c *callTelemetry) finish() {
	if c == nil || c.recorded {
		return
	}
	c.recorded = true
	end := c.end
	if end.IsZero() {
		end = time.Now()
	}
	c.ev.LatencyMS = int(end.Sub(c.start).Milliseconds())
	if !c.firstToken.IsZero() {
		ttft := int(c.firstToken.Sub(c.start).Milliseconds())
		c.ev.TTFTMS = &ttft
	}
	m := c.r.metrics
	m.RecordLLMCall(c.provider.Name, c.ev.PromptTokens, c.ev.CompletionTokens, end.Sub(c.start), c.ev.TTFTMS, c.ev.ErrorClass)
	m.ObserveStreamDuration(end.Sub(c.start).Seconds())
	m.IncRoutingDecision(c.ev.Strategy, c.ev.Path, c.ev.Stage)
	if c.ev.FallbackFrom != "" {
		m.IncRoutingFallback(c.ev.FallbackFrom, c.provider.Name)
	}
	c.r.telemetry.Record(c.ev)
}

// flushTelemetry sends whatever routing telemetry is queued. Nil-safe.
func (r *Router) flushTelemetry() {
	if r.telemetry == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), telemetryPostTimeout)
	defer cancel()
	r.telemetry.Flush(ctx)
}
