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
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func goodFeedback() Feedback {
	return Feedback{Namespace: "ns", RunName: "run", Rating: -1, UserID: "ada", Source: FeedbackSourceUI}
}

func TestFeedbackValidate(t *testing.T) {
	neg := -1
	tests := []struct {
		name   string
		mutate func(*Feedback)
		ok     bool
	}{
		{"good", func(*Feedback) {}, true},
		{"thumbs up", func(f *Feedback) { f.Rating = 1 }, true},
		{"api source", func(f *Feedback) { f.Source = FeedbackSourceAPI }, true},
		{"comment at the limit", func(f *Feedback) { f.Comment = strings.Repeat("x", MaxFeedbackCommentBytes) }, true},
		{"rating zero", func(f *Feedback) { f.Rating = 0 }, false},
		{"rating two", func(f *Feedback) { f.Rating = 2 }, false},
		{"comment over the limit", func(f *Feedback) { f.Comment = strings.Repeat("x", MaxFeedbackCommentBytes+1) }, false},
		{"no user", func(f *Feedback) { f.UserID = "" }, false},
		{"no run", func(f *Feedback) { f.RunName = "" }, false},
		{"no namespace", func(f *Feedback) { f.Namespace = "" }, false},
		{"unknown source", func(f *Feedback) { f.Source = "email" }, false},
		{"negative turn", func(f *Feedback) { f.TurnIndex = &neg }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := goodFeedback()
			tt.mutate(&f)
			err := f.Validate()
			if tt.ok && err != nil {
				t.Errorf("Validate = %v", err)
			}
			if !tt.ok && !errors.Is(err, ErrInvalidFeedback) {
				t.Errorf("Validate = %v, want ErrInvalidFeedback", err)
			}
		})
	}
}

func TestUpsertFeedback_BindsEveryColumn(t *testing.T) {
	s, mock := costMock(t)
	turn := 3
	f := Feedback{Tenant: "acme", Namespace: "ns", RunName: "run", DecisionID: "d-1", TurnIndex: &turn,
		Rating: 1, Comment: "right call", UserID: "ada", Source: FeedbackSourceAPI}
	mock.ExpectExec(`INSERT INTO routing_feedback`).
		WithArgs("acme", "ns", "run", "d-1", 3, 1, "right call", "ada", "api").
		WillReturnResult(sqlmock.NewResult(1, 1))
	if err := s.UpsertFeedback(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestUpsertFeedback_NoTurnBindsNull(t *testing.T) {
	s, mock := costMock(t)
	mock.ExpectExec(`INSERT INTO routing_feedback`).
		WithArgs("", "ns", "run", "", nil, -1, "", "ada", "ui").
		WillReturnResult(sqlmock.NewResult(1, 1))
	if err := s.UpsertFeedback(context.Background(), goodFeedback()); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A second rating of the same decision by the same user changes the first.
func TestUpsertFeedbackSQL_ReplacesBySameUserAndDecision(t *testing.T) {
	for _, want := range []string{"ON CONFLICT (namespace, run_name, decision_id, user_id) DO UPDATE", "rating = EXCLUDED.rating", "comment = EXCLUDED.comment"} {
		if !strings.Contains(upsertFeedbackSQL, want) {
			t.Errorf("upsert SQL lacks %q", want)
		}
	}
}

func TestUpsertFeedback_RejectsInvalidBeforeTheDatabase(t *testing.T) {
	s, mock := costMock(t)
	f := goodFeedback()
	f.Rating = 5
	if err := s.UpsertFeedback(context.Background(), f); !errors.Is(err, ErrInvalidFeedback) {
		t.Fatalf("err = %v, want ErrInvalidFeedback", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("the database was called: %v", err)
	}
}

func TestFeedbackForRun(t *testing.T) {
	s, mock := costMock(t)
	at := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	cols := []string{"tenant", "decision_id", "turn_index", "rating", "comment", "user_id", "source", "created_at", "updated_at"}
	mock.ExpectQuery(`FROM routing_feedback`).WithArgs("ns", "run", "ada").
		WillReturnRows(sqlmock.NewRows(cols).
			AddRow("acme", "d-1", 2, 1, "good", "ada", "ui", at, at).
			AddRow("acme", "", nil, -1, "", "ada", "ui", at, at))
	got, err := s.FeedbackForRun(context.Background(), "ns", "run", "ada")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].DecisionID != "d-1" || got[0].TurnIndex == nil || *got[0].TurnIndex != 2 || got[0].Rating != 1 {
		t.Errorf("first = %+v", got)
	}
	if got[1].DecisionID != "" || got[1].TurnIndex != nil || got[1].Rating != -1 {
		t.Errorf("run-level rating = %+v", got[1])
	}
}
