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

package controller

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	syengine "github.com/heddles/agent-orca/internal/router/switchyard"
)

// The shipped Switchyard samples must be selectors the reconciler would call
// Ready, validated by Switchyard itself: a sample that stops being valid after a
// Switchyard bump fails here, not in a user's cluster.
func TestSwitchyardSamplesAreReady(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "config", "samples", "switchyard-*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no switchyard samples found (err %v)", err)
	}
	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			found := map[string]*agentorcav1alpha1.ModelProvider{}
			var selectors []*agentorcav1alpha1.ModelSelector
			for doc := range strings.SplitSeq(string(raw), "\n---\n") {
				var meta metav1.TypeMeta
				if err := yaml.Unmarshal([]byte(doc), &meta); err != nil {
					t.Fatalf("%s: %v", file, err)
				}
				switch meta.Kind {
				case "ModelProvider":
					var mp agentorcav1alpha1.ModelProvider
					if err := yaml.UnmarshalStrict([]byte(doc), &mp); err != nil {
						t.Fatalf("ModelProvider: %v", err)
					}
					mp.Status.Ready = true
					found[mp.Name] = &mp
				case "ModelSelector":
					var sel agentorcav1alpha1.ModelSelector
					if err := yaml.UnmarshalStrict([]byte(doc), &sel); err != nil {
						t.Fatalf("ModelSelector: %v", err)
					}
					sel.Generation = 1
					selectors = append(selectors, &sel)
				}
			}
			if len(selectors) != 1 {
				t.Fatalf("a sample holds %d ModelSelectors, want 1", len(selectors))
			}
			sel := selectors[0]
			if sel.Spec.Strategy != "switchyard" || sel.Spec.Switchyard == nil {
				t.Fatalf("not a switchyard selector: %+v", sel.Spec)
			}

			var algo algorithmOutcome
			if err := syengine.ValidateAlgorithm(sel.Spec.Switchyard.Raw, providerNames(sel)); err != nil {
				t.Fatalf("Switchyard rejects the sample: %v", err)
			}
			algo.reason = reasonAlgorithmValid

			a := assessSelector(sel, found, algo)
			for _, c := range a.conditions {
				if c.Status != metav1.ConditionTrue {
					t.Errorf("%s = %s (%s): %s", c.Type, c.Status, c.Reason, c.Message)
				}
			}
		})
	}
}
