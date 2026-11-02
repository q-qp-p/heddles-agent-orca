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
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/postgresql"
)

// fakeFeedbackStore keeps one rating per (namespace, run, decision, user), as
// the table's unique key does.
type fakeFeedbackStore struct {
	rows map[string]postgresql.Feedback
	err  error
	hits int
}

func feedbackKey(f postgresql.Feedback) string {
	return strings.Join([]string{f.Namespace, f.RunName, f.DecisionID, f.UserID}, "|")
}

func (f *fakeFeedbackStore) UpsertFeedback(_ context.Context, fb postgresql.Feedback) error {
	f.hits++
	if f.err != nil {
		return f.err
	}
	if f.rows == nil {
		f.rows = map[string]postgresql.Feedback{}
	}
	f.rows[feedbackKey(fb)] = fb
	return nil
}

func (f *fakeFeedbackStore) FeedbackForRun(_ context.Context, ns, run, user string) ([]postgresql.Feedback, error) {
	f.hits++
	var out []postgresql.Feedback
	for _, fb := range f.rows {
		if fb.Namespace == ns && fb.RunName == run && (user == "" || fb.UserID == user) {
			out = append(out, fb)
		}
	}
	return out, f.err
}

func feedbackReq(s *UIServer, ctx context.Context, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
	rec := httptest.NewRecorder()
	s.handleRunOrStream(rec, req)
	return rec
}

func tenantCtx(t *TenantIdentity) context.Context {
	return context.WithValue(context.Background(), tenantIdentityKey, t)
}

var ada = &TenantIdentity{TenantName: shadowTenant, Namespaces: []string{"team-a"}, UserID: "ada"}

// feedbackUIServer is a UI server whose cluster has the runs the tests rate:
// plain runs of the Agent "coder", a deployment's chat run, a workflow step's
// run, and the deployment itself.
func feedbackUIServer(t *testing.T, store feedbackStore) *UIServer {
	t.Helper()
	run := func(ns, name string, labels map[string]string) *agentorcav1alpha1.AgentRun {
		return &agentorcav1alpha1.AgentRun{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
			Spec:       agentorcav1alpha1.AgentRunSpec{AgentRef: shadowAgent},
		}
	}
	cl := fake.NewClientBuilder().WithScheme(adminScheme(t)).WithObjects(
		run("team-a", "run-1", nil), run("team-a", "r", nil), run("ns", "run", nil),
		run("team-a", "chat-run", map[string]string{"agentorca.io/deployment": "assistant"}),
		run("team-a", "step-run", map[string]string{"agentorca.io/workflow": "release", "agentorca.io/workflow-step": "review"}),
		&agentorcav1alpha1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: "assistant", Namespace: "team-a"},
			Spec:       agentorcav1alpha1.AgentDeploymentSpec{AgentRef: "helper"},
		},
	).Build()
	return &UIServer{feedback: store, crdClient: cl}
}

func TestRunFeedback_StoresARatingOfADecision(t *testing.T) {
	store := &fakeFeedbackStore{}
	s := feedbackUIServer(t, store)
	rec := feedbackReq(s, tenantCtx(ada), http.MethodPost, "/api/runs/team-a/run-1/feedback",
		`{"rating":-1,"comment":"wrong model","decisionId":"d-1","turn":2}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	got := store.rows["team-a|run-1|d-1|ada"]
	if got.Rating != -1 || got.Comment != "wrong model" || got.Source != postgresql.FeedbackSourceUI ||
		got.Tenant != shadowTenant || got.TurnIndex == nil || *got.TurnIndex != 2 {
		t.Errorf("stored = %+v", got)
	}
}

// A rating counts towards the Agent whose work it is, however that work ran, so
// ratings build up per Agent across runs, deployment sessions and workflow steps.
func TestRunFeedback_FilesTheRatingUnderTheRunsAgent(t *testing.T) {
	store := &fakeFeedbackStore{}
	s := feedbackUIServer(t, store)
	for _, run := range []string{"run-1", "chat-run", "step-run"} {
		if rec := feedbackReq(s, tenantCtx(ada), http.MethodPost, "/api/runs/team-a/"+run+"/feedback", `{"rating":1}`); rec.Code != http.StatusNoContent {
			t.Fatalf("%s: status = %d: %s", run, rec.Code, rec.Body)
		}
	}
	plain, chat, step := store.rows["team-a|run-1||ada"], store.rows["team-a|chat-run||ada"], store.rows["team-a|step-run||ada"]
	if plain.Agent != shadowAgent || plain.Deployment != "" || plain.Workflow != "" {
		t.Errorf("plain run = %+v", plain)
	}
	if chat.Agent != shadowAgent || chat.Deployment != "assistant" {
		t.Errorf("a deployment's chat run = %+v, want the Agent and the deployment", chat)
	}
	if step.Agent != shadowAgent || step.Workflow != "release" || step.WorkflowStep != "review" {
		t.Errorf("a workflow step's run = %+v, want the Agent, the workflow and the step", step)
	}
}

func TestRunFeedback_RatingADeploymentByNameUsesItsAgent(t *testing.T) {
	store := &fakeFeedbackStore{}
	s := feedbackUIServer(t, store)
	if rec := feedbackReq(s, tenantCtx(ada), http.MethodPost, "/api/runs/team-a/assistant/feedback", `{"rating":-1}`); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if got := store.rows["team-a|assistant||ada"]; got.Agent != "helper" || got.Deployment != "assistant" {
		t.Errorf("stored = %+v, want the deployment's own Agent", got)
	}
}

func TestRunFeedback_UnknownRunIs404AndStoresNothing(t *testing.T) {
	store := &fakeFeedbackStore{}
	s := feedbackUIServer(t, store)
	if rec := feedbackReq(s, tenantCtx(ada), http.MethodPost, "/api/runs/team-a/ghost/feedback", `{"rating":1}`); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if store.hits != 0 {
		t.Error("a rating of a run that does not exist reached the store")
	}
}

func TestRunFeedback_RatingAgainReplacesTheFirst(t *testing.T) {
	store := &fakeFeedbackStore{}
	s := feedbackUIServer(t, store)
	for _, body := range []string{`{"rating":-1,"decisionId":"d-1"}`, `{"rating":1,"decisionId":"d-1"}`} {
		if rec := feedbackReq(s, tenantCtx(ada), http.MethodPost, "/api/runs/team-a/run-1/feedback", body); rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d", rec.Code)
		}
	}
	if len(store.rows) != 1 || store.rows["team-a|run-1|d-1|ada"].Rating != 1 {
		t.Errorf("rows = %+v, want one rating, now a thumbs up", store.rows)
	}
}

func TestRunFeedback_RejectsWhatTheTableWould(t *testing.T) {
	s := feedbackUIServer(t, &fakeFeedbackStore{})
	for name, body := range map[string]string{
		"rating zero":  `{"rating":0}`,
		"rating three": `{"rating":3}`,
		"long comment": `{"rating":1,"comment":"` + strings.Repeat("x", 2049) + `"}`,
		"not json":     `thumbs up`,
	} {
		if rec := feedbackReq(s, tenantCtx(ada), http.MethodPost, "/api/runs/team-a/run-1/feedback", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, rec.Code)
		}
	}
}

func TestRunFeedback_TenantsReachOnlyTheirNamespaces(t *testing.T) {
	store := &fakeFeedbackStore{}
	s := feedbackUIServer(t, store)
	if rec := feedbackReq(s, tenantCtx(ada), http.MethodPost, "/api/runs/team-b/run-1/feedback", `{"rating":1}`); rec.Code != http.StatusForbidden {
		t.Errorf("other namespace: status = %d, want 403", rec.Code)
	}
	if rec := feedbackReq(s, tenantCtx(ada), http.MethodGet, "/api/runs/team-b/run-1/feedback", ""); rec.Code != http.StatusForbidden {
		t.Errorf("other namespace GET: status = %d, want 403", rec.Code)
	}
	if store.hits != 0 {
		t.Error("a forbidden request reached the store")
	}
}

func TestRunFeedback_GetReturnsOnlyTheCallersRatings(t *testing.T) {
	store := &fakeFeedbackStore{}
	s := feedbackUIServer(t, store)
	bob := &TenantIdentity{TenantName: shadowTenant, Namespaces: []string{"team-a"}, UserID: "bob"}
	feedbackReq(s, tenantCtx(ada), http.MethodPost, "/api/runs/team-a/run-1/feedback", `{"rating":1,"decisionId":"d-1"}`)
	feedbackReq(s, tenantCtx(bob), http.MethodPost, "/api/runs/team-a/run-1/feedback", `{"rating":-1,"decisionId":"d-1"}`)

	rec := feedbackReq(s, tenantCtx(ada), http.MethodGet, "/api/runs/team-a/run-1/feedback", "")
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), `"user":"bob"`) || !strings.Contains(rec.Body.String(), `"user":"ada"`) {
		t.Errorf("status %d body %s: want only ada's rating", rec.Code, rec.Body)
	}
}

func TestRunFeedback_WithoutPostgresIs503AndErrorsAre500(t *testing.T) {
	if rec := feedbackReq(&UIServer{}, tenantCtx(ada), http.MethodPost, "/api/runs/team-a/r/feedback", `{"rating":1}`); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("no store: status = %d, want 503 (a rating must not vanish silently)", rec.Code)
	}
	s := feedbackUIServer(t, &fakeFeedbackStore{err: errors.New("boom")})
	if rec := feedbackReq(s, tenantCtx(ada), http.MethodPost, "/api/runs/team-a/r/feedback", `{"rating":1}`); rec.Code != http.StatusInternalServerError {
		t.Errorf("store error: status = %d, want 500", rec.Code)
	}
	if rec := feedbackReq(feedbackUIServer(t, &fakeFeedbackStore{}), tenantCtx(ada), http.MethodDelete, "/api/runs/team-a/r/feedback", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE: status = %d, want 405", rec.Code)
	}
}

func TestRunFeedback_WithoutAuthenticationIsAnonymous(t *testing.T) {
	store := &fakeFeedbackStore{}
	s := feedbackUIServer(t, store)
	if rec := feedbackReq(s, context.Background(), http.MethodPost, "/api/runs/ns/run/feedback", `{"rating":1}`); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d", rec.Code)
	}
	if _, ok := store.rows["ns|run||anonymous"]; !ok {
		t.Errorf("rows = %+v, want an anonymous rating", store.rows)
	}
}

func taskFeedback(t *testing.T, store feedbackStore, tenant *TenantIdentity, taskTenantLabel, body string) *httptest.ResponseRecorder {
	t.Helper()
	run := &agentorcav1alpha1.AgentRun{ObjectMeta: metav1.ObjectMeta{
		Name: "task-1", Namespace: "team-a", Labels: map[string]string{"agentorca.io/tenant": taskTenantLabel},
	}, Spec: agentorcav1alpha1.AgentRunSpec{AgentRef: shadowAgent}}
	s := &ExternalAPIServer{
		auth:      &ExternalAuth{},
		crdClient: fake.NewClientBuilder().WithScheme(adminScheme(t)).WithObjects(run).Build(),
		feedback:  store,
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/tasks/task-1/feedback", strings.NewReader(body)).
		WithContext(context.WithValue(context.Background(), tenantIdentityKey, tenant))
	rec := httptest.NewRecorder()
	s.handleTaskByID(rec, req)
	return rec
}

func TestTaskFeedback_StoresAnAPIRating(t *testing.T) {
	store := &fakeFeedbackStore{}
	tenant := &TenantIdentity{TenantName: shadowTenant, Namespaces: []string{"team-a"}}
	rec := taskFeedback(t, store, tenant, shadowTenant, `{"rating":1,"comment":"nice"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	got := store.rows["team-a|task-1||tenant:acme"]
	if got.Rating != 1 || got.Source != postgresql.FeedbackSourceAPI || got.Comment != "nice" || got.Agent != shadowAgent {
		t.Errorf("stored = %+v", store.rows)
	}
}

func TestTaskFeedback_AnotherTenantsTaskDoesNotExist(t *testing.T) {
	store := &fakeFeedbackStore{}
	tenant := &TenantIdentity{TenantName: shadowTenant, Namespaces: []string{"team-a"}}
	if rec := taskFeedback(t, store, tenant, "globex", `{"rating":1}`); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if store.hits != 0 {
		t.Error("feedback on another tenant's task reached the store")
	}
}

func TestTaskFeedback_InvalidAndUnavailable(t *testing.T) {
	tenant := &TenantIdentity{TenantName: shadowTenant, Namespaces: []string{"team-a"}}
	rec := taskFeedback(t, &fakeFeedbackStore{}, tenant, shadowTenant, `{"rating":9}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"error"`) {
		t.Errorf("bad rating: status %d body %s", rec.Code, rec.Body)
	}
	rec = taskFeedback(t, nil, tenant, shadowTenant, `{"rating":1}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("no store: status = %d, want 503", rec.Code)
	}
}
