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

// Package apiserver provides the operator's internal HTTP API (localhost:8082).
// This API is called exclusively by the tool executor running inside agent pods:
//
//	POST /agentrun/{namespace}          — create a child AgentRun
//	GET  /agentrun/{namespace}/{name}   — get AgentRun status
//
// The server is NOT exposed outside the cluster. It binds to 0.0.0.0:8082 so
// that agent pods in the same namespace can reach it via the operator Service.
// All requests must carry a valid Kubernetes SA token in the Authorization header;
// the server validates it with a TokenReview before processing.
package apiserver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/acpfs"
	"github.com/heddles/agent-orca/internal/rag"
	"github.com/heddles/agent-orca/internal/security"
)

// Server handles the operator's internal API for agent-to-agent communication.
type Server struct {
	k8s       kubernetes.Interface
	crdClient client.Client
	// fsRelay carries ACP fs/* requests from agent pods to the workstation-side
	// bridge. nil disables filesystem access (fs requests fail with 501).
	fsRelay acpfs.Relay
	// approvals backs tool-call approvals (tool_approval_api.go). nil when there
	// is no PostgreSQL store; such requests fail with 503.
	approvals ToolApprovalStore
	// approvalNotifier tells approvers about new requests. Optional.
	approvalNotifier ApprovalNotifier
	// routingEvents stores routing telemetry (routing_events_api.go). nil drops
	// posted events.
	routingEvents RoutingEventStore
}

// New creates a new API server.
func New(k8s kubernetes.Interface, crdClient client.Client) *Server {
	return &Server{k8s: k8s, crdClient: crdClient}
}

// internalAPIRequestTimeout bounds the total time for an internal API request.
// Agent pods communicate with the operator over this server; a stuck handler
// should not hold connections open indefinitely.
const internalAPIRequestTimeout = 60 * time.Second

// Handler returns an http.Handler for the internal API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/agentrun/", s.handleAgentRun)
	mux.HandleFunc("/workflow/", s.handleWorkflow)
	mux.HandleFunc("/knowledgebase/", s.handleKnowledgeBase)
	return requestTimeoutMiddleware(mux, internalAPIRequestTimeout)
}

// handleAgentRun dispatches POST (create) and GET (status) for AgentRun resources.
func (s *Server) handleAgentRun(w http.ResponseWriter, r *http.Request) { //nolint:gocyclo

	// Authenticate the caller via Kubernetes TokenReview.
	token := extractBearer(r)
	if token == "" {
		writeOperatorAuthFailure(w, true, nil)
		return
	}
	caller, err := s.validateToken(r.Context(), token)
	if err != nil {
		writeOperatorAuthFailure(w, false, err)
		return
	}

	// Path: /agentrun/{namespace} or /agentrun/{namespace}/{name}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/agentrun/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, "namespace required", http.StatusBadRequest)
		return
	}
	namespace := parts[0]

	switch {
	case r.Method == http.MethodPost && len(parts) == 1:
		s.createAgentRun(w, r, namespace)
	case r.Method == http.MethodGet && len(parts) == 2:
		s.getAgentRun(w, r, namespace, parts[1])
	case r.Method == http.MethodPost && len(parts) == 3 && parts[2] == "handoff":
		s.handoffAgentRun(w, r, namespace, parts[1])
	case r.Method == http.MethodPost && len(parts) == 3 && parts[2] == "clarify":
		s.clarifyAgentRun(w, r, namespace, parts[1])
	case r.Method == http.MethodPost && len(parts) == 3 && parts[2] == "loop-detected":
		s.loopDetectedAgentRun(w, r, namespace, parts[1])
	case r.Method == http.MethodPost && len(parts) == 3 && parts[2] == "tool-approval":
		s.requestToolApproval(w, r, caller, namespace, parts[1])
	case r.Method == http.MethodGet && len(parts) == 4 && parts[2] == "tool-approval":
		s.getToolApproval(w, r, caller, namespace, parts[1], parts[3])
	case r.Method == http.MethodPost && len(parts) == 3 && parts[2] == "budget":
		s.reportBudget(w, r, caller, namespace, parts[1])
	case r.Method == http.MethodGet && len(parts) == 3 && parts[2] == "budget":
		s.getBudget(w, r, caller, namespace, parts[1])
	case r.Method == http.MethodPost && len(parts) == 3 && parts[2] == "propose-step":
		s.proposeWorkflowStep(w, r, namespace, parts[1])
	case r.Method == http.MethodPost && len(parts) == 3 && parts[2] == "done":
		s.doneAgentRun(w, r, namespace, parts[1])
	case r.Method == http.MethodPost && len(parts) == 3 && parts[2] == "fail":
		s.failAgentRun(w, r, namespace, parts[1])
	case r.Method == http.MethodPost && len(parts) == 3 && parts[2] == "context":
		s.updateContextAgentRun(w, r, namespace, parts[1])
	case r.Method == http.MethodPost && len(parts) == 3 && parts[2] == "emit-event":
		s.emitEventAgentRun(w, r, namespace, parts[1])
	case r.Method == http.MethodPost && len(parts) == 3 && parts[2] == "route":
		s.recordRoutingDecision(w, r, namespace, parts[1])
	case r.Method == http.MethodPost && len(parts) == 3 && parts[2] == "routing-events":
		s.recordRoutingEvents(w, r, caller, namespace, parts[1])
	case r.Method == http.MethodPost && len(parts) == 3 && parts[2] == "fs":
		s.enqueueFSRequest(w, r, namespace, parts[1])
	case r.Method == http.MethodGet && len(parts) == 4 && parts[2] == "fs":
		s.takeFSResponse(w, r, namespace, parts[1], parts[3])
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

// createAgentRun deserializes an AgentRun from the request body and creates it.
func (s *Server) createAgentRun(w http.ResponseWriter, r *http.Request, namespace string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "reading body", http.StatusBadRequest)
		return
	}

	var run agentorcav1alpha1.AgentRun
	if err := json.Unmarshal(body, &run); err != nil {
		http.Error(w, fmt.Sprintf("invalid JSON: %v", err), http.StatusBadRequest)
		return
	}

	// Force the namespace from the URL path — never trust the body.
	run.Namespace = namespace

	// Validate required fields.
	if run.Spec.AgentRef == "" || run.Spec.Input == "" {
		http.Error(w, "agentRef and input are required", http.StatusBadRequest)
		return
	}

	// Ensure operator-managed label is present.
	if run.Labels == nil {
		run.Labels = make(map[string]string)
	}
	run.Labels[security.LabelManagedBy] = security.ManagedByValue

	if err := s.crdClient.Create(r.Context(), &run); err != nil {
		slog.Error("creating child AgentRun", "err", err, "run", run.Name)
		http.Error(w, fmt.Sprintf("creating AgentRun: %v", err), http.StatusInternalServerError)
		return
	}

	slog.Info("child AgentRun created via internal API", "name", run.Name, "ns", namespace)

	// Update the parent's ChildRunRefs so the parent has an authoritative child list.
	if run.Spec.ParentRunRef != "" {
		var parent agentorcav1alpha1.AgentRun
		if err := s.crdClient.Get(r.Context(), client.ObjectKey{
			Name: run.Spec.ParentRunRef, Namespace: namespace,
		}, &parent); err == nil {
			parent.Status.ChildRunRefs = append(parent.Status.ChildRunRefs, run.Name)
			if err := s.crdClient.Status().Update(r.Context(), &parent); err != nil {
				slog.Warn("updating parent ChildRunRefs", "parent", run.Spec.ParentRunRef, "child", run.Name, "err", err)
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(run)
}

// getAgentRun retrieves an AgentRun by name and returns its current state.
func (s *Server) getAgentRun(w http.ResponseWriter, r *http.Request, namespace, name string) {
	var run agentorcav1alpha1.AgentRun
	if err := s.crdClient.Get(r.Context(), client.ObjectKey{Name: name, Namespace: namespace}, &run); err != nil {
		http.Error(w, fmt.Sprintf("getting AgentRun: %v", err), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(run)
}

// handoffAgentRun marks an AgentRun as HandedOff and records the target agent name.
// Called by the model-router sidecar when the _handoff built-in tool completes.
func (s *Server) handoffAgentRun(w http.ResponseWriter, r *http.Request, namespace, name string) {
	var req struct {
		HandoffTarget string `json:"handoffTarget"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "reading body", http.StatusBadRequest)
		return
	}
	if err := json.Unmarshal(body, &req); err != nil || req.HandoffTarget == "" {
		http.Error(w, "handoffTarget is required", http.StatusBadRequest)
		return
	}

	var run agentorcav1alpha1.AgentRun
	if err := s.crdClient.Get(r.Context(), client.ObjectKey{Name: name, Namespace: namespace}, &run); err != nil {
		http.Error(w, fmt.Sprintf("getting AgentRun: %v", err), http.StatusNotFound)
		return
	}

	run.Status.Phase = agentorcav1alpha1.AgentRunPhaseHandedOff
	run.Status.HandoffTarget = req.HandoffTarget
	if err := s.crdClient.Status().Update(r.Context(), &run); err != nil {
		http.Error(w, fmt.Sprintf("updating AgentRun status: %v", err), http.StatusInternalServerError)
		return
	}

	slog.Info("AgentRun handed off", "run", name, "target", req.HandoffTarget)
	w.WriteHeader(http.StatusNoContent)
}

// clarifyAgentRun marks an AgentRun as WaitingForInput and records the clarifying question.
// Called by the model-router sidecar when the _clarify built-in tool is invoked.
func (s *Server) clarifyAgentRun(w http.ResponseWriter, r *http.Request, namespace, name string) {
	var req struct {
		Question string `json:"question"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "reading body", http.StatusBadRequest)
		return
	}
	if err := json.Unmarshal(body, &req); err != nil || req.Question == "" {
		http.Error(w, "question is required", http.StatusBadRequest)
		return
	}

	var run agentorcav1alpha1.AgentRun
	if err := s.crdClient.Get(r.Context(), client.ObjectKey{Name: name, Namespace: namespace}, &run); err != nil {
		http.Error(w, fmt.Sprintf("getting AgentRun: %v", err), http.StatusNotFound)
		return
	}

	patch := client.MergeFrom(run.DeepCopy())
	now := metav1.Now()
	run.Status.Phase = agentorcav1alpha1.AgentRunPhaseWaitingForInput
	run.Status.ClarifyQuestion = req.Question
	run.Status.ClarifyAnswer = ""
	run.Status.WaitingSince = &now
	if err := s.crdClient.Status().Patch(r.Context(), &run, patch); err != nil {
		http.Error(w, fmt.Sprintf("patching AgentRun status: %v", err), http.StatusInternalServerError)
		return
	}

	slog.Info("AgentRun waiting for input", "run", name, "question", req.Question)
	w.WriteHeader(http.StatusNoContent)
}

// loopDetectedAgentRun marks an AgentRun as Failed with LoopDetected info.
// Called by the model-router sidecar when a safeguard trip is detected.
func (s *Server) loopDetectedAgentRun(w http.ResponseWriter, r *http.Request, namespace, name string) {
	var req agentorcav1alpha1.LoopDetectedInfo
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "reading body", http.StatusBadRequest)
		return
	}
	if err := json.Unmarshal(body, &req); err != nil || req.Reason == "" {
		http.Error(w, "reason is required", http.StatusBadRequest)
		return
	}

	var run agentorcav1alpha1.AgentRun
	if err := s.crdClient.Get(r.Context(), client.ObjectKey{Name: name, Namespace: namespace}, &run); err != nil {
		http.Error(w, fmt.Sprintf("getting AgentRun: %v", err), http.StatusNotFound)
		return
	}

	run.Status.LoopDetected = &req
	if err := s.crdClient.Status().Update(r.Context(), &run); err != nil {
		http.Error(w, fmt.Sprintf("updating AgentRun status: %v", err), http.StatusInternalServerError)
		return
	}

	slog.Info("AgentRun safeguard tripped", "run", name, "reason", req.Reason)
	w.WriteHeader(http.StatusNoContent)
}

// maxRunOutputBytes caps AgentRun.Status.Output: the CRD lives in etcd, so a
// long _done payload is clipped there. The untruncated text still reaches live
// consumers through the run's `done` trace event.
const maxRunOutputBytes = 10240

// runOutputTruncationMarker tells a consumer that reads only the CRD that the
// output was clipped, instead of silently handing it a prefix.
const runOutputTruncationMarker = "\n\n[output truncated]"

// truncateRunOutput clips out to maxRunOutputBytes (marker included) on a rune
// boundary so a multi-byte character is never split.
func truncateRunOutput(out string) string {
	if len(out) <= maxRunOutputBytes {
		return out
	}
	cut := maxRunOutputBytes - len(runOutputTruncationMarker)
	for cut > 0 && !utf8.RuneStart(out[cut]) {
		cut--
	}
	return out[:cut] + runOutputTruncationMarker
}

// doneAgentRun marks an AgentRun as Succeeded with the agent's explicit output.
// Called by the model-router sidecar when the _done built-in tool is invoked.
func (s *Server) doneAgentRun(w http.ResponseWriter, r *http.Request, namespace, name string) {
	var req struct {
		Output string `json:"output"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "reading body", http.StatusBadRequest)
		return
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, fmt.Sprintf("invalid JSON: %v", err), http.StatusBadRequest)
		return
	}

	var run agentorcav1alpha1.AgentRun
	if err := s.crdClient.Get(r.Context(), client.ObjectKey{Name: name, Namespace: namespace}, &run); err != nil {
		http.Error(w, fmt.Sprintf("getting AgentRun: %v", err), http.StatusNotFound)
		return
	}

	patch := client.MergeFrom(run.DeepCopy())
	now := metav1.Now()
	run.Status.Phase = agentorcav1alpha1.AgentRunPhaseSucceeded
	run.Status.Output = truncateRunOutput(req.Output)
	run.Status.CompletionTime = &now
	if err := s.crdClient.Status().Patch(r.Context(), &run, patch); err != nil {
		http.Error(w, fmt.Sprintf("patching AgentRun status: %v", err), http.StatusInternalServerError)
		return
	}

	slog.Info("AgentRun marked done by agent", "run", name)
	w.WriteHeader(http.StatusNoContent)
}

// failAgentRun marks an AgentRun as Failed with the agent's explicit reason.
// Called by the model-router sidecar when the _fail built-in tool is invoked.
func (s *Server) failAgentRun(w http.ResponseWriter, r *http.Request, namespace, name string) {
	var req struct {
		Reason    string `json:"reason"`
		Retryable bool   `json:"retryable"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "reading body", http.StatusBadRequest)
		return
	}
	if err := json.Unmarshal(body, &req); err != nil || req.Reason == "" {
		http.Error(w, "reason is required", http.StatusBadRequest)
		return
	}

	var run agentorcav1alpha1.AgentRun
	if err := s.crdClient.Get(r.Context(), client.ObjectKey{Name: name, Namespace: namespace}, &run); err != nil {
		http.Error(w, fmt.Sprintf("getting AgentRun: %v", err), http.StatusNotFound)
		return
	}

	patch := client.MergeFrom(run.DeepCopy())
	now := metav1.Now()
	run.Status.Phase = agentorcav1alpha1.AgentRunPhaseFailed
	run.Status.FailureReason = req.Reason
	run.Status.CompletionTime = &now
	if err := s.crdClient.Status().Patch(r.Context(), &run, patch); err != nil {
		http.Error(w, fmt.Sprintf("patching AgentRun status: %v", err), http.StatusInternalServerError)
		return
	}

	slog.Info("AgentRun marked failed by agent", "run", name, "reason", req.Reason)
	w.WriteHeader(http.StatusNoContent)
}

// maxRoutingDecisions bounds AgentRun.status.routingDecisions. A long run
// makes one decision per LLM call; unbounded, the list would grow the object
// toward the etcd size limit. The full history lives in routing telemetry.
const maxRoutingDecisions = 100

// routingDecisionBackoff retries a routing-decision append that lost a write
// race. client-go's default (5 steps, 10ms) is tuned for one competing
// writer; a busy run can have several decisions in flight.
var routingDecisionBackoff = wait.Backoff{Steps: 12, Duration: 10 * time.Millisecond, Factor: 1.5, Jitter: 0.2}

// recordRoutingDecision appends a runtime routing decision to the AgentRun status.
// Called by the model-router sidecar after each selection so the UI can show
// which model was actually chosen, including meta-router escalations and
// fallbacks.
func (s *Server) recordRoutingDecision(w http.ResponseWriter, r *http.Request, namespace, name string) {
	var req struct {
		Model            string `json:"model"`
		Provider         string `json:"provider"`
		Strategy         string `json:"strategy"`
		Reason           string `json:"reason"`
		Confidence       string `json:"confidence"`
		DecisionID       string `json:"decisionID"`
		Path             string `json:"path"`
		Category         string `json:"category"`
		ParentDecisionID string `json:"parentDecisionID"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		http.Error(w, "reading body", http.StatusBadRequest)
		return
	}
	if err := json.Unmarshal(body, &req); err != nil || req.Provider == "" {
		http.Error(w, "provider is required", http.StatusBadRequest)
		return
	}

	// Several decisions can land at once (a fallback post races the next
	// request's), so the append is a read-modify-write guarded by the object's
	// resourceVersion: a conflicting writer makes this one retry on fresh state.
	var notFound bool
	err = retry.RetryOnConflict(routingDecisionBackoff, func() error {
		var run agentorcav1alpha1.AgentRun
		if err := s.crdClient.Get(r.Context(), client.ObjectKey{Name: name, Namespace: namespace}, &run); err != nil {
			if apierrors.IsNotFound(err) {
				notFound = true
			}
			return err
		}
		patch := client.MergeFromWithOptions(run.DeepCopy(), client.MergeFromWithOptimisticLock{})
		now := metav1.Now()
		// Decisions the controller seeded count toward the total too.
		run.Status.RoutingDecisionsTotal = max(run.Status.RoutingDecisionsTotal, len(run.Status.RoutingDecisions)) + 1
		run.Status.RoutingDecisions = append(run.Status.RoutingDecisions, agentorcav1alpha1.RoutingDecision{
			Model:            req.Model,
			Provider:         req.Provider,
			Strategy:         req.Strategy,
			Reason:           req.Reason,
			Confidence:       req.Confidence,
			Timestamp:        &now,
			DecisionID:       req.DecisionID,
			Path:             req.Path,
			Category:         req.Category,
			ParentDecisionID: req.ParentDecisionID,
		})
		if extra := len(run.Status.RoutingDecisions) - maxRoutingDecisions; extra > 0 {
			run.Status.RoutingDecisions = slices.Clone(run.Status.RoutingDecisions[extra:])
		}
		return s.crdClient.Status().Patch(r.Context(), &run, patch)
	})
	if err != nil {
		if notFound {
			http.Error(w, fmt.Sprintf("getting AgentRun: %v", err), http.StatusNotFound)
			return
		}
		http.Error(w, fmt.Sprintf("patching AgentRun status: %v", err), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// updateContextAgentRun updates the context token counts for an AgentRun.
// Called by the model-router sidecar after each LLM response to track context usage.
// Also updates the parent AgentDeployment's context if this run belongs to one.
func (s *Server) updateContextAgentRun(w http.ResponseWriter, r *http.Request, namespace, name string) {
	var req struct {
		ContextUsedTokens int `json:"contextUsedTokens"`
		MaxContextTokens  int `json:"maxContextTokens"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		http.Error(w, "reading body", http.StatusBadRequest)
		return
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, fmt.Sprintf("invalid JSON: %v", err), http.StatusBadRequest)
		return
	}

	var run agentorcav1alpha1.AgentRun
	if err := s.crdClient.Get(r.Context(), client.ObjectKey{Name: name, Namespace: namespace}, &run); err != nil {
		http.Error(w, fmt.Sprintf("getting AgentRun: %v", err), http.StatusNotFound)
		return
	}

	patch := client.MergeFrom(run.DeepCopy())
	run.Status.ContextUsedTokens = req.ContextUsedTokens
	run.Status.MaxContextTokens = req.MaxContextTokens
	if err := s.crdClient.Status().Patch(r.Context(), &run, patch); err != nil {
		http.Error(w, fmt.Sprintf("patching AgentRun status: %v", err), http.StatusInternalServerError)
		return
	}

	// Also update parent AgentDeployment's context if this is a chat run.
	if deploymentName, ok := run.Labels["agentorca.io/deployment"]; ok && deploymentName != "" {
		var deployment agentorcav1alpha1.AgentDeployment
		if err := s.crdClient.Get(r.Context(), client.ObjectKey{Name: deploymentName, Namespace: namespace}, &deployment); err == nil {
			depPatch := client.MergeFrom(deployment.DeepCopy())
			deployment.Status.ContextUsedTokens = req.ContextUsedTokens
			deployment.Status.MaxContextTokens = req.MaxContextTokens
			if err := s.crdClient.Status().Patch(r.Context(), &deployment, depPatch); err != nil {
				slog.Warn("patching AgentDeployment context status", "deployment", deploymentName, "err", err)
			}
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

// emitEventAgentRun creates a Kubernetes Event on the AgentRun for observability.
// Called by the model-router sidecar when the _emit_event built-in tool is invoked.
func (s *Server) emitEventAgentRun(w http.ResponseWriter, r *http.Request, namespace, name string) {
	var req struct {
		EventType string `json:"eventType"`
		Message   string `json:"message"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "reading body", http.StatusBadRequest)
		return
	}
	if err := json.Unmarshal(body, &req); err != nil || req.EventType == "" || req.Message == "" {
		http.Error(w, "eventType and message are required", http.StatusBadRequest)
		return
	}

	var run agentorcav1alpha1.AgentRun
	if err := s.crdClient.Get(r.Context(), client.ObjectKey{Name: name, Namespace: namespace}, &run); err != nil {
		http.Error(w, fmt.Sprintf("getting AgentRun: %v", err), http.StatusNotFound)
		return
	}

	now := metav1.Now()
	event := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: name + "-",
			Namespace:    namespace,
		},
		InvolvedObject: corev1.ObjectReference{
			Kind:       "AgentRun",
			Name:       name,
			Namespace:  namespace,
			UID:        run.UID,
			APIVersion: "agentorca.agentorca.io/v1alpha1",
		},
		Type:                corev1.EventTypeNormal,
		Reason:              req.EventType,
		Message:             req.Message,
		FirstTimestamp:      now,
		LastTimestamp:       now,
		Count:               1,
		ReportingController: "agentorca.io/agent-orca",
		ReportingInstance:   name,
	}
	if _, err := s.k8s.CoreV1().Events(namespace).Create(r.Context(), event, metav1.CreateOptions{}); err != nil {
		http.Error(w, fmt.Sprintf("creating event: %v", err), http.StatusInternalServerError)
		return
	}

	slog.Info("agent event emitted", "run", name, "eventType", req.EventType)
	w.WriteHeader(http.StatusNoContent)
}

// handleWorkflow dispatches requests under /workflow/{namespace}/{name}/...
func (s *Server) handleWorkflow(w http.ResponseWriter, r *http.Request) {
	token := extractBearer(r)
	if token == "" {
		writeOperatorAuthFailure(w, true, nil)
		return
	}
	if _, err := s.validateToken(r.Context(), token); err != nil {
		writeOperatorAuthFailure(w, false, err)
		return
	}

	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/workflow/"), "/")
	namespace := parts[0]

	switch {
	case r.Method == http.MethodPost && len(parts) == 1:
		s.createAgentWorkflow(w, r, namespace)
	case r.Method == http.MethodGet && len(parts) == 1:
		s.listAgentWorkflows(w, r, namespace)
	case r.Method == http.MethodGet && len(parts) == 2:
		s.getAgentWorkflow(w, r, namespace, parts[1])
	case r.Method == http.MethodPost && len(parts) == 3 && parts[2] == "approve-proposal":
		s.approveWorkflowProposal(w, r, namespace, parts[1])
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

// getAgentWorkflow retrieves an AgentWorkflow by name and returns its current state.
func (s *Server) getAgentWorkflow(w http.ResponseWriter, r *http.Request, namespace, name string) {
	var wf agentorcav1alpha1.AgentWorkflow
	if err := s.crdClient.Get(r.Context(), client.ObjectKey{Name: name, Namespace: namespace}, &wf); err != nil {
		http.Error(w, fmt.Sprintf("getting AgentWorkflow: %v", err), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(wf)
}

// listAgentWorkflows lists AgentWorkflows in a namespace.
func (s *Server) listAgentWorkflows(w http.ResponseWriter, r *http.Request, namespace string) {
	var wfList agentorcav1alpha1.AgentWorkflowList
	if err := s.crdClient.List(r.Context(), &wfList, client.InNamespace(namespace)); err != nil {
		http.Error(w, fmt.Sprintf("listing AgentWorkflows: %v", err), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(wfList)
}

// createAgentWorkflow creates an AgentWorkflow from the request body.
// Called by the model-router sidecar when the _create_workflow built-in tool is invoked.
func (s *Server) createAgentWorkflow(w http.ResponseWriter, r *http.Request, namespace string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "reading body", http.StatusBadRequest)
		return
	}

	var wf agentorcav1alpha1.AgentWorkflow
	if err := json.Unmarshal(body, &wf); err != nil {
		http.Error(w, fmt.Sprintf("invalid JSON: %v", err), http.StatusBadRequest)
		return
	}

	// Force the namespace from the URL path — never trust the body.
	wf.Namespace = namespace

	// Validate required fields.
	if len(wf.Spec.Steps) == 0 {
		http.Error(w, "steps are required", http.StatusBadRequest)
		return
	}
	for _, step := range wf.Spec.Steps {
		if step.AgentRef == "" || step.Input == "" {
			http.Error(w, "each step requires agentRef and input", http.StatusBadRequest)
			return
		}
	}

	// Ensure operator-managed label is present.
	if wf.Labels == nil {
		wf.Labels = make(map[string]string)
	}
	wf.Labels[security.LabelManagedBy] = security.ManagedByValue

	if err := s.crdClient.Create(r.Context(), &wf); err != nil {
		slog.Error("creating AgentWorkflow", "err", err, "wf", wf.Name)
		http.Error(w, fmt.Sprintf("creating AgentWorkflow: %v", err), http.StatusInternalServerError)
		return
	}

	slog.Info("AgentWorkflow created via internal API", "name", wf.Name, "ns", namespace)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(wf)
}

// proposeWorkflowStep handles POST /agentrun/{namespace}/{name}/propose-step.
// It looks up the parent AgentWorkflow via the agentorca.io/workflow label on the AgentRun,
// validates the proposal against the workflow's AdaptivePolicy, and either approves or rejects it.
func (s *Server) proposeWorkflowStep(w http.ResponseWriter, r *http.Request, namespace, runName string) {
	var proposal struct {
		Name      string   `json:"name"`
		AgentRef  string   `json:"agentRef"`
		Input     string   `json:"input"`
		Reason    string   `json:"reason"`
		DependsOn []string `json:"dependsOn"`
		IsLoop    bool     `json:"isLoop"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "reading body", http.StatusBadRequest)
		return
	}
	if err := json.Unmarshal(body, &proposal); err != nil {
		http.Error(w, fmt.Sprintf("invalid JSON: %v", err), http.StatusBadRequest)
		return
	}
	if proposal.Name == "" || proposal.AgentRef == "" {
		http.Error(w, "name and agentRef are required", http.StatusBadRequest)
		return
	}

	// Look up the AgentRun to find its parent workflow.
	var run agentorcav1alpha1.AgentRun
	if err := s.crdClient.Get(r.Context(), client.ObjectKey{Name: runName, Namespace: namespace}, &run); err != nil {
		http.Error(w, fmt.Sprintf("getting AgentRun: %v", err), http.StatusNotFound)
		return
	}
	workflowName := run.Labels["agentorca.io/workflow"]
	if workflowName == "" {
		http.Error(w, "this run is not part of a workflow", http.StatusBadRequest)
		return
	}
	stepName := run.Labels["agentorca.io/workflow-step"]

	// Get the workflow.
	var wf agentorcav1alpha1.AgentWorkflow
	if err := s.crdClient.Get(r.Context(), client.ObjectKey{Name: workflowName, Namespace: namespace}, &wf); err != nil {
		http.Error(w, fmt.Sprintf("getting AgentWorkflow: %v", err), http.StatusNotFound)
		return
	}

	now := metav1.Now()
	proposedStep := agentorcav1alpha1.WorkflowStep{
		Name:      proposal.Name,
		AgentRef:  proposal.AgentRef,
		Input:     proposal.Input,
		DependsOn: proposal.DependsOn,
	}
	entry := agentorcav1alpha1.StepProposal{
		ProposingStep: stepName,
		ProposedStep:  proposedStep,
		Timestamp:     now,
	}

	// Validate against AdaptivePolicy.
	policy := wf.Spec.AdaptivePolicy
	if policy == nil {
		entry.Reason = "adaptive workflows are not enabled on this workflow"
		wf.Status.ProposalLog = append(wf.Status.ProposalLog, entry)
		_ = s.crdClient.Status().Update(r.Context(), &wf)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"approved": false, "reason": entry.Reason})
		return
	}

	// Check max dynamic steps.
	if len(wf.Status.DynamicSteps) >= policy.MaxDynamicSteps {
		entry.Reason = fmt.Sprintf("max dynamic steps reached (%d)", policy.MaxDynamicSteps)
		wf.Status.ProposalLog = append(wf.Status.ProposalLog, entry)
		_ = s.crdClient.Status().Update(r.Context(), &wf)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"approved": false, "reason": entry.Reason})
		return
	}

	// Check loop policy.
	if proposal.IsLoop && !policy.AllowLoops {
		entry.Reason = "loops are not allowed by adaptive policy"
		wf.Status.ProposalLog = append(wf.Status.ProposalLog, entry)
		_ = s.crdClient.Status().Update(r.Context(), &wf)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"approved": false, "reason": entry.Reason})
		return
	}

	// Check loop iteration count.
	if proposal.IsLoop && policy.MaxLoopIterations > 0 {
		loopCount := 0
		for _, ds := range wf.Status.DynamicSteps {
			if strings.HasPrefix(ds.Name, stepName+"-loop-") {
				loopCount++
			}
		}
		if loopCount >= policy.MaxLoopIterations {
			entry.Reason = fmt.Sprintf("max loop iterations reached (%d)", policy.MaxLoopIterations)
			wf.Status.ProposalLog = append(wf.Status.ProposalLog, entry)
			_ = s.crdClient.Status().Update(r.Context(), &wf)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"approved": false, "reason": entry.Reason})
			return
		}
		// Rename the step to encode loop iteration.
		proposedStep.Name = fmt.Sprintf("%s-loop-%d", stepName, loopCount+1)
		proposedStep.DependsOn = []string{stepName}
		entry.ProposedStep = proposedStep
	}

	// Check allowlist.
	agentAllowed := len(policy.AllowedAgentRefs) == 0
	if slices.Contains(policy.AllowedAgentRefs, proposal.AgentRef) {
		agentAllowed = true
	}

	if !agentAllowed {
		if policy.RequireApprovalForUnlisted {
			// Pause for human approval.
			entry.PendingHumanApproval = true
			entry.Reason = fmt.Sprintf("agent %q not in allowlist; awaiting human approval", proposal.AgentRef)
			wf.Status.ProposalLog = append(wf.Status.ProposalLog, entry)
			_ = s.crdClient.Status().Update(r.Context(), &wf)
			slog.Info("workflow step proposal pending human approval",
				"workflow", workflowName, "step", proposal.Name, "agent", proposal.AgentRef)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"approved":             false,
				"pendingHumanApproval": true,
				"reason":               entry.Reason,
			})
			return
		}
		entry.Reason = fmt.Sprintf("agent %q not in allowlist", proposal.AgentRef)
		wf.Status.ProposalLog = append(wf.Status.ProposalLog, entry)
		_ = s.crdClient.Status().Update(r.Context(), &wf)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"approved": false, "reason": entry.Reason})
		return
	}

	// Approved — append to dynamic steps.
	entry.Approved = true
	entry.Reason = "policy check passed"
	wf.Status.DynamicSteps = append(wf.Status.DynamicSteps, proposedStep)
	wf.Status.DynamicStepStatuses = append(wf.Status.DynamicStepStatuses, agentorcav1alpha1.WorkflowStepStatus{
		Name:   proposedStep.Name,
		Phase:  agentorcav1alpha1.WorkflowStepPhasePending,
		Source: "dynamic",
	})
	wf.Status.ProposalLog = append(wf.Status.ProposalLog, entry)

	if err := s.crdClient.Status().Update(r.Context(), &wf); err != nil {
		http.Error(w, fmt.Sprintf("updating workflow status: %v", err), http.StatusInternalServerError)
		return
	}

	slog.Info("workflow step proposal approved",
		"workflow", workflowName, "step", proposedStep.Name, "agent", proposal.AgentRef)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"approved": true, "stepName": proposedStep.Name})
}

// approveWorkflowProposal handles POST /workflow/{namespace}/{name}/approve-proposal.
// A human calls this via the UI to approve or reject a pending proposal.
func (s *Server) approveWorkflowProposal(w http.ResponseWriter, r *http.Request, namespace, workflowName string) {
	var req struct {
		ProposalIndex int  `json:"proposalIndex"`
		Approved      bool `json:"approved"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "reading body", http.StatusBadRequest)
		return
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, fmt.Sprintf("invalid JSON: %v", err), http.StatusBadRequest)
		return
	}

	var wf agentorcav1alpha1.AgentWorkflow
	if err := s.crdClient.Get(r.Context(), client.ObjectKey{Name: workflowName, Namespace: namespace}, &wf); err != nil {
		http.Error(w, fmt.Sprintf("getting AgentWorkflow: %v", err), http.StatusNotFound)
		return
	}

	if req.ProposalIndex < 0 || req.ProposalIndex >= len(wf.Status.ProposalLog) {
		http.Error(w, "invalid proposalIndex", http.StatusBadRequest)
		return
	}
	proposal := &wf.Status.ProposalLog[req.ProposalIndex]
	if !proposal.PendingHumanApproval {
		http.Error(w, "proposal is not pending human approval", http.StatusBadRequest)
		return
	}

	proposal.PendingHumanApproval = false
	proposal.Approved = req.Approved
	if req.Approved {
		proposal.Reason = "approved by human"
		wf.Status.DynamicSteps = append(wf.Status.DynamicSteps, proposal.ProposedStep)
		wf.Status.DynamicStepStatuses = append(wf.Status.DynamicStepStatuses, agentorcav1alpha1.WorkflowStepStatus{
			Name:   proposal.ProposedStep.Name,
			Phase:  agentorcav1alpha1.WorkflowStepPhasePending,
			Source: "dynamic",
		})
	} else {
		proposal.Reason = "rejected by human"
	}

	if err := s.crdClient.Status().Update(r.Context(), &wf); err != nil {
		http.Error(w, fmt.Sprintf("updating workflow status: %v", err), http.StatusInternalServerError)
		return
	}

	slog.Info("workflow proposal human decision", "workflow", workflowName,
		"step", proposal.ProposedStep.Name, "approved", req.Approved)
	w.WriteHeader(http.StatusNoContent)
}

// handleKnowledgeBase dispatches requests under /knowledgebase/{namespace}/{name}/...
func (s *Server) handleKnowledgeBase(w http.ResponseWriter, r *http.Request) {
	token := extractBearer(r)
	if token == "" {
		writeOperatorAuthFailure(w, true, nil)
		return
	}
	username, err := s.validateToken(r.Context(), token)
	if err != nil {
		writeOperatorAuthFailure(w, false, err)
		return
	}

	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/knowledgebase/"), "/")
	if len(parts) < 3 {
		http.Error(w, "namespace, name, and action required", http.StatusBadRequest)
		return
	}
	namespace, name, action := parts[0], parts[1], parts[2]

	// Enforce KB-managed access control: verify the calling agent is in AllowedAgents.
	if err := s.authorizeKBAccess(r.Context(), username, namespace, name); err != nil {
		slog.Warn("KB access denied", "username", username, "kb", name, "err", err)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	switch {
	case r.Method == http.MethodPost && action == "search":
		s.ragSearch(w, r, namespace, name)
	case r.Method == http.MethodPost && action == "ingest":
		s.ragIngest(w, r, namespace, name)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

// authorizeKBAccess checks that the agent identified by saUsername is in KB.Spec.AllowedAgents.
// saUsername is the TokenReview user — "system:serviceaccount:<ns>:<sa-name>".
// The SA must carry the label agentorca.io/agent set by the operator when it creates the SA.
func (s *Server) authorizeKBAccess(ctx context.Context, saUsername, namespace, kbName string) error {
	// Parse SA name from "system:serviceaccount:<ns>:<sa-name>".
	userParts := strings.Split(saUsername, ":")
	if len(userParts) != 4 {
		return fmt.Errorf("unexpected SA username format: %q", saUsername)
	}
	saName := userParts[3]

	// Look up the SA to read the agentorca.io/agent label.
	sa, err := s.k8s.CoreV1().ServiceAccounts(namespace).Get(ctx, saName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("looking up SA %q: %w", saName, err)
	}
	agentName := sa.Labels["agentorca.io/agent"]
	if agentName == "" {
		return fmt.Errorf("SA %q has no agentorca.io/agent label", saName)
	}

	// Look up the KnowledgeBase and check AllowedAgents.
	var kb agentorcav1alpha1.KnowledgeBase
	if err := s.crdClient.Get(ctx, client.ObjectKey{Name: kbName, Namespace: namespace}, &kb); err != nil {
		return fmt.Errorf("getting KnowledgeBase %q: %w", kbName, err)
	}
	// nil AllowedAgents means the field has not been configured — allow all (backward compatible).
	// An explicitly empty slice means "deny all".
	if kb.Spec.AllowedAgents != nil && !slices.Contains(kb.Spec.AllowedAgents, agentName) {
		return fmt.Errorf("agent %q is not in KnowledgeBase %q allowedAgents", agentName, kbName)
	}
	return nil
}

// ragSearch handles POST /knowledgebase/{namespace}/{name}/search.
// Embeds the query, searches Qdrant, and returns top-K chunks.
// This is a stub — the full implementation requires a Qdrant client and embedding calls.
func (s *Server) ragSearch(w http.ResponseWriter, r *http.Request, namespace, name string) {
	var req struct {
		Query          string `json:"query"`
		TopK           int    `json:"topK"`
		CollectionName string `json:"collectionName"`
		VectorStoreURL string `json:"vectorStoreURL"`
		EmbeddingModel string `json:"embeddingModel"`
		Dimensions     int    `json:"dimensions"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "reading body", http.StatusBadRequest)
		return
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, fmt.Sprintf("invalid JSON: %v", err), http.StatusBadRequest)
		return
	}

	// Verify KnowledgeBase exists and is ready.
	var kb agentorcav1alpha1.KnowledgeBase
	if err := s.crdClient.Get(r.Context(), client.ObjectKey{Name: name, Namespace: namespace}, &kb); err != nil {
		http.Error(w, fmt.Sprintf("getting KnowledgeBase: %v", err), http.StatusNotFound)
		return
	}
	if !kb.Status.Ready {
		msg := "KnowledgeBase is not ready"
		if kb.Status.Message != "" {
			msg = fmt.Sprintf("KnowledgeBase is not ready: %s", kb.Status.Message)
		}
		http.Error(w, msg, http.StatusServiceUnavailable)
		return
	}

	// Resolve embedding provider from KnowledgeBase's ModelSelector.
	embedder, err := s.resolveEmbedder(r.Context(), namespace, &kb)
	if err != nil {
		http.Error(w, fmt.Sprintf("resolving embedding provider: %v", err), http.StatusInternalServerError)
		return
	}

	// Embed the query (forQuery=true → uses queryPrompt).
	vectors, err := embedder.Embed(r.Context(), []string{req.Query}, true)
	if err != nil {
		http.Error(w, fmt.Sprintf("embedding query: %v", err), http.StatusInternalServerError)
		return
	}

	// Search Qdrant.
	qClient, err := rag.NewQdrantClient(req.VectorStoreURL)
	if err != nil {
		http.Error(w, fmt.Sprintf("connecting to qdrant: %v", err), http.StatusInternalServerError)
		return
	}
	defer func() { _ = qClient.Close() }()

	topK := req.TopK
	if topK <= 0 {
		topK = 5
	}

	results, err := qClient.Search(r.Context(), req.CollectionName, vectors[0], topK)
	if err != nil {
		http.Error(w, fmt.Sprintf("searching qdrant: %v", err), http.StatusInternalServerError)
		return
	}

	slog.Info("RAG search completed", "kb", name, "query", req.Query, "results", len(results))

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"results": results,
	})
}

// ragIngest handles POST /knowledgebase/{namespace}/{name}/ingest.
// Chunks documents, embeds them, and upserts into Qdrant.
// This is a stub — the full implementation requires chunking, embedding, and Qdrant upsert.
func (s *Server) ragIngest(w http.ResponseWriter, r *http.Request, namespace, name string) {
	var req struct {
		Documents []struct {
			ID       string            `json:"id"`
			Content  string            `json:"content"`
			Metadata map[string]string `json:"metadata"`
		} `json:"documents"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 10<<20)) // 10MB limit for ingestion
	if err != nil {
		http.Error(w, "reading body", http.StatusBadRequest)
		return
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, fmt.Sprintf("invalid JSON: %v", err), http.StatusBadRequest)
		return
	}

	var kb agentorcav1alpha1.KnowledgeBase
	if err := s.crdClient.Get(r.Context(), client.ObjectKey{Name: name, Namespace: namespace}, &kb); err != nil {
		http.Error(w, fmt.Sprintf("getting KnowledgeBase: %v", err), http.StatusNotFound)
		return
	}
	if !kb.Status.Ready {
		msg := "KnowledgeBase is not ready"
		if kb.Status.Message != "" {
			msg = fmt.Sprintf("KnowledgeBase is not ready: %s", kb.Status.Message)
		}
		http.Error(w, msg, http.StatusServiceUnavailable)
		return
	}

	// Resolve embedding provider.
	embedder, err := s.resolveEmbedder(r.Context(), namespace, &kb)
	if err != nil {
		http.Error(w, fmt.Sprintf("resolving embedding provider: %v", err), http.StatusInternalServerError)
		return
	}

	// Build documents.
	docs := make([]rag.Document, len(req.Documents))
	for i, d := range req.Documents {
		meta := make(map[string]any, len(d.Metadata))
		for k, v := range d.Metadata {
			meta[k] = v
		}
		docs[i] = rag.Document{
			ID:       d.ID,
			Content:  d.Content,
			Metadata: meta,
		}
	}

	chunkCfg := rag.ChunkConfig{
		ChunkSize:    kb.Spec.Embedding.ChunkSize,
		ChunkOverlap: kb.Spec.Embedding.ChunkOverlap,
	}
	if chunkCfg.ChunkSize == 0 {
		chunkCfg = rag.DefaultChunkConfig()
	}

	dimensions := kb.Status.EmbeddingDimensions

	result, err := rag.IngestDocuments(
		r.Context(),
		docs,
		kb.Status.VectorStoreURL,
		kb.Status.CollectionName,
		uint64(dimensions),
		embedder,
		chunkCfg,
	)
	if err != nil {
		http.Error(w, fmt.Sprintf("ingesting documents: %v", err), http.StatusInternalServerError)
		return
	}

	slog.Info("RAG ingest completed", "kb", name, "docs", result.DocumentCount, "chunks", result.ChunkCount)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ingested": result.DocumentCount,
		"chunks":   result.ChunkCount,
	})
}

// resolveEmbedder looks up the KnowledgeBase's embedding ModelSelector and builds an EmbeddingClient.
// On first use it pins the resolved provider to kb.Status.EmbeddingModelProvider so that all
// subsequent calls use the same provider, preventing Qdrant dimension mismatches when the
// ModelSelector routes to different models with incompatible vector sizes.
func (s *Server) resolveEmbedder(ctx context.Context, namespace string, kb *agentorcav1alpha1.KnowledgeBase) (*rag.EmbeddingClient, error) {
	var mpName string
	if kb.Status.EmbeddingModelProvider != "" {
		// Already pinned — use the recorded provider directly.
		mpName = kb.Status.EmbeddingModelProvider
	} else {
		// First use — resolve from ModelSelector.
		var ms agentorcav1alpha1.ModelSelector
		if err := s.crdClient.Get(ctx, client.ObjectKey{Name: kb.Spec.Embedding.ModelSelectorRef, Namespace: namespace}, &ms); err != nil {
			return nil, fmt.Errorf("getting ModelSelector %q: %w", kb.Spec.Embedding.ModelSelectorRef, err)
		}
		if len(ms.Spec.Providers) == 0 {
			return nil, fmt.Errorf("ModelSelector %q has no providers", kb.Spec.Embedding.ModelSelectorRef)
		}
		mpName = ms.Spec.Providers[0].Name
	}

	var mp agentorcav1alpha1.ModelProvider
	if err := s.crdClient.Get(ctx, client.ObjectKey{Name: mpName, Namespace: namespace}, &mp); err != nil {
		return nil, fmt.Errorf("getting ModelProvider %q: %w", mpName, err)
	}

	endpoint := mp.Spec.BaseURL
	if endpoint == "" {
		endpoint = embeddingBaseURL(mp.Spec.LiteLLMModel)
	}

	// Read the API key from the Kubernetes Secret directly (operator doesn't have file mounts).
	secretNS := namespace
	if mp.Spec.CredentialsRef.Namespace != "" {
		secretNS = mp.Spec.CredentialsRef.Namespace
	}
	secret, err := s.k8s.CoreV1().Secrets(secretNS).Get(ctx, mp.Spec.CredentialsRef.Name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("getting secret %s/%s: %w", secretNS, mp.Spec.CredentialsRef.Name, err)
	}
	apiKey := string(secret.Data[mp.Spec.CredentialsRef.Key])

	// Strip the provider prefix for the actual model name sent to the API.
	model := mp.Spec.LiteLLMModel
	if idx := strings.Index(model, "/"); idx >= 0 {
		model = model[idx+1:]
	}

	// Pin to status on first use so all future calls use the same provider.
	// A conflict error means a concurrent request already pinned it — non-fatal.
	if kb.Status.EmbeddingModelProvider == "" {
		kb.Status.EmbeddingModelProvider = mpName
		kb.Status.EmbeddingModel = mp.Spec.LiteLLMModel
		if err := s.crdClient.Status().Update(ctx, kb); err != nil {
			slog.Warn("could not pin embedding provider to KnowledgeBase status", "kb", kb.Name, "err", err)
		}
	}

	return rag.NewEmbeddingClientWithKey(endpoint, apiKey, model, mp.Spec.DocPrompt, mp.Spec.QueryPrompt).
		WithRateLimits(mp.Spec.Constraints.RPM, mp.Spec.Constraints.TPM), nil
}

// embeddingBaseURL returns the base URL for embedding API calls based on the LiteLLM model prefix.
func embeddingBaseURL(litellmModel string) string {
	switch {
	case strings.HasPrefix(litellmModel, "openai/"):
		return "https://api.openai.com"
	case strings.HasPrefix(litellmModel, "anthropic/"):
		return "https://api.anthropic.com"
	case strings.HasPrefix(litellmModel, "gemini/"):
		return "https://generativelanguage.googleapis.com/v1beta/openai"
	case strings.HasPrefix(litellmModel, "ollama/"):
		return "http://localhost:11434"
	default:
		return "https://api.openai.com"
	}
}

// validateToken performs a TokenReview to verify the caller is a legitimate agent pod SA.
func (s *Server) validateToken(ctx context.Context, token string) (string, error) {
	tr := &authv1.TokenReview{
		Spec: authv1.TokenReviewSpec{
			Token:     token,
			Audiences: []string{security.ModelRouterTokenAudience},
		},
	}
	result, err := s.k8s.AuthenticationV1().TokenReviews().Create(ctx, tr, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("TokenReview failed: %w", err)
	}
	if !result.Status.Authenticated {
		return "", fmt.Errorf("token not authenticated")
	}
	return result.Status.User.Username, nil
}

// extractBearer extracts the Bearer token from the Authorization header.
func extractBearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if after, ok := strings.CutPrefix(h, "Bearer "); ok {
		return after
	}
	return ""
}
