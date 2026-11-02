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
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	sy "github.com/NVIDIA-NeMo/Switchyard/bindings/go"
)

// allowedTypes are the algorithms the router can serve: they choose a model and
// leave answering to the router. Their judge, if any, is an ordinary model call.
var allowedTypes = []string{"auto", "stage_router", "llm_classifier", "composite", "plan_execute", "random", "passthrough", "subagents"}

// rejectedTypes are Switchyard algorithms the router cannot serve yet, and why.
var rejectedTypes = map[string]string{
	"advisor":        "it answers on the model's behalf, which the router cannot relay yet",
	"vgr":            "it answers on the model's behalf, which the router cannot relay yet",
	"noop":           "it answers on the model's behalf, which the router cannot relay yet",
	"prefill_router": "it needs a PyTorch classifier checkpoint",
}

// ValidateAlgorithm reports why spec cannot be served, or nil. targets are the
// provider names the algorithm may refer to. It checks the algorithm type (and
// any nested one) against the supported set, that every target the spec names is
// a known provider, and finally that Switchyard itself accepts the configuration.
func ValidateAlgorithm(spec json.RawMessage, targets []string) error {
	return checkAlgorithm(spec, targets)
}

func checkAlgorithm(spec json.RawMessage, targets []string) error {
	var root map[string]any
	if err := json.Unmarshal(spec, &root); err != nil || root == nil {
		return errors.New("the switchyard algorithm must be a JSON object")
	}
	typ, _ := root["type"].(string)
	if typ == "" {
		return errors.New(`the switchyard algorithm needs a "type"`)
	}
	if why, bad := rejectedTypes[typ]; bad {
		return fmt.Errorf("%w: algorithm %q: %s", ErrUnsupported, typ, why)
	}
	if !slices.Contains(allowedTypes, typ) {
		return fmt.Errorf("unknown switchyard algorithm %q; supported: %s", typ, strings.Join(allowedTypes, ", "))
	}
	var problems []error
	walk(root, "algorithm", targets, &problems)
	if len(problems) > 0 {
		return errors.Join(problems...)
	}
	router, err := sy.New(engineConfig(spec, targets))
	if err != nil {
		return fmt.Errorf("switchyard rejected the algorithm: %w", err)
	}
	router.Close()
	return nil
}

// walk visits every object of the spec, collecting references to unsupported
// algorithm types and to targets that are not providers.
func walk(node any, path string, targets []string, problems *[]error) {
	switch v := node.(type) {
	case map[string]any:
		if t, ok := v["type"].(string); ok && path != "algorithm" {
			if why, bad := rejectedTypes[t]; bad {
				*problems = append(*problems, fmt.Errorf("%w: %s: algorithm %q: %s", ErrUnsupported, path, t, why))
			}
		}
		for _, k := range sortedKeys(v) {
			child := path + "." + k
			switch val := v[k].(type) {
			case string:
				if isTargetKey(k) {
					checkTarget(val, child, targets, problems)
				}
			case []any:
				if k == "targets" {
					for i, item := range val {
						if s, ok := item.(string); ok {
							checkTarget(s, fmt.Sprintf("%s[%d]", child, i), targets, problems)
						}
					}
				}
				walk(val, child, targets, problems)
			case map[string]any:
				if k == "response_schema" {
					continue // a JSON schema: its keys are not part of the algorithm
				}
				walk(val, child, targets, problems)
			}
		}
	case []any:
		for i, item := range v {
			walk(item, fmt.Sprintf("%s[%d]", path, i), targets, problems)
		}
	}
}

func isTargetKey(k string) bool { return k == "target" || strings.HasSuffix(k, "_target") }

func checkTarget(name, path string, targets []string, problems *[]error) {
	if !slices.Contains(targets, name) {
		*problems = append(*problems, fmt.Errorf("%s names %q, which is not a provider of this ModelSelector", path, name))
	}
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// RequiredGroups lists the model groups an algorithm cannot run without, which a
// ModelSelector supplies through capabilityRouting roles. Switchyard only
// discovers a missing one when it routes, and the router then falls back to the
// rule router for every request; this lets a controller say so up front.
//
// It is deliberately conservative: only groups the upstream documentation lists
// as required are returned, so an algorithm it does not cover reports none and
// leaves Switchyard to judge.
func RequiredGroups(spec json.RawMessage) []string {
	var t struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(spec, &t) != nil {
		return nil
	}
	switch t.Type {
	case "composite":
		return []string{"capable", "efficient", "judge"}
	case "stage_router", "auto", "plan_execute":
		return []string{"capable", "efficient"}
	}
	return nil
}

// ToolSemantics are tool names grouped by what using them says about the agent's
// progress: Observe is investigation, Mutate is production.
type ToolSemantics struct {
	Observe []string
	Mutate  []string
}

// FillToolSemantics adds tool names to the spec's tool_semantics, for the
// algorithms that read it (stage_router and plan_execute at the top level,
// composite under "stage"). A tool the spec already lists in any category is left
// where the user put it: Switchyard rejects a name in two categories, and the
// user's classification wins. The spec is returned unchanged when nothing is added.
func FillToolSemantics(spec json.RawMessage, sem ToolSemantics) (json.RawMessage, error) {
	var root map[string]any
	if err := json.Unmarshal(spec, &root); err != nil {
		return nil, err
	}
	target := root
	typ, _ := root["type"].(string)
	categories := []string{"observe", "mutate"}
	switch typ {
	case "stage_router":
	case "plan_execute":
		categories = []string{"mutate"} // handoff happens on the first mutating tool
	case "composite":
		stage, ok := root["stage"].(map[string]any)
		if !ok {
			return spec, nil
		}
		target = stage
	default:
		return spec, nil
	}
	existing, _ := target["tool_semantics"].(map[string]any)
	if existing == nil {
		existing = map[string]any{}
	}
	used := map[string]bool{}
	for _, v := range existing {
		if names, ok := v.([]any); ok {
			for _, n := range names {
				if s, ok := n.(string); ok {
					used[s] = true
				}
			}
		}
	}
	added := false
	for _, cat := range categories {
		names := sem.Observe
		if cat == "mutate" {
			names = sem.Mutate
		}
		current, _ := existing[cat].([]any)
		for _, n := range slices.Sorted(slices.Values(names)) {
			if n == "" || used[n] {
				continue
			}
			used[n] = true
			current = append(current, n)
			added = true
		}
		if len(current) > 0 {
			existing[cat] = current
		}
	}
	if !added {
		return spec, nil
	}
	target["tool_semantics"] = existing
	return json.Marshal(root)
}
