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

package apiserver

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

// annotationActionRequestedBy records who asked for the action on a
// RoutingRecommendation, for the audit trail.
const annotationActionRequestedBy = "agentorca.io/action-requested-by"

// recommendationCondition is a condition as the UI shows it.
type recommendationCondition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// recommendationView is one RoutingRecommendation with what the caller can do
// with it. The rules for what may be asked live here, next to the data, so the
// browser never has to work them out.
type recommendationView struct {
	Name           string          `json:"name"`
	Namespace      string          `json:"namespace"`
	Selector       string          `json:"selector"`
	Benchmark      string          `json:"benchmark,omitempty"`
	Detector       string          `json:"detector"`
	Agent          string          `json:"agent,omitempty"`
	Category       string          `json:"category,omitempty"`
	Summary        string          `json:"summary"`
	Patch          string          `json:"patch,omitempty"`
	RollbackPatch  string          `json:"rollbackPatch,omitempty"`
	Reason         string          `json:"reason,omitempty"`
	Caveat         string          `json:"caveat,omitempty"`
	BaseGeneration int64           `json:"baseGeneration"`
	Action         string          `json:"action"`
	Phase          string          `json:"phase"`
	Message        string          `json:"message,omitempty"`
	Evidence       json.RawMessage `json:"evidence,omitempty"`
	Corroboration  []string        `json:"corroboration,omitempty"`
	LastEvaluated  *metav1.Time    `json:"lastEvaluated,omitempty"`
	CreatedAt      metav1.Time     `json:"createdAt"`

	AppliedAt            *metav1.Time                       `json:"appliedAt,omitempty"`
	AppliedRollbackPatch string                             `json:"appliedRollbackPatch,omitempty"`
	PostApply            *agentorcav1alpha1.PostApplyStatus `json:"postApply,omitempty"`
	Conditions           []recommendationCondition          `json:"conditions,omitempty"`

	// CanRequestApply and CanRollback say whether the operator would raise an
	// approval if asked. BlockedReason says why not.
	CanRequestApply bool   `json:"canRequestApply"`
	CanRollback     bool   `json:"canRollback"`
	BlockedReason   string `json:"blockedReason,omitempty"`
	// KubectlPatch applies the patch by hand.
	KubectlPatch string `json:"kubectlPatch,omitempty"`
}

// mayApplyRecommendations mirrors the operator's rule: a benchmark in
// proposeApply mode that names who may approve. It is repeated here because the
// API server does not import the controllers; the operator enforces it again.
func mayApplyRecommendations(b *agentorcav1alpha1.RoutingBenchmark) bool {
	return b != nil && b.Spec.Mode == agentorcav1alpha1.BenchmarkModeProposeApply && len(b.Spec.ApproverGroups) > 0
}

const needsProposeApplyMessage = "applying needs a RoutingBenchmark with mode proposeApply and approverGroups covering this selector's Agents"

// shellQuote wraps s in single quotes for a POSIX shell.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func viewOf(rec *agentorcav1alpha1.RoutingRecommendation, bench *agentorcav1alpha1.RoutingBenchmark) recommendationView {
	v := recommendationView{
		Name: rec.Name, Namespace: rec.Namespace, Selector: rec.Spec.SelectorRef, Benchmark: rec.Spec.BenchmarkRef,
		Detector: rec.Spec.Detector, Agent: rec.Spec.Agent, Category: rec.Spec.Category, Summary: rec.Spec.Summary,
		Patch: rec.Spec.Patch, RollbackPatch: rec.Spec.RollbackPatch, Reason: rec.Spec.Reason, Caveat: rec.Spec.Caveat,
		BaseGeneration: rec.Spec.BaseGeneration, Action: rec.Spec.Action, Phase: rec.Status.Phase, Message: rec.Status.Message,
		Corroboration: rec.Status.Corroboration, LastEvaluated: rec.Status.LastEvaluated, CreatedAt: rec.CreationTimestamp,
		AppliedAt: rec.Status.AppliedAt, AppliedRollbackPatch: rec.Status.AppliedRollbackPatch, PostApply: rec.Status.PostApply,
	}
	if v.Action == "" {
		v.Action = agentorcav1alpha1.RecommendationActionNone
	}
	if rec.Status.Evidence != nil {
		v.Evidence = json.RawMessage(rec.Status.Evidence.Raw)
	}
	for _, c := range rec.Status.Conditions {
		v.Conditions = append(v.Conditions, recommendationCondition{Type: c.Type, Status: string(c.Status), Reason: c.Reason, Message: c.Message})
	}
	if rec.Spec.Patch != "" {
		v.KubectlPatch = fmt.Sprintf("kubectl patch modelselector %s -n %s --type merge -p %s", rec.Spec.SelectorRef, rec.Namespace, shellQuote(rec.Spec.Patch))
	}
	allowed := mayApplyRecommendations(bench)
	switch rec.Status.Phase {
	case agentorcav1alpha1.RecommendationProposed:
		switch {
		case rec.Spec.Patch == "":
			v.BlockedReason = "report only: " + rec.Spec.Reason
		case !allowed:
			v.BlockedReason = needsProposeApplyMessage
		case rec.Spec.Action != agentorcav1alpha1.RecommendationActionRequestApply:
			v.CanRequestApply = true
		}
	case agentorcav1alpha1.RecommendationApplied:
		switch {
		case rec.Status.AppliedRollbackPatch == "":
			v.BlockedReason = "there is no rollback patch to apply"
		case !allowed:
			v.BlockedReason = needsProposeApplyMessage
		case !meta.IsStatusConditionTrue(rec.Status.Conditions, "RollbackRequested"):
			v.CanRollback = true
		}
	}
	return v
}

// handleListRecommendations serves GET /api/routing/recommendations. The
// optional namespace and phase query parameters narrow the list.
func (s *UIServer) handleListRecommendations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ns, phase := r.URL.Query().Get("namespace"), r.URL.Query().Get("phase")
	var recs agentorcav1alpha1.RoutingRecommendationList
	if err := listCRDsMultiNamespace(r.Context(), s.crdClient, &recs, ns); err != nil {
		slog.Error("listing routing recommendations", "err", err)
		http.Error(w, "listing recommendations failed", http.StatusInternalServerError)
		return
	}
	var sels agentorcav1alpha1.ModelSelectorList
	var benches agentorcav1alpha1.RoutingBenchmarkList
	if err := listCRDsMultiNamespace(r.Context(), s.crdClient, &sels, ns); err != nil {
		http.Error(w, "listing selectors failed", http.StatusInternalServerError)
		return
	}
	if err := listCRDsMultiNamespace(r.Context(), s.crdClient, &benches, ns); err != nil {
		http.Error(w, "listing benchmarks failed", http.StatusInternalServerError)
		return
	}
	benchOf := map[string]*agentorcav1alpha1.RoutingBenchmark{}
	for i := range benches.Items {
		benchOf[benches.Items[i].Namespace+"/"+benches.Items[i].Name] = &benches.Items[i]
	}
	governing := map[string]*agentorcav1alpha1.RoutingBenchmark{} // namespace/selector -> its benchmark
	for i := range sels.Items {
		sel := &sels.Items[i]
		if sel.Status.BenchmarkRef != "" {
			governing[sel.Namespace+"/"+sel.Name] = benchOf[sel.Namespace+"/"+sel.Status.BenchmarkRef]
		}
	}
	out := make([]recommendationView, 0, len(recs.Items))
	for i := range recs.Items {
		rec := &recs.Items[i]
		if phase != "" && rec.Status.Phase != phase {
			continue
		}
		out = append(out, viewOf(rec, governing[rec.Namespace+"/"+rec.Spec.SelectorRef]))
	}
	// Open ones first, then the newest.
	slices.SortStableFunc(out, func(a, b recommendationView) int {
		if oa, ob := openPhase(a.Phase), openPhase(b.Phase); oa != ob {
			if oa {
				return -1
			}
			return 1
		}
		return b.CreatedAt.Compare(a.CreatedAt.Time)
	})
	jsonResponse(w, map[string]any{"recommendations": out})
}

func openPhase(phase string) bool {
	return phase == agentorcav1alpha1.RecommendationProposed || phase == agentorcav1alpha1.RecommendationPendingApproval
}

// benchmarkCategoryView is the verdicts for one Agent and task category.
type benchmarkCategoryView struct {
	Agent      string `json:"agent"`
	Category   string `json:"category"`
	Samples    int32  `json:"samples"`
	ServedWins int32  `json:"servedWins"`
	ShadowWins int32  `json:"shadowWins"`
	Ties       int32  `json:"ties"`
}

// benchmarkView is one RoutingBenchmark's configuration and what it has found.
type benchmarkView struct {
	Name                  string                  `json:"name"`
	Namespace             string                  `json:"namespace"`
	Mode                  string                  `json:"mode"`
	Agents                []string                `json:"agents"`
	Ready                 bool                    `json:"ready"`
	ReadyReason           string                  `json:"readyReason,omitempty"`
	ReadyMessage          string                  `json:"readyMessage,omitempty"`
	Warnings              []string                `json:"warnings,omitempty"`
	SampleRate            string                  `json:"sampleRate,omitempty"`
	Judge                 string                  `json:"judge,omitempty"`
	PerDayUSD             string                  `json:"perDayUSD"`
	ShadowSpendTodayUSD   string                  `json:"shadowSpendTodayUSD,omitempty"`
	BudgetExhausted       bool                    `json:"budgetExhausted,omitempty"`
	SampledCalls          int32                   `json:"sampledCalls"`
	JudgedPairs           int32                   `json:"judgedPairs"`
	JudgeInconsistentRate string                  `json:"judgeInconsistentRate,omitempty"`
	Categories            []benchmarkCategoryView `json:"categories,omitempty"`
	LastEvaluated         *metav1.Time            `json:"lastEvaluated,omitempty"`
}

// handleListBenchmarks serves GET /api/routing/benchmarks.
func (s *UIServer) handleListBenchmarks(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var list agentorcav1alpha1.RoutingBenchmarkList
	if err := listCRDsMultiNamespace(r.Context(), s.crdClient, &list, r.URL.Query().Get("namespace")); err != nil {
		slog.Error("listing routing benchmarks", "err", err)
		http.Error(w, "listing benchmarks failed", http.StatusInternalServerError)
		return
	}
	out := make([]benchmarkView, 0, len(list.Items))
	for i := range list.Items {
		b := &list.Items[i]
		v := benchmarkView{
			Name: b.Name, Namespace: b.Namespace, Mode: b.Spec.Mode, Agents: b.Status.MatchedAgents,
			PerDayUSD: b.Spec.Budget.PerDayUSD, ShadowSpendTodayUSD: b.Status.ShadowSpendTodayUSD, BudgetExhausted: b.Status.BudgetExhausted,
			SampledCalls: b.Status.SampledCalls, JudgedPairs: b.Status.JudgedPairs, JudgeInconsistentRate: b.Status.JudgeInconsistentRate,
			LastEvaluated: b.Status.LastEvaluated,
		}
		if v.Mode == "" {
			v.Mode = agentorcav1alpha1.BenchmarkModeReportOnly
		}
		if b.Spec.Shadow != nil {
			v.SampleRate = b.Spec.Shadow.SampleRate
		}
		if b.Spec.Judge != nil {
			v.Judge = b.Spec.Judge.ProviderRef
		}
		if c := meta.FindStatusCondition(b.Status.Conditions, agentorcav1alpha1.BenchmarkConditionReady); c != nil {
			v.Ready, v.ReadyReason, v.ReadyMessage = c.Status == metav1.ConditionTrue, c.Reason, c.Message
		}
		for _, c := range b.Status.Conditions {
			if c.Type != agentorcav1alpha1.BenchmarkConditionReady && c.Status == metav1.ConditionTrue {
				v.Warnings = append(v.Warnings, c.Type+": "+c.Message)
			}
		}
		for _, c := range b.Status.Categories {
			v.Categories = append(v.Categories, benchmarkCategoryView{
				Agent: c.Agent, Category: c.Category, Samples: c.Samples, ServedWins: c.ServedWins, ShadowWins: c.ShadowWins, Ties: c.Ties,
			})
		}
		out = append(out, v)
	}
	jsonResponse(w, map[string]any{"benchmarks": out})
}

type recommendationActionRequest struct {
	Action string `json:"action"`
}

// handleRecommendationAction serves POST /api/routing/recommendations/{ns}/{name}/action.
// It asks for something to be done, by setting spec.action; the operator raises
// the approval, and nothing changes until an approver agrees.
func (s *UIServer) handleRecommendationAction(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/routing/recommendations/")
	parts := strings.Split(rest, "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] != "action" {
		http.NotFound(w, r)
		return
	}
	ns, name := parts[0], parts[1]
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	tenant, ok := TenantFromContext(r.Context())
	if !ok || tenant == nil || tenant.UserID == "" {
		http.Error(w, "asking for a routing change needs a signed-in user", http.StatusForbidden)
		return
	}
	if !slices.Contains(tenantNamespaces(tenant), ns) {
		http.Error(w, "namespace not allowed", http.StatusForbidden)
		return
	}
	var req recommendationActionRequest
	if body, err := io.ReadAll(io.LimitReader(r.Body, 4<<10)); err != nil || json.Unmarshal(body, &req) != nil {
		http.Error(w, `body must be JSON like {"action":"requestApply"}`, http.StatusBadRequest)
		return
	}
	if req.Action != agentorcav1alpha1.RecommendationActionRequestApply && req.Action != agentorcav1alpha1.RecommendationActionRollback {
		http.Error(w, "action must be requestApply or rollback", http.StatusBadRequest)
		return
	}
	var rec agentorcav1alpha1.RoutingRecommendation
	if err := s.crdClient.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &rec); err != nil {
		http.Error(w, "recommendation not found", http.StatusNotFound)
		return
	}
	// Each action fits one phase; anything else is a request that cannot be met.
	want := map[string]string{
		agentorcav1alpha1.RecommendationActionRequestApply: agentorcav1alpha1.RecommendationProposed,
		agentorcav1alpha1.RecommendationActionRollback:     agentorcav1alpha1.RecommendationApplied,
	}[req.Action]
	if rec.Status.Phase != want {
		http.Error(w, fmt.Sprintf("%s is for a %s recommendation, and this one is %s", req.Action, want, rec.Status.Phase), http.StatusConflict)
		return
	}
	base := rec.DeepCopy()
	rec.Spec.Action = req.Action
	if rec.Annotations == nil {
		rec.Annotations = map[string]string{}
	}
	rec.Annotations[annotationActionRequestedBy] = tenant.UserID
	if err := s.crdClient.Patch(r.Context(), &rec, client.MergeFrom(base)); err != nil {
		slog.Error("asking for a routing action", "recommendation", name, "namespace", ns, "err", err)
		http.Error(w, "recording the request failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
