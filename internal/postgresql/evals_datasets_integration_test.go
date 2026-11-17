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
	"database/sql"
	"errors"
	"reflect"
	"testing"
)

const (
	evalNSA  = "team-a"
	evalNSB  = "team-b"
	evalUser = "ada"
)

func evalDS(ctx context.Context, t *testing.T, s *Store, ns, name string) EvalDataset {
	t.Helper()
	d, err := s.CreateEvalDataset(ctx, EvalDataset{Namespace: ns, Name: name, CreatedBy: evalUser})
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	return d
}

// evalFlow is one dataset that the steps of TestEvalDatasetsAgainstPostgres
// work through in order, each checking the version it leaves behind.
type evalFlow struct {
	t   *testing.T
	s   *Store
	ctx context.Context
	d   EvalDataset
	l   EvalLimits
	// one and two are the first two cases.
	one, two EvalCase
}

func (f *evalFlow) version() int {
	f.t.Helper()
	got, err := f.s.GetEvalDataset(f.ctx, f.d.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	return got.Version
}

func (f *evalFlow) wantVersion(want int, what string) {
	f.t.Helper()
	if got := f.version(); got != want {
		f.t.Fatalf("version after %s = %d, want %d", what, got, want)
	}
}

func TestEvalDatasetsAgainstPostgres(t *testing.T) {
	s, _ := appStore(t)
	ctx := WithTenant(context.Background(), tenantA)
	f := &evalFlow{t: t, s: s, ctx: ctx, l: EvalLimits{}}

	f.d = evalDS(ctx, t, s, evalNSA, "support")
	if f.d.Version != 1 || f.d.TenantID != tenantA || f.d.ID == 0 {
		t.Fatalf("created = %+v", f.d)
	}
	if _, err := s.CreateEvalDataset(ctx, EvalDataset{Namespace: evalNSA, Name: "support", CreatedBy: evalUser}); !errors.Is(err, ErrEvalExists) {
		t.Errorf("duplicate name err = %v", err)
	}
	// The same name in another workspace is fine.
	evalDS(ctx, t, s, evalNSB, "support")

	f.stepAdd()
	f.stepEdit()
	f.stepRemove()
	f.stepLimits()
	f.stepSourceAndDelete()
}

// Add: one bump per call, however many cases.
func (f *evalFlow) stepAdd() {
	t := f.t
	added, v, err := f.s.AddEvalCases(f.ctx, f.d.ID, []EvalCase{
		{Input: "one", Tags: []string{"a", "b", "a"}},
		{Input: "two", Reference: "2", ReferenceKind: ReferenceExample, Weight: 2},
	}, f.l)
	if err != nil || v != 2 || len(added) != 2 || added[0].AddedIn != 2 || added[0].TenantID != tenantA {
		t.Fatalf("bulk add = %+v, %d, %v", added, v, err)
	}
	if !reflect.DeepEqual(added[0].Tags, []string{"a", "b"}) || added[1].Weight != 2 || added[0].Weight != 1 {
		t.Errorf("tags and weight = %+v", added)
	}
	f.wantVersion(2, "a bulk add")
	f.one, f.two = added[0], added[1]
}

// Edit: bumps only when something changes.
func (f *evalFlow) stepEdit() {
	t, ctx, s, l := f.t, f.ctx, f.s, f.l
	same := "one"
	if _, v, changed, err := s.UpdateEvalCase(ctx, f.one.ID, EvalCasePatch{Input: &same}, l); err != nil || v != 2 || changed {
		t.Fatalf("no-op edit: version %d, changed %v, %v; want 2, false", v, changed, err)
	}
	newInput := "uno"
	got, v, changed, err := s.UpdateEvalCase(ctx, f.one.ID, EvalCasePatch{Input: &newInput}, l)
	if err != nil || v != 3 || !changed || got.Input != "uno" {
		t.Fatalf("edit = %+v, %d, %v", got, v, err)
	}
	tags := []string{"x"}
	if got, v, _, err = s.UpdateEvalCase(ctx, f.one.ID, EvalCasePatch{Tags: &tags}, l); err != nil || v != 4 || !reflect.DeepEqual(got.Tags, []string{"x"}) {
		t.Fatalf("tag edit = %+v, %d, %v", got, v, err)
	}
	// Setting a reference without a kind gives exact; clearing it gives none.
	ref := "answer"
	if got, _, _, err = s.UpdateEvalCase(ctx, f.one.ID, EvalCasePatch{Reference: &ref}, l); err != nil || got.ReferenceKind != ReferenceExact || got.Reference != "answer" {
		t.Fatalf("reference edit = %+v, %v", got, err)
	}
	empty := ""
	if got, _, _, err = s.UpdateEvalCase(ctx, f.one.ID, EvalCasePatch{Reference: &empty}, l); err != nil || got.ReferenceKind != ReferenceNone {
		t.Fatalf("clearing the reference = %+v, %v", got, err)
	}
	f.wantVersion(6, "edits")
	bad := ReferenceRubric
	if _, _, _, err = s.UpdateEvalCase(ctx, f.one.ID, EvalCasePatch{ReferenceKind: &bad}, l); !errors.Is(err, ErrInvalidEval) {
		t.Errorf("a rubric kind with no reference err = %v", err)
	}
	f.wantVersion(6, "a refused edit")

	// Dataset metadata does not bump.
	desc := "support questions"
	if _, err := s.UpdateEvalDataset(ctx, f.d.ID, EvalDatasetPatch{Description: &desc}); err != nil {
		t.Fatal(err)
	}
	f.wantVersion(6, "a description change")
}

// Remove: soft delete with one bump; the case disappears from reads.
func (f *evalFlow) stepRemove() {
	t, ctx, s := f.t, f.ctx, f.s
	if v, err := s.DeleteEvalCase(ctx, f.two.ID); err != nil || v != 7 {
		t.Fatalf("delete: version %d, %v", v, err)
	}
	if _, err := s.GetEvalCase(ctx, f.two.ID); !errors.Is(err, ErrEvalNotFound) {
		t.Errorf("get removed case err = %v", err)
	}
	if _, err := s.DeleteEvalCase(ctx, f.two.ID); !errors.Is(err, ErrEvalNotFound) {
		t.Errorf("second delete err = %v", err)
	}
	f.wantVersion(7, "a removal")
	list, err := s.ListEvalCases(ctx, f.d.ID, EvalCaseQuery{})
	if err != nil || len(list) != 1 || list[0].ID != f.one.ID || list[0].Namespace != evalNSA {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if list, _ = s.ListEvalCases(ctx, f.d.ID, EvalCaseQuery{Tag: "x"}); len(list) != 1 {
		t.Errorf("tag filter = %+v", list)
	}
	if list, _ = s.ListEvalCases(ctx, f.d.ID, EvalCaseQuery{Tag: "nope"}); len(list) != 0 {
		t.Errorf("tag filter nope = %+v", list)
	}
	if info, _ := s.GetEvalDataset(ctx, f.d.ID); info.CaseCount != 1 {
		t.Errorf("caseCount = %d, want 1 live case", info.CaseCount)
	}
}

// A failed add changes nothing; the cap and the byte limit are honoured.
func (f *evalFlow) stepLimits() {
	t, ctx, s := f.t, f.ctx, f.s
	two := EvalLimits{MaxCasesPerDataset: 2}
	if _, _, err := s.AddEvalCases(ctx, f.d.ID, []EvalCase{{Input: "x"}, {Input: "y"}}, two); !errors.Is(err, ErrDatasetFull) {
		t.Errorf("cap err = %v", err)
	}
	if _, _, err := s.AddEvalCases(ctx, f.d.ID, []EvalCase{{Input: "x"}, {Input: ""}}, f.l); !errors.Is(err, ErrInvalidEval) {
		t.Errorf("bad case err = %v", err)
	}
	f.wantVersion(7, "failed adds")
	// Exactly at the cap is allowed; removed cases do not count.
	if _, v, err := s.AddEvalCases(ctx, f.d.ID, []EvalCase{{Input: "y"}}, two); err != nil || v != 8 {
		t.Errorf("add up to the cap: version %d, %v", v, err)
	}
	if _, _, err := s.AddEvalCases(ctx, f.d.ID, []EvalCase{{Input: "z"}}, two); !errors.Is(err, ErrDatasetFull) {
		t.Errorf("over the cap err = %v", err)
	}
	// The byte limit is configurable and the table's CHECK is the ceiling.
	if _, _, err := s.AddEvalCases(ctx, f.d.ID, []EvalCase{{Input: "0123456789"}}, EvalLimits{MaxCaseBytes: 9}); !errors.Is(err, ErrEvalTooLarge) {
		t.Errorf("byte limit err = %v", err)
	}
}

// The source run pointer round-trips, an edit of the text clears the redacted
// badge, and deleting the dataset deletes its cases, removed ones included.
func (f *evalFlow) stepSourceAndDelete() {
	t, ctx, s := f.t, f.ctx, f.s
	src, _, err := s.AddEvalCases(ctx, f.d.ID, []EvalCase{{Input: "from run", SourceRunNamespace: evalNSA, SourceRun: "r1", SourceUser: evalUser, Redacted: true}}, f.l)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetEvalCase(ctx, src[0].ID); got.SourceRun != "r1" || got.SourceRunNamespace != evalNSA || !got.Redacted || got.SourceUser != evalUser {
		t.Errorf("source round trip = %+v", got)
	}
	edited := "from run, edited"
	if got, _, _, err := s.UpdateEvalCase(ctx, src[0].ID, EvalCasePatch{Input: &edited}, f.l); err != nil || got.Redacted {
		t.Errorf("an edit of the text must clear redacted: %+v, %v", got, err)
	}

	gone, err := s.DeleteEvalDataset(ctx, f.d.ID)
	if err != nil || gone.ID != f.d.ID {
		t.Fatalf("delete dataset = %+v, %v", gone, err)
	}
	if _, err = s.GetEvalCase(ctx, f.one.ID); !errors.Is(err, ErrEvalNotFound) {
		t.Errorf("case survived its dataset: %v", err)
	}
	var n int
	if err = s.scoped(ctx, func(tx dbtx, _ string) error {
		return tx.QueryRowContext(ctx, `SELECT count(*) FROM eval_cases`).Scan(&n)
	}); err != nil || n != 0 {
		t.Errorf("%d case rows left after deleting the dataset, %v", n, err)
	}
}

func TestEvalDatasetListFilters(t *testing.T) {
	s, _ := appStore(t)
	ctx := WithTenant(context.Background(), tenantA)
	evalDS(ctx, t, s, evalNSA, "b")
	evalDS(ctx, t, s, evalNSA, "a")
	evalDS(ctx, t, s, evalNSB, "c")
	list, err := s.ListEvalDatasets(ctx, EvalDatasetQuery{})
	if err != nil || len(list) != 3 || list[0].Name != "a" || list[2].Name != "c" {
		t.Fatalf("all = %+v, %v", list, err)
	}
	if list, _ = s.ListEvalDatasets(ctx, EvalDatasetQuery{Namespaces: []string{evalNSB}}); len(list) != 1 || list[0].Name != "c" {
		t.Errorf("namespaces = %+v", list)
	}
	if list, _ = s.ListEvalDatasets(ctx, EvalDatasetQuery{Namespaces: []string{}}); len(list) != 0 {
		t.Errorf("no namespaces = %+v", list)
	}
	if list, _ = s.ListEvalDatasets(ctx, EvalDatasetQuery{Namespace: evalNSA}); len(list) != 2 {
		t.Errorf("namespace = %+v", list)
	}
}

// Tenant A can neither see nor change tenant B's datasets and cases, through the
// store and through the database itself, and a case cannot carry another tenant
// than its dataset's. The role in appStore is not a superuser.
func TestEvalTablesTenantIsolationAgainstPostgres(t *testing.T) {
	s, raw := appStore(t)
	base := context.Background()
	a, b, plat := WithTenant(base, tenantA), WithTenant(base, tenantB), WithPlatform(base)

	dA, dB := evalDS(a, t, s, "shared", "d"), evalDS(b, t, s, "shared", "d")
	if dA.ID == dB.ID {
		t.Fatal("two tenants got the same dataset")
	}
	if _, _, err := s.AddEvalCases(a, dA.ID, []EvalCase{{Input: "a secret"}}, EvalLimits{}); err != nil {
		t.Fatal(err)
	}
	cB, _, err := s.AddEvalCases(b, dB.ID, []EvalCase{{Input: "b secret"}}, EvalLimits{})
	if err != nil {
		t.Fatal(err)
	}

	checkEvalReadsAreScoped(t, s, a, dA, dB, cB[0])
	checkEvalWritesAreScoped(t, s, a, b, dB, cB[0])
	if _, err := s.ListEvalDatasets(base, EvalDatasetQuery{}); !errors.Is(err, ErrNoTenantScope) {
		t.Errorf("unscoped list err = %v", err)
	}
	// The platform sees both, and cannot create a dataset without naming a tenant.
	if list, _ := s.ListEvalDatasets(plat, EvalDatasetQuery{}); len(list) != 2 {
		t.Errorf("platform sees %d datasets", len(list))
	}
	if _, err := s.CreateEvalDataset(plat, EvalDataset{Namespace: "n", Name: "x", CreatedBy: evalUser}); !errors.Is(err, ErrNoTenantScope) {
		t.Errorf("platform create without a tenant err = %v", err)
	}
	checkEvalRLSInTheDatabase(t, s, raw, a, plat, dA, dB)
}

func checkEvalReadsAreScoped(t *testing.T, s *Store, a context.Context, dA, dB EvalDataset, cB EvalCase) {
	t.Helper()
	if list, _ := s.ListEvalDatasets(a, EvalDatasetQuery{}); len(list) != 1 || list[0].ID != dA.ID {
		t.Errorf("A lists %+v", list)
	}
	if _, err := s.GetEvalDataset(a, dB.ID); !errors.Is(err, ErrEvalNotFound) {
		t.Errorf("A reads B's dataset: %v", err)
	}
	if _, err := s.GetEvalCase(a, cB.ID); !errors.Is(err, ErrEvalNotFound) {
		t.Errorf("A reads B's case: %v", err)
	}
	if _, err := s.ListEvalCases(a, dB.ID, EvalCaseQuery{}); !errors.Is(err, ErrEvalNotFound) {
		t.Errorf("A lists B's cases: %v", err)
	}
}

func checkEvalWritesAreScoped(t *testing.T, s *Store, a, b context.Context, dB EvalDataset, cB EvalCase) {
	t.Helper()
	l := EvalLimits{}
	x := "changed"
	if _, _, _, err := s.UpdateEvalCase(a, cB.ID, EvalCasePatch{Input: &x}, l); !errors.Is(err, ErrEvalNotFound) {
		t.Errorf("A edits B's case: %v", err)
	}
	if _, err := s.DeleteEvalCase(a, cB.ID); !errors.Is(err, ErrEvalNotFound) {
		t.Errorf("A deletes B's case: %v", err)
	}
	if _, _, err := s.AddEvalCases(a, dB.ID, []EvalCase{{Input: "planted"}}, l); !errors.Is(err, ErrEvalNotFound) {
		t.Errorf("A adds to B's dataset: %v", err)
	}
	if _, err := s.UpdateEvalDataset(a, dB.ID, EvalDatasetPatch{Description: &x}); !errors.Is(err, ErrEvalNotFound) {
		t.Errorf("A edits B's dataset: %v", err)
	}
	if _, err := s.DeleteEvalDataset(a, dB.ID); !errors.Is(err, ErrEvalNotFound) {
		t.Errorf("A deletes B's dataset: %v", err)
	}
	if got, err := s.GetEvalCase(b, cB.ID); err != nil || got.Input != "b secret" {
		t.Errorf("B's case was changed: %+v, %v", got, err)
	}
	if _, err := s.CreateEvalDataset(a, EvalDataset{TenantID: tenantB, Namespace: "n", Name: "x", CreatedBy: evalUser}); err == nil {
		t.Error("A created a dataset for B")
	}
}

// checkEvalRLSInTheDatabase goes under the store, with raw statements, to show
// the policy and the composite key do the work whatever the Go code does.
func checkEvalRLSInTheDatabase(t *testing.T, s *Store, raw *sql.DB, a, plat context.Context, dA, dB EvalDataset) {
	t.Helper()
	base := context.Background()
	for _, table := range []string{"eval_datasets", "eval_cases"} {
		var n int
		if err := raw.QueryRowContext(base, `SELECT count(*) FROM `+table).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s: unset setting returned %d rows, %v; want 0", table, n, err)
		}
		err := s.scoped(a, func(tx dbtx, _ string) error {
			return tx.QueryRowContext(base, `SELECT count(*) FROM `+table).Scan(&n)
		})
		if err != nil || n != 1 {
			t.Errorf("%s: an unfiltered query under A saw %d rows, %v; want 1", table, n, err)
		}
		errRollback := errors.New("roll back")
		err = s.scoped(a, func(tx dbtx, _ string) error {
			res, err := tx.ExecContext(base, `DELETE FROM `+table)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				t.Errorf("%s: an unfiltered DELETE under A removed %d rows, want 1", table, n)
			}
			return errRollback
		})
		if !errors.Is(err, errRollback) {
			t.Errorf("%s: %v", table, err)
		}
	}
	// A raw insert for another tenant is refused by the policy.
	err := s.scoped(a, func(tx dbtx, _ string) error {
		_, err := tx.ExecContext(base, `INSERT INTO eval_datasets (tenant_id, namespace, name, created_by) VALUES ($1, 'n', 'raw', 'x')`, tenantB)
		return err
	})
	if err == nil {
		t.Error("RLS let A insert a dataset row for B")
	}
	// A case row whose tenant differs from its dataset's is refused, even when
	// the caller could write both tenants (the platform scope).
	err = s.scoped(plat, func(tx dbtx, _ string) error {
		_, err := tx.ExecContext(base, `INSERT INTO eval_cases (tenant_id, dataset_id, added_in, input) VALUES ($1, $2, 1, 'x')`, tenantB, dA.ID)
		return err
	})
	if err == nil {
		t.Error("a case was stored under a tenant other than its dataset's")
	}
	err = s.scoped(a, func(tx dbtx, _ string) error {
		_, err := tx.ExecContext(base, `INSERT INTO eval_cases (tenant_id, dataset_id, added_in, input) VALUES ($1, $2, 1, 'x')`, tenantA, dB.ID)
		return err
	})
	if err == nil {
		t.Error("A stored a case in B's dataset")
	}
	// Both tables carry FORCE: the policy applies to the owner too.
	var forced int
	if err := raw.QueryRowContext(base, `SELECT count(*) FROM pg_class WHERE relname IN ('eval_datasets','eval_cases') AND relrowsecurity AND relforcerowsecurity`).Scan(&forced); err != nil || forced != 2 {
		t.Errorf("tables with ENABLE and FORCE ROW LEVEL SECURITY = %d, %v; want 2", forced, err)
	}
}
