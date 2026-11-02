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
	"database/sql/driver"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

func costMock(t *testing.T) (*Store, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &Store{db: db}, mock
}

var costCols = []string{"name", "namespace", "agent_ref", "phase", "spend_usd", "cost_capture", "model", "when"}

// Archived runs must contribute to cost totals once their CRD is GC'd.
func TestQueryCostRows_ReturnsArchivedRuns(t *testing.T) {
	s, mock := costMock(t)
	done := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`SELECT name, namespace, agent_ref, phase.*FROM archived_runs`).
		WithArgs(maxCostRows).
		WillReturnRows(sqlmock.NewRows(costCols).
			AddRow("a", "ns", "agent-1", "Succeeded", "0.5", "complete", "gpt-4o", done).
			AddRow("b", "ns", "agent-1", "Failed", "", "incomplete", "", done))

	rows, err := s.QueryCostRows(context.Background(), CostQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	if rows[0].SpendUSD != "0.5" || rows[0].Model != "gpt-4o" || rows[0].CostIncomplete {
		t.Fatalf("unexpected first row: %+v", rows[0])
	}
	if !rows[1].CostIncomplete {
		t.Fatalf("second row must be flagged incomplete: %+v", rows[1])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestQueryCostRows_BindsFilters(t *testing.T) {
	s, mock := costMock(t)
	since := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`namespace IN \(\$1, \$2\).*labels ->> 'agentorca.io/deployment' = \$3.*name = \$4.*>= \$5.*LIMIT \$6`).
		WithArgs("a", "b", "chat", "run-1", since, maxCostRows).
		WillReturnRows(sqlmock.NewRows(costCols))

	rows, err := s.QueryCostRows(context.Background(), CostQuery{
		Namespaces: []string{"a", "b"}, Deployment: "chat", Run: "run-1", Since: &since,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rows == nil || len(rows) != 0 {
		t.Fatalf("rows = %#v, want a non-nil empty slice", rows)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestQueryCostRows_QueryErrorIsReturned(t *testing.T) {
	s, mock := costMock(t)
	mock.ExpectQuery(`FROM archived_runs`).WillReturnError(context.DeadlineExceeded)
	if _, err := s.QueryCostRows(context.Background(), CostQuery{}); err == nil {
		t.Fatal("expected the query error to be returned")
	}
}

// ArchiveRun must carry the CostCapture condition so an archived run keeps its
// "untracked" flag after the CRD is gone.
func TestArchiveRunBindsCostCapture(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status metav1.ConditionStatus
		want   string
	}{
		{"incomplete", metav1.ConditionFalse, "incomplete"},
		{"complete", metav1.ConditionTrue, "complete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, mock := costMock(t)
			run := &agentorcav1alpha1.AgentRun{
				ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "default"},
				Spec:       agentorcav1alpha1.AgentRunSpec{AgentRef: "a"},
				Status: agentorcav1alpha1.AgentRunStatus{
					Phase:      agentorcav1alpha1.AgentRunPhaseFailed,
					Conditions: []metav1.Condition{{Type: "CostCapture", Status: tc.status, Reason: "x"}},
				},
			}
			args := make([]driver.Value, 25)
			for i := range args {
				args[i] = sqlmock.AnyArg()
			}
			mock.ExpectBegin()
			mock.ExpectExec(`INSERT INTO archived_runs`).
				WithArgs(append(args, tc.want)...).
				WillReturnResult(sqlmock.NewResult(1, 1))
			mock.ExpectExec(`INSERT INTO run_outcomes`).WillReturnResult(sqlmock.NewResult(1, 1))
			mock.ExpectCommit()
			if err := s.ArchiveRun(context.Background(), run, nil); err != nil {
				t.Fatal(err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMigrationAddsCostCaptureColumn(t *testing.T) {
	if !strings.Contains(migration0001(t), "ADD COLUMN IF NOT EXISTS cost_capture TEXT") {
		t.Fatal("migration must add cost_capture for tables created before this column existed")
	}
}
