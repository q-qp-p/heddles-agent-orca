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
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/heddles/agent-orca/internal/postgresql"
)

// feedbackStore persists feedback on routing. *postgresql.Store implements it.
type feedbackStore interface {
	UpsertFeedback(ctx context.Context, f postgresql.Feedback) error
	FeedbackForRun(ctx context.Context, namespace, run, user string) ([]postgresql.Feedback, error)
}

// maxFeedbackBody bounds a feedback request: a comment of at most 2 KiB plus
// a few small fields.
const maxFeedbackBody = 8 << 10

// feedbackRequest is the body of a feedback POST, on the UI and the task API.
type feedbackRequest struct {
	// Rating is 1 for a thumbs up and -1 for a thumbs down.
	Rating int `json:"rating"`
	// Comment is optional free text, up to 2 KiB.
	Comment string `json:"comment,omitempty"`
	// DecisionID rates one routing decision; empty rates the whole run.
	DecisionID string `json:"decisionId,omitempty"`
	// Turn is the turn the rating is for, when the caller knows it.
	Turn *int `json:"turn,omitempty"`
}

// readFeedback reads and checks a feedback body. The error text is safe to
// show the caller.
func readFeedback(w http.ResponseWriter, r *http.Request) (feedbackRequest, error) {
	var req feedbackRequest
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxFeedbackBody))
	if err != nil {
		return req, errors.New("reading body")
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return req, errors.New("body must be JSON like {\"rating\":1,\"comment\":\"...\"}")
	}
	req.Comment = strings.TrimSpace(req.Comment)
	return req, nil
}

// feedbackFrom builds the stored feedback for a request, or returns the reason
// it cannot be stored.
func feedbackFrom(req feedbackRequest, namespace, run, tenantName, user, source string) (postgresql.Feedback, error) {
	f := postgresql.Feedback{
		Tenant: tenantName, Namespace: namespace, RunName: run,
		DecisionID: req.DecisionID, TurnIndex: req.Turn,
		Rating: req.Rating, Comment: req.Comment, UserID: user, Source: source,
	}
	return f, f.Validate()
}

// feedbackUser names who rated: the signed-in user, else the tenant the
// request authenticated as, else "anonymous" when authentication is off.
func feedbackUser(t *TenantIdentity) string {
	switch {
	case t == nil:
		return "anonymous"
	case t.UserID != "":
		return t.UserID
	default:
		return "tenant:" + t.TenantName
	}
}

func feedbackTenantName(t *TenantIdentity) string {
	if t == nil {
		return ""
	}
	return t.TenantName
}

// feedbackSource returns the feedback store, or nil without PostgreSQL.
func (s *UIServer) feedbackSource() feedbackStore {
	if s.feedback != nil {
		return s.feedback
	}
	if s.pgStore != nil {
		return s.pgStore
	}
	return nil
}

// handleRunFeedback serves /api/runs/{ns}/{name}/feedback. POST records the
// caller's thumbs up or down on the run or on one of its routing decisions
// (a second rating of the same target replaces the first); GET returns the
// caller's own ratings of the run so the UI can show which thumbs are set.
func (s *UIServer) handleRunFeedback(w http.ResponseWriter, r *http.Request, ns, runName string) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	tenant, _ := TenantFromContext(r.Context())
	if tenant != nil && !slices.Contains(tenantNamespaces(tenant), ns) {
		http.Error(w, "namespace not allowed", http.StatusForbidden)
		return
	}
	store := s.feedbackSource()
	if store == nil {
		http.Error(w, `{"error":"feedback needs the PostgreSQL store"}`, http.StatusServiceUnavailable)
		return
	}
	user := feedbackUser(tenant)

	if r.Method == http.MethodGet {
		list, err := store.FeedbackForRun(r.Context(), ns, runName, user)
		if err != nil {
			slog.Error("loading feedback", "run", runName, "ns", ns, "err", err)
			http.Error(w, "loading feedback failed", http.StatusInternalServerError)
			return
		}
		jsonResponse(w, map[string]any{"feedback": list})
		return
	}

	req, err := readFeedback(w, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f, err := feedbackFrom(req, ns, runName, feedbackTenantName(tenant), user, postgresql.FeedbackSourceUI)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := store.UpsertFeedback(r.Context(), f); err != nil {
		slog.Error("storing feedback", "run", runName, "ns", ns, "err", err)
		http.Error(w, "storing feedback failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// WithFeedbackStore gives the task API the feedback store. Without one,
// feedback is refused with 503: dropping a rating silently would mislead the
// caller. It returns s for chaining.
func (s *ExternalAPIServer) WithFeedbackStore(store feedbackStore) *ExternalAPIServer {
	s.feedback = store
	return s
}

// feedbackTask handles POST /v1/tasks/{id}/feedback.
func (s *ExternalAPIServer) feedbackTask(w http.ResponseWriter, r *http.Request, taskID string) {
	tenant, ok := TenantFromContext(r.Context())
	if !ok || tenant == nil {
		http.Error(w, `{"error":"no tenant identity"}`, http.StatusUnauthorized)
		return
	}
	run, err := findRunInNamespaces(r.Context(), s.crdClient, tenant, taskID)
	if err != nil || run.Labels["agentorca.io/tenant"] != tenant.TenantName {
		http.Error(w, `{"error":"task not found"}`, http.StatusNotFound)
		return
	}
	if s.feedback == nil {
		http.Error(w, `{"error":"feedback needs the PostgreSQL store"}`, http.StatusServiceUnavailable)
		return
	}
	req, err := readFeedback(w, r)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	f, err := feedbackFrom(req, run.Namespace, run.Name, tenant.TenantName, feedbackUser(tenant), postgresql.FeedbackSourceAPI)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.feedback.UpsertFeedback(r.Context(), f); err != nil {
		slog.Error("storing feedback", "task", taskID, "err", err)
		http.Error(w, `{"error":"storing feedback failed"}`, http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// jsonError writes the {"error": ...} body the task API uses.
func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
