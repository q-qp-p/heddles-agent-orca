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

package routingbench

import (
	"encoding/json"
	"maps"
	"reflect"
	"testing"
)

// applyMergePatch is RFC 7386, for checking that a rollback restores the object.
func applyMergePatch(target, patch any) any {
	pm, ok := patch.(map[string]any)
	if !ok {
		return patch
	}
	tm, ok := target.(map[string]any)
	if !ok {
		tm = map[string]any{}
	}
	out := maps.Clone(tm)
	for k, v := range pm {
		if v == nil {
			delete(out, k)
			continue
		}
		out[k] = applyMergePatch(out[k], v)
	}
	return out
}

func mustJSON(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("bad JSON %q: %v", s, err)
	}
	return v
}

// Whatever a patch does to an object, applying the patch and then its inverse
// gives back the object that was there.
func TestInverseMergePatchRestoresTheObject(t *testing.T) {
	cases := map[string]struct{ current, patch string }{
		"adds a key":                  {`{"spec":{"a":1}}`, `{"spec":{"b":2}}`},
		"replaces a value":            {`{"spec":{"a":1}}`, `{"spec":{"a":2}}`},
		"adds a nested key":           {`{"spec":{"routes":{"x":"1"}}}`, `{"spec":{"routes":{"stage:chat":"weak"}}}`},
		"replaces an array whole":     {`{"spec":{"providers":[{"name":"a"},{"name":"b"}]}}`, `{"spec":{"providers":[{"name":"a"}]}}`},
		"deletes a key":               {`{"spec":{"a":1,"b":2}}`, `{"spec":{"a":null}}`},
		"deletes a key that is gone":  {`{"spec":{"b":2}}`, `{"spec":{"a":null}}`},
		"object over a scalar":        {`{"spec":{"a":1}}`, `{"spec":{"a":{"x":1}}}`},
		"scalar over an object":       {`{"spec":{"a":{"x":1,"y":2}}}`, `{"spec":{"a":3}}`},
		"adds an object":              {`{"spec":{}}`, `{"spec":{"switchyard":{"type":"stage_router","picker":"efficient_first"}}}`},
		"nested change beside others": {`{"spec":{"sw":{"classifier":{"base_threshold":0.5,"target":"judge"},"type":"composite"}}}`, `{"spec":{"sw":{"classifier":{"base_threshold":0.3}}}}`},
		"strategy switch":             {`{"spec":{"strategy":"rule-based","capabilityRouting":{"capable":"strong"}}}`, `{"spec":{"strategy":"switchyard","capabilityRouting":{"capable":"strong","efficient":"weak"},"switchyard":{"type":"stage_router"}}}`},
	}
	for name, tc := range cases {
		inv, err := InverseMergePatch([]byte(tc.current), []byte(tc.patch))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		applied := applyMergePatch(mustJSON(t, tc.current), mustJSON(t, tc.patch))
		restored := applyMergePatch(applied, mustJSON(t, string(inv)))
		if want := mustJSON(t, tc.current); !reflect.DeepEqual(restored, want) {
			t.Errorf("%s: patch %s then inverse %s gave %v, want %v", name, tc.patch, inv, restored, want)
		}
	}
}

func TestInverseMergePatchGolden(t *testing.T) {
	inv, err := InverseMergePatch(
		[]byte(`{"spec":{"capabilityRouting":{"capable":"strong"},"fallbackChain":["a"]}}`),
		[]byte(`{"spec":{"capabilityRouting":{"stage:chat":"weak"},"fallbackChain":[]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"spec":{"capabilityRouting":{"stage:chat":null},"fallbackChain":["a"]}}`; string(inv) != want {
		t.Errorf("inverse = %s, want %s", inv, want)
	}
}

func TestInverseMergePatchRejectsWhatIsNotAnObject(t *testing.T) {
	if _, err := InverseMergePatch([]byte(`[]`), []byte(`{}`)); err == nil {
		t.Error("a current value that is not an object was accepted")
	}
	if _, err := InverseMergePatch([]byte(`{}`), []byte(`not json`)); err == nil {
		t.Error("a patch that is not JSON was accepted")
	}
}

func TestCheckDowngrade(t *testing.T) {
	// The downgrade sent the work to the weak model, so the replays are the
	// capable model's answers against the weak model's.
	servedWeak := func(n, capableWins, weakWins int) Group {
		pairs := make([]Pair, 0, n)
		for i := range n {
			res := Equal
			switch {
			case i < capableWins:
				res = CapableBetter
			case i < capableWins+weakWins:
				res = EfficientBetter
			}
			pairs = append(pairs, Pair{Result: res, RoutedCapable: false, CapableCostUSD: 0.01, EfficientCostUSD: 0.001})
		}
		return Group{GroupKey: GroupKey{Agent: "a", Category: stageC, Capable: strong, Efficient: weak}, Pairs: pairs}
	}
	for name, tc := range map[string]struct {
		g    Group
		want string
	}{
		"as good as before":       {servedWeak(100, 1, 1), VerdictConfirmed},
		"clearly worse":           {servedWeak(100, 40, 2), VerdictRegressed},
		"too few pairs":           {servedWeak(12, 8, 0), VerdictInconclusive},
		"worse but not by margin": {servedWeak(100, 6, 3), VerdictConfirmed},
	} {
		if got, _ := CheckDowngrade(tc.g, Options{}); got != tc.want {
			t.Errorf("%s: verdict = %s, want %s", name, got, tc.want)
		}
	}
	unreliable := servedWeak(100, 40, 2)
	for i := range unreliable.Pairs {
		unreliable.Pairs[i].Inconsistent = true
	}
	if got, _ := CheckDowngrade(unreliable, Options{}); got != VerdictInconclusive {
		t.Errorf("a judge that contradicted itself on every pair gave %s, want Inconclusive", got)
	}
}
