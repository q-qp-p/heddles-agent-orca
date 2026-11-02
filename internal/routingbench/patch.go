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
	"fmt"
	"slices"
	"strings"
)

// ProviderWeight mirrors a ModelSelector provider entry.
type ProviderWeight struct {
	Name        string `json:"name"`
	Weight      int    `json:"weight,omitempty"`
	RoutingHint string `json:"routingHint,omitempty"`
}

// Selector is the part of a ModelSelector a finding reads and a patch writes.
type Selector struct {
	Name              string
	Strategy          string
	Providers         []ProviderWeight
	FallbackChain     []string
	CapabilityRouting map[string]string
}

// strategySwitchyard is the strategy whose routing the stage keys do not steer.
const strategySwitchyard = "switchyard"

// stageKeyPrefix starts the capabilityRouting keys that route one task stage.
const stageKeyPrefix = "stage:"

// ActionKind is what a finding proposes to change.
type ActionKind string

const (
	// ActionRouteCategory sends one task category to one provider, through the
	// capabilityRouting key stage:<name>.
	ActionRouteCategory ActionKind = "RouteCategory"
	// ActionRemoveProvider takes a dead provider out of the selector.
	ActionRemoveProvider ActionKind = "RemoveProvider"
)

// Action is a change to a ModelSelector.
type Action struct {
	Kind     ActionKind
	Selector string
	// Category and Provider say where to route (RouteCategory) or what to
	// remove (RemoveProvider uses Provider only).
	Category string
	Provider string
}

// Patch is an RFC 7386 merge patch against a ModelSelector, and the patch that
// undoes it from the selector as it is now.
type Patch struct {
	Forward  json.RawMessage
	Rollback json.RawMessage
}

// NoLeverError says why a finding cannot be turned into a patch. The finding is
// still worth reporting; it is just left for a person to act on.
type NoLeverError struct{ Reason string }

func (e *NoLeverError) Error() string { return e.Reason }

// BuildPatch turns an action into a merge patch against sel, with its rollback.
// Merge patches replace arrays whole, so a change to providers or the fallback
// chain carries the full new list. A selector that cannot take the change
// (a switchyard strategy has no stage keys) yields a *NoLeverError.
func BuildPatch(sel Selector, a Action) (Patch, error) {
	switch a.Kind {
	case ActionRouteCategory:
		return routeCategoryPatch(sel, a)
	case ActionRemoveProvider:
		return removeProviderPatch(sel, a)
	}
	return Patch{}, fmt.Errorf("unknown action kind %q", a.Kind)
}

func routeCategoryPatch(sel Selector, a Action) (Patch, error) {
	switch {
	case !strings.HasPrefix(a.Category, stageKeyPrefix) || len(a.Category) == len(stageKeyPrefix):
		return Patch{}, &NoLeverError{fmt.Sprintf("%q is not a task stage, and only stages can be routed", a.Category)}
	case sel.Strategy == strategySwitchyard:
		return Patch{}, &NoLeverError{"a switchyard selector decides by its algorithm and ignores stage routes; tune its algorithm instead"}
	case !slices.ContainsFunc(sel.Providers, func(p ProviderWeight) bool { return p.Name == a.Provider }):
		return Patch{}, &NoLeverError{fmt.Sprintf("provider %q is not one of the selector's providers", a.Provider)}
	}
	forward := map[string]any{"spec": map[string]any{"capabilityRouting": map[string]any{a.Category: a.Provider}}}
	// A merge patch deletes a key by setting it to null.
	var prev any
	if old, ok := sel.CapabilityRouting[a.Category]; ok {
		prev = old
	}
	rollback := map[string]any{"spec": map[string]any{"capabilityRouting": map[string]any{a.Category: prev}}}
	return marshalPatch(forward, rollback)
}

func removeProviderPatch(sel Selector, a Action) (Patch, error) {
	if !slices.ContainsFunc(sel.Providers, func(p ProviderWeight) bool { return p.Name == a.Provider }) {
		return Patch{}, &NoLeverError{fmt.Sprintf("provider %q is not one of the selector's providers", a.Provider)}
	}
	var keep []ProviderWeight
	for _, p := range sel.Providers {
		if p.Name != a.Provider {
			keep = append(keep, p)
		}
	}
	if !slices.ContainsFunc(keep, func(p ProviderWeight) bool { return p.Weight > 0 }) {
		return Patch{}, &NoLeverError{fmt.Sprintf("removing %q would leave the selector with nothing to route to", a.Provider)}
	}
	spec := map[string]any{"providers": keep}
	prev := map[string]any{"providers": sel.Providers}
	if slices.Contains(sel.FallbackChain, a.Provider) {
		spec["fallbackChain"] = slices.DeleteFunc(slices.Clone(sel.FallbackChain), func(n string) bool { return n == a.Provider })
		prev["fallbackChain"] = sel.FallbackChain
	}
	// A capability route that names the provider would now point nowhere.
	for key, to := range sel.CapabilityRouting {
		if to == a.Provider {
			return Patch{}, &NoLeverError{fmt.Sprintf("capabilityRouting %q still sends traffic to %q; change that first", key, a.Provider)}
		}
	}
	return marshalPatch(map[string]any{"spec": spec}, map[string]any{"spec": prev})
}

func marshalPatch(forward, rollback any) (Patch, error) {
	f, err := json.Marshal(forward)
	if err != nil {
		return Patch{}, err
	}
	r, err := json.Marshal(rollback)
	if err != nil {
		return Patch{}, err
	}
	return Patch{Forward: f, Rollback: r}, nil
}
