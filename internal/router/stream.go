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
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/heddles/agent-orca/internal/ratelimit"
)

const maxRateLimitRetries = 3

// finishReasonStop is the canonical OpenAI finish reason for a completed turn.
const finishReasonStop = "stop"

// finishReasonToolCalls is the OpenAI finish reason for a turn that ends in tool calls.
const finishReasonToolCalls = "tool_calls"

// traceEventTypeDone is the schema-aligned completion signal trace event.
const traceEventTypeDone = "done"

// doWithRateLimitRetry executes an HTTP request, retrying on 429 responses
// with exponential backoff. The buildReq function is called for each attempt
// to produce a fresh request (the body reader is consumed on each attempt).
func doWithRateLimitRetry(ctx context.Context, buildReq func() (*http.Request, error)) (*http.Response, error) {
	for attempt := 0; attempt <= maxRateLimitRetries; attempt++ {
		req, err := buildReq()
		if err != nil {
			return nil, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusTooManyRequests || attempt == maxRateLimitRetries {
			return resp, nil
		}
		// Read the error body to determine if this is a transient rate limit
		// or a permanent "request too large" error.
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		bodyStr := string(body)
		// "Request too large" 429s will never succeed with retries —
		// the request itself exceeds the model's token limit.
		if strings.Contains(bodyStr, "Request too large") ||
			strings.Contains(bodyStr, "input or output tokens must be reduced") {
			return &http.Response{
				StatusCode: resp.StatusCode,
				Header:     resp.Header,
				Body:       io.NopCloser(strings.NewReader(bodyStr)),
			}, nil
		}
		delay := retryAfterDelay(resp.Header, body, attempt)
		slog.Warn("rate limited (429), retrying",
			"attempt", attempt+1,
			"delay", delay,
			"body", string(body),
		)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}
	// unreachable, but satisfies the compiler
	return nil, fmt.Errorf("rate limit retries exhausted")
}

// retryAfterDelay returns the provider-declared retry delay when one exists
// (google.rpc.RetryInfo retryDelay in the error body, or the Retry-After
// header), falling back to exponential backoff (2^attempt seconds, capped at
// 30s). A declared delay wins: retrying sooner just burns quota.
func retryAfterDelay(headers http.Header, body []byte, attempt int) time.Duration {
	if d := ratelimit.ParseRetryDelay(headers, body); d > 0 {
		return min(d, 60*time.Second)
	}
	d := min(time.Duration(1<<uint(attempt))*time.Second, 30*time.Second)
	return d
}

// streamingChatCompletionChunk mirrors the OpenAI streaming chunk format.
type streamingChatCompletionChunk struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Choices []streamChoice `json:"choices"`
	Usage   *TokenUsage    `json:"usage,omitempty"`
}

type streamChoice struct {
	Index        int         `json:"index"`
	Delta        streamDelta `json:"delta"`
	FinishReason *string     `json:"finish_reason"`
}

type streamDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
	// Reasoning holds streaming extended-thinking tokens (OpenAI o-series models emit
	// these as delta.reasoning). Captured and forwarded to the UI as `thought` trace
	// events; without this field the thinking is silently dropped while still being
	// proxied raw to the client, which can leave the run appearing to terminate as
	// soon as the (content-less) thinking block ends.
	Reasoning string     `json:"reasoning,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

// forwardToProviderStream sends a streaming request to the provider and returns the
// raw *http.Response for SSE proxying. The caller is responsible for closing the body.
// Uses a detached context so the outgoing call survives agent-side disconnects.
func (r *Router) forwardToProviderStream(ctx context.Context, provider *ProviderConfig, chatReq ChatCompletionRequest) (*http.Response, error) { //nolint:unparam

	// Use the run-scoped cancel context so the LLM call is detached from the
	// agent's HTTP connection (survives disconnects) but can still be cancelled
	// explicitly via Router.Cancel() when the run is stopped. Timeout is
	// config-driven (default 1h).
	llmCtx, cancel := context.WithTimeout(r.cancelCtx, r.cfg.LLMRequestTimeout)

	if strings.HasPrefix(provider.LiteLLMModel, "anthropic/") && provider.BaseURL == "" {
		resp, err := r.forwardToAnthropicStream(llmCtx, provider, chatReq)
		if err != nil {
			cancel()
			return nil, err
		}
		// Caller owns resp.Body; cancel when body is closed.
		resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
		return resp, nil
	}

	modelName := provider.LiteLLMModel
	// Keep full model identifier when custom BaseURL is set
	// since the proxy expects the complete identifier including provider prefix.
	if provider.BaseURL == "" {
		if idx := strings.Index(modelName, "/"); idx != -1 {
			modelName = modelName[idx+1:]
		}
	}
	chatReq.Model = modelName
	chatReq.Messages = adaptToolCallsForProvider(provider, chatReq.Messages)
	chatReq.Stream = true

	reqBody, err := json.Marshal(chatReq)
	if err != nil {
		cancel()
		return nil, err
	}

	endpoint := liteLLMEndpoint(provider)
	key, err := readAPIKey(provider.APIKeyFile)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("reading API key for %s: %w", provider.Name, err)
	}

	resp, err := doWithRateLimitRetry(llmCtx, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(llmCtx, http.MethodPost, endpoint, bytes.NewReader(reqBody))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+key)
		return req, nil
	})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("provider %s: %w", provider.Name, err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("provider %s returned %d: %s", provider.Name, resp.StatusCode, body)
	}
	// Caller owns resp.Body; cancel the context when the body is closed.
	resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

// cancelOnClose wraps an io.ReadCloser and calls a cancel function on Close.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

// forwardToAnthropicStream sends a streaming request to Anthropic and returns a response
// whose body emits OpenAI-compatible SSE events (translated on the fly).
func (r *Router) forwardToAnthropicStream(ctx context.Context, provider *ProviderConfig, chatReq ChatCompletionRequest) (*http.Response, error) {
	key, err := readAPIKey(provider.APIKeyFile)
	if err != nil {
		return nil, fmt.Errorf("reading API key for %s: %w", provider.Name, err)
	}

	// Concatenate system messages (user prompt + builtin hints) rather than replacing.
	// Note: SystemPrompt is now always injected into the messages array in HandleChatCompletions,
	// so we don't need to add it from config separately.
	systemParts, anthropicMessages := toAnthropicMessages(chatReq.Messages)
	if len(anthropicMessages) == 0 {
		return nil, fmt.Errorf("anthropic: no user messages in request")
	}

	modelName := strings.TrimPrefix(provider.LiteLLMModel, "anthropic/")
	reqBody := map[string]any{
		"model":      modelName,
		"messages":   anthropicMessages,
		"max_tokens": 4096,
		"stream":     true,
	}
	if len(systemParts) > 0 {
		reqBody["system"] = strings.Join(systemParts, "\n\n")
	}

	if len(chatReq.Tools) > 0 {
		reqBody["tools"] = toAnthropicTools(chatReq.Tools)
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshalling anthropic request: %w", err)
	}

	resp, err := doWithRateLimitRetry(ctx, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			anthropicMessagesURL, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		setAnthropicHeaders(req, provider, key)
		return req, nil
	})
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return nil, fmt.Errorf("anthropic returned %d: %s", resp.StatusCode, respBody)
	}

	// Wrap the Anthropic SSE body in a translator that emits OpenAI-format SSE.
	pr, pw := io.Pipe()
	go func() {
		defer func() { _ = pw.Close() }()
		translateAnthropicSSE(resp.Body, pw)
		_ = resp.Body.Close()
	}()

	// Return a synthetic response with the translated body.
	translated := *resp
	translated.Body = pr
	return &translated, nil
}

// handleStreamingResponse proxies SSE chunks from the provider to the client,
// accumulating the full message for conversation history tracking.
func (r *Router) handleStreamingResponse(w http.ResponseWriter, req *http.Request, provider *ProviderConfig, resp *http.Response, chatReq ChatCompletionRequest) { //nolint:gocyclo

	// The call's telemetry event is recorded when this returns, whichever way it
	// ends; the tool-call recursion below finishes its own event first.
	ct := callTelemetryFrom(req.Context())
	defer ct.finish()

	tokenStreamKey := "tokens:" + r.cfg.RunNamespace + ":" + r.cfg.RunName
	slog.Info("streaming response started", "provider", provider.Name, "tokenStreamKey", tokenStreamKey, "storeType", fmt.Sprintf("%T", r.store))

	// A recursive (tool-call-loop) invocation reuses the controller's ResponseWriter:
	// the outer call has already written the 200 + SSE headers. Re-calling
	// WriteHeader here is logged as "superfluous response.WriteHeader call" and, on
	// some servers, resets framing — so skip it for continuations.
	isContinuation := req.Context().Value(continuationKey{}) != nil

	flusher, ok := w.(http.Flusher)
	if !ok {
		if !isContinuation {
			slog.Error("ResponseWriter does not support flushing, falling back to buffered")
			body, _ := io.ReadAll(resp.Body)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
		}
		return
	}

	// A budget pause may already have begun the response to hold it open.
	if !isContinuation && !r.sseStarted.Swap(false) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
	}

	// Accumulate the full response for conversation history.
	var contentBuilder strings.Builder
	var toolCalls []ToolCall
	var usage TokenUsage
	var finishReason string

	// sawToolCall is set whenever ANY tool-call delta is observed on this turn,
	// independent of mergeToolCallDeltas' index-based merging. The terminal
	// guards — the canonical client [DONE] (below) and the run-level done sentinel
	// written to Redis — are keyed off sawToolCall rather than len(toolCalls)==0.
	// This decouples "is this a terminal text turn?" from the fragility of
	// mergeToolCallDeltas: if a proxy streams tool-call deltas with a missing or
	// colliding `index` field, the merge could undercount and make a tool-call turn
	// look terminal (len==0), which would emit [DONE] and the done sentinel
	// mid-loop and cause the UI to exit prematurely ("done sentinel too soon").
	// sawToolCall can only go true when real tool-call content was streamed, so it
	// is a faithful, merge-independent terminal detector.
	var sawToolCall bool

	// Agent tools declared for this turn (local_tools.go); nil when the agent
	// runs none.
	r.mu.Lock()
	localTools := r.turnLocalTools
	r.mu.Unlock()

	scanner := bufio.NewScanner(resp.Body)
	// Allow large SSE lines (up to 1MB).
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	slog.Debug("scanner started", "isContinuation", isContinuation, "provider", provider.Name)

	// We withhold EVERY upstream `data: [DONE]` and emit a single canonical
	// terminal [DONE] ourselves once the scan completes (see below). The OpenAI
	// streaming spec permits exactly one terminating [DONE]; some providers
	// (e.g. thinking-model proxies) emit a premature [DONE] right after the
	// reasoning block and then keep streaming content. Forwarding it verbatim
	// makes the OpenAI-SDK client close the stream early ("exit too soon") and
	// discard the trailing content. Withholding normalizes the stream to exactly
	// one terminal [DONE]. This also covers the tool-call case (the recursion
	// emits its own [DONE] at the true end of the loop), so the per-turn
	// suppression logic that used to live here is no longer needed.
	lineCount := 0
	// Reasoning arrives a few characters at a time. It is gathered here and
	// emitted as one `thought` event when the thinking block ends (content, a
	// tool call, the finish reason or the end of the stream), so a trace holds one
	// entry per block rather than one per delta.
	var thought strings.Builder
	flushThought := func() {
		if thought.Len() == 0 {
			return
		}
		ev, _ := json.Marshal(map[string]string{"type": "thought", "content": thought.String()})
		thought.Reset()
		r.emitTraceEventBoth(string(ev))
	}
	for scanner.Scan() {
		lineCount++
		line := scanner.Text()

		// Parse "data: " lines first so we can decide whether to proxy them.
		if after, ok0 := strings.CutPrefix(line, "data: "); ok0 {
			data := after

			if data == "[DONE]" {
				// Withhold; a single canonical [DONE] is emitted after the scan.
				continue
			}

			var chunk streamingChatCompletionChunk
			if err := json.Unmarshal([]byte(data), &chunk); err == nil {
				for _, choice := range chunk.Choices {
					// Reasoning in this chunk precedes its content and tool calls.
					thought.WriteString(choice.Delta.Reasoning)
					if choice.Delta.Reasoning != "" || choice.Delta.Content != "" || len(choice.Delta.ToolCalls) > 0 {
						ct.noteFirstToken()
					}
					// Some providers send an empty finish_reason on every chunk; only a
					// real one ends the thinking block.
					if choice.Delta.Content != "" || len(choice.Delta.ToolCalls) > 0 || (choice.FinishReason != nil && *choice.FinishReason != "") {
						flushThought()
					}
					if choice.Delta.Content != "" {
						contentBuilder.WriteString(choice.Delta.Content)
						// Broadcast to external subscribers (operator SSE stream).
						r.tokens.Send(choice.Delta.Content)
						// Record model-router token output for :9091 metrics.
						r.metrics.AddTokens(len(choice.Delta.Content))
						// Write to Redis Stream for UI token streaming.
						if r.store != nil {
							if err := r.store.SaveToken(req.Context(), tokenStreamKey, choice.Delta.Content); err != nil {
								slog.Warn("failed to save token to Redis stream", "key", tokenStreamKey, "err", err)
							}
						}
					}
					if len(choice.Delta.ToolCalls) > 0 {
						sawToolCall = true
						toolCalls = mergeToolCallDeltas(toolCalls, choice.Delta.ToolCalls)
					}
					if choice.FinishReason != nil {
						finishReason = *choice.FinishReason
					}
				}
				if chunk.Usage != nil {
					usage = *chunk.Usage
				}
				// An agent that runs its own tools gets only its calls, sent
				// once the turn's tool calls are known (returnToolCallsToAgent).
				if localTools != nil {
					if stripped, changed := stripToolCallDeltas(chunk); changed {
						if stripped == nil {
							continue
						}
						line = "data: " + string(stripped)
					}
				}
			}
		}

		// Proxy every line (including empty lines for SSE framing) to the client,
		// except [DONE] which is handled above.
		_, _ = fmt.Fprintf(w, "%s\n", line)
		flusher.Flush()
	}
	flushThought()
	slog.Debug("scanner finished", "isContinuation", isContinuation, "provider", provider.Name, "lines", lineCount, "sawToolCall", sawToolCall, "toolCalls", len(toolCalls))

	spendOutcome := spendOutcomeSuccess
	if err := scanner.Err(); err != nil {
		slog.Warn("error reading SSE stream", "err", err)
		spendOutcome = spendOutcomeStreamInterrupted
	}
	// The provider reports usage in its last chunk, which an interrupted or
	// cancelled stream never delivers. The tokens were still generated and
	// billed, so approximate them rather than recording the call as free.
	partial := Message{Role: "assistant", Content: contentBuilder.String(), ToolCalls: toolCalls}
	if usage.PromptTokens == 0 && usage.CompletionTokens == 0 &&
		(spendOutcome == spendOutcomeStreamInterrupted || r.cancelCtx.Err() != nil) {
		usage = estimateInterruptedUsage(chatReq.Messages, partial)
	}
	ct.noteStreamEnd(usage, finishReason)
	switch {
	case r.cancelCtx.Err() != nil:
		ct.setError(errClassCancelled)
	case spendOutcome == spendOutcomeStreamInterrupted:
		ct.setError(errClassStreamBroken)
	}

	// If the run was cancelled, the OpenAI turn loop never saw a terminal finish
	// reason. Emit a `fail` trace event so TailTokens closes promptly (the poller
	// would also catch this, but the fail event gives an immediate, authoritative
	// signal). No magic done sentinel — completion follows the OpenAI schema's
	// [DONE] to the agent client; the UI relies on finalOutput (with output) not
	// a done event (without output).
	if r.cancelCtx.Err() != nil {
		slog.Info("run cancelled, emitting terminal fail event", "run", r.cfg.RunName)
		// This early return skips the normal spend accounting below, which
		// dropped the cost of every call that was in flight at cancellation.
		r.mu.Lock()
		r.recordSpend(usage, provider, spendOutcomeCancelled)
		r.mu.Unlock()
		_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
		failEv, _ := json.Marshal(map[string]string{"type": "fail", "reason": "router cancel"})
		r.emitTraceEventBoth(string(failEv))
		return
	}

	// NOTE: the terminal `done` trace event is emitted below (after this clarify
	// safety-net check) only on the genuine terminal text turn. If _clarify is
	// auto-triggered, executeClarify emits its own `clarify` trace event — a terminal
	// event — and the run phase moves to WaitingForInput, so TailTokens closes via
	// that event (and/or the CRD terminal-state poller). No empty-token sentinel is
	// written here either.

	if localTools != nil {
		ensureToolCallIDs(toolCalls)
	}

	// Build the complete message for conversation history.
	assistantMsg := Message{
		Role:      "assistant",
		Content:   contentBuilder.String(),
		ToolCalls: toolCalls,
	}

	if r.cfg.Hindsight.Enabled {

		bankID, errEnsureHindsightBank := r.ensureHindsightBank(req.Context())

		if errEnsureHindsightBank != nil {
			slog.Warn("hindsight bank does not exist, memory system usage will fail.")
		} else if bankID != "" {
			r.announceHindsightBank()
		}
		content := fmt.Sprintf("ai assistant: %s", messageText(assistantMsg))
		if err := r.hindsightRetain(r.cfg.RunName, content); err != nil {
			slog.Warn("hindsight retain failed", "run", r.cfg.RunName, "err", err)
			r.emitHindsightTraceEvent("retain", true, err.Error())
		} else {
			slog.Info("hindsight retain success", "run", r.cfg.RunName, "bankID", r.hindsightBankID())
			r.emitHindsightTraceEvent("retain", false, "")
		}
	}

	// Update conversation history. Only the client's new input is stored, never
	// chatReq.Messages: the sent request also holds the stored history and the
	// injected preamble, and storing it doubled the history every turn (#47).
	streamIsContinuation := req.Context().Value(continuationKey{}) != nil
	r.mu.Lock()
	// liveBufferTokens counts the stored conversation, so add only what is
	// stored. usage.PromptTokens is the whole request (stored history,
	// preamble, tools): adding it double-counted the history and tripped the
	// live-buffer compaction far below its budget (#47).
	if !streamIsContinuation {
		turnInput, _ := req.Context().Value(turnInputKey{}).([]Message)
		r.messages = append(r.messages, turnInput...)
		r.incrementBufferTokens(estimateTokens(turnInput))
	}
	r.messages = append(r.messages, assistantMsg)
	r.incrementBufferTokens(messageTokens(assistantMsg))
	r.recordSpend(usage, provider, spendOutcome)
	r.ruleRouter.IncrementTurn()
	// concludeTurn folds the finished turn and caps the buffer. It is gated to
	// the final (non-tool) text response; during the tool-call loop the buffer is
	// capped inline by capLiveBufferDuringToolLoop() (called after each batch of
	// tool results is appended) so history doesn't only-grow until the loop exits.
	if len(toolCalls) == 0 {
		r.concludeTurn()
	}
	r.mu.Unlock()

	// Checkpoint if needed.
	if r.cfg.CheckpointEvery > 0 && r.ruleRouter.TurnCount()%r.cfg.CheckpointEvery == 0 {
		go r.checkpoint(context.Background())
	}

	// Safety net: if the LLM output a question as text instead of calling _clarify,
	// detect it and synthetically trigger _clarify. The SSE stream has already been
	// proxied to the agent, but we can still intercept here: trigger clarify, which
	// sets WaitingForInput on the run and causes the next agent request to get 410 Gone.
	autoClarify := false
	fullText := ""
	// The clarify safety net only applies to genuine text-completion finishes.
	if len(toolCalls) == 0 && (finishReason == finishReasonStop || finishReason == "") {
		fullText = contentBuilder.String()
		syntheticResp := ChatCompletionResponse{
			Choices: []Choice{{
				Message:      Message{Role: "assistant", Content: fullText},
				FinishReason: finishReasonStop,
			}},
		}
		if r.shouldAutoTriggerClarify(syntheticResp) {
			autoClarify = true
			slog.Info("auto-triggering _clarify from streaming response", "run", r.cfg.RunName)
			// Checkpoint conversation so the pod can resume with the answer.
			r.checkpoint(context.Background())
			// Notify the operator to set WaitingForInput — do NOT write to the
			// token stream here because the LLM's text tokens have already been
			// streamed. The UI API's Phase 3 polling will emit the clarify SSE event.
			if err := r.notifyOperatorClarify(fullText); err != nil {
				slog.Error("failed to notify operator for clarify", "err", err)
			}
			// No `done` trace event here: the run PAUSED (WaitingForInput), it did
			// not complete — stream consumers close this stream via the CRD terminal
			// state instead.
		}
	}
	// Emit the terminal `done` trace event whenever the OpenAI turn loop ended
	// without dispatching tools (stop, length, or null finish — all terminate the
	// loop): TailTokens closes on it (state.IsTerminalTraceEventJSON), ending the
	// run's SSE stream immediately instead of tailing an already-finished stream
	// (the editor bridge then finalized the turn seconds — not minutes — after
	// the final response arrived). Auto-clarify turns are excluded: the run
	// PAUSED (WaitingForInput), it did not complete.
	if len(toolCalls) == 0 && !autoClarify {
		doneEv, _ := json.Marshal(map[string]string{"type": traceEventTypeDone, "output": fullText})
		r.emitTraceEventBoth(string(doneEv))
	}
	if len(toolCalls) == 0 {
		tokens := usage.CompletionTokens
		if tokens == 0 {
			tokens = estimateTokens([]Message{{Role: "assistant", Content: contentBuilder.String()}})
		}
		r.mu.Lock()
		trip := r.recordTextTurnLocked(tokens)
		r.mu.Unlock()
		if trip != nil {
			// The reply has already streamed; the trip makes the next request 410.
			r.tripSafeguard(*trip)
		}
	}
	// Handle tool calls if the LLM requested them.
	// NOTE: r.messages already holds this turn's input + assistantMsg (added above),
	// so we only need to dispatch, append results, and recurse — not re-append the history.
	//
	// Dispatch on len(toolCalls) > 0 ALONE — the same predicate the non-streaming path
	// uses (HandleChatCompletions, router.go:879). Do NOT additionally require
	// finishReason == "tool_calls": OpenAI-compatible proxies (LiteLLM)
	// frequently stream finish_reason: null and signal completion solely via
	// `data: [DONE]`. Gating dispatch on finish_reason there desyncs the [DONE]
	// suppression from the dispatch — [DONE] is suppressed (tool calls present) but the
	// tool call is never dispatched, so it is silently dropped and the client never
	// receives a terminating [DONE]. Mirroring the non-streaming path fixes this.
	//
	// The terminal guards above key off sawToolCall (any tool-call delta observed),
	// which is true whenever len(toolCalls) > 0 after a successful merge — so the
	// dispatch predicate and the terminal gate stay in sync even if a future merge
	// change alters index handling.
	if len(toolCalls) > 0 {

		toolResults := make([]Message, 0, len(toolCalls))
		var agentCalls []ToolCall
		for _, tc := range toolCalls {
			r.mu.Lock()
			trip := r.recordToolCallLocked(tc)
			r.mu.Unlock()
			if trip != nil {
				ct.setSafeguardTrip(trip.Reason)
				r.tripSafeguard(*trip)
				writeSafeguardHalt(w, flusher, *trip)
				return
			}
			if localTools[tc.Function.Name] {
				agentCalls = append(agentCalls, tc)
				continue
			}
			result := r.dispatchToolCall(req.Context(), tc)
			ct.noteToolResult(result)
			r.metrics.IncToolCall()

			// Stream tool call delta to client, unless it runs its own tools:
			// it would take the router's call for one of its own.
			if localTools == nil {
				toolChunk := streamingChatCompletionChunk{
					ID:     "tool-call-" + tc.ID,
					Object: "chat.completion.chunk",
					Choices: []streamChoice{{
						Index: 0,
						Delta: streamDelta{ToolCalls: []ToolCall{tc}},
					}},
				}
				data, _ := json.Marshal(toolChunk)
				_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
				flusher.Flush()
			}

			toolResults = append(toolResults, Message{
				Role:       "tool",
				ToolCallID: tc.ID,
				Content:    result,
			})
		}
		r.mu.Lock()
		r.messages = append(r.messages, toolResults...)
		r.incrementBufferTokens(estimateTokens(toolResults))
		if len(agentCalls) > 0 {
			// The agent's results arrive on its next request (local_tools.go).
			r.mu.Unlock()
			r.returnToolCallsToAgent(w, flusher, agentCalls)
			return
		}
		// The continuation compacts the conversation if it has grown past the
		// threshold (compactStoredConversation), like any other request.
		continueReq := ChatCompletionRequest{
			Model:         chatReq.Model,
			Messages:      r.messages,
			Tools:         chatReq.Tools,
			Stream:        true,
			StreamOptions: &StreamOptions{},
		}

		r.mu.Unlock()
		// Recursive call with the full conversation as the new request body.
		slog.Debug("recursive HandleChatCompletions call", "toolCalls", len(toolCalls), "messages", len(r.messages))
		newBody, _ := json.Marshal(continueReq)
		ctx := context.WithValue(req.Context(), continuationKey{}, true)
		newReq := req.Clone(ctx)
		newReq.Body = io.NopCloser(bytes.NewReader(newBody))
		newReq.ContentLength = int64(len(newBody))
		r.HandleChatCompletions(w, newReq)
		slog.Debug("recursive HandleChatCompletions returned", "toolCalls", len(toolCalls))
		return
	}
	_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
	// Notify operator of context usage for UI display (streaming path).
	go r.notifyOperatorContext()
}

// tryFallbackStream attempts streaming fallback to providers in the fallback chain.
func (r *Router) tryFallbackStream(
	ctx context.Context, origReq ChatCompletionRequest, failedProvider string,
) (*http.Response, *ProviderConfig, fittedRequest, error) {
	// Each fallback is fitted to its OWN context window from the original
	// (un-truncated) request: replaying the primary's request verbatim sent
	// over-window payloads to smaller-window fallbacks, which all answered 400
	// (#42). A provider the request cannot fit is skipped without a call.
	skippedForWindow, tried := 0, 0
	for _, name := range r.cfg.FallbackChain {
		if name == failedProvider {
			continue
		}
		for i := range r.cfg.Providers {
			if r.cfg.Providers[i].Name == name {
				p := &r.cfg.Providers[i]
				tried++
				fitted, err := r.fitRequestToProvider(p, origReq)
				if err != nil {
					skippedForWindow++
					slog.Warn("fallback provider skipped: request does not fit its window", "provider", name, "err", err)
					continue
				}
				slog.Info("trying fallback provider (streaming)", "provider", name)
				resp, err := r.forwardToProviderStream(ctx, p, fitted.req)
				if err == nil {
					return resp, p, fitted, nil
				}
				slog.Warn("fallback provider failed (streaming)", "provider", name, "err", err)
				r.recordProviderError(p)
			}
		}
	}
	if tried == 0 {
		return nil, nil, fittedRequest{req: origReq}, errNoFallbackProviders
	}
	if skippedForWindow == tried {
		return nil, nil, fittedRequest{req: origReq}, fmt.Errorf("all fallback providers exhausted: %w", errExceedsContextWindow)
	}
	return nil, nil, fittedRequest{req: origReq}, fmt.Errorf("all fallback providers exhausted")
}

// mergeToolCallDeltas merges incremental tool call deltas into a running list.
// OpenAI streams tool calls as incremental chunks identified by their Index field.
// Each parallel tool call has a unique index; chunks with the same index are merged.
func mergeToolCallDeltas(existing []ToolCall, deltas []ToolCall) []ToolCall {
	for _, d := range deltas {
		// Gemini's OpenAI-compatible stream omits "index", so every parallel tool
		// call arrives as index 0. A different non-empty ID on an occupied index
		// is therefore a new call, not a continuation: give it its own index
		// instead of concatenating its name and arguments onto the first call.
		for i := range existing {
			if existing[i].Index == d.Index && d.ID != "" && existing[i].ID != "" && d.ID != existing[i].ID {
				d.Index = nextToolCallIndex(existing)
				break
			}
		}
		// Find existing entry by index.
		found := false
		for i := range existing {
			if existing[i].Index == d.Index {
				existing[i].Function.Arguments += d.Function.Arguments
				if d.ID != "" {
					existing[i].ID = d.ID
				}
				if d.Function.Name != "" {
					existing[i].Function.Name = d.Function.Name
				}
				if d.Type != "" {
					existing[i].Type = d.Type
				}
				if len(d.ExtraContent) > 0 {
					existing[i].ExtraContent = d.ExtraContent
				}
				found = true
				break
			}
		}
		if !found {
			existing = append(existing, d)
		}
	}
	return existing
}

// nextToolCallIndex returns the smallest index above every index in calls.
func nextToolCallIndex(calls []ToolCall) int {
	next := 0
	for _, c := range calls {
		if c.Index >= next {
			next = c.Index + 1
		}
	}
	return next
}

// translateAnthropicSSE reads Anthropic SSE events and writes OpenAI-format SSE events to w.
func translateAnthropicSSE(r io.Reader, w io.Writer) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var msgID string
	// Track the current content block index → tool call index mapping.
	// Anthropic uses content block index; OpenAI uses tool call index.
	blockIndexToToolIndex := map[int]int{}
	toolCallCount := 0

	for scanner.Scan() {
		line := scanner.Text()

		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")

		var event map[string]any
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			continue
		}

		eventType, _ := event["type"].(string)
		switch eventType {
		case "message_start":
			if msg, ok := event["message"].(map[string]any); ok {
				msgID, _ = msg["id"].(string)
			}
			// Emit initial role chunk.
			chunk := streamingChatCompletionChunk{
				ID:     msgID,
				Object: "chat.completion.chunk",
				Choices: []streamChoice{{
					Index: 0,
					Delta: streamDelta{Role: "assistant"},
				}},
			}
			writeSSEChunk(w, chunk)

		case "content_block_start":
			// Detect tool_use blocks and register their index mapping.
			blockIdx, _ := event["index"].(float64)
			if cb, ok := event["content_block"].(map[string]any); ok {
				if cbType, _ := cb["type"].(string); cbType == "tool_use" { //nolint:goconst

					toolIdx := toolCallCount
					toolCallCount++
					blockIndexToToolIndex[int(blockIdx)] = toolIdx
					toolID, _ := cb["id"].(string)
					toolName, _ := cb["name"].(string)
					toolIdxCopy := toolIdx
					chunk := streamingChatCompletionChunk{
						ID:     msgID,
						Object: "chat.completion.chunk",
						Choices: []streamChoice{{
							Index: 0,
							Delta: streamDelta{
								ToolCalls: []ToolCall{{
									Index: toolIdxCopy,
									ID:    toolID,
									Type:  "function",
									Function: FunctionCall{
										Name:      toolName,
										Arguments: "",
									},
								}},
							},
						}},
					}
					writeSSEChunk(w, chunk)
				}
			}

		case "content_block_delta":
			blockIdx, _ := event["index"].(float64)
			if delta, ok := event["delta"].(map[string]any); ok {
				deltaType, _ := delta["type"].(string)
				switch deltaType {
				case "text_delta":
					if text, ok := delta["text"].(string); ok {
						chunk := streamingChatCompletionChunk{
							ID:     msgID,
							Object: "chat.completion.chunk",
							Choices: []streamChoice{{
								Index: 0,
								Delta: streamDelta{Content: text},
							}},
						}
						writeSSEChunk(w, chunk)
					}
				case "input_json_delta":
					// Streaming tool call argument chunk.
					if partial, ok := delta["partial_json"].(string); ok {
						if toolIdx, mapped := blockIndexToToolIndex[int(blockIdx)]; mapped {
							toolIdxCopy := toolIdx
							chunk := streamingChatCompletionChunk{
								ID:     msgID,
								Object: "chat.completion.chunk",
								Choices: []streamChoice{{
									Index: 0,
									Delta: streamDelta{
										ToolCalls: []ToolCall{{
											Index: toolIdxCopy,
											Function: FunctionCall{
												Arguments: partial,
											},
										}},
									},
								}},
							}
							writeSSEChunk(w, chunk)
						}
					}
				}
			}

		case "message_delta":
			if delta, ok := event["delta"].(map[string]any); ok {
				stopReason, _ := delta["stop_reason"].(string)
				switch stopReason {
				case "end_turn": //nolint:goconst

					stopReason = finishReasonStop
				case "tool_use":
					stopReason = "tool_calls"
				}
				chunk := streamingChatCompletionChunk{
					ID:     msgID,
					Object: "chat.completion.chunk",
					Choices: []streamChoice{{
						Index:        0,
						Delta:        streamDelta{},
						FinishReason: &stopReason,
					}},
				}
				// Include usage if present.
				if usage, ok := event["usage"].(map[string]any); ok {
					outputTokens, _ := usage["output_tokens"].(float64)
					chunk.Usage = &TokenUsage{
						CompletionTokens: int(outputTokens),
					}
				}
				writeSSEChunk(w, chunk)
			}

		case "message_stop":
			_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
		}
	}
	if err := scanner.Err(); err != nil {
		slog.Warn("error reading SSE stream", "err", err)
	}
}

func writeSSEChunk(w io.Writer, chunk streamingChatCompletionChunk) {
	data, err := json.Marshal(chunk)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
}
