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
	"math"
)

// InverseMergePatch returns the RFC 7386 merge patch that undoes patch when
// applied to current: it puts back every value patch would overwrite, and
// deletes (null) every key patch would add. current and patch are JSON objects.
//
// A recommendation's patch is made against the selector as it was when the
// finding was found; the rollback has to restore the selector as it is when the
// patch is applied, so it is derived again at that moment.
func InverseMergePatch(current, patch []byte) ([]byte, error) {
	var cur, p map[string]any
	if err := json.Unmarshal(current, &cur); err != nil {
		return nil, fmt.Errorf("current object is not a JSON object: %w", err)
	}
	if err := json.Unmarshal(patch, &p); err != nil {
		return nil, fmt.Errorf("patch is not a JSON object: %w", err)
	}
	return json.Marshal(inverse(cur, p))
}

func inverse(cur, patch map[string]any) map[string]any {
	out := make(map[string]any, len(patch))
	for k, pv := range patch {
		cv, present := cur[k]
		pm, patchIsObject := pv.(map[string]any)
		switch {
		case pv == nil: // the patch deletes k; restoring means putting it back, if it was there
			if present {
				out[k] = cv
			}
		case patchIsObject && present:
			if cm, ok := cv.(map[string]any); ok {
				out[k] = inverse(cm, pm) // merged into an existing object: undo within it
			} else {
				out[k] = cv // the patch replaces a non-object with an object
			}
		case present:
			out[k] = cv
		default:
			out[k] = nil // the patch added k
		}
	}
	return out
}

// Verdicts of the check made one window after a downgrade was applied.
const (
	VerdictConfirmed    = "Confirmed"
	VerdictRegressed    = "Regressed"
	VerdictInconclusive = "Inconclusive"
)

// CheckDowngrade judges a change that sent a kind of work to the cheaper model,
// from the pairs replayed since it was applied. It is the change that can
// quietly make answers worse, so it is the one checked afterwards: Regressed when
// the capable model is clearly better again by more than the non-inferiority
// margin, Inconclusive when there are too few pairs, Confirmed otherwise.
func CheckDowngrade(g Group, opts Options) (verdict string, w Worth) {
	o := opts.withDefaults()
	w = Evaluate(g, o)
	switch {
	case w.N < o.MinSamples || math.IsNaN(w.NetWinCI.Lo):
		return VerdictInconclusive, w
	case w.InconsistentRate > o.JudgeUnreliableRate:
		return VerdictInconclusive, w
	case w.NetWinCI.Lo > o.NonInferiorityMargin:
		return VerdictRegressed, w
	}
	return VerdictConfirmed, w
}
