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

	"github.com/DATA-DOG/go-sqlmock"
)

func TestEvalDatasetValidate(t *testing.T) {
	good := EvalDataset{Namespace: "ns", Name: "support-core", CreatedBy: "ada"}
	tests := []struct {
		name   string
		mutate func(*EvalDataset)
		ok     bool
	}{
		{"good", func(*EvalDataset) {}, true},
		{"with agent", func(d *EvalDataset) { d.Agent = "support-bot" }, true},
		{"no namespace", func(d *EvalDataset) { d.Namespace = "" }, false},
		{"upper case name", func(d *EvalDataset) { d.Name = "Support" }, false},
		{"name with space", func(d *EvalDataset) { d.Name = "a b" }, false},
		{"name ends with dash", func(d *EvalDataset) { d.Name = "a-" }, false},
		{"empty name", func(d *EvalDataset) { d.Name = "" }, false},
		{"name too long", func(d *EvalDataset) { d.Name = strings.Repeat("a", 64) }, false},
		{"bad agent", func(d *EvalDataset) { d.Agent = "Not An Agent" }, false},
		{"long description", func(d *EvalDataset) { d.Description = strings.Repeat("x", MaxEvalDescription+1) }, false},
		{"no creator", func(d *EvalDataset) { d.CreatedBy = "" }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := good
			tt.mutate(&d)
			err := d.Validate()
			if tt.ok != (err == nil) || (err != nil && !errors.Is(err, ErrInvalidEval)) {
				t.Errorf("Validate = %v, want ok=%v", err, tt.ok)
			}
		})
	}
}

func TestEvalCaseValidate(t *testing.T) {
	manyTags := func(c *EvalCase) {
		for i := 0; i <= MaxEvalTags; i++ {
			c.Tags = append(c.Tags, "tag-"+string(rune('a'+i)))
		}
	}
	tests := []struct {
		name   string
		mutate func(*EvalCase)
		limits EvalLimits
		want   error // nil means valid
	}{
		{"input only", func(*EvalCase) {}, EvalLimits{}, nil},
		{"exact reference", func(c *EvalCase) { c.Reference, c.ReferenceKind = "42", ReferenceExact }, EvalLimits{}, nil},
		{"empty input", func(c *EvalCase) { c.Input = "  " }, EvalLimits{}, ErrInvalidEval},
		{"input at the default limit", func(c *EvalCase) { c.Input = strings.Repeat("x", MaxEvalCaseBytes) }, EvalLimits{}, nil},
		{"input over the default limit", func(c *EvalCase) { c.Input = strings.Repeat("x", MaxEvalCaseBytes+1) }, EvalLimits{}, ErrEvalTooLarge},
		{"input over a lower limit", func(c *EvalCase) { c.Input = "12345" }, EvalLimits{MaxCaseBytes: 4}, ErrEvalTooLarge},
		{"limit above the table's is capped", func(c *EvalCase) { c.Input = strings.Repeat("x", MaxEvalCaseBytes+1) }, EvalLimits{MaxCaseBytes: 1 << 20}, ErrEvalTooLarge},
		{"reference over the limit", func(c *EvalCase) { c.Reference, c.ReferenceKind = "12345", ReferenceExact }, EvalLimits{MaxCaseBytes: 4}, ErrEvalTooLarge},
		{"unknown kind", func(c *EvalCase) { c.Reference, c.ReferenceKind = "x", "golden" }, EvalLimits{}, ErrInvalidEval},
		{"kind none with a reference", func(c *EvalCase) { c.Reference, c.ReferenceKind = "x", ReferenceNone }, EvalLimits{}, ErrInvalidEval},
		{"kind without a reference", func(c *EvalCase) { c.ReferenceKind = ReferenceRubric }, EvalLimits{}, ErrInvalidEval},
		{"negative weight", func(c *EvalCase) { c.Weight = -1 }, EvalLimits{}, ErrInvalidEval},
		{"too many tags", manyTags, EvalLimits{}, ErrInvalidEval},
		{"long tag", func(c *EvalCase) { c.Tags = []string{strings.Repeat("t", MaxEvalTagBytes+1)} }, EvalLimits{}, ErrInvalidEval},
		{"half a source run", func(c *EvalCase) { c.SourceRun = "r" }, EvalLimits{}, ErrInvalidEval},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := EvalCase{Input: "hello"}
			tt.mutate(&c)
			c.normalize()
			err := c.Validate(tt.limits)
			if tt.want == nil && err != nil {
				t.Errorf("Validate = %v", err)
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Errorf("Validate = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestEvalCaseNormalize(t *testing.T) {
	c := EvalCase{Input: "x", Reference: "y", Tags: []string{" a ", "", "a", "b"}}
	c.normalize()
	if c.ReferenceKind != ReferenceExact || c.Weight != 1 || strings.Join(c.Tags, ",") != "a,b" {
		t.Errorf("normalized = %+v", c)
	}
	c = EvalCase{Input: "x"}
	c.normalize()
	if c.ReferenceKind != ReferenceNone {
		t.Errorf("kind = %q", c.ReferenceKind)
	}
}

func TestEvalLimitsDefaults(t *testing.T) {
	if (EvalLimits{}).cases() != 2000 || (EvalLimits{}).bytes() != 65536 {
		t.Error("defaults are 2000 cases and 65536 bytes")
	}
	if (EvalLimits{MaxCasesPerDataset: 5, MaxCaseBytes: 100}).cases() != 5 {
		t.Error("configured case limit ignored")
	}
}

// Without a scope nothing runs, and a bad dataset never reaches the database.
func TestEvalStoreRefusesBeforeTheDatabase(t *testing.T) {
	s, _ := costMock(t)
	strict := &Store{db: s.db}
	if _, err := strict.GetEvalDataset(context.Background(), 1); !errors.Is(err, ErrNoTenantScope) {
		t.Errorf("unscoped get err = %v", err)
	}
	if _, err := s.CreateEvalDataset(context.Background(), EvalDataset{Name: "Bad"}); !errors.Is(err, ErrInvalidEval) {
		t.Errorf("bad dataset err = %v", err)
	}
	if _, _, err := s.AddEvalCases(context.Background(), 1, nil, EvalLimits{}); !errors.Is(err, ErrInvalidEval) {
		t.Errorf("no cases err = %v", err)
	}
	_, _, err := s.AddEvalCases(context.Background(), 1, []EvalCase{{Input: "ok"}, {Input: ""}}, EvalLimits{})
	if err == nil || !strings.Contains(err.Error(), "case 2") {
		t.Errorf("the error must name the bad case, got %v", err)
	}
}

// A dataset that is not in scope is ErrEvalNotFound, not a database error.
func TestGetEvalDataset_NotFound(t *testing.T) {
	s, mock := costMock(t)
	expectScopeBegin(mock)
	mock.ExpectQuery(`FROM eval_datasets d WHERE d.id = \$1`).WithArgs(int64(9), PlatformScope).
		WillReturnRows(sqlmock.NewRows([]string{"id", "tenant_id", "namespace", "name", "description", "agent", "version", "count", "created_by", "created_at", "updated_at"}))
	mock.ExpectRollback()
	if _, err := s.GetEvalDataset(context.Background(), 9); !errors.Is(err, ErrEvalNotFound) {
		t.Errorf("err = %v, want ErrEvalNotFound", err)
	}
}
