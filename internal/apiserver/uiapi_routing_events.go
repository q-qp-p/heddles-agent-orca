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
	"log/slog"
	"net/http"
	"slices"
	"strconv"

	"github.com/heddles/agent-orca/internal/postgresql"
)

// routingEventReader reads a run's routing telemetry. *postgresql.Store
// implements it.
type routingEventReader interface {
	RoutingEventsForRun(ctx context.Context, namespace, run string, limit int) ([]postgresql.RoutingEvent, error)
}

// routingEventSource returns the telemetry reader, or nil without PostgreSQL.
func (s *UIServer) routingEventSource() routingEventReader {
	if s.routingReader != nil {
		return s.routingReader
	}
	if s.pgStore != nil {
		return s.pgStore
	}
	return nil
}

// handleRunRoutingEvents serves GET /api/runs/{ns}/{name}/routing-events: every
// LLM call the run made, oldest first. AgentRun.status keeps only the most
// recent routing decisions, so this is the full history for long runs.
func (s *UIServer) handleRunRoutingEvents(w http.ResponseWriter, r *http.Request, ns, runName string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if tenant, ok := TenantFromContext(r.Context()); ok && tenant != nil {
		if !slices.Contains(tenantNamespaces(tenant), ns) {
			http.Error(w, "namespace not allowed", http.StatusForbidden)
			return
		}
	}
	src := s.routingEventSource()
	if src == nil {
		http.Error(w, `{"error":"routing history needs the PostgreSQL store"}`, http.StatusServiceUnavailable)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	events, err := src.RoutingEventsForRun(r.Context(), ns, runName, limit)
	if err != nil {
		slog.Error("loading routing events", "run", runName, "ns", ns, "err", err)
		http.Error(w, "loading routing events", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]any{"events": events})
}
