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

package switchyard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	sy "github.com/NVIDIA-NeMo/Switchyard/bindings/go"
)

// Wire formats and work kinds of the binding.
const (
	formatOpenAIChat = "openai_chat"
	workCallModel    = "call_model"
)

// DefaultDecisionTimeout bounds one routing decision, judge calls included.
const DefaultDecisionTimeout = 10 * time.Second

// ErrUnsupported marks an algorithm, or a request an algorithm makes of its host,
// that the model-router does not provide. Callers fall back to their own selector.
var ErrUnsupported = errors.New("unsupported by the model-router")

// ModelCaller sends one OpenAI chat completion request to the named provider and
// returns its OpenAI chat completion response. The router serves it with its own
// provider forwarders, so every provider's wire format is spoken correctly and
// the call is accounted to the run.
type ModelCaller func(ctx context.Context, provider string, openAIChat []byte) ([]byte, error)

// Config describes one routing algorithm and the models it may choose among.
type Config struct {
	// Algorithm is the Switchyard algorithm spec ({"type": "stage_router", ...}).
	// Targets in it are provider names.
	Algorithm json.RawMessage
	// Targets lists every provider the algorithm may name, judges included.
	Targets []string
	// Groups are the model groups of a run: any (answer targets), capable,
	// efficient, judge. Values are provider names.
	Groups map[string][]string
	// SubagentGroups are the groups used for delegated sub-agent work.
	SubagentGroups map[string][]string
	// DecisionTimeout bounds one decision; DefaultDecisionTimeout when zero.
	DecisionTimeout time.Duration
}

// Metadata identifies the conversation a request belongs to. Switchyard keeps
// per-session state (holds, escalation latches) keyed by it.
type Metadata struct {
	SessionID  string
	AgentID    string
	IsSubagent bool
}

// Result is a routing decision.
type Result struct {
	// Selected lists provider names, best first.
	Selected []string
	// Metadata is the algorithm's own decision evidence, kept raw.
	Metadata json.RawMessage
}

// Engine runs one Switchyard algorithm. It is safe for concurrent use.
type Engine struct {
	router  *sy.Router
	cfg     Config
	call    ModelCaller
	timeout time.Duration
}

// engineConfig renders the JSON Switchyard's constructor takes. Targets map each
// provider name to itself: the algorithm spec and the model groups then use the
// same names.
func engineConfig(algorithm json.RawMessage, targets []string) json.RawMessage {
	aliases := make(map[string]string, len(targets))
	for _, t := range targets {
		aliases[t] = t
	}
	b, _ := json.Marshal(map[string]any{"algorithm": algorithm, "targets": aliases})
	return b
}

// New builds an Engine. The algorithm is validated first (see ValidateAlgorithm).
func New(cfg Config, call ModelCaller) (*Engine, error) {
	if call == nil {
		return nil, errors.New("switchyard: a ModelCaller is required")
	}
	if err := checkAlgorithm(cfg.Algorithm, cfg.Targets); err != nil {
		return nil, err
	}
	router, err := sy.New(engineConfig(cfg.Algorithm, cfg.Targets))
	if err != nil {
		return nil, fmt.Errorf("switchyard rejected the algorithm: %w", err)
	}
	timeout := cfg.DecisionTimeout
	if timeout <= 0 {
		timeout = DefaultDecisionTimeout
	}
	return &Engine{router: router, cfg: cfg, call: call, timeout: timeout}, nil
}

// Close releases the engine. Calling it more than once is safe.
func (e *Engine) Close() {
	if e != nil {
		e.router.Close()
	}
}

// Route decides which provider should serve the OpenAI chat request openAIChat.
// It returns an error when the algorithm fails, times out, asks for something the
// router does not provide, or produces an answer instead of a choice; the caller
// then routes another way.
func (e *Engine) Route(ctx context.Context, openAIChat []byte, md Metadata) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	request, err := sy.DecodeRequest(formatOpenAIChat, openAIChat)
	if err != nil {
		return Result{}, fmt.Errorf("decoding the request: %w", err)
	}
	outcome, err := e.router.Route(ctx, sy.Input{
		Request:        request,
		Models:         e.cfg.Groups,
		SubagentModels: e.cfg.SubagentGroups,
		Metadata: &sy.RequestMetadata{
			SessionID:  md.SessionID,
			AgentID:    md.AgentID,
			IsSubagent: md.IsSubagent,
		},
	}, e.hostCall)
	if err != nil {
		return Result{}, err
	}
	if outcome.Response != nil {
		// The algorithm answered the user itself. The router streams provider
		// answers to the agent; it cannot relay one produced here.
		return Result{}, fmt.Errorf("%w: the algorithm produced a response instead of a choice", ErrUnsupported)
	}
	if len(outcome.SelectedModelIDs) == 0 {
		return Result{}, errors.New("the algorithm selected no model")
	}
	return Result{Selected: outcome.SelectedModelIDs, Metadata: outcome.Metadata}, nil
}

// hostCall serves the model calls an algorithm makes while deciding: today, its
// LLM judge. Typed decision services are not provided, and abort routing.
func (e *Engine) hostCall(ctx context.Context, w sy.Work) (sy.Reply, error) {
	if w.Type != workCallModel {
		return sy.Reply{}, sy.Abort(fmt.Errorf("%w: %s", ErrUnsupported, w.Type))
	}
	var last error
	for _, model := range w.Models {
		reply, err := e.callOne(ctx, model, w.Request)
		if err == nil {
			return reply, nil
		}
		last = err
		if ctx.Err() != nil {
			break
		}
	}
	if last == nil {
		last = errors.New("no model to call")
	}
	return sy.Reply{}, last
}

func (e *Engine) callOne(ctx context.Context, model string, request json.RawMessage) (sy.Reply, error) {
	wire, err := sy.EncodeRequest(formatOpenAIChat, request, model)
	if err != nil {
		return sy.Reply{}, fmt.Errorf("encoding the request for %s: %w", model, err)
	}
	body, err := e.call(ctx, model, wire)
	if err != nil {
		return sy.Reply{}, fmt.Errorf("calling %s: %w", model, err)
	}
	response, err := sy.DecodeResponse(formatOpenAIChat, body)
	if err != nil {
		return sy.Reply{}, fmt.Errorf("decoding the response of %s: %w", model, err)
	}
	return sy.Reply{Response: response, ServedModel: model}, nil
}
