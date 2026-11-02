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
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func goodResult() ShadowResult {
	return ShadowResult{
		DecisionID: "d-1", Namespace: "ns", Agent: "coder", SourceKind: SourceKindRun, SourceName: "run", Benchmark: "bench",
		ServedProvider: "strong", ShadowProvider: "weak", Winner: ShadowWinnerTie,
	}
}

func TestShadowResultValidate(t *testing.T) {
	if err := goodResult().Validate(); err != nil {
		t.Fatalf("valid result rejected: %v", err)
	}
	for name, mut := range map[string]func(*ShadowResult){
		"no decision":     func(r *ShadowResult) { r.DecisionID = "" },
		"no benchmark":    func(r *ShadowResult) { r.Benchmark = "" },
		"no agent":        func(r *ShadowResult) { r.Agent = "" },
		"no source":       func(r *ShadowResult) { r.SourceName = "" },
		"unknown source":  func(r *ShadowResult) { r.SourceKind = "Job" },
		"no shadow":       func(r *ShadowResult) { r.ShadowProvider = "" },
		"unknown verdict": func(r *ShadowResult) { r.Winner = "draw" },
	} {
		r := goodResult()
		mut(&r)
		if err := r.Validate(); !errors.Is(err, ErrInvalidShadowResult) {
			t.Errorf("%s: err = %v, want ErrInvalidShadowResult", name, err)
		}
	}
}

func TestUpsertShadowResult_StoresNullTranscriptWhenAbsent(t *testing.T) {
	s, mock := costMock(t)
	r := goodResult()
	r.CreatedAt = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	mock.ExpectExec(`INSERT INTO shadow_results`).
		WithArgs("d-1", "", "ns", "coder", "AgentRun", "run", "bench", "", int64(0), "", "", "", "strong", "weak", "tie", false, 0.0, "", nil,
			"", 0, 0, 0.0, 0.0, 0, 0, "", nil, r.CreatedAt).
		WillReturnResult(sqlmock.NewResult(1, 1))
	if err := s.UpsertShadowResult(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestUpsertShadowResult_RejectsBeforeTouchingTheDatabase(t *testing.T) {
	s, mock := costMock(t)
	r := goodResult()
	r.Winner = ""
	if err := s.UpsertShadowResult(context.Background(), r); !errors.Is(err, ErrInvalidShadowResult) {
		t.Fatalf("err = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func leaseReq(amount float64) LeaseRequest {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	return LeaseRequest{
		Namespace: "ns", Benchmark: "bench", Agent: "coder", SourceKind: SourceKindRun, SourceName: "run", AmountUSD: amount,
		DayStart: now.Truncate(24 * time.Hour), PerDayUSD: 1.0, TTL: 10 * time.Minute, Now: now,
	}
}

func expectSpend(mock sqlmock.Sqlmock, settled, leased float64) {
	mock.ExpectBegin()
	mock.ExpectExec(`pg_advisory_xact_lock`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`FROM shadow_results`).
		WillReturnRows(sqlmock.NewRows([]string{"day", "leased"}).AddRow(settled, leased))
}

func TestTakeShadowLease_GrantsWithinTheDayCap(t *testing.T) {
	s, mock := costMock(t)
	expectSpend(mock, 0.50, 0.30)
	mock.ExpectQuery(`INSERT INTO shadow_leases`).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(7))
	mock.ExpectCommit()
	id, err := s.TakeShadowLease(context.Background(), leaseReq(0.10))
	if err != nil || id != 7 {
		t.Fatalf("lease = %d, %v; want 7", id, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestTakeShadowLease_LiveLeasesCountAgainstTheCap(t *testing.T) {
	s, mock := costMock(t)
	// 0.50 settled + 0.45 leased leaves 0.05; a 0.10 reservation must be denied.
	expectSpend(mock, 0.50, 0.45)
	mock.ExpectRollback()
	if _, err := s.TakeShadowLease(context.Background(), leaseReq(0.10)); !errors.Is(err, ErrLeaseDenied) {
		t.Fatalf("err = %v, want ErrLeaseDenied", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestTakeShadowLease_PerRunCap(t *testing.T) {
	s, mock := costMock(t)
	req := leaseReq(0.10)
	req.PerRunUSD = 0.15
	expectSpend(mock, 0, 0)
	mock.ExpectQuery(`FROM shadow_results`).WillReturnRows(sqlmock.NewRows([]string{"run"}).AddRow(0.10))
	mock.ExpectRollback()
	if _, err := s.TakeShadowLease(context.Background(), req); !errors.Is(err, ErrLeaseDenied) {
		t.Fatalf("err = %v, want ErrLeaseDenied", err)
	}
}

func TestTakeShadowLease_DeploymentRunCapCountsTheDay(t *testing.T) {
	s, mock := costMock(t)
	req := leaseReq(0.10)
	req.SourceKind, req.SourceName, req.PerRunUSD = SourceKindDeployment, "assistant", 0.15
	expectSpend(mock, 0, 0)
	// The cap query for a deployment is bounded by the start of the UTC day;
	// a run's is not, so it counts everything the run ever spent.
	mock.ExpectQuery(`FROM shadow_results`).
		WithArgs("ns", "bench", "AgentDeployment", "assistant", req.Now, req.DayStart).
		WillReturnRows(sqlmock.NewRows([]string{"run"}).AddRow(0.10))
	mock.ExpectRollback()
	if _, err := s.TakeShadowLease(context.Background(), req); !errors.Is(err, ErrLeaseDenied) {
		t.Fatalf("err = %v, want ErrLeaseDenied", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestTakeShadowLease_HourlyCapIsPerAgent(t *testing.T) {
	req := leaseReq(0.01)
	req.MaxPerHour = 2
	for name, tc := range map[string]struct {
		calls int
		deny  bool
	}{
		"under the cap": {calls: 1},
		"at the cap":    {calls: 2, deny: true},
	} {
		t.Run(name, func(t *testing.T) {
			s, mock := costMock(t)
			expectSpend(mock, 0, 0)
			// Counted by Agent, over the hour before now, however many runs or
			// deployments the samples came from.
			mock.ExpectQuery(`FROM shadow_results`).
				WithArgs("ns", "bench", "coder", req.Now.Add(-time.Hour), req.Now).
				WillReturnRows(sqlmock.NewRows([]string{"calls"}).AddRow(tc.calls))
			if tc.deny {
				mock.ExpectRollback()
			} else {
				mock.ExpectQuery(`INSERT INTO shadow_leases`).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(3))
				mock.ExpectCommit()
			}
			_, err := s.TakeShadowLease(context.Background(), req)
			if tc.deny != errors.Is(err, ErrLeaseDenied) || (!tc.deny && err != nil) {
				t.Fatalf("err = %v, deny = %v", err, tc.deny)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestTakeShadowLease_RequiresAnAgentAndSource(t *testing.T) {
	s, mock := costMock(t)
	req := leaseReq(0.01)
	req.Agent = ""
	if _, err := s.TakeShadowLease(context.Background(), req); !errors.Is(err, ErrInvalidShadowResult) {
		t.Fatalf("err = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestShadowTallies(t *testing.T) {
	s, mock := costMock(t)
	mock.ExpectQuery(`FROM shadow_results`).WithArgs("ns", "bench", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"agent", "stage", "served", "shadow", "tie", "invalid", "inconsistent"}).
			AddRow("coder", "chat", 3, 1, 2, 1, 2))
	got, err := s.ShadowTallies(context.Background(), "ns", "bench", time.Now())
	if err != nil || len(got) != 1 || got[0].Agent != "coder" || got[0].Category != "chat" || got[0].Ties != 2 || got[0].Inconsistent != 2 {
		t.Fatalf("tallies = %+v, %v", got, err)
	}
}
