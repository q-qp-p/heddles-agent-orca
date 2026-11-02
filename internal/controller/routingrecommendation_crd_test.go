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
	"context"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

// The recommendation's spec is a proposal the operator wrote, and what an
// approver looks at is what gets applied. These run against a real API server
// because the rules are CEL in the CRD, which a fake client never evaluates.
var _ = Describe("RoutingRecommendation CRD", func() {
	ctx := context.Background()
	const ns = "default"

	rec := func(name string, mutate func(*agentorcav1alpha1.RoutingRecommendationSpec)) *agentorcav1alpha1.RoutingRecommendation {
		r := &agentorcav1alpha1.RoutingRecommendation{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: agentorcav1alpha1.RoutingRecommendationSpec{
				SelectorRef: "adaptive", Detector: agentorcav1alpha1.DetectorOverspend, Agent: benchAgent, Category: "stage:chat",
				Summary: "weak answers as well", BaseGeneration: 3,
				Patch: `{"spec":{"capabilityRouting":{"stage:chat":"weak"}}}`,
			},
		}
		if mutate != nil {
			mutate(&r.Spec)
		}
		return r
	}
	update := func(name string, mutate func(*agentorcav1alpha1.RoutingRecommendationSpec)) error {
		var got agentorcav1alpha1.RoutingRecommendation
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &got)).To(Succeed())
		mutate(&got.Spec)
		return k8sClient.Update(ctx, &got)
	}
	cleanup := func(name string) {
		_ = k8sClient.Delete(ctx, &agentorcav1alpha1.RoutingRecommendation{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}})
	}

	It("defaults the action to none", func() {
		defer cleanup("rr-default")
		Expect(k8sClient.Create(ctx, rec("rr-default", nil))).To(Succeed())
		var got agentorcav1alpha1.RoutingRecommendation
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "rr-default"}, &got)).To(Succeed())
		Expect(got.Spec.Action).To(Equal(agentorcav1alpha1.RecommendationActionNone))
	})

	It("lets a person change the action", func() {
		defer cleanup("rr-action")
		Expect(k8sClient.Create(ctx, rec("rr-action", nil))).To(Succeed())
		Expect(update("rr-action", func(s *agentorcav1alpha1.RoutingRecommendationSpec) {
			s.Action = agentorcav1alpha1.RecommendationActionRequestApply
		})).To(Succeed())
	})

	It("rejects an action it does not know", func() {
		defer cleanup("rr-badaction")
		err := k8sClient.Create(ctx, rec("rr-badaction", func(s *agentorcav1alpha1.RoutingRecommendationSpec) { s.Action = "applyNow" }))
		Expect(err).To(HaveOccurred())
	})

	It("rejects a detector it does not know", func() {
		defer cleanup("rr-baddetector")
		err := k8sClient.Create(ctx, rec("rr-baddetector", func(s *agentorcav1alpha1.RoutingRecommendationSpec) { s.Detector = "vibes" }))
		Expect(err).To(HaveOccurred())
	})

	DescribeTable("cannot change what the operator proposed",
		func(field string, change func(*agentorcav1alpha1.RoutingRecommendationSpec)) {
			name := "rr-immutable-" + strings.ToLower(field)
			defer cleanup(name)
			Expect(k8sClient.Create(ctx, rec(name, func(s *agentorcav1alpha1.RoutingRecommendationSpec) {
				s.BenchmarkRef, s.RollbackPatch, s.Reason, s.Caveat = evalBench, `{"spec":{}}`, "why", "careful"
			}))).To(Succeed())
			err := update(name, change)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(field + " is immutable"))
		},
		Entry("selectorRef", "selectorRef", func(s *agentorcav1alpha1.RoutingRecommendationSpec) { s.SelectorRef = evalOther }),
		Entry("detector", "detector", func(s *agentorcav1alpha1.RoutingRecommendationSpec) { s.Detector = agentorcav1alpha1.DetectorThreshold }),
		Entry("summary", "summary", func(s *agentorcav1alpha1.RoutingRecommendationSpec) { s.Summary = "trust me" }),
		Entry("baseGeneration", "baseGeneration", func(s *agentorcav1alpha1.RoutingRecommendationSpec) { s.BaseGeneration = 99 }),
		Entry("benchmarkRef", "benchmarkRef", func(s *agentorcav1alpha1.RoutingRecommendationSpec) { s.BenchmarkRef = evalOther }),
		Entry("agent", "agent", func(s *agentorcav1alpha1.RoutingRecommendationSpec) { s.Agent = "writer" }),
		Entry("category", "category", func(s *agentorcav1alpha1.RoutingRecommendationSpec) { s.Category = "stage:code" }),
		Entry("patch", "patch", func(s *agentorcav1alpha1.RoutingRecommendationSpec) {
			s.Patch = `{"spec":{"providers":[]}}`
		}),
		Entry("rollbackPatch", "rollbackPatch", func(s *agentorcav1alpha1.RoutingRecommendationSpec) { s.RollbackPatch = `{}` }),
		Entry("reason", "reason", func(s *agentorcav1alpha1.RoutingRecommendationSpec) { s.Reason = "changed" }),
		Entry("caveat", "caveat", func(s *agentorcav1alpha1.RoutingRecommendationSpec) { s.Caveat = "changed" }),
	)

	// A report-only recommendation has no patch. Adding one later would let a
	// person turn a finding the operator could not act on into one it applies.
	DescribeTable("cannot gain a field it was created without",
		func(field string, add func(*agentorcav1alpha1.RoutingRecommendationSpec)) {
			name := "rr-add-" + strings.ToLower(field)
			defer cleanup(name)
			Expect(k8sClient.Create(ctx, rec(name, func(s *agentorcav1alpha1.RoutingRecommendationSpec) {
				s.Patch, s.Reason = "", "no lever"
			}))).To(Succeed())
			err := update(name, add)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(field + " is immutable"))
		},
		Entry("patch", "patch", func(s *agentorcav1alpha1.RoutingRecommendationSpec) {
			s.Patch = `{"spec":{"providers":[]}}`
		}),
		Entry("rollbackPatch", "rollbackPatch", func(s *agentorcav1alpha1.RoutingRecommendationSpec) { s.RollbackPatch = `{}` }),
		Entry("benchmarkRef", "benchmarkRef", func(s *agentorcav1alpha1.RoutingRecommendationSpec) { s.BenchmarkRef = evalBench }),
		Entry("caveat", "caveat", func(s *agentorcav1alpha1.RoutingRecommendationSpec) { s.Caveat = "new" }),
	)

	It("cannot lose a field it was created with", func() {
		defer cleanup("rr-drop")
		Expect(k8sClient.Create(ctx, rec("rr-drop", nil))).To(Succeed())
		err := update("rr-drop", func(s *agentorcav1alpha1.RoutingRecommendationSpec) { s.Patch = "" })
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("patch is immutable"))
	})
})
