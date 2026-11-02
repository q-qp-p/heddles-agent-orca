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
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/heddles/agent-orca/internal/postgresql"
)

type fakeRoutingReader struct {
	events         []postgresql.RoutingEvent
	err            error
	gotNS, gotRun  string
	gotLimit, hits int
}

func (f *fakeRoutingReader) RoutingEventsForRun(_ context.Context, ns, run string, limit int) ([]postgresql.RoutingEvent, error) {
	f.hits++
	f.gotNS, f.gotRun, f.gotLimit = ns, run, limit
	return f.events, f.err
}

func getRunRoutingEvents(s *UIServer, ctx context.Context, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	s.handleRunOrStream(rec, req)
	return rec
}

func TestRunRoutingEvents_ReturnsTheRunsHistory(t *testing.T) {
	reader := &fakeRoutingReader{events: []postgresql.RoutingEvent{
		{DecisionID: "d-1", Provider: "p", Path: "rule:weighted"},
		{DecisionID: "d-2", Provider: "q", Path: "fallback", ParentDecisionID: "d-1"},
	}}
	s := &UIServer{routingReader: reader}

	rec := getRunRoutingEvents(s, context.Background(), "/api/runs/team-a/run-1/routing-events?limit=500")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var body struct {
		Events []postgresql.RoutingEvent `json:"events"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Events) != 2 || body.Events[1].ParentDecisionID != "d-1" {
		t.Errorf("events = %+v", body.Events)
	}
	if reader.gotNS != "team-a" || reader.gotRun != "run-1" || reader.gotLimit != 500 {
		t.Errorf("queried %q/%q limit %d", reader.gotNS, reader.gotRun, reader.gotLimit)
	}
}

func TestRunRoutingEvents_TenantsSeeOnlyTheirNamespaces(t *testing.T) {
	reader := &fakeRoutingReader{}
	s := &UIServer{routingReader: reader}
	ctx := context.WithValue(context.Background(), tenantIdentityKey, &TenantIdentity{TenantName: "acme", Namespaces: []string{"team-a"}})

	if rec := getRunRoutingEvents(s, ctx, "/api/runs/team-b/run-1/routing-events"); rec.Code != http.StatusForbidden {
		t.Errorf("other namespace: status = %d, want 403", rec.Code)
	}
	if reader.hits != 0 {
		t.Errorf("a forbidden request reached the store")
	}
	if rec := getRunRoutingEvents(s, ctx, "/api/runs/team-a/run-1/routing-events"); rec.Code != http.StatusOK {
		t.Errorf("own namespace: status = %d, want 200", rec.Code)
	}
}

func TestRunRoutingEvents_WithoutPostgresIs503(t *testing.T) {
	s := &UIServer{}
	if rec := getRunRoutingEvents(s, context.Background(), "/api/runs/ns/run/routing-events"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 so the UI falls back to the last 100", rec.Code)
	}
}

func TestRunRoutingEvents_StoreErrorAndMethod(t *testing.T) {
	s := &UIServer{routingReader: &fakeRoutingReader{err: errors.New("boom")}}
	if rec := getRunRoutingEvents(s, context.Background(), "/api/runs/ns/run/routing-events"); rec.Code != http.StatusInternalServerError {
		t.Errorf("store error: status = %d, want 500", rec.Code)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/runs/ns/run/routing-events", nil)
	rec := httptest.NewRecorder()
	s.handleRunOrStream(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: status = %d, want 405", rec.Code)
	}
}
