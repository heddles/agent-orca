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

// Evaluation datasets and cases (tables eval_datasets and eval_cases, migration
// 0013). Every call runs in the tenant scope of its context. A case's tenant_id
// is copied from its dataset inside the same transaction, never taken from the
// caller.
//
// The dataset version. eval_datasets.version starts at 1 and goes up by exactly
// one for each call that changes the dataset's cases:
//
//   - AddEvalCases: +1 per call, however many cases it adds (a bulk import is one
//     change). The new cases have added_in = the new version.
//   - UpdateEvalCase: +1 when at least one field really changes. A patch that
//     leaves every field as it was changes nothing and does not bump.
//   - DeleteEvalCase: +1. The case is kept with removed_in = the new version.
//
// Changing only the dataset's own description or agent does not bump it, and
// neither does any read. The bump and the change are in one transaction, and the
// dataset row is locked first (SELECT ... FOR UPDATE), so two concurrent writers
// get distinct versions and the case cap cannot be raced past.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Evaluation dataset errors.
var (
	// ErrEvalNotFound: no such dataset or case in the call's tenant scope.
	ErrEvalNotFound = errors.New("evaluation dataset or case not found")
	// ErrEvalExists: a dataset with that workspace and name exists.
	ErrEvalExists = errors.New("an evaluation dataset with that name already exists")
	// ErrDatasetFull: the add would take the dataset over its case limit.
	ErrDatasetFull = errors.New("the dataset is full")
	// ErrInvalidEval: a dataset or case the tables would not take.
	ErrInvalidEval = errors.New("invalid evaluation data")
	// ErrEvalTooLarge: a case field over the size limit. It is an ErrInvalidEval.
	ErrEvalTooLarge = fmt.Errorf("%w: too large", ErrInvalidEval)
)

// Reference kinds of a case.
const (
	ReferenceNone    = "none"
	ReferenceExact   = "exact"
	ReferenceExample = "example"
	ReferenceRubric  = "rubric"
)

// Hard limits the tables enforce. A configured limit can be lower, not higher.
const (
	MaxEvalCaseBytes      = 65536
	MaxEvalTags           = 20
	MaxEvalTagBytes       = 64
	MaxEvalDescription    = 2000
	MaxEvalCasesPerPage   = 200
	defaultCasesPerPage   = 50
	maxEvalDatasetsListed = 500
)

var evalNameRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// EvalLimits are the configurable limits on writes. A zero field uses the
// default (2000 cases, MaxEvalCaseBytes); MaxCaseBytes above MaxEvalCaseBytes
// is capped to it, as the table's CHECK would refuse more.
type EvalLimits struct {
	MaxCasesPerDataset int
	MaxCaseBytes       int
}

// DefaultMaxCasesPerDataset is the default case limit of a dataset.
const DefaultMaxCasesPerDataset = 2000

func (l EvalLimits) cases() int {
	if l.MaxCasesPerDataset <= 0 {
		return DefaultMaxCasesPerDataset
	}
	return l.MaxCasesPerDataset
}

func (l EvalLimits) bytes() int {
	if l.MaxCaseBytes <= 0 || l.MaxCaseBytes > MaxEvalCaseBytes {
		return MaxEvalCaseBytes
	}
	return l.MaxCaseBytes
}

// EvalDataset is a named, versioned set of cases.
type EvalDataset struct {
	ID        int64  `json:"id"`
	TenantID  string `json:"tenantId,omitempty"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	// Description is free text, at most MaxEvalDescription bytes.
	Description string `json:"description"`
	// Agent optionally names the Agent the dataset is meant for.
	Agent string `json:"agent"`
	// Version is raised by every change to the dataset's cases (see above).
	Version int `json:"version"`
	// CaseCount is the number of live (not removed) cases. Filled on reads.
	CaseCount int       `json:"caseCount"`
	CreatedBy string    `json:"createdBy"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Validate reports why d cannot be stored.
func (d EvalDataset) Validate() error {
	switch {
	case d.Namespace == "":
		return fmt.Errorf("%w: namespace is required", ErrInvalidEval)
	case !evalNameRe.MatchString(d.Name) || len(d.Name) > 63:
		return fmt.Errorf("%w: name must be a DNS label (lowercase letters, digits and dashes, at most 63 characters)", ErrInvalidEval)
	case d.Agent != "" && (!evalNameRe.MatchString(d.Agent) || len(d.Agent) > 253):
		return fmt.Errorf("%w: agent must be an Agent name", ErrInvalidEval)
	case len(d.Description) > MaxEvalDescription:
		return fmt.Errorf("%w: description is over %d bytes", ErrInvalidEval, MaxEvalDescription)
	case d.CreatedBy == "":
		return fmt.Errorf("%w: created by is required", ErrInvalidEval)
	}
	return nil
}

// EvalDatasetPatch changes a dataset's description or agent. A nil field is left alone.
type EvalDatasetPatch struct {
	Description *string
	Agent       *string
}

// EvalDatasetQuery filters ListEvalDatasets. Empty fields match everything.
type EvalDatasetQuery struct {
	// Namespaces limits the list to a tenant's namespaces; nil does not limit.
	Namespaces []string
	Namespace  string
	Agent      string
}

// EvalCase is one input, with an optional reference, in a dataset.
type EvalCase struct {
	ID        int64  `json:"id"`
	DatasetID int64  `json:"datasetId"`
	TenantID  string `json:"tenantId,omitempty"`
	// Namespace and DatasetName are the dataset's. Filled on reads.
	Namespace   string `json:"namespace,omitempty"`
	DatasetName string `json:"datasetName,omitempty"`
	// AddedIn is the dataset version that added the case; RemovedIn the one that
	// removed it, nil while the case is live.
	AddedIn       int      `json:"addedIn"`
	RemovedIn     *int     `json:"removedIn,omitempty"`
	Input         string   `json:"input"`
	Reference     string   `json:"reference,omitempty"`
	ReferenceKind string   `json:"referenceKind"`
	Tags          []string `json:"tags"`
	Weight        float64  `json:"weight"`
	// SourceRunNamespace and SourceRun point at the run the case was copied from.
	// A pointer only: the run may be deleted.
	SourceRunNamespace string `json:"sourceRunNamespace,omitempty"`
	SourceRun          string `json:"sourceRun,omitempty"`
	SourceUser         string `json:"sourceUser,omitempty"`
	// Redacted is true when the text went through redaction on capture.
	Redacted  bool      `json:"redacted"`
	CreatedAt time.Time `json:"createdAt"`
}

// normalize fills defaults and cleans the tags. It does not check limits.
func (c *EvalCase) normalize() {
	if c.Weight == 0 {
		c.Weight = 1
	}
	if c.ReferenceKind == "" {
		if c.Reference == "" {
			c.ReferenceKind = ReferenceNone
		} else {
			c.ReferenceKind = ReferenceExact
		}
	}
	c.Tags = cleanTags(c.Tags)
}

func cleanTags(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, t := range in {
		t = strings.TrimSpace(t)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

// Validate reports why c cannot be stored under limits.
func (c EvalCase) Validate(l EvalLimits) error {
	limit := l.bytes()
	switch {
	case strings.TrimSpace(c.Input) == "":
		return fmt.Errorf("%w: input is required", ErrInvalidEval)
	case len(c.Input) > limit:
		return fmt.Errorf("%w: input is %d bytes, the limit is %d", ErrEvalTooLarge, len(c.Input), limit)
	case len(c.Reference) > limit:
		return fmt.Errorf("%w: reference is %d bytes, the limit is %d", ErrEvalTooLarge, len(c.Reference), limit)
	case c.ReferenceKind != ReferenceNone && c.ReferenceKind != ReferenceExact &&
		c.ReferenceKind != ReferenceExample && c.ReferenceKind != ReferenceRubric:
		return fmt.Errorf("%w: referenceKind must be none, exact, example or rubric", ErrInvalidEval)
	case c.ReferenceKind == ReferenceNone && c.Reference != "":
		return fmt.Errorf("%w: a reference needs a referenceKind other than none", ErrInvalidEval)
	case c.ReferenceKind != ReferenceNone && strings.TrimSpace(c.Reference) == "":
		return fmt.Errorf("%w: referenceKind %s needs a reference", ErrInvalidEval, c.ReferenceKind)
	case !(c.Weight > 0):
		return fmt.Errorf("%w: weight must be above 0", ErrInvalidEval)
	case len(c.Tags) > MaxEvalTags:
		return fmt.Errorf("%w: at most %d tags", ErrInvalidEval, MaxEvalTags)
	case (c.SourceRun == "") != (c.SourceRunNamespace == ""):
		return fmt.Errorf("%w: source run needs a namespace and a name", ErrInvalidEval)
	}
	for _, t := range c.Tags {
		if len(t) > MaxEvalTagBytes {
			return fmt.Errorf("%w: a tag is over %d bytes", ErrInvalidEval, MaxEvalTagBytes)
		}
	}
	return nil
}

// EvalCasePatch changes a case. A nil field is left alone.
type EvalCasePatch struct {
	Input         *string
	Reference     *string
	ReferenceKind *string
	Tags          *[]string
	Weight        *float64
}

// EvalCaseQuery filters ListEvalCases.
type EvalCaseQuery struct {
	// Tag limits the list to cases carrying it.
	Tag string
	// After is a cursor: only cases with an ID above it.
	After int64
	// Limit is the page size: default 50, at most MaxEvalCasesPerPage.
	Limit int
}

// CreateEvalDataset stores a dataset at version 1. A platform-scoped call must
// name the tenant in d.TenantID; a dataset always has one.
func (s *Store) CreateEvalDataset(ctx context.Context, d EvalDataset) (EvalDataset, error) {
	if err := d.Validate(); err != nil {
		return EvalDataset{}, err
	}
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		tid, err := rowTenant(scope, d.TenantID)
		if err != nil {
			return err
		}
		if tid == "" {
			return fmt.Errorf("%w: a dataset belongs to a tenant", ErrNoTenantScope)
		}
		d.TenantID = tid
		err = tx.QueryRowContext(ctx, `
		INSERT INTO eval_datasets (tenant_id, namespace, name, description, agent, created_by)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (tenant_id, namespace, name) DO NOTHING
		RETURNING id, version, created_at, updated_at`,
			tid, d.Namespace, d.Name, d.Description, d.Agent, d.CreatedBy,
		).Scan(&d.ID, &d.Version, &d.CreatedAt, &d.UpdatedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrEvalExists
		}
		return err
	})
	if err != nil {
		return EvalDataset{}, wrapEval("creating the dataset", err)
	}
	return d, nil
}

const evalDatasetCols = `d.id, d.tenant_id, d.namespace, d.name, d.description, d.agent, d.version,
	(SELECT count(*) FROM eval_cases c WHERE c.dataset_id = d.id AND c.tenant_id = d.tenant_id AND c.removed_in IS NULL),
	d.created_by, d.created_at, d.updated_at`

func scanEvalDataset(row interface{ Scan(...any) error }) (EvalDataset, error) {
	var d EvalDataset
	err := row.Scan(&d.ID, &d.TenantID, &d.Namespace, &d.Name, &d.Description, &d.Agent, &d.Version,
		&d.CaseCount, &d.CreatedBy, &d.CreatedAt, &d.UpdatedAt)
	return d, err
}

// GetEvalDataset returns one dataset, or ErrEvalNotFound.
func (s *Store) GetEvalDataset(ctx context.Context, id int64) (EvalDataset, error) {
	var d EvalDataset
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		var err error
		d, err = scanEvalDataset(tx.QueryRowContext(ctx,
			`SELECT `+evalDatasetCols+` FROM eval_datasets d WHERE d.id = $1 AND ($2::text = '*' OR d.tenant_id = $2)`,
			id, scope))
		return notFound(err)
	})
	if err != nil {
		return EvalDataset{}, wrapEval("loading the dataset", err)
	}
	return d, nil
}

// ListEvalDatasets returns datasets ordered by workspace and name.
func (s *Store) ListEvalDatasets(ctx context.Context, q EvalDatasetQuery) ([]EvalDataset, error) {
	out := []EvalDataset{}
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		where := []string{"($1::text = '*' OR d.tenant_id = $1)"}
		args := []any{scope}
		arg := func(v any) string {
			args = append(args, v)
			return fmt.Sprintf("$%d", len(args))
		}
		if q.Namespaces != nil {
			if len(q.Namespaces) == 0 {
				return nil
			}
			ph := make([]string, len(q.Namespaces))
			for i, ns := range q.Namespaces {
				ph[i] = arg(ns)
			}
			where = append(where, "d.namespace IN ("+strings.Join(ph, ", ")+")")
		}
		if q.Namespace != "" {
			where = append(where, "d.namespace = "+arg(q.Namespace))
		}
		if q.Agent != "" {
			where = append(where, "d.agent = "+arg(q.Agent))
		}
		rows, err := tx.QueryContext(ctx, `SELECT `+evalDatasetCols+` FROM eval_datasets d WHERE `+
			strings.Join(where, " AND ")+` ORDER BY d.namespace, d.name LIMIT `+arg(maxEvalDatasetsListed), args...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			d, err := scanEvalDataset(rows)
			if err != nil {
				return fmt.Errorf("scanning dataset: %w", err)
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, wrapEval("listing datasets", err)
	}
	return out, nil
}

// UpdateEvalDataset changes a dataset's description or agent. It does not bump
// the version.
func (s *Store) UpdateEvalDataset(ctx context.Context, id int64, p EvalDatasetPatch) (EvalDataset, error) {
	var d EvalDataset
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		var err error
		d, err = scanEvalDataset(tx.QueryRowContext(ctx,
			`SELECT `+evalDatasetCols+` FROM eval_datasets d WHERE d.id = $1 AND ($2::text = '*' OR d.tenant_id = $2) FOR UPDATE OF d`,
			id, scope))
		if err != nil {
			return notFound(err)
		}
		if p.Description != nil {
			d.Description = *p.Description
		}
		if p.Agent != nil {
			d.Agent = *p.Agent
		}
		if err := d.Validate(); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx,
			`UPDATE eval_datasets SET description = $2, agent = $3, updated_at = now() WHERE id = $1 RETURNING updated_at`,
			id, d.Description, d.Agent).Scan(&d.UpdatedAt)
	})
	if err != nil {
		return EvalDataset{}, wrapEval("updating the dataset", err)
	}
	return d, nil
}

// DeleteEvalDataset deletes a dataset and, through the foreign key, its cases,
// in one statement. It returns the dataset as it was.
func (s *Store) DeleteEvalDataset(ctx context.Context, id int64) (EvalDataset, error) {
	var d EvalDataset
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		var err error
		d, err = scanEvalDataset(tx.QueryRowContext(ctx,
			`SELECT `+evalDatasetCols+` FROM eval_datasets d WHERE d.id = $1 AND ($2::text = '*' OR d.tenant_id = $2) FOR UPDATE OF d`,
			id, scope))
		if err != nil {
			return notFound(err)
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM eval_datasets WHERE id = $1`, id)
		return err
	})
	if err != nil {
		return EvalDataset{}, wrapEval("deleting the dataset", err)
	}
	return d, nil
}

// lockDataset locks a dataset row and returns what a case write needs.
func lockDataset(ctx context.Context, tx dbtx, scope string, id int64) (tenant, namespace string, version int, err error) {
	err = tx.QueryRowContext(ctx,
		`SELECT tenant_id, namespace, version FROM eval_datasets WHERE id = $1 AND ($2::text = '*' OR tenant_id = $2) FOR UPDATE`,
		id, scope).Scan(&tenant, &namespace, &version)
	return tenant, namespace, version, notFound(err)
}

// bumpDataset sets the dataset's version to v.
func bumpDataset(ctx context.Context, tx dbtx, id int64, v int) error {
	_, err := tx.ExecContext(ctx, `UPDATE eval_datasets SET version = $2, updated_at = now() WHERE id = $1`, id, v)
	return err
}

func tagsJSON(tags []string) string {
	b, _ := json.Marshal(cleanTags(tags))
	return string(b)
}

// AddEvalCases adds cases to a dataset in one transaction and returns them with
// the dataset's new version (one bump for the whole call). It fails with
// ErrDatasetFull, adding nothing, when the live cases would go over the limit.
// TenantID, DatasetID, AddedIn and CreatedAt on the input are ignored.
func (s *Store) AddEvalCases(ctx context.Context, datasetID int64, cases []EvalCase, l EvalLimits) ([]EvalCase, int, error) {
	if len(cases) == 0 {
		return nil, 0, fmt.Errorf("%w: no cases to add", ErrInvalidEval)
	}
	for i := range cases {
		cases[i].normalize()
		if err := cases[i].Validate(l); err != nil {
			return nil, 0, fmt.Errorf("case %d: %w", i+1, err)
		}
	}
	var version int
	out := make([]EvalCase, len(cases))
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		tenant, ns, v, err := lockDataset(ctx, tx, scope, datasetID)
		if err != nil {
			return err
		}
		var live int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM eval_cases WHERE dataset_id = $1 AND tenant_id = $2 AND removed_in IS NULL`,
			datasetID, tenant).Scan(&live); err != nil {
			return err
		}
		if live+len(cases) > l.cases() {
			return fmt.Errorf("%w: it holds %d cases, adding %d would pass the limit of %d", ErrDatasetFull, live, len(cases), l.cases())
		}
		version = v + 1
		if err := bumpDataset(ctx, tx, datasetID, version); err != nil {
			return err
		}
		for i, c := range cases {
			c.TenantID, c.DatasetID, c.Namespace, c.AddedIn = tenant, datasetID, ns, version
			err := tx.QueryRowContext(ctx, `
			INSERT INTO eval_cases (tenant_id, dataset_id, added_in, input, reference, reference_kind, tags, weight,
				source_run_ns, source_run, source_user, redacted)
			VALUES ($1, $2, $3, $4, NULLIF($5, ''), $6, ARRAY(SELECT jsonb_array_elements_text($7::jsonb)), $8,
				NULLIF($9, ''), NULLIF($10, ''), $11, $12)
			RETURNING id, created_at`,
				tenant, datasetID, version, c.Input, c.Reference, c.ReferenceKind, tagsJSON(c.Tags), c.Weight,
				c.SourceRunNamespace, c.SourceRun, c.SourceUser, c.Redacted).Scan(&c.ID, &c.CreatedAt)
			if err != nil {
				return err
			}
			out[i] = c
		}
		return nil
	})
	if err != nil {
		return nil, 0, wrapEval("adding cases", err)
	}
	return out, version, nil
}

const evalCaseCols = `c.id, c.dataset_id, c.tenant_id, d.namespace, d.name, c.added_in, c.removed_in, c.input,
	COALESCE(c.reference, ''), c.reference_kind, to_jsonb(c.tags), c.weight,
	COALESCE(c.source_run_ns, ''), COALESCE(c.source_run, ''), c.source_user, c.redacted, c.created_at`

const evalCaseFrom = ` FROM eval_cases c JOIN eval_datasets d ON d.id = c.dataset_id AND d.tenant_id = c.tenant_id`

func scanEvalCase(row interface{ Scan(...any) error }) (EvalCase, error) {
	var c EvalCase
	var removed sql.NullInt64
	var tags []byte
	err := row.Scan(&c.ID, &c.DatasetID, &c.TenantID, &c.Namespace, &c.DatasetName, &c.AddedIn, &removed, &c.Input,
		&c.Reference, &c.ReferenceKind, &tags, &c.Weight,
		&c.SourceRunNamespace, &c.SourceRun, &c.SourceUser, &c.Redacted, &c.CreatedAt)
	if err != nil {
		return c, err
	}
	if removed.Valid {
		v := int(removed.Int64)
		c.RemovedIn = &v
	}
	c.Tags = []string{}
	if len(tags) > 0 {
		if err := json.Unmarshal(tags, &c.Tags); err != nil {
			return c, fmt.Errorf("reading tags: %w", err)
		}
	}
	return c, nil
}

// ListEvalCases returns a dataset's live cases in ID order. It returns
// ErrEvalNotFound when the dataset is not in the call's scope.
func (s *Store) ListEvalCases(ctx context.Context, datasetID int64, q EvalCaseQuery) ([]EvalCase, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = defaultCasesPerPage
	}
	// One above the page maximum, so a caller can tell whether another page follows.
	if limit > MaxEvalCasesPerPage+1 {
		limit = MaxEvalCasesPerPage + 1
	}
	out := []EvalCase{}
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		var one int
		if err := tx.QueryRowContext(ctx,
			`SELECT 1 FROM eval_datasets WHERE id = $1 AND ($2::text = '*' OR tenant_id = $2)`, datasetID, scope).Scan(&one); err != nil {
			return notFound(err)
		}
		args := []any{datasetID, q.After}
		where := `c.dataset_id = $1 AND c.removed_in IS NULL AND c.id > $2`
		if q.Tag != "" {
			args = append(args, q.Tag)
			where += fmt.Sprintf(" AND $%d = ANY(c.tags)", len(args))
		}
		args = append(args, limit)
		rows, err := tx.QueryContext(ctx, `SELECT `+evalCaseCols+evalCaseFrom+` WHERE `+where+
			fmt.Sprintf(` ORDER BY c.id LIMIT $%d`, len(args)), args...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			c, err := scanEvalCase(rows)
			if err != nil {
				return fmt.Errorf("scanning case: %w", err)
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, wrapEval("listing cases", err)
	}
	return out, nil
}

// GetEvalCase returns one live case, or ErrEvalNotFound.
func (s *Store) GetEvalCase(ctx context.Context, id int64) (EvalCase, error) {
	var c EvalCase
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		var err error
		c, err = scanEvalCase(tx.QueryRowContext(ctx,
			`SELECT `+evalCaseCols+evalCaseFrom+` WHERE c.id = $1 AND c.removed_in IS NULL AND ($2::text = '*' OR c.tenant_id = $2)`,
			id, scope))
		return notFound(err)
	})
	if err != nil {
		return EvalCase{}, wrapEval("loading the case", err)
	}
	return c, nil
}

// lockCase locks a live case and its dataset (dataset first, as the writers
// of AddEvalCases do) and returns the case and the dataset's version.
func lockCase(ctx context.Context, tx dbtx, scope string, id int64) (EvalCase, int, error) {
	var datasetID int64
	err := tx.QueryRowContext(ctx,
		`SELECT dataset_id FROM eval_cases WHERE id = $1 AND removed_in IS NULL AND ($2::text = '*' OR tenant_id = $2)`,
		id, scope).Scan(&datasetID)
	if err != nil {
		return EvalCase{}, 0, notFound(err)
	}
	if _, _, _, err := lockDataset(ctx, tx, scope, datasetID); err != nil {
		return EvalCase{}, 0, err
	}
	c, err := scanEvalCase(tx.QueryRowContext(ctx,
		`SELECT `+evalCaseCols+evalCaseFrom+` WHERE c.id = $1 AND c.removed_in IS NULL FOR UPDATE OF c`, id))
	if err != nil {
		return EvalCase{}, 0, notFound(err)
	}
	var version int
	if err := tx.QueryRowContext(ctx, `SELECT version FROM eval_datasets WHERE id = $1`, datasetID).Scan(&version); err != nil {
		return EvalCase{}, 0, err
	}
	return c, version, nil
}

// UpdateEvalCase changes a live case and returns it with the dataset's
// version and whether anything changed. The version goes up by one only if a
// field changed. Changing the
// input or reference clears the redacted flag: the text is no longer what the
// redaction produced.
func (s *Store) UpdateEvalCase(ctx context.Context, id int64, p EvalCasePatch, l EvalLimits) (EvalCase, int, bool, error) {
	var c EvalCase
	var version int
	var changed bool
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		cur, v, err := lockCase(ctx, tx, scope, id)
		if err != nil {
			return err
		}
		next := cur
		if p.Input != nil {
			next.Input = *p.Input
		}
		if p.Reference != nil {
			next.Reference = *p.Reference
			if p.ReferenceKind == nil {
				next.ReferenceKind = ReferenceNone
				if next.Reference != "" {
					next.ReferenceKind = ReferenceExact
					if cur.ReferenceKind != ReferenceNone {
						next.ReferenceKind = cur.ReferenceKind
					}
				}
			}
		}
		if p.ReferenceKind != nil {
			next.ReferenceKind = *p.ReferenceKind
		}
		if p.Tags != nil {
			next.Tags = cleanTags(*p.Tags)
		}
		if p.Weight != nil {
			next.Weight = *p.Weight
		}
		if err := next.Validate(l); err != nil {
			return err
		}
		c, version = cur, v
		if evalCaseEqual(cur, next) {
			return nil
		}
		changed = true
		if next.Input != cur.Input || next.Reference != cur.Reference {
			next.Redacted = false
		}
		version = v + 1
		if err := bumpDataset(ctx, tx, cur.DatasetID, version); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
		UPDATE eval_cases SET input = $2, reference = NULLIF($3, ''), reference_kind = $4,
			tags = ARRAY(SELECT jsonb_array_elements_text($5::jsonb)), weight = $6, redacted = $7
		WHERE id = $1`, id, next.Input, next.Reference, next.ReferenceKind, tagsJSON(next.Tags), next.Weight, next.Redacted)
		c = next
		return err
	})
	if err != nil {
		return EvalCase{}, 0, false, wrapEval("updating the case", err)
	}
	return c, version, changed, nil
}

func evalCaseEqual(a, b EvalCase) bool {
	if a.Input != b.Input || a.Reference != b.Reference || a.ReferenceKind != b.ReferenceKind || a.Weight != b.Weight ||
		len(a.Tags) != len(b.Tags) {
		return false
	}
	for i := range a.Tags {
		if a.Tags[i] != b.Tags[i] {
			return false
		}
	}
	return true
}

// DeleteEvalCase removes a live case from the dataset's current version: the
// row stays with removed_in set. It returns the dataset's new version (one bump).
func (s *Store) DeleteEvalCase(ctx context.Context, id int64) (int, error) {
	var version int
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		c, v, err := lockCase(ctx, tx, scope, id)
		if err != nil {
			return err
		}
		version = v + 1
		if err := bumpDataset(ctx, tx, c.DatasetID, version); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE eval_cases SET removed_in = $2 WHERE id = $1`, id, version)
		return err
	})
	if err != nil {
		return 0, wrapEval("removing the case", err)
	}
	return version, nil
}

func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrEvalNotFound
	}
	return err
}

// wrapEval adds context to an error but keeps the package's sentinel errors
// reachable with errors.Is, and does not prefix them (their text is for callers).
func wrapEval(what string, err error) error {
	for _, sentinel := range []error{ErrEvalNotFound, ErrEvalExists, ErrDatasetFull, ErrInvalidEval, ErrNoTenantScope} {
		if errors.Is(err, sentinel) {
			return err
		}
	}
	return fmt.Errorf("%s: %w", what, err)
}
