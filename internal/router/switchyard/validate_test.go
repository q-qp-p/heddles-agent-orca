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
	"reflect"
	"strings"
	"testing"
)

func TestValidateAlgorithm_AcceptsTheSupportedAlgorithms(t *testing.T) {
	readGuest(t)
	for name, spec := range map[string]string{
		algStageRouter: stageSpec,
		"composite":    compositeSpec,
		"random":       `{"type":"random","targets":["strong","weak"]}`,
		"passthrough":  `{"type":"passthrough","target":"strong"}`,
		"auto":         `{"type":"auto","capable_target":"strong","efficient_target":"weak"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateAlgorithm(json.RawMessage(spec), allTargets); err != nil {
				t.Errorf("rejected %s: %v", spec, err)
			}
		})
	}
}

func TestValidateAlgorithm_RejectsAlgorithmsThatAnswerOrNeedExtras(t *testing.T) {
	for _, typ := range []string{"advisor", "vgr", "noop", "prefill_router"} {
		t.Run(typ, func(t *testing.T) {
			err := ValidateAlgorithm(json.RawMessage(`{"type":"`+typ+`"}`), allTargets)
			if !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), typ) {
				t.Errorf("err = %v, want ErrUnsupported naming %q", err, typ)
			}
		})
	}
}

func TestValidateAlgorithm_RejectsAnUnsupportedAlgorithmNestedInASupportedOne(t *testing.T) {
	spec := `{"type":"stage_router","picker":"efficient_first","capable_target":"strong","efficient_target":"weak",` +
		`"confidence_threshold":0.5,"subagents":{"default":{"type":"advisor"}}}`
	err := ValidateAlgorithm(json.RawMessage(spec), allTargets)
	if !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "subagents") {
		t.Errorf("err = %v, want ErrUnsupported pointing at the nested algorithm", err)
	}
}

func TestValidateAlgorithm_BadShapes(t *testing.T) {
	for name, tc := range map[string]struct{ spec, want string }{
		"not an object": {`[1]`, "JSON object"},
		"not json":      {`nope`, "JSON object"},
		"no type":       {`{"picker":"x"}`, `needs a "type"`},
		"unknown type":  {`{"type":"telepathy"}`, `unknown switchyard algorithm "telepathy"`},
	} {
		t.Run(name, func(t *testing.T) {
			err := ValidateAlgorithm(json.RawMessage(tc.spec), allTargets)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestValidateAlgorithm_NamesTheTargetsThatAreNotProviders(t *testing.T) {
	spec := `{"type":"stage_router","picker":"efficient_first","capable_target":"strnog","efficient_target":"weak","confidence_threshold":0.5}`
	err := ValidateAlgorithm(json.RawMessage(spec), allTargets)
	if err == nil || !strings.Contains(err.Error(), `algorithm.capable_target names "strnog"`) {
		t.Errorf("err = %v, want the typo named with its path", err)
	}

	random := ValidateAlgorithm(json.RawMessage(`{"type":"random","targets":["strong","missing"]}`), allTargets)
	if random == nil || !strings.Contains(random.Error(), `algorithm.targets[1] names "missing"`) {
		t.Errorf("err = %v, want the missing random target named", random)
	}

	judge := ValidateAlgorithm(json.RawMessage(strings.Replace(compositeSpec, `"target":"judge"`, `"target":"jduge"`, 1)), allTargets)
	if judge == nil || !strings.Contains(judge.Error(), `names "jduge"`) {
		t.Errorf("err = %v, want the judge typo named", judge)
	}
}

func TestValidateAlgorithm_ReportsEveryProblemAtOnce(t *testing.T) {
	spec := `{"type":"stage_router","picker":"efficient_first","capable_target":"a","efficient_target":"b","confidence_threshold":0.5}`
	err := ValidateAlgorithm(json.RawMessage(spec), allTargets)
	if err == nil || !strings.Contains(err.Error(), `"a"`) || !strings.Contains(err.Error(), `"b"`) {
		t.Errorf("err = %v, want both bad targets reported", err)
	}
}

func TestValidateAlgorithm_SwitchyardsOwnValidationRuns(t *testing.T) {
	readGuest(t)
	// Every target is a provider and the type is supported, but the configuration
	// is incomplete: only Switchyard knows that the picker is required.
	err := ValidateAlgorithm(json.RawMessage(`{"type":"stage_router","capable_target":"strong","efficient_target":"weak"}`), allTargets)
	if err == nil || !strings.Contains(err.Error(), "switchyard rejected the algorithm") {
		t.Errorf("err = %v, want Switchyard's own rejection surfaced", err)
	}
}

func TestValidateAlgorithm_AResponseSchemaIsNotPartOfTheAlgorithm(t *testing.T) {
	readGuest(t)
	// A JSON schema legitimately has "type" and "target" properties of its own.
	spec := `{"type":"llm_classifier","mode":"custom","classifier_target":"judge",` +
		`"response_schema":{"type":"object","properties":{"target":{"type":"string"}}}}`
	err := ValidateAlgorithm(json.RawMessage(spec), allTargets)
	if err != nil && strings.Contains(err.Error(), "names") {
		t.Errorf("schema keys were treated as targets: %v", err)
	}
}

func decode(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("not JSON: %s", raw)
	}
	return m
}

func names(v any) []string {
	var out []string
	if list, ok := v.([]any); ok {
		for _, n := range list {
			out = append(out, n.(string))
		}
	}
	return out
}

func TestFillToolSemantics_StageRouter(t *testing.T) {
	sem := ToolSemantics{Observe: []string{"search_kb", "get_customer"}, Mutate: []string{"update_inventory", "send_payment"}}
	got, err := FillToolSemantics(json.RawMessage(stageSpec), sem)
	if err != nil {
		t.Fatal(err)
	}
	ts := decode(t, got)["tool_semantics"].(map[string]any)
	if !reflect.DeepEqual(names(ts["observe"]), []string{"get_customer", "search_kb"}) ||
		!reflect.DeepEqual(names(ts["mutate"]), []string{"send_payment", "update_inventory"}) {
		t.Errorf("tool_semantics = %v, want both categories filled, sorted", ts)
	}
}

func TestFillToolSemantics_UsersClassificationWins(t *testing.T) {
	spec := `{"type":"stage_router","picker":"efficient_first",` + stageFields + `,` +
		`"tool_semantics":{"mutate":["search_kb"],"new":["start_chat"]}}`
	got, err := FillToolSemantics(json.RawMessage(spec), ToolSemantics{
		Observe: []string{"search_kb", "get_customer", "start_chat"}, Mutate: []string{"send_payment"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := decode(t, got)["tool_semantics"].(map[string]any)
	// search_kb stays where the user put it (mutate); adding it to observe too would
	// make Switchyard reject the route for listing a tool in two categories.
	if reflect.DeepEqual(names(ts["observe"]), []string{"search_kb"}) || len(names(ts["observe"])) != 1 || names(ts["observe"])[0] != "get_customer" {
		t.Errorf("observe = %v, want only get_customer (search_kb and start_chat are the user's)", ts["observe"])
	}
	if got := names(ts["mutate"]); !reflect.DeepEqual(got, []string{"search_kb", "send_payment"}) {
		t.Errorf("mutate = %v, want the user's search_kb kept and send_payment added", got)
	}
	if got := names(ts["new"]); !reflect.DeepEqual(got, []string{"start_chat"}) {
		t.Errorf("new = %v, want the user's list untouched", got)
	}
}

func TestFillToolSemantics_ByAlgorithm(t *testing.T) {
	sem := ToolSemantics{Observe: []string{"read_x"}, Mutate: []string{"write_x"}}

	composite, _ := FillToolSemantics(json.RawMessage(compositeSpec), sem)
	stage := decode(t, composite)["stage"].(map[string]any)
	if _, ok := stage["tool_semantics"]; !ok {
		t.Errorf("composite: tool_semantics is stage-local, got %s", composite)
	}

	planExecute, _ := FillToolSemantics(json.RawMessage(`{"type":"plan_execute","capable_target":"strong","efficient_target":"weak"}`), sem)
	pe := decode(t, planExecute)["tool_semantics"].(map[string]any)
	if _, hasObserve := pe["observe"]; hasObserve || !reflect.DeepEqual(names(pe["mutate"]), []string{"write_x"}) {
		t.Errorf("plan_execute: tool_semantics = %v, want only mutate (handoff happens on the first mutating tool)", pe)
	}
}

func TestFillToolSemantics_LeavesOthersUnchanged(t *testing.T) {
	sem := ToolSemantics{Observe: []string{"a"}, Mutate: []string{"b"}}
	for name, spec := range map[string]string{
		"random":             `{"type":"random","targets":["strong","weak"]}`,
		"nothing to add":     stageSpec,
		"composite no stage": `{"type":"composite","classifier":{"target":"judge"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			s := sem
			if name == "nothing to add" {
				s = ToolSemantics{}
			}
			got, err := FillToolSemantics(json.RawMessage(spec), s)
			if err != nil || string(got) != spec {
				t.Errorf("got %s, %v; want the spec byte for byte", got, err)
			}
		})
	}
}

func TestFillToolSemantics_RejectsNonJSON(t *testing.T) {
	if _, err := FillToolSemantics(json.RawMessage(`nope`), ToolSemantics{}); err == nil {
		t.Error("accepted a spec that is not JSON")
	}
}
