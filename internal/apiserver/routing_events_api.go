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
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/postgresql"
)

// RoutingEventStore persists routing telemetry. *postgresql.Store implements it.
type RoutingEventStore interface {
	UpsertRoutingEvents(ctx context.Context, events []postgresql.RoutingEvent) error
}

// WithRoutingStore gives the server the routing telemetry store. Without one,
// posted events are acknowledged and dropped: the router keeps working and its
// Prometheus metrics still count. It returns s for chaining.
func (s *Server) WithRoutingStore(store RoutingEventStore) *Server {
	s.routingEvents = store
	return s
}

const (
	// maxRoutingEventsBody bounds one batch on the wire.
	maxRoutingEventsBody = 1 << 20
	// maxRoutingEventsPerBatch bounds the events in one batch.
	maxRoutingEventsPerBatch = 200
)

// routingEventsRequest is the body of POST /agentrun/{ns}/{name}/routing-events.
type routingEventsRequest struct {
	Events []postgresql.RoutingEvent `json:"events"`
}

// recordRoutingEvents stores a batch of routing telemetry from a run's router.
// Namespace, run and tenant come from the URL and the run, never from the body,
// so a router can only write events for its own run. Re-posting an event
// refreshes it, so the router can retry a batch safely.
func (s *Server) recordRoutingEvents(w http.ResponseWriter, r *http.Request, caller, namespace, name string) {
	if !callerInNamespace(caller, namespace) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRoutingEventsBody+1))
	if err != nil {
		http.Error(w, "reading body", http.StatusBadRequest)
		return
	}
	if len(body) > maxRoutingEventsBody {
		http.Error(w, "batch too large", http.StatusRequestEntityTooLarge)
		return
	}
	var req routingEventsRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if len(req.Events) > maxRoutingEventsPerBatch {
		http.Error(w, fmt.Sprintf("at most %d events per batch", maxRoutingEventsPerBatch), http.StatusRequestEntityTooLarge)
		return
	}
	for i, e := range req.Events {
		if e.DecisionID == "" || e.Provider == "" {
			http.Error(w, fmt.Sprintf("event %d needs decisionID and provider", i), http.StatusBadRequest)
			return
		}
	}
	if s.routingEvents == nil || len(req.Events) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	var run agentorcav1alpha1.AgentRun
	if err := s.crdClient.Get(r.Context(), client.ObjectKey{Name: name, Namespace: namespace}, &run); err != nil {
		http.Error(w, fmt.Sprintf("getting AgentRun: %v", err), http.StatusNotFound)
		return
	}
	tenant := run.Labels["agentorca.io/tenant"]
	now := time.Now().UTC()
	for i := range req.Events {
		e := &req.Events[i]
		e.Namespace, e.RunName, e.Tenant = namespace, name, tenant
		if e.CreatedAt.IsZero() {
			e.CreatedAt = now
		}
	}
	if err := s.routingEvents.UpsertRoutingEvents(r.Context(), req.Events); err != nil {
		slog.Error("storing routing events", "run", name, "namespace", namespace, "err", err)
		http.Error(w, "storing routing events", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
