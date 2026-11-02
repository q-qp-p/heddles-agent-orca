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

// Agent-run ("local") tools: with Config.LocalTools, a function tool the agent
// declares in its request that the router does not provide is executed by the
// agent process, e.g. a shell inside its sandbox.
//
//  1. The turn streams as usual, but the model's raw tool-call deltas and
//     finish_reason "tool_calls" are withheld from the agent: within one HTTP
//     response the tool-call loop makes several LLM calls whose delta indices
//     all restart at 0, so an OpenAI client accumulator would merge the router's
//     calls into the agent's.
//  2. When the model calls agent tools, the router runs its own calls from the
//     same turn, then sends the agent only its calls (indexed from 0) with
//     finish_reason "tool_calls" and [DONE], and stops the loop.
//  3. The agent's next request must end with one "tool" message per pending
//     call. Pending calls are derived from the stored conversation (the last
//     assistant message's unanswered tool calls), so they survive a router
//     restart from a checkpoint.

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
)

// backendTypeAgent marks trace events for tools the agent process runs.
const backendTypeAgent = "agent"

// builtinToolAliases are the names dispatchToolCall handles itself without the
// leading underscore. Agent tools may not use them (or any "_" name), or the
// router would run its built-in instead.
var builtinToolAliases = map[string]bool{
	"handoff": true, "clarify": true, "done": true, "fail": true, "spawn": true,
	"create_workflow": true, "emit_event": true, "mcp_read_resource": true,
	"propose_step": true, "write_state": true, "read_state": true,
	"list_state": true, "delete_state": true, "search_history": true,
	"rag_search": true, "rag_ingest": true, "webhook_notify": true,
	"memory_store": true, "propose_fix": true, "confirm_fix": true,
	"list_resources": true,
	fsToolNameRead:   true, fsToolNameWrite: true, fsToolNameList: true,
}

// isRouterToolName reports whether the router runs a tool of this name.
func (r *Router) isRouterToolName(name string) bool {
	if strings.HasPrefix(name, "_") || builtinToolAliases[name] {
		return true
	}
	for _, td := range r.cfg.ToolDefinitions {
		if td.Name == name {
			return true
		}
	}
	return false
}

// localToolsForRequest returns the names of the agent tools a top-level
// request declares, or nil when agent tools are disabled or none are declared.
// A declared name the router also provides is an error: the call would be
// ambiguous.
func (r *Router) localToolsForRequest(tools []map[string]any) (map[string]bool, error) {
	if !r.cfg.LocalTools {
		return nil, nil
	}
	var local map[string]bool
	for _, t := range tools {
		fn, _ := t["function"].(map[string]any)
		name, _ := fn["name"].(string)
		if name == "" {
			continue
		}
		if r.isRouterToolName(name) {
			return nil, fmt.Errorf("tool %q is provided by the model-router; rename the agent tool", name)
		}
		if local == nil {
			local = make(map[string]bool)
		}
		local[name] = true
	}
	return local, nil
}

// unansweredToolCalls returns the tool calls of the last assistant message in
// msgs that have no "tool" result after it.
func unansweredToolCalls(msgs []Message) []ToolCall {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != "assistant" {
			continue
		}
		answered := make(map[string]bool)
		for _, m := range msgs[i+1:] {
			if m.Role == "tool" {
				answered[m.ToolCallID] = true
			}
		}
		var pending []ToolCall
		for _, tc := range msgs[i].ToolCalls {
			if !answered[tc.ID] {
				pending = append(pending, tc)
			}
		}
		return pending
	}
	return nil
}

// localToolResults takes the agent's results for the pending calls from the
// trailing "tool" messages of its request. Earlier messages are ignored: the
// stored conversation is the source of truth. Every pending call must be
// answered exactly once, and nothing else may be.
func (r *Router) localToolResults(msgs []Message, pending []ToolCall) ([]Message, error) {
	start := len(msgs)
	for start > 0 && msgs[start-1].Role == "tool" {
		start--
	}
	want := make(map[string]string, len(pending))
	for _, tc := range pending {
		want[tc.ID] = tc.Function.Name
	}
	results := make([]Message, 0, len(msgs)-start)
	for _, m := range msgs[start:] {
		name, ok := want[m.ToolCallID]
		if !ok {
			return nil, fmt.Errorf("tool result for %q does not answer a pending tool call", m.ToolCallID)
		}
		delete(want, m.ToolCallID)
		if text, isText := m.Content.(string); isText {
			m.Content = annotateTrivialToolResult(name, truncateToolResult(text, r.cfg.MaxToolResultTokens))
		}
		results = append(results, Message{Role: "tool", ToolCallID: m.ToolCallID, Content: m.Content})
	}
	if len(want) > 0 {
		missing := make([]string, 0, len(want))
		for _, tc := range pending {
			if _, ok := want[tc.ID]; ok {
				missing = append(missing, tc.ID)
			}
		}
		return nil, fmt.Errorf("request must end with tool results for pending tool calls %s", strings.Join(missing, ", "))
	}
	return results, nil
}

// traceLocalToolResults records the agent's results like router tool results.
func (r *Router) traceLocalToolResults(results []Message, pending []ToolCall) {
	names := make(map[string]string, len(pending))
	for _, tc := range pending {
		names[tc.ID] = tc.Function.Name
	}
	for _, m := range results {
		text := messageText(m)
		if len(text) > 500 {
			text = text[:500] + "…"
		}
		ev, err := json.Marshal(map[string]any{
			"type":        "toolResult",
			"name":        names[m.ToolCallID],
			"result":      text,
			"backendType": backendTypeAgent,
		})
		if err == nil {
			r.emitTraceEventBoth(string(ev))
		}
	}
}

// stripToolCallDeltas removes tool-call deltas and a "tool_calls" finish
// reason from a chunk bound for an agent that runs its own tools. It reports
// whether the chunk changed and returns nil when nothing is left to forward.
func stripToolCallDeltas(chunk streamingChatCompletionChunk) ([]byte, bool) {
	changed := false
	keep := chunk.Usage != nil
	choices := make([]streamChoice, len(chunk.Choices))
	for i, c := range chunk.Choices {
		if len(c.Delta.ToolCalls) > 0 {
			c.Delta.ToolCalls = nil
			changed = true
		}
		if c.FinishReason != nil && *c.FinishReason == finishReasonToolCalls {
			c.FinishReason = nil
			changed = true
		}
		if c.Delta.Role != "" || c.Delta.Content != "" || c.Delta.Reasoning != "" || c.FinishReason != nil {
			keep = true
		}
		choices[i] = c
	}
	if !changed {
		return nil, false
	}
	if !keep {
		return nil, true
	}
	chunk.Choices = choices
	data, err := json.Marshal(chunk)
	if err != nil {
		return nil, true
	}
	return data, true
}

// ensureToolCallIDs gives every call an ID: the agent answers by ID, and some
// OpenAI-compatible providers omit it.
func ensureToolCallIDs(calls []ToolCall) {
	for i := range calls {
		if calls[i].ID == "" {
			calls[i].ID = fmt.Sprintf("call_%d", i)
		}
	}
}

// returnToolCallsToAgent ends the turn by handing the agent its tool calls.
// The assistant message holding them is already stored.
func (r *Router) returnToolCallsToAgent(w http.ResponseWriter, flusher http.Flusher, calls []ToolCall) {
	out := make([]ToolCall, len(calls))
	for i, tc := range calls {
		tc.Index = i
		tc.Type = toolTypeFunction
		out[i] = tc
		if ev, err := json.Marshal(map[string]string{
			"type":        "toolCall",
			"name":        tc.Function.Name,
			"arguments":   tc.Function.Arguments,
			"backendType": backendTypeAgent,
		}); err == nil {
			r.emitTraceEvent(string(ev))
		}
	}
	finish := finishReasonToolCalls
	for _, chunk := range []streamingChatCompletionChunk{
		{
			ID: "agent-tool-calls", Object: "chat.completion.chunk",
			Choices: []streamChoice{{Index: 0, Delta: streamDelta{Role: "assistant", ToolCalls: out}}},
		},
		{
			ID: "agent-tool-calls", Object: "chat.completion.chunk",
			Choices: []streamChoice{{Index: 0, FinishReason: &finish}},
		},
	} {
		data, _ := json.Marshal(chunk)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
	}
	_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
	slog.Info("returned tool calls to the agent", "run", r.cfg.RunName, "calls", len(out))
	go r.notifyOperatorContext()
}
