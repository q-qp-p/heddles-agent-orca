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
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
)

// ShadowConfig tells the router to replay a sample of its calls on another
// model and have a judge compare the answers (a RoutingBenchmark). The replay
// never runs a tool and is never charged to the run.
type ShadowConfig struct {
	// Benchmark is the RoutingBenchmark the results are filed under.
	Benchmark string `json:"benchmark"`
	// SampleRate is the fraction of calls replayed, 0 to 1.
	SampleRate float64 `json:"sampleRate"`
	// Arms is "counterfactual" or "providers".
	Arms string `json:"arms"`
	// Providers are the replay targets when Arms is "providers".
	Providers []string `json:"providers,omitempty"`
	// MaxPerRun is the most calls replayed from this run; 0 means no local cap,
	// as for a deployment, where the operator limits samples per hour instead.
	MaxPerRun int `json:"maxPerRun"`
	// IncludeToolTurns also replays calls that answer with tool calls.
	IncludeToolTurns bool `json:"includeToolTurns"`
	// Judge is the provider that compares the answers; it is never routed to.
	Judge string `json:"judge"`
	// PositionSwap judges twice with the answers in opposite order.
	PositionSwap bool `json:"positionSwap"`
	// Rubric replaces the default grading instructions when set.
	Rubric string `json:"rubric,omitempty"`
	// JudgeMaxTokens bounds one judge reply.
	JudgeMaxTokens int `json:"judgeMaxTokens,omitempty"`
	// StoreTranscripts keeps the prompt and both answers with the result.
	StoreTranscripts bool `json:"storeTranscripts,omitempty"`
}

// Shadow call tuning.
const (
	shadowCallTimeout   = 2 * time.Minute
	shadowPostTimeout   = 10 * time.Second
	defaultJudgeTokens  = 512
	shadowContextRunes  = 6000
	shadowAnswerRunes   = 4000
	shadowLeaseHeadroom = 1.5
	// shadowFinalizeWait is how long Finalize waits for a running replay.
	shadowFinalizeWait = 20 * time.Second
)

// Judge verdicts for one ordering of the two answers.
const (
	verdictA   = "A"
	verdictB   = "B"
	verdictTie = "tie"
)

// Winners recorded for a shadow result.
const (
	winnerServed  = "served"
	winnerShadow  = "shadow"
	winnerTie     = "tie"
	winnerInvalid = "invalid"
)

// ShadowResult is the wire form of postgresql.ShadowResult; the operator keeps
// the matching type because this image must not link the database package, and
// a test in internal/apiserver pins the two to one format.
type ShadowResult struct {
	DecisionID     string `json:"decisionId"`
	Benchmark      string `json:"benchmark"`
	Stage          string `json:"stage,omitempty"`
	SYLabel        string `json:"syLabel,omitempty"`
	Agent          string `json:"agent,omitempty"`
	WorkflowStep   string `json:"workflowStep,omitempty"`
	ServedProvider string `json:"servedProvider"`
	ShadowProvider string `json:"shadowProvider"`

	Winner          string          `json:"winner"`
	Inconsistent    bool            `json:"inconsistent,omitempty"`
	JudgeConfidence float64         `json:"judgeConfidence,omitempty"`
	JudgeProvider   string          `json:"judgeProvider,omitempty"`
	Dimensions      json.RawMessage `json:"dimensions,omitempty"`
	JudgeReason     string          `json:"judgeReason,omitempty"`

	ShadowPromptTokens     int     `json:"shadowPromptTokens,omitempty"`
	ShadowCompletionTokens int     `json:"shadowCompletionTokens,omitempty"`
	ShadowCostUSD          float64 `json:"shadowCostUsd,omitempty"`
	JudgeCostUSD           float64 `json:"judgeCostUsd,omitempty"`
	ShadowLatencyMS        int     `json:"shadowLatencyMs,omitempty"`
	ShadowToolCalls        int     `json:"shadowToolCalls,omitempty"`
	ShadowError            string  `json:"shadowError,omitempty"`

	Transcript json.RawMessage `json:"transcript,omitempty"`
	CreatedAt  time.Time       `json:"createdAt"`
}

// ShadowResultEnvelope is the body of POST .../shadow-results.
type ShadowResultEnvelope struct {
	LeaseID int64        `json:"leaseId,omitempty"`
	Result  ShadowResult `json:"result"`
}

// shadowRunner samples served calls and replays them in the background. One
// replay runs at a time; a call that arrives meanwhile is not sampled.
type shadowRunner struct {
	r   *Router
	cfg ShadowConfig
	sem chan struct{}
	wg  sync.WaitGroup

	mu      sync.Mutex
	sampled int
}

func newShadowRunner(r *Router, cfg *ShadowConfig) *shadowRunner {
	if cfg == nil || cfg.Benchmark == "" || cfg.Judge == "" || cfg.SampleRate <= 0 {
		return nil
	}
	c := *cfg
	if c.JudgeMaxTokens <= 0 {
		c.JudgeMaxTokens = defaultJudgeTokens
	}
	return &shadowRunner{r: r, cfg: c, sem: make(chan struct{}, 1)}
}

// shadowSampled decides from the decision ID alone whether a call is in the
// sample, so the same call is always in or out and tests need no random seed.
func shadowSampled(decisionID string, rate float64) bool {
	if rate >= 1 {
		return true
	}
	if rate <= 0 || decisionID == "" {
		return false
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(decisionID))
	return float64(h.Sum32())/float64(math.MaxUint32) < rate
}

// servedCall is everything a replay needs from a call that has just completed.
type servedCall struct {
	ev     RoutingEvent
	served *ProviderConfig
	req    ChatCompletionRequest
	answer Message
	usage  TokenUsage
}

// consider samples one completed call and, if chosen, replays it in the
// background. It never blocks the caller and never fails the run.
func (s *shadowRunner) consider(c servedCall) {
	if s == nil || c.served == nil || c.ev.DecisionID == "" {
		return
	}
	if c.ev.ErrorClass != "" || c.ev.FallbackFrom != "" || c.ev.IsShadow {
		return
	}
	if len(c.answer.ToolCalls) > 0 && !s.cfg.IncludeToolTurns {
		return
	}
	if !shadowSampled(c.ev.DecisionID, s.cfg.SampleRate) {
		return
	}
	arms := s.arms(c.served)
	if len(arms) == 0 {
		return
	}
	select {
	case s.sem <- struct{}{}:
	default:
		return // a replay is already running
	}
	s.mu.Lock()
	if s.cfg.MaxPerRun > 0 && s.sampled >= s.cfg.MaxPerRun {
		s.mu.Unlock()
		<-s.sem
		return
	}
	s.sampled++
	s.mu.Unlock()

	s.wg.Go(func() {
		defer func() { <-s.sem }()
		for _, arm := range arms {
			s.replay(c, arm)
		}
	})
}

// wait blocks until the running replay ends; Finalize calls it so a result is
// not lost when the pod stops.
func (s *shadowRunner) wait(timeout time.Duration) {
	if s == nil {
		return
	}
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

// arms returns the providers to replay a call that served answered.
func (s *shadowRunner) arms(served *ProviderConfig) []*ProviderConfig {
	if s.cfg.Arms == "providers" {
		var out []*ProviderConfig
		for _, name := range s.cfg.Providers {
			if name == served.Name {
				continue
			}
			if p := s.r.findProvider(name); p != nil {
				out = append(out, p)
			}
		}
		return out
	}
	if p := s.counterfactual(served); p != nil {
		return []*ProviderConfig{p}
	}
	return nil
}

// counterfactual picks the other tier: the capable provider when the efficient
// one served, and the reverse. Without capable and efficient roles it is the
// cheapest other provider when the served one is not the cheapest, and the
// dearest otherwise.
func (s *shadowRunner) counterfactual(served *ProviderConfig) *ProviderConfig {
	roles := s.r.cfg.CapabilityRouting
	capable, efficient := roles["capable"], roles["efficient"]
	if capable != "" && efficient != "" && capable != efficient {
		switch served.Name {
		case capable:
			return s.r.findProvider(efficient)
		case efficient:
			return s.r.findProvider(capable)
		}
	}
	var others []*ProviderConfig
	for i := range s.r.cfg.Providers {
		if p := &s.r.cfg.Providers[i]; p.Name != served.Name {
			others = append(others, p)
		}
	}
	if len(others) == 0 {
		return nil
	}
	cost := func(p *ProviderConfig) float64 { return p.CostPerInputToken + p.CostPerOutputToken }
	slices.SortStableFunc(others, func(a, b *ProviderConfig) int {
		switch {
		case cost(a) < cost(b):
			return -1
		case cost(a) > cost(b):
			return 1
		}
		return strings.Compare(a.Name, b.Name)
	})
	if cost(served) <= cost(others[0]) {
		return others[len(others)-1]
	}
	return others[0]
}

// findProvider resolves a provider by name among the routable providers and
// the auxiliary ones (the judge), which are never routed to.
func (r *Router) findProvider(name string) *ProviderConfig {
	for i := range r.cfg.Providers {
		if r.cfg.Providers[i].Name == name {
			return &r.cfg.Providers[i]
		}
	}
	for i := range r.cfg.AuxProviders {
		if r.cfg.AuxProviders[i].Name == name {
			return &r.cfg.AuxProviders[i]
		}
	}
	return nil
}

// replay runs one shadow call and its judging, then files the result. Every
// failure is logged and dropped: benchmarking must never disturb a run.
func (s *shadowRunner) replay(c servedCall, arm *ProviderConfig) {
	judge := s.r.findProvider(s.cfg.Judge)
	if judge == nil {
		slog.Warn("shadow judge provider is not configured", "judge", s.cfg.Judge)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), shadowCallTimeout)
	defer cancel()

	lease, ok := s.takeLease(ctx, s.estimateUSD(c, arm, judge))
	if !ok {
		return
	}
	res := ShadowResult{
		DecisionID: c.ev.DecisionID, Benchmark: s.cfg.Benchmark,
		Stage: c.ev.Stage, SYLabel: c.ev.SYLabel, Agent: c.ev.Agent, WorkflowStep: c.ev.WorkflowStep,
		ServedProvider: c.served.Name, ShadowProvider: arm.Name, JudgeProvider: judge.Name,
		CreatedAt: time.Now().UTC(),
	}

	shadowMsg, usage, latency, err := s.callShadow(ctx, c, arm)
	res.ShadowLatencyMS = int(latency.Milliseconds())
	res.ShadowPromptTokens, res.ShadowCompletionTokens = usage.PromptTokens, usage.CompletionTokens
	res.ShadowCostUSD = usageCost(usage, arm)
	if err != nil {
		// A failed replay says nothing about quality: record it unjudged so the
		// cost and the failure are visible, and the lease is released.
		res.Winner, res.ShadowError = winnerInvalid, classifyProviderError(err)
		s.post(ctx, lease, res)
		return
	}
	res.ShadowToolCalls = len(shadowMsg.ToolCalls)

	task := taskExcerpt(c.req.Messages)
	servedText, shadowText := answerText(c.answer), answerText(shadowMsg)
	verdict := s.judge(ctx, judge, task, servedText, shadowText)
	res.Winner, res.Inconsistent = verdict.winner, verdict.inconsistent
	res.JudgeConfidence, res.Dimensions, res.JudgeReason = verdict.confidence, verdict.dimensions, verdict.reason
	res.JudgeCostUSD = verdict.costUSD
	if s.cfg.StoreTranscripts {
		res.Transcript, _ = json.Marshal(map[string]string{"task": task, "served": servedText, "shadow": shadowText})
	}
	s.post(ctx, lease, res)
}

// estimateUSD is the most a replay and its judging may cost, for the lease: the
// served call's token counts priced on the arm, and two judge calls over both
// answers, with headroom.
func (s *shadowRunner) estimateUSD(c servedCall, arm, judge *ProviderConfig) float64 {
	armCost := usageCost(c.usage, arm)
	judgeIn := TokenUsage{PromptTokens: 2*(c.usage.PromptTokens/4) + 2*c.usage.CompletionTokens + 500, CompletionTokens: s.cfg.JudgeMaxTokens}
	calls := 1.0
	if s.cfg.PositionSwap {
		calls = 2
	}
	return (armCost + calls*usageCost(judgeIn, judge)) * shadowLeaseHeadroom
}

// callShadow replays the call on arm, without streaming and without running any
// tool the model asks for: the tool calls it returns are only counted and shown
// to the judge.
func (s *shadowRunner) callShadow(ctx context.Context, c servedCall, arm *ProviderConfig) (Message, TokenUsage, time.Duration, error) {
	req := c.req
	req.Stream, req.StreamOptions = false, nil
	fitted, err := s.r.fitRequestToProvider(arm, req)
	if err != nil {
		return Message{}, TokenUsage{}, 0, err
	}
	start := time.Now()
	body, err := s.r.forwardToProviderWithin(ctx, arm, fitted.req, shadowCallTimeout)
	latency := time.Since(start)
	if err != nil {
		return Message{}, TokenUsage{}, latency, err
	}
	var resp ChatCompletionResponse
	if err := json.Unmarshal(body, &resp); err != nil || len(resp.Choices) == 0 {
		return Message{}, resp.Usage, latency, fmt.Errorf("shadow provider %s returned no usable answer", arm.Name)
	}
	return resp.Choices[0].Message, resp.Usage, latency, nil
}

// judgement is the combined verdict of one or two judge calls.
type judgement struct {
	winner       string
	inconsistent bool
	confidence   float64
	dimensions   json.RawMessage
	reason       string
	costUSD      float64
}

// judge compares the served and shadow answers. With position swap it asks
// twice with the answers in opposite order; if the two verdicts disagree about
// which answer won, the result is a tie marked inconsistent.
func (s *shadowRunner) judge(ctx context.Context, judge *ProviderConfig, task, served, shadow string) judgement {
	first, usage1, err1 := s.askJudge(ctx, judge, task, served, shadow)
	out := judgement{costUSD: usageCost(usage1, judge)}
	if err1 != nil {
		out.winner = winnerInvalid
		return out
	}
	if !s.cfg.PositionSwap {
		out.winner = servedOrShadow(first.Winner, false)
		out.confidence, out.dimensions, out.reason = first.Confidence, first.Dimensions, first.Reason
		return out
	}
	second, usage2, err2 := s.askJudge(ctx, judge, task, shadow, served)
	out.costUSD += usageCost(usage2, judge)
	if err2 != nil {
		out.winner = winnerInvalid
		return out
	}
	w1, w2 := servedOrShadow(first.Winner, false), servedOrShadow(second.Winner, true)
	out.confidence = (first.Confidence + second.Confidence) / 2
	out.dimensions, out.reason = first.Dimensions, first.Reason
	if w1 == w2 {
		out.winner = w1
		return out
	}
	out.winner, out.inconsistent = winnerTie, true
	return out
}

// servedOrShadow maps a verdict about answers A and B to who won. In the
// swapped order the served answer was B.
func servedOrShadow(v string, swapped bool) string {
	switch v {
	case verdictA:
		if swapped {
			return winnerShadow
		}
		return winnerServed
	case verdictB:
		if swapped {
			return winnerServed
		}
		return winnerShadow
	}
	return winnerTie
}

// judgeVerdict is the judge's reply for one ordering.
type judgeVerdict struct {
	Winner     string          `json:"winner"`
	Confidence float64         `json:"confidence"`
	Dimensions json.RawMessage `json:"dimensions"`
	Reason     string          `json:"reason"`
}

// parseVerdict reads the judge's strict-JSON reply. A reply wrapped in prose or
// a code fence is accepted when it holds one JSON object; anything else, or a
// winner other than A, B or tie, is an error.
func parseVerdict(text string) (judgeVerdict, error) {
	start, end := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if start < 0 || end < start {
		return judgeVerdict{}, fmt.Errorf("judge reply has no JSON object")
	}
	var v judgeVerdict
	if err := json.Unmarshal([]byte(text[start:end+1]), &v); err != nil {
		return judgeVerdict{}, fmt.Errorf("judge reply is not valid JSON: %w", err)
	}
	switch w := strings.TrimSpace(v.Winner); {
	case strings.EqualFold(w, verdictA):
		v.Winner = verdictA
	case strings.EqualFold(w, verdictB):
		v.Winner = verdictB
	case strings.EqualFold(w, verdictTie):
		v.Winner = verdictTie
	default:
		return judgeVerdict{}, fmt.Errorf("judge winner %q is not A, B or tie", v.Winner)
	}
	if v.Confidence < 0 || v.Confidence > 1 {
		v.Confidence = math.Min(1, math.Max(0, v.Confidence))
	}
	return v, nil
}

const defaultRubric = `Judge which answer better serves the user's latest request in the conversation.
Weigh correctness first, then whether the right tools were chosen with sensible arguments, then completeness.
Do not prefer an answer for being longer, and ignore the order the answers are shown in.`

// askJudge sends one ordering of the two answers to the judge.
func (s *shadowRunner) askJudge(ctx context.Context, judge *ProviderConfig, task, a, b string) (judgeVerdict, TokenUsage, error) {
	rubric := s.cfg.Rubric
	if rubric == "" {
		rubric = defaultRubric
	}
	prompt := fmt.Sprintf(`%s

Reply with one JSON object and nothing else:
{"winner": "A" | "B" | "tie", "confidence": 0 to 1, "dimensions": {"correctness": "A"|"B"|"tie", "toolChoice": "A"|"B"|"tie", "completeness": "A"|"B"|"tie"}, "reason": "one sentence"}

CONVERSATION (latest part):
%s

ANSWER A:
%s

ANSWER B:
%s`, rubric, task, a, b)
	temperature := 0.0
	body, err := s.r.forwardToProviderWithin(ctx, judge, ChatCompletionRequest{
		Messages:    []Message{{Role: "user", Content: prompt}},
		Temperature: &temperature,
		// A judge reply is short; max_completion_tokens is the field the newer
		// OpenAI models accept.
		MaxCompletionTokens: s.cfg.JudgeMaxTokens,
	}, shadowCallTimeout)
	if err != nil {
		return judgeVerdict{}, TokenUsage{}, err
	}
	var resp ChatCompletionResponse
	if err := json.Unmarshal(body, &resp); err != nil || len(resp.Choices) == 0 {
		return judgeVerdict{}, resp.Usage, fmt.Errorf("judge returned no choices")
	}
	v, err := parseVerdict(messageText(resp.Choices[0].Message))
	return v, resp.Usage, err
}

// taskExcerpt renders the tail of the conversation the judge needs to see.
func taskExcerpt(msgs []Message) string {
	var sb strings.Builder
	for _, m := range msgs {
		if m.Role == "system" {
			continue
		}
		fmt.Fprintf(&sb, "[%s] %s\n", m.Role, answerText(m))
	}
	text := sb.String()
	if r := []rune(text); len(r) > shadowContextRunes {
		return "…" + string(r[len(r)-shadowContextRunes:])
	}
	return text
}

// answerText renders an answer for the judge: its text, then the tool calls it
// makes.
func answerText(m Message) string {
	var sb strings.Builder
	sb.WriteString(messageText(m))
	for _, tc := range m.ToolCalls {
		fmt.Fprintf(&sb, "\n[tool call] %s(%s)", tc.Function.Name, tc.Function.Arguments)
	}
	text := strings.TrimSpace(sb.String())
	if text == "" {
		return "(empty answer)"
	}
	if r := []rune(text); len(r) > shadowAnswerRunes {
		return string(r[:shadowAnswerRunes]) + "…"
	}
	return text
}

// takeLease reserves budget for one replay. Any answer but a grant, including an
// unreachable operator, means no replay.
func (s *shadowRunner) takeLease(ctx context.Context, amountUSD float64) (int64, bool) {
	var resp struct {
		LeaseID int64 `json:"leaseId"`
	}
	status, body, err := s.r.postOperator(ctx, "shadow-lease", map[string]any{"benchmark": s.cfg.Benchmark, "amountUsd": amountUSD})
	if err != nil {
		slog.Warn("shadow lease request failed", "err", err)
		return 0, false
	}
	if status == http.StatusTooManyRequests {
		slog.Debug("shadow lease denied: budget or hourly limit reached")
		return 0, false
	}
	if status != http.StatusOK {
		// Anything but a spent budget is a setup problem, such as a missing route
		// or a benchmark that does not cover this Agent; say so instead of
		// quietly never sampling.
		slog.Warn("shadow lease refused", "status", status, "body", strings.TrimSpace(string(body)))
		return 0, false
	}
	if json.Unmarshal(body, &resp) != nil || resp.LeaseID == 0 {
		return 0, false
	}
	return resp.LeaseID, true
}

// post files a result, which also releases its lease.
func (s *shadowRunner) post(ctx context.Context, lease int64, res ShadowResult) {
	// The replay's own context may be spent by a slow judge; the result still matters.
	if ctx.Err() != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), shadowPostTimeout)
		defer cancel()
	}
	status, _, err := s.r.postOperator(ctx, "shadow-results", ShadowResultEnvelope{LeaseID: lease, Result: res})
	if err != nil || status >= 300 {
		slog.Warn("posting shadow result failed", "status", status, "err", err)
	}
}

// postOperator POSTs a JSON body to /agentrun/{ns}/{run}/{path} on the operator.
func (r *Router) postOperator(ctx context.Context, path string, payload any) (int, []byte, error) {
	if r.cfg.RunName == "" || r.cfg.OperatorAPIURL == "" {
		return 0, nil, fmt.Errorf("no operator API configured")
	}
	tokenBytes, err := os.ReadFile(r.cfg.SATokenFile)
	if err != nil {
		return 0, nil, fmt.Errorf("reading service account token: %w", err)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, shadowPostTimeout)
	defer cancel()
	url := fmt.Sprintf("%s/agentrun/%s/%s/%s", r.cfg.OperatorAPIURL, r.cfg.RunNamespace, r.cfg.RunName, path)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tokenBytes)))
	resp, err := routingDecisionClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	return resp.StatusCode, out, nil
}
