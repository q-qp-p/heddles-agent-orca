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

var ada = &TenantIdentity{TenantName: "acme", Namespaces: []string{"team-a"}, UserID: "ada"}

func TestRunFeedback_StoresARatingOfADecision(t *testing.T) {
	store := &fakeFeedbackStore{}
	s := &UIServer{feedback: store}
	rec := feedbackReq(s, tenantCtx(ada), http.MethodPost, "/api/runs/team-a/run-1/feedback",
		`{"rating":-1,"comment":"wrong model","decisionId":"d-1","turn":2}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	got := store.rows["team-a|run-1|d-1|ada"]
	if got.Rating != -1 || got.Comment != "wrong model" || got.Source != postgresql.FeedbackSourceUI ||
		got.Tenant != "acme" || got.TurnIndex == nil || *got.TurnIndex != 2 {
		t.Errorf("stored = %+v", got)
	}
}

func TestRunFeedback_RatingAgainReplacesTheFirst(t *testing.T) {
	store := &fakeFeedbackStore{}
	s := &UIServer{feedback: store}
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
	s := &UIServer{feedback: &fakeFeedbackStore{}}
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
	s := &UIServer{feedback: store}
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
	s := &UIServer{feedback: store}
	bob := &TenantIdentity{TenantName: "acme", Namespaces: []string{"team-a"}, UserID: "bob"}
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
	s := &UIServer{feedback: &fakeFeedbackStore{err: errors.New("boom")}}
	if rec := feedbackReq(s, tenantCtx(ada), http.MethodPost, "/api/runs/team-a/r/feedback", `{"rating":1}`); rec.Code != http.StatusInternalServerError {
		t.Errorf("store error: status = %d, want 500", rec.Code)
	}
	if rec := feedbackReq(&UIServer{feedback: &fakeFeedbackStore{}}, tenantCtx(ada), http.MethodDelete, "/api/runs/team-a/r/feedback", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE: status = %d, want 405", rec.Code)
	}
}

func TestRunFeedback_WithoutAuthenticationIsAnonymous(t *testing.T) {
	store := &fakeFeedbackStore{}
	s := &UIServer{feedback: store}
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
	}}
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
	tenant := &TenantIdentity{TenantName: "acme", Namespaces: []string{"team-a"}}
	rec := taskFeedback(t, store, tenant, "acme", `{"rating":1,"comment":"nice"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	got := store.rows["team-a|task-1||tenant:acme"]
	if got.Rating != 1 || got.Source != postgresql.FeedbackSourceAPI || got.Comment != "nice" {
		t.Errorf("stored = %+v", store.rows)
	}
}

func TestTaskFeedback_AnotherTenantsTaskDoesNotExist(t *testing.T) {
	store := &fakeFeedbackStore{}
	tenant := &TenantIdentity{TenantName: "acme", Namespaces: []string{"team-a"}}
	if rec := taskFeedback(t, store, tenant, "globex", `{"rating":1}`); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if store.hits != 0 {
		t.Error("feedback on another tenant's task reached the store")
	}
}

func TestTaskFeedback_InvalidAndUnavailable(t *testing.T) {
	tenant := &TenantIdentity{TenantName: "acme", Namespaces: []string{"team-a"}}
	rec := taskFeedback(t, &fakeFeedbackStore{}, tenant, "acme", `{"rating":9}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"error"`) {
		t.Errorf("bad rating: status %d body %s", rec.Code, rec.Body)
	}
	rec = taskFeedback(t, nil, tenant, "acme", `{"rating":1}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("no store: status = %d, want 503", rec.Code)
	}
}
