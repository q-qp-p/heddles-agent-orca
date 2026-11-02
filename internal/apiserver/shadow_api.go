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
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/postgresql"
)

// ShadowStore persists shadow results and the budget leases that gate them.
// *postgresql.Store implements it.
type ShadowStore interface {
	UpsertShadowResult(ctx context.Context, r postgresql.ShadowResult) error
	TakeShadowLease(ctx context.Context, req postgresql.LeaseRequest) (int64, error)
	ReleaseShadowLease(ctx context.Context, id int64) error
}

// WithShadowStore gives the server the shadow benchmarking store. Without one
// every lease is denied, so the router makes no shadow calls. It returns s for
// chaining.
func (s *Server) WithShadowStore(store ShadowStore) *Server {
	s.shadow = store
	return s
}

const (
	// defaultShadowMaxPerHour applies when a benchmark leaves maxPerHour unset.
	defaultShadowMaxPerHour = 20
	maxShadowBody           = 512 << 10
	// shadowLeaseTTL is how long an unsettled lease holds budget: longer than a
	// shadow call and its two judge calls can take.
	shadowLeaseTTL = 10 * time.Minute
)

// shadowLeaseRequest is the body of POST /agentrun/{ns}/{name}/shadow-lease.
// {name} is the AgentRun, or the AgentDeployment a deployment sidecar names
// itself after.
type shadowLeaseRequest struct {
	Benchmark string  `json:"benchmark"`
	AmountUSD float64 `json:"amountUsd"`
}

// shadowLeaseResponse carries the lease the router must settle or release.
type shadowLeaseResponse struct {
	LeaseID int64 `json:"leaseId"`
}

// shadowResultRequest is the body of POST /agentrun/{ns}/{name}/shadow-results.
type shadowResultRequest struct {
	// LeaseID is the lease taken for this call; it is released once the result
	// is stored, and also when the call failed (Result.ShadowError set).
	LeaseID int64                   `json:"leaseId,omitempty"`
	Result  postgresql.ShadowResult `json:"result"`
}

// shadowCall is what the operator knows about one sampled call, all of it
// derived from the cluster and none from the router's body.
type shadowCall struct {
	src   callSource
	bench *agentorcav1alpha1.RoutingBenchmark
	// selector and generation say which ModelSelector configuration routed it.
	selector   string
	generation int64
}

// resolveShadowCall finds the source of a call, the Agent it belongs to, and
// the benchmark it is filed under, and checks that the benchmark covers that
// Agent. A pod can therefore spend only the budget of a benchmark that
// measures its own Agent. The status is the HTTP status to reply with on error.
func (s *Server) resolveShadowCall(ctx context.Context, namespace, name, benchmark string) (shadowCall, int, error) {
	src, err := s.resolveSource(ctx, namespace, name)
	if err != nil {
		return shadowCall{}, http.StatusNotFound, err
	}
	var bench agentorcav1alpha1.RoutingBenchmark
	if err := s.crdClient.Get(ctx, client.ObjectKey{Name: benchmark, Namespace: namespace}, &bench); err != nil {
		return shadowCall{}, http.StatusNotFound, fmt.Errorf("getting RoutingBenchmark: %w", err)
	}
	var agent agentorcav1alpha1.Agent
	if err := s.crdClient.Get(ctx, client.ObjectKey{Name: src.Agent, Namespace: namespace}, &agent); err != nil {
		return shadowCall{}, http.StatusNotFound, fmt.Errorf("getting Agent %q: %w", src.Agent, err)
	}
	if !bench.Covers(&agent) {
		return shadowCall{}, http.StatusForbidden, fmt.Errorf("benchmark %q does not cover Agent %q", benchmark, src.Agent)
	}
	call := shadowCall{src: src, bench: &bench, selector: agent.Spec.ModelSelectorRef}
	var sel agentorcav1alpha1.ModelSelector
	if err := s.crdClient.Get(ctx, client.ObjectKey{Name: agent.Spec.ModelSelectorRef, Namespace: namespace}, &sel); err == nil {
		call.generation = sel.Generation
	}
	return call, 0, nil
}

// takeShadowLease serves POST /agentrun/{ns}/{name}/shadow-lease: it reserves
// budget for one shadow call. 429 means the day or run cap would be exceeded.
func (s *Server) takeShadowLease(w http.ResponseWriter, r *http.Request, caller, namespace, run string) {
	if !callerInNamespace(caller, namespace) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var req shadowLeaseRequest
	if !readShadowBody(w, r, &req) {
		return
	}
	if req.Benchmark == "" || req.AmountUSD < 0 {
		http.Error(w, "benchmark and a non-negative amountUsd are required", http.StatusBadRequest)
		return
	}
	if s.shadow == nil {
		http.Error(w, "shadow benchmarking needs Postgres", http.StatusTooManyRequests)
		return
	}
	call, status, err := s.resolveShadowCall(r.Context(), namespace, run, req.Benchmark)
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	bench := call.bench
	perDay, err := strconv.ParseFloat(bench.Spec.Budget.PerDayUSD, 64)
	if err != nil {
		http.Error(w, "benchmark budget.perDayUSD is not a number", http.StatusUnprocessableEntity)
		return
	}
	perRun, _ := strconv.ParseFloat(bench.Spec.Budget.PerRunUSD, 64)
	maxPerHour := defaultShadowMaxPerHour
	if bench.Spec.Shadow != nil && bench.Spec.Shadow.MaxPerHour > 0 {
		maxPerHour = int(bench.Spec.Shadow.MaxPerHour)
	}
	now := time.Now().UTC()
	id, err := s.shadow.TakeShadowLease(r.Context(), postgresql.LeaseRequest{
		Namespace: namespace, Benchmark: req.Benchmark,
		Agent: call.src.Agent, SourceKind: call.src.Kind, SourceName: call.src.Name,
		AmountUSD: req.AmountUSD, DayStart: now.Truncate(24 * time.Hour), PerDayUSD: perDay, PerRunUSD: perRun,
		MaxPerHour: maxPerHour, TTL: shadowLeaseTTL, Now: now,
	})
	switch {
	case errors.Is(err, postgresql.ErrLeaseDenied):
		http.Error(w, "shadow budget or hourly sample limit reached", http.StatusTooManyRequests)
	case err != nil:
		slog.Error("taking shadow lease", "run", run, "namespace", namespace, "err", err)
		http.Error(w, "taking shadow lease", http.StatusInternalServerError)
	default:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(shadowLeaseResponse{LeaseID: id})
	}
}

// recordShadowResult serves POST /agentrun/{ns}/{name}/shadow-results. The
// namespace, source, Agent, tenant and selector come from the URL and the
// cluster, never from the body. A transcript is dropped unless the benchmark
// stores them.
func (s *Server) recordShadowResult(w http.ResponseWriter, r *http.Request, caller, namespace, name string) {
	if !callerInNamespace(caller, namespace) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var req shadowResultRequest
	if !readShadowBody(w, r, &req) {
		return
	}
	res := req.Result
	// Check the router's part of the body before looking anything up; the
	// operator's part is filled in below and checked again.
	probe := res
	probe.Namespace, probe.Agent, probe.SourceKind, probe.SourceName = namespace, "-", postgresql.SourceKindRun, name
	if err := probe.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if s.shadow == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	call, status, err := s.resolveShadowCall(r.Context(), namespace, name, res.Benchmark)
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	res.Namespace = namespace
	res.Agent, res.SourceKind, res.SourceName, res.Tenant = call.src.Agent, call.src.Kind, call.src.Name, call.src.Tenant
	res.Selector, res.SelectorGeneration = call.selector, call.generation
	if err := res.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !call.bench.Spec.StoreTranscripts {
		res.Transcript = nil
	}
	if res.CreatedAt.IsZero() {
		res.CreatedAt = time.Now().UTC()
	}
	if err := s.shadow.UpsertShadowResult(r.Context(), res); err != nil {
		slog.Error("storing shadow result", "run", name, "namespace", namespace, "err", err)
		http.Error(w, "storing shadow result", http.StatusInternalServerError)
		return
	}
	if req.LeaseID != 0 {
		if err := s.shadow.ReleaseShadowLease(r.Context(), req.LeaseID); err != nil {
			slog.Warn("releasing shadow lease", "lease", req.LeaseID, "err", err)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// readShadowBody decodes a bounded JSON body into v, replying 4xx on failure.
func readShadowBody(w http.ResponseWriter, r *http.Request, v any) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxShadowBody+1))
	if err != nil {
		http.Error(w, "reading body", http.StatusBadRequest)
		return false
	}
	if len(body) > maxShadowBody {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return false
	}
	if err := json.Unmarshal(body, v); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return false
	}
	return true
}
