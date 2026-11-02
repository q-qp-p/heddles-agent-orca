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

package postgresql

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/heddles/agent-orca/internal/routingbench"
)

// The store is the source the recommendation evaluator reads.
var _ routingbench.Source = (*Store)(nil)

func TestLoadObservations_MapsRows(t *testing.T) {
	s, mock := costMock(t)
	mock.ExpectQuery(`FROM shadow_results r`).
		WillReturnRows(sqlmock.NewRows([]string{"agent", "stage", "served", "shadow", "winner", "inconsistent", "served_cost", "shadow_cost", "algorithm", "confidence"}).
			AddRow("coder", "chat", "strong", "weak", "served", false, 0.01, 0.001, "composite", 0.4))
	mock.ExpectQuery(`FROM routing_events\s+WHERE NOT is_shadow AND namespace = \$1 AND agent = ANY\(\$2::text\[\]\)\s+AND created_at >= \$3 AND created_at <= \$4\s+AND error_class = ''`).
		WillReturnRows(sqlmock.NewRows([]string{"agent", "stage", "provider", "n"}).AddRow("coder", "chat", "strong", 7))
	mock.ExpectQuery(`COUNT\(\*\) FILTER \(WHERE error_class <> ''\)`).
		WillReturnRows(sqlmock.NewRows([]string{"selector", "provider", "calls", "errors"}).AddRow("balanced", "weak", 10, 4))
	mock.ExpectQuery(`FROM routing_feedback f`).
		WillReturnRows(sqlmock.NewRows([]string{"agent", "stage", "up", "down"}).AddRow("coder", "chat", 1, 3))

	now := time.Now()
	got, err := s.LoadObservations(context.Background(), routingbench.Query{Namespace: "ns", Agents: []string{"coder"}, Since: now.Add(-time.Hour), Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	if len(got.Samples) != 1 || got.Samples[0].Category != "stage:chat" || got.Samples[0].ServedCostUSD != 0.01 ||
		got.Samples[0].Algorithm != "composite" || got.Samples[0].Confidence != 0.4 {
		t.Errorf("samples = %+v", got.Samples)
	}
	if len(got.Volumes) != 1 || got.Volumes[0].Category != "stage:chat" || got.Volumes[0].Calls != 7 {
		t.Errorf("volumes = %+v", got.Volumes)
	}
	if len(got.ProviderStats) != 1 || got.ProviderStats[0].Errors != 4 {
		t.Errorf("provider stats = %+v", got.ProviderStats)
	}
	if r := got.Ratings[routingbench.AgentCategory{Agent: "coder", Category: "stage:chat"}]; r.Up != 1 || r.Down != 3 {
		t.Errorf("ratings = %+v", got.Ratings)
	}
}

func TestLoadObservations_NamesTheQueryThatFailed(t *testing.T) {
	s, mock := costMock(t)
	mock.ExpectQuery(`FROM shadow_results r`).WillReturnError(context.DeadlineExceeded)
	_, err := s.LoadObservations(context.Background(), routingbench.Query{Namespace: "ns"})
	if err == nil || !strings.Contains(err.Error(), "loading shadow samples") || !strings.Contains(err.Error(), "deadline") {
		t.Errorf("err = %v", err)
	}
}
