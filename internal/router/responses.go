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
)

// OpenAI's gpt-6 reasoning models reject function tools on /v1/chat/completions
// ("Function tools with reasoning_effort are not supported ... use /v1/responses")
// and some, such as gpt-6.1-sol, cannot turn reasoning off. Every agent call
// carries tools, so those models only work through the Responses API.
//
// The router stays on chat completions for every provider and switches a provider
// to Responses the first time OpenAI says so. It remembers the switch for the
// life of the router, so the cost is one fast 400. No ModelProvider field is
// needed and a model that later gains chat-completions support keeps working.
//
// The Responses stream is translated to chat-completion chunks, as the
// Anthropic stream is, so nothing downstream knows which API answered.
//
// Reasoning items are not carried between turns: the router stores chat
// messages, which have nowhere to keep them. The model reasons afresh each call.

// responsesHint is the phrase OpenAI puts in the 400 that asks for the switch.
const responsesHint = "/v1/responses"

// Names the Responses translation shares with the chat-completion shapes.
const (
	toolTypeFunction   = "function"
	partTypeImageURL   = "image_url"
	finishReasonLength = "length"
)

// needsResponsesAPI reports whether a chat-completions error body asks the
// caller to use the Responses API instead.
func needsResponsesAPI(body []byte) bool {
	return bytes.Contains(body, []byte(responsesHint))
}

// responsesEndpoint returns the Responses URL for a provider, or "" when the
// provider has no known one.
func responsesEndpoint(p *ProviderConfig) string {
	if p.BaseURL != "" {
		base := strings.TrimRight(p.BaseURL, "/")
		switch {
		case strings.HasSuffix(base, "/v1/chat/completions"):
			return strings.TrimSuffix(base, "/chat/completions") + "/responses"
		case strings.HasSuffix(base, "/v1"):
			return base + "/responses"
		default:
			return base + "/v1/responses"
		}
	}
	if strings.HasPrefix(p.LiteLLMModel, "openai/") {
		return "https://api.openai.com/v1/responses"
	}
	return ""
}

// forwardToResponsesStream sends the request to the Responses API and returns
// a response whose body is chat-completion SSE. It takes over cancel: it calls
// it on failure and ties it to the body's Close on success.
func (r *Router) forwardToResponsesStream(ctx context.Context, cancel context.CancelFunc, provider *ProviderConfig, chatReq ChatCompletionRequest) (*http.Response, error) {
	endpoint := responsesEndpoint(provider)
	key, err := readAPIKey(provider.APIKeyFile)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("reading API key for %s: %w", provider.Name, err)
	}
	body, err := json.Marshal(toResponsesRequest(chatReq))
	if err != nil {
		cancel()
		return nil, err
	}
	resp, err := doWithRateLimitRetry(ctx, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
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
		respBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("provider %s returned %d: %s", provider.Name, resp.StatusCode, respBody)
	}

	pr, pw := io.Pipe()
	go func() {
		defer func() { _ = pw.Close() }()
		translateResponsesSSE(resp.Body, pw)
		_ = resp.Body.Close()
	}()
	translated := *resp
	translated.Body = &cancelOnClose{ReadCloser: pr, cancel: cancel}
	return &translated, nil
}

// toResponsesRequest converts a chat-completion request to a Responses request.
// The conversation is stateless (store is off): the whole history goes in input.
func toResponsesRequest(req ChatCompletionRequest) map[string]any {
	instructions, input := toResponsesInput(req.Messages)
	out := map[string]any{
		"model":  req.Model,
		"input":  input,
		"stream": true,
		"store":  false,
	}
	if instructions != "" {
		out["instructions"] = instructions
	}
	if tools := toResponsesTools(req.Tools); len(tools) > 0 {
		out["tools"] = tools
	}
	if s, ok := req.ToolChoice.(string); ok && s != "" {
		out["tool_choice"] = s
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if n := max(req.MaxCompletionTokens, req.MaxTokens); n > 0 {
		out["max_output_tokens"] = n
	}
	return out
}

// toResponsesInput splits system text into instructions and maps the rest to
// Responses input items: messages, function_call and function_call_output.
func toResponsesInput(msgs []Message) (string, []any) {
	var instructions []string
	input := make([]any, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case "system", "developer":
			if text := messageText(m); text != "" {
				instructions = append(instructions, text)
			}
		case "user":
			input = append(input, map[string]any{"role": "user", "content": responsesUserContent(m.Content)})
		case "assistant":
			if text := messageText(m); text != "" {
				input = append(input, map[string]any{"role": "assistant", "content": text})
			}
			for _, tc := range m.ToolCalls {
				args := strings.TrimSpace(tc.Function.Arguments)
				if args == "" {
					args = "{}"
				}
				input = append(input, map[string]any{
					"type":      "function_call",
					"call_id":   tc.ID,
					"name":      tc.Function.Name,
					"arguments": args,
				})
			}
		case "tool":
			input = append(input, map[string]any{
				"type":    "function_call_output",
				"call_id": m.ToolCallID,
				"output":  messageText(m),
			})
		}
	}
	return strings.Join(instructions, "\n\n"), input
}

// responsesUserContent maps a user message's content: a string stays a string,
// text and image parts become input_text and input_image.
func responsesUserContent(content any) any {
	if s, ok := content.(string); ok {
		return s
	}
	raw, err := json.Marshal(content)
	if err != nil {
		return ""
	}
	var parts []map[string]any
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	out := make([]any, 0, len(parts))
	for _, p := range parts {
		switch p["type"] {
		case "text":
			out = append(out, map[string]any{"type": "input_text", "text": p["text"]})
		case partTypeImageURL:
			url := p[partTypeImageURL]
			if m, ok := url.(map[string]any); ok {
				url = m["url"]
			}
			out = append(out, map[string]any{"type": "input_image", partTypeImageURL: url})
		}
	}
	return out
}

// toResponsesTools flattens chat-completion function tools. strict is off
// because chat completions never enforced it and tool schemas from MCP servers
// rarely satisfy strict mode, which Responses would otherwise turn on.
func toResponsesTools(tools []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		fn, ok := t["function"].(map[string]any)
		if t["type"] != toolTypeFunction || !ok {
			out = append(out, t)
			continue
		}
		flat := map[string]any{"type": toolTypeFunction, "strict": false}
		for _, k := range []string{"name", "description", "parameters"} {
			if v, ok := fn[k]; ok {
				flat[k] = v
			}
		}
		out = append(out, flat)
	}
	return out
}

// responsesEvent is the part of a Responses stream event the translator reads.
type responsesEvent struct {
	Type        string `json:"type"`
	Delta       string `json:"delta"`
	OutputIndex int    `json:"output_index"`
	Message     string `json:"message"`
	Item        struct {
		Type   string `json:"type"`
		CallID string `json:"call_id"`
		Name   string `json:"name"`
	} `json:"item"`
	Response struct {
		ID                string `json:"id"`
		IncompleteDetails *struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
		Usage *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
			TotalTokens  int `json:"total_tokens"`
		} `json:"usage"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	} `json:"response"`
}

// translateResponsesSSE reads Responses SSE events and writes chat-completion
// SSE chunks to w.
func translateResponsesSSE(rd io.Reader, w io.Writer) {
	scanner := bufio.NewScanner(rd)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	var respID string
	toolIndex := map[int]int{} // output_index → tool call index
	chunkOf := func(d streamDelta) streamingChatCompletionChunk {
		return streamingChatCompletionChunk{
			ID:      respID,
			Object:  "chat.completion.chunk",
			Choices: []streamChoice{{Index: 0, Delta: d}},
		}
	}

	for scanner.Scan() {
		data, ok := strings.CutPrefix(scanner.Text(), "data: ")
		if !ok {
			continue
		}
		var ev responsesEvent
		if json.Unmarshal([]byte(data), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "response.created":
			respID = ev.Response.ID
			writeSSEChunk(w, chunkOf(streamDelta{Role: "assistant"}))

		case "response.output_item.added":
			if ev.Item.Type != "function_call" {
				continue
			}
			idx := len(toolIndex)
			toolIndex[ev.OutputIndex] = idx
			writeSSEChunk(w, chunkOf(streamDelta{ToolCalls: []ToolCall{{
				Index:    idx,
				ID:       ev.Item.CallID,
				Type:     toolTypeFunction,
				Function: FunctionCall{Name: ev.Item.Name},
			}}}))

		case "response.function_call_arguments.delta":
			idx, ok := toolIndex[ev.OutputIndex]
			if !ok {
				continue
			}
			writeSSEChunk(w, chunkOf(streamDelta{ToolCalls: []ToolCall{{
				Index:    idx,
				Function: FunctionCall{Arguments: ev.Delta},
			}}}))

		case "response.output_text.delta":
			writeSSEChunk(w, chunkOf(streamDelta{Content: ev.Delta}))

		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			writeSSEChunk(w, chunkOf(streamDelta{Reasoning: ev.Delta}))

		case "response.completed", "response.incomplete":
			finish := finishReasonStop
			switch {
			case len(toolIndex) > 0:
				finish = finishReasonToolCalls
			case ev.Response.IncompleteDetails != nil && ev.Response.IncompleteDetails.Reason == "max_output_tokens":
				finish = finishReasonLength
			}
			chunk := chunkOf(streamDelta{})
			chunk.Choices[0].FinishReason = &finish
			if u := ev.Response.Usage; u != nil {
				chunk.Usage = &TokenUsage{PromptTokens: u.InputTokens, CompletionTokens: u.OutputTokens, TotalTokens: u.TotalTokens}
			}
			writeSSEChunk(w, chunk)
			_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
			return

		case "response.failed", "error":
			msg := ev.Message
			if ev.Response.Error != nil {
				msg = ev.Response.Error.Message
			}
			slog.Warn("responses stream failed", "err", msg)
			return
		}
	}
	if err := scanner.Err(); err != nil {
		slog.Warn("error reading responses SSE stream", "err", err)
	}
}
