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
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/security"
	"github.com/heddles/agent-orca/internal/state"
)

// ExternalAPIServer handles the external-facing REST API for enterprise integrations.
// Port 8084, authenticated via ExternalAuth middleware.
type ExternalAPIServer struct {
	k8s       kubernetes.Interface
	crdClient client.Client
	auth      *ExternalAuth
	store     state.Store
	hasStore  bool
	// feedback stores ratings of routing; nil refuses them.
	feedback feedbackStore

	// rateLimiter enforces per-tenant submission rate limits. nil disables it.
	rateLimiter *RateLimiter

	// Admin surface (under /admin/*) authenticates via Kubernetes SA token +
	// the `agentorca.io/admin` label, separately from the tenant JWT auth above.
	// Both fields are injectable so requireAdminAuth is fully unit-testable.
	reviewSAToken func(ctx context.Context, token string) (username string, ok bool, err error)
	isAdminSA     func(ctx context.Context, namespace, name string) (bool, error)
}

// NewExternalAPIServer creates a new external API server.
func NewExternalAPIServer(k8s kubernetes.Interface, crdClient client.Client, auth *ExternalAuth, hasStore bool, store state.Store) *ExternalAPIServer {
	s := &ExternalAPIServer{
		k8s:         k8s,
		crdClient:   crdClient,
		auth:        auth,
		store:       store,
		hasStore:    hasStore,
		rateLimiter: auth.rateLimiter,
	}
	s.reviewSAToken = s.defaultReviewSAToken
	s.isAdminSA = s.defaultIsAdminSA
	return s
}

// adminNamespace is the namespace where TenantConfig CRs and their client
// secrets live (defaults to the operator's own namespace).
func (s *ExternalAPIServer) adminNamespace() string {
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns
	}
	return "agent-orca-system" //nolint:goconst
}

// defaultReviewSAToken authenticates a Kubernetes SA token via TokenReview
// (no audience restriction — the SA token may be a long-lived TokenRequest).
func (s *ExternalAPIServer) defaultReviewSAToken(ctx context.Context, token string) (string, bool, error) {
	if s.k8s == nil {
		return "", false, fmt.Errorf("no kubernetes client configured for admin auth")
	}
	tr := &authv1.TokenReview{
		Spec: authv1.TokenReviewSpec{Token: token},
	}
	res, err := s.k8s.AuthenticationV1().TokenReviews().Create(ctx, tr, metav1.CreateOptions{})
	if err != nil {
		return "", false, fmt.Errorf("token review: %w", err)
	}
	if !res.Status.Authenticated {
		return "", false, nil
	}
	return res.Status.User.Username, true, nil
}

// defaultIsAdminSA returns true iff the ServiceAccount has the admin label.
func (s *ExternalAPIServer) defaultIsAdminSA(ctx context.Context, namespace, name string) (bool, error) {
	if s.k8s == nil {
		return false, fmt.Errorf("no kubernetes client configured for admin auth")
	}
	sa, err := s.k8s.CoreV1().ServiceAccounts(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	return sa.Labels["agentorca.io/admin"] == "true", nil
}

// Handler returns an http.Handler for the external API.
func (s *ExternalAPIServer) Handler() http.Handler {
	mux := http.NewServeMux()

	// Observability (public — for scrapers/probes).
	mux.HandleFunc("/healthz", healthzHandler)
	mux.HandleFunc("/readyz", readyzHandlerBuilder(k8sReady(s.k8s), s.hasStore, s.store))
	mux.HandleFunc("/version", versionHandler)
	mux.Handle("/metrics", metricsHandler())

	// OpenAPI contract (unauthenticated — for SDK / tooling discovery).
	mux.HandleFunc("/openapi.json", s.handleExternalOpenAPI)

	// Token endpoint (unauthenticated).
	mux.HandleFunc("/oauth/token", s.auth.HandleTokenRequest)

	// Task endpoints (authenticated via middleware).
	mux.HandleFunc("/v1/tasks", s.handleTasks)
	mux.HandleFunc("/v1/tasks/", s.handleTaskByID)

	// /admin/* is the cluster-wide admin surface. Two auth layers are stacked in
	// front of it:
	//
	//   - requireAdminAuth: a Kubernetes ServiceAccount token with the
	//     `agentorca.io/admin` label. Full access (list/create/delete/rotate,
	//     any tenant).
	//   - requireSAOrSelfTenant: the same SA-token path, PLUS a federated OIDC
	//     id_token that may only touch ITS OWN tenant for the operations listed
	//     in selfTenantAdminOps (today: GET /admin/tenants/<own>). This is the
	//     human-OIDC bridge: `aoctl login --auth-method oidc` can read its own
	//     tenant config without a service-account token.
	//
	// Per-tenant RBAC (granting mutations via IdP roles) is intentionally not
	// wired yet — see the `role` field on adminCapability and the TODO in
	// requireSAOrSelfTenant. Mutating/list-all admin ops stay SA-only.
	adminMux := http.NewServeMux()
	adminMux.HandleFunc("/admin/tenants", s.handleAdminTenants)
	adminMux.HandleFunc("/admin/tenants/", s.handleAdminTenantByID)

	// instrument sits INSIDE the auth middleware so authenticated requests carry
	// the tenant identity into the audit log. /admin/* is wrapped by the admin
	// auth layer instead.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/admin/") {
			// An OIDC id_token may perform the self-tenant capabilities in
			// selfTenantAdminOps for its OWN tenant. Everything else (list/
			// create/delete/rotate) is SA-only.
			if name, capability, ok := selfTenantOp(r.Method, r.URL.Path); ok {
				corsMiddleware(instrument("admin", s.requireSAOrSelfTenant(name, capability, adminMux))).ServeHTTP(w, r)
				return
			}
			corsMiddleware(instrument("admin", s.requireAdminAuth(adminMux))).ServeHTTP(w, r)
			return
		}
		corsMiddleware(s.auth.Middleware(instrument("external", mux))).ServeHTTP(w, r)
	})
}

// adminCapability describes one /admin/* operation an OIDC id_token may perform
// on its OWN tenant. ServiceAccount admin tokens bypass these entirely (full
// access via requireAdminAuth).
//
// role is the reserved IdP-derived role required for this capability via
// federated login; empty means "any authenticated id_token for the matching
// tenant" (no RBAC today). Future per-tenant RBAC maps IdP groups → roles and
// fills in role for mutations such as rotate-secret.
type adminCapability struct {
	method     string
	pathPrefix string // tenant name = the single segment immediately after this
	role       string // empty == no IdP-role gate (current default)
}

// selfTenantAdminOps is the explicit, RBAC-extensible allow-list of admin
// operations an OIDC id_token may perform on the tenant it authenticated as.
// Operations NOT listed here (GET /admin/tenants list, POST create, DELETE,
// POST .../rotate-secret, and any cross-tenant target) fall through to
// requireAdminAuth and stay SA-only.
var selfTenantAdminOps = []adminCapability{
	{method: http.MethodGet, pathPrefix: "/admin/tenants/"}, // read own tenant config
}

// selfTenantOp maps an admin (method, path) to the tenant name plus the matched
// capability. It returns ok=true only for a SELF-tenant operation: the tenant
// name must be exactly one path segment (no "/"), so /admin/tenants (list) and
// /admin/tenants/<name>/rotate-secret (mutation) return ok=false and are routed
// to requireAdminAuth instead. Method-aware so future POST/DELETE self-tenant
// caps can be appended to the table without touching routing.
func selfTenantOp(method, p string) (tenant string, capability adminCapability, ok bool) {
	for _, c := range selfTenantAdminOps {
		if c.method != method || !strings.HasPrefix(p, c.pathPrefix) {
			continue
		}
		name := strings.TrimPrefix(p, c.pathPrefix)
		if name == "" || strings.Contains(name, "/") {
			return "", adminCapability{}, false
		}
		return name, c, true
	}
	return "", adminCapability{}, false
}

// TaskSubmission is the request body for POST /v1/tasks.
type TaskSubmission struct {
	Agent    string            `json:"agent"`
	Input    string            `json:"input"`
	Timeout  string            `json:"timeout,omitempty"`
	Callback *TaskCallback     `json:"callback,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
	// Egress configures durable result delivery to an external message bus.
	// +optional
	Egress *agentorcav1alpha1.EgressConfig `json:"egress,omitempty"`
}

// TaskCallback configures webhook delivery on task completion.
type TaskCallback struct {
	URL string `json:"url"`
	// SecretRef is the name of a Kubernetes Secret in the tenant namespace.
	// When set, the controller reads its `hmac-key` value and signs each
	// delivery with an `X-Agentorc-Signature: sha256=<hex>` header (HMAC-SHA256
	// over the raw JSON body) plus an `X-Agentorc-Timestamp`. Receivers verify
	// the signature with the same shared key. When absent, the callback is
	// delivered unsigned (backward compatible).
	SecretRef string `json:"secretRef,omitempty"`
}

// TaskResponse is the response body for task operations.
type TaskResponse struct {
	ID          string            `json:"id"`
	Agent       string            `json:"agent"`
	Status      string            `json:"status"`
	Output      string            `json:"output,omitempty"`
	SpendUSD    string            `json:"spendUSD,omitempty"`
	CreatedAt   string            `json:"createdAt,omitempty"`
	CompletedAt string            `json:"completedAt,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	Links       *TaskLinks        `json:"links,omitempty"`
}

// TaskLinks provides HATEOAS-style links for task navigation.
type TaskLinks struct {
	Self   string `json:"self"`
	Stream string `json:"stream,omitempty"`
}

// handleTasks dispatches GET (list) and POST (create) for /v1/tasks.
func (s *ExternalAPIServer) handleTasks(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.createTask(w, r)
	case http.MethodGet:
		s.listTasks(w, r)
	default:
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
	}
}

// handleTaskByID dispatches requests for /v1/tasks/{id} and sub-resources.
func (s *ExternalAPIServer) handleTaskByID(w http.ResponseWriter, r *http.Request) {
	// Path: /v1/tasks/{id} or /v1/tasks/{id}/{action}
	path := strings.TrimPrefix(r.URL.Path, "/v1/tasks/")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, `{"error":"task ID required"}`, http.StatusBadRequest)
		return
	}
	taskID := parts[0]
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}

	switch {
	case r.Method == http.MethodGet && action == "":
		s.getTask(w, r, taskID)
	case r.Method == http.MethodGet && action == "stream": //nolint:goconst

		s.streamTask(w, r, taskID)
	case r.Method == http.MethodPost && action == "answer":
		s.answerTask(w, r, taskID)
	case r.Method == http.MethodPost && action == "feedback":
		s.feedbackTask(w, r, taskID)
	case r.Method == http.MethodDelete && action == "":
		s.cancelTask(w, r, taskID)
	default:
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
	}
}

// createTask handles POST /v1/tasks.
func (s *ExternalAPIServer) createTask(w http.ResponseWriter, r *http.Request) {
	tenant, ok := TenantFromContext(r.Context())
	if !ok {
		http.Error(w, `{"error":"no tenant identity"}`, http.StatusUnauthorized)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, `{"error":"reading body"}`, http.StatusBadRequest)
		return
	}

	var req TaskSubmission
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"invalid JSON: %s"}`, err), http.StatusBadRequest)
		return
	}

	if req.Agent == "" || req.Input == "" {
		http.Error(w, `{"error":"agent and input are required"}`, http.StatusBadRequest)
		return
	}

	// Check if the tenant is allowed to invoke this agent.
	if tenant.AllowedAgents != nil && !slices.Contains(tenant.AllowedAgents, req.Agent) {
		http.Error(w, fmt.Sprintf(`{"error":"agent %q is not allowed for this tenant"}`, req.Agent), http.StatusForbidden)
		return
	}

	// Resolve the agent to the namespace it actually lives in — which must be one
	// of the tenant's authorized namespaces (allowedNamespaces). This directs the
	// run to the agent's own namespace rather than assuming a single tenant
	// namespace, so a tenant spanning multiple namespaces can reach any agent.
	agent, err := findAgentInNamespaces(r.Context(), s.crdClient, tenant, req.Agent)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"agent %q not found in any authorized namespace"}`, req.Agent), http.StatusNotFound)
		return
	}

	// Enforce tenant quotas (rate limit, concurrent runs, daily budget) before
	// allocating a pod. This is what stops a runaway or compromised client from
	// burning a tenant's daily budget in a burst.
	quotaErr := enforceQuotas(r.Context(), s.crdClient, s.rateLimiter, tenant)
	if qerr, ok := quotaErr.(*QuotaError); ok && qerr != nil {
		if qerr.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(qerr.RetryAfter.Seconds())))
		}
		http.Error(w, fmt.Sprintf(`{"error":%q}`, qerr.Message), qerr.Code)
		return
	}
	if quotaErr != nil {
		http.Error(w, fmt.Sprintf(`{"error":"admission failed: %s"}`, quotaErr), http.StatusInternalServerError)
		return
	}

	// Build the AgentRun CR.
	run := &agentorcav1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: fmt.Sprintf("task-%s-", req.Agent),
			Namespace:    agent.Namespace,
			Labels: map[string]string{
				security.LabelManagedBy:      security.ManagedByValue,
				"agentorca.io/external-task": "true",
				"agentorca.io/tenant":        tenant.TenantName,
			},
		},
		Spec: agentorcav1alpha1.AgentRunSpec{
			AgentRef: req.Agent,
			Input:    req.Input,
		},
	}

	// Set correlation ID from metadata.
	if req.Metadata != nil {
		if run.Annotations == nil {
			run.Annotations = make(map[string]string)
		}
		if cid, ok := req.Metadata["correlationId"]; ok {
			run.Labels["agentorca.io/correlation-id"] = cid
		}
		// Store all metadata as annotations.
		for k, v := range req.Metadata {
			run.Annotations["agentorca.io/meta-"+k] = v
		}
	}

	// Set timeout.
	if req.Timeout != "" {
		d, err := time.ParseDuration(req.Timeout)
		if err == nil {
			run.Spec.Timeout = &metav1.Duration{Duration: d}
		}
	}

	// Set callbacks.
	if req.Callback != nil && req.Callback.URL != "" {
		run.Spec.Callbacks = &agentorcav1alpha1.CallbackConfig{
			OnComplete: req.Callback.URL,
			OnFailed:   req.Callback.URL,
		}
		if req.Callback.SecretRef != "" {
			if run.Annotations == nil {
				run.Annotations = make(map[string]string)
			}
			run.Annotations["agentorca.io/callback-secret"] = req.Callback.SecretRef
		}
	}

	// Set egress.
	if req.Egress != nil {
		run.Spec.Egress = req.Egress
	}

	if err := s.crdClient.Create(r.Context(), run); err != nil {
		slog.Error("creating task AgentRun", "err", err)
		http.Error(w, fmt.Sprintf(`{"error":"creating task: %s"}`, err), http.StatusInternalServerError)
		return
	}

	slog.Info("External task created", "task", run.Name, "agent", req.Agent, "tenant", tenant.TenantName)

	resp := TaskResponse{
		ID:        run.Name,
		Agent:     req.Agent,
		Status:    string(agentorcav1alpha1.AgentRunPhasePending),
		CreatedAt: run.CreationTimestamp.Format(time.RFC3339),
		Metadata:  req.Metadata,
		Links: &TaskLinks{
			Self:   "/v1/tasks/" + run.Name,
			Stream: "/v1/tasks/" + run.Name + "/stream",
		},
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(resp)
}

// getTask handles GET /v1/tasks/{id}.
func (s *ExternalAPIServer) getTask(w http.ResponseWriter, r *http.Request, taskID string) {
	tenant, ok := TenantFromContext(r.Context())
	if !ok {
		http.Error(w, `{"error":"no tenant identity"}`, http.StatusUnauthorized)
		return
	}

	run, err := findRunInNamespaces(r.Context(), s.crdClient, tenant, taskID)
	if err != nil {
		http.Error(w, `{"error":"task not found"}`, http.StatusNotFound)
		return
	}

	// Verify the run belongs to this tenant.
	if run.Labels["agentorca.io/tenant"] != tenant.TenantName {
		http.Error(w, `{"error":"task not found"}`, http.StatusNotFound)
		return
	}

	resp := s.runToTaskResponse(run)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// listTasks handles GET /v1/tasks?agent=X&status=Y.
func (s *ExternalAPIServer) listTasks(w http.ResponseWriter, r *http.Request) {
	tenant, ok := TenantFromContext(r.Context())
	if !ok {
		http.Error(w, `{"error":"no tenant identity"}`, http.StatusUnauthorized)
		return
	}

	labels := client.MatchingLabels{
		"agentorca.io/external-task": "true",
		"agentorca.io/tenant":        tenant.TenantName,
	}

	runList, err := listRunsInNamespaces(r.Context(), s.crdClient, tenant, labels)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"listing tasks: %s"}`, err), http.StatusInternalServerError)
		return
	}

	// Filter by query params.
	agentFilter := r.URL.Query().Get("agent")
	statusFilter := r.URL.Query().Get("status")

	var tasks []TaskResponse
	for i := range runList.Items {
		run := &runList.Items[i]
		if agentFilter != "" && run.Spec.AgentRef != agentFilter {
			continue
		}
		if statusFilter != "" && string(run.Status.Phase) != statusFilter {
			continue
		}
		tasks = append(tasks, s.runToTaskResponse(run))
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"tasks": tasks,
		"count": len(tasks),
	})
}

// streamTask handles GET /v1/tasks/{id}/stream (SSE).
func (s *ExternalAPIServer) streamTask(w http.ResponseWriter, r *http.Request, taskID string) {
	tenant, ok := TenantFromContext(r.Context())
	if !ok {
		http.Error(w, `{"error":"no tenant identity"}`, http.StatusUnauthorized)
		return
	}

	run, err := findRunInNamespaces(r.Context(), s.crdClient, tenant, taskID)
	if err != nil {
		http.Error(w, `{"error":"task not found"}`, http.StatusNotFound)
		return
	}

	if run.Labels["agentorca.io/tenant"] != tenant.TenantName {
		http.Error(w, `{"error":"task not found"}`, http.StatusNotFound)
		return
	}

	if !s.hasStore || s.store == nil {
		http.Error(w, `{"error":"streaming not available (no state store)"}`, http.StatusServiceUnavailable)
		return
	}

	// Set SSE headers.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, `{"error":"streaming not supported"}`, http.StatusInternalServerError)
		return
	}
	// Renew the write deadline per event so the stream outlives WriteTimeout.
	flusher = newRollingDeadlineFlusher(w, r, flusher, sseWriteIdleTimeout)

	// Emit initial status event.
	_, _ = fmt.Fprintf(w, "event: status\ndata: %s\n\n", mustJSON(map[string]string{
		"phase": string(run.Status.Phase),
	}))
	flusher.Flush()

	// Subscribe to the Redis token stream. The stream key is namespaced by the
	// run's own namespace (matching the controller/executor producers, which key by
	// run.Namespace) — so a run created in a non-primary allowed namespace streams
	// from the correct key.
	streamKey := fmt.Sprintf("tokens:%s:%s", run.Namespace, taskID)
	tokenCh, err := s.store.TailTokens(r.Context(), streamKey)
	if err != nil {
		_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", mustJSON(map[string]string{
			"error": "failed to subscribe to token stream",
		}))
		flusher.Flush()
		return
	}

	for token := range tokenCh {
		// Trace events are prefixed with "\x00" (see state.Store.TailTokens).
		// Terminal events (done/fail/finalOutput) mark OpenAI-turn-loop completion
		// and end the HTTP stream; non-terminal trace events are not deliverable as
		// tokens in this protocol, so they are skipped.
		if len(token) > 0 && token[0] == '\x00' {
			if state.IsTerminalTraceEventJSON(token[1:]) {
				break
			}
			continue
		}
		_, _ = fmt.Fprintf(w, "event: token\ndata: %s\n\n", mustJSON(map[string]string{
			"content": token,
		}))
		flusher.Flush()
	}

	// If the stream closes without a terminal event, fall back to fetching
	// the final result from the CRD so clients always get a complete response.
	var final agentorcav1alpha1.AgentRun
	if err := s.crdClient.Get(r.Context(), client.ObjectKey{
		Name: taskID, Namespace: run.Namespace,
	}, &final); err == nil {
		resp := s.runToTaskResponse(&final)
		_, _ = fmt.Fprintf(w, "event: complete\ndata: %s\n\n", mustJSON(resp))
		flusher.Flush()
	}
}

// answerTask handles POST /v1/tasks/{id}/answer for clarification responses.
func (s *ExternalAPIServer) answerTask(w http.ResponseWriter, r *http.Request, taskID string) {
	tenant, ok := TenantFromContext(r.Context())
	if !ok {
		http.Error(w, `{"error":"no tenant identity"}`, http.StatusUnauthorized)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, `{"error":"reading body"}`, http.StatusBadRequest)
		return
	}

	var req struct {
		Answer string `json:"answer"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.Answer == "" {
		http.Error(w, `{"error":"answer is required"}`, http.StatusBadRequest)
		return
	}

	run, err := findRunInNamespaces(r.Context(), s.crdClient, tenant, taskID)
	if err != nil {
		http.Error(w, `{"error":"task not found"}`, http.StatusNotFound)
		return
	}

	if run.Labels["agentorca.io/tenant"] != tenant.TenantName {
		http.Error(w, `{"error":"task not found"}`, http.StatusNotFound)
		return
	}

	if run.Status.Phase != agentorcav1alpha1.AgentRunPhaseWaitingForInput {
		http.Error(w, `{"error":"task is not waiting for input"}`, http.StatusConflict)
		return
	}

	// Store the answer in the state store for the model-router to pick up.
	if s.hasStore && s.store != nil {
		answerKey := fmt.Sprintf("answer:%s:%s", run.Namespace, taskID)
		if err := s.store.SaveAnswer(r.Context(), answerKey, req.Answer, 24*time.Hour); err != nil {
			slog.Warn("failed to save answer to state store", "task", taskID, "err", err)
		}
	}

	// Update the AgentRun status with the answer.
	patch := client.MergeFrom(run.DeepCopy())
	run.Status.ClarifyAnswer = req.Answer
	if err := s.crdClient.Status().Patch(r.Context(), run, patch); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"updating task: %s"}`, err), http.StatusInternalServerError)
		return
	}

	slog.Info("Clarification answer submitted", "task", taskID, "tenant", tenant.TenantName)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "accepted"})
}

// cancelTask handles DELETE /v1/tasks/{id}.
func (s *ExternalAPIServer) cancelTask(w http.ResponseWriter, r *http.Request, taskID string) {
	tenant, ok := TenantFromContext(r.Context())
	if !ok {
		http.Error(w, `{"error":"no tenant identity"}`, http.StatusUnauthorized)
		return
	}

	run, err := findRunInNamespaces(r.Context(), s.crdClient, tenant, taskID)
	if err != nil {
		http.Error(w, `{"error":"task not found"}`, http.StatusNotFound)
		return
	}

	if run.Labels["agentorca.io/tenant"] != tenant.TenantName {
		http.Error(w, `{"error":"task not found"}`, http.StatusNotFound)
		return
	}

	// Only cancel running or pending tasks.
	if run.Status.Phase != agentorcav1alpha1.AgentRunPhasePending &&
		run.Status.Phase != agentorcav1alpha1.AgentRunPhaseRunning &&
		run.Status.Phase != agentorcav1alpha1.AgentRunPhaseWaitingForInput {
		http.Error(w, `{"error":"task is already in a terminal state"}`, http.StatusConflict)
		return
	}

	// Mark the run as failed with a cancellation reason.
	patch := client.MergeFrom(run.DeepCopy())
	now := metav1.Now()
	run.Status.Phase = agentorcav1alpha1.AgentRunPhaseFailed
	run.Status.FailureReason = "cancelled by tenant via external API"
	run.Status.CompletionTime = &now
	if err := s.crdClient.Status().Patch(r.Context(), run, patch); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"cancelling task: %s"}`, err), http.StatusInternalServerError)
		return
	}

	// Signal cancellation via Redis so the model-router sidecar aborts in-flight
	// LLM requests within ~1 second, rather than waiting for the next call.
	if s.store != nil {
		_ = s.store.SignalCancel(r.Context(), run.Namespace, run.Name)
	}

	slog.Info("Task cancelled", "task", taskID, "tenant", tenant.TenantName)
	w.WriteHeader(http.StatusNoContent)
}

// runToTaskResponse converts an AgentRun to a TaskResponse.
func (s *ExternalAPIServer) runToTaskResponse(run *agentorcav1alpha1.AgentRun) TaskResponse {
	resp := TaskResponse{
		ID:       run.Name,
		Agent:    run.Spec.AgentRef,
		Status:   string(run.Status.Phase),
		Output:   run.Status.Output,
		SpendUSD: run.Status.SpendUSD,
		Links: &TaskLinks{
			Self:   "/v1/tasks/" + run.Name,
			Stream: "/v1/tasks/" + run.Name + "/stream",
		},
	}
	if !run.CreationTimestamp.IsZero() {
		resp.CreatedAt = run.CreationTimestamp.Format(time.RFC3339)
	}
	if run.Status.CompletionTime != nil {
		resp.CompletedAt = run.Status.CompletionTime.Format(time.RFC3339)
	}
	// Reconstruct metadata from annotations.
	meta := make(map[string]string)
	for k, v := range run.Annotations {
		if after, ok := strings.CutPrefix(k, "agentorca.io/meta-"); ok {
			meta[after] = v
		}
	}
	if len(meta) > 0 {
		resp.Metadata = meta
	}
	return resp
}

// mustJSON marshals v to JSON, returning "{}" on error.
func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}
