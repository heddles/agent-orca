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
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// RevisionRestore is the action of a revision written by a restore. Create,
// update and delete reuse the Audit* names.
const RevisionRestore = "restore"

// MaxRevisionObjectBytes bounds a stored object, the same as the UI API's body limit.
const MaxRevisionObjectBytes = 2 << 20

// MaxRevisionSummaryBytes bounds the summary of a revision.
const MaxRevisionSummaryBytes = 1024

var (
	// ErrInvalidRevision is returned for a revision the table would not take.
	ErrInvalidRevision = errors.New("invalid revision")
	// ErrRevisionNotFound means the object has no such revision in the caller's scope.
	ErrRevisionNotFound = errors.New("revision not found")
)

// Revision is one saved state of an object written through the UI API (table
// revisions, migration 0012).
type Revision struct {
	Revision        int       `json:"revision"`
	Action          string    `json:"action"`
	UserID          string    `json:"userId"`
	CreatedAt       time.Time `json:"createdAt"`
	Summary         string    `json:"summary"`
	ResourceVersion string    `json:"resourceVersion"`
	RestoredFrom    *int      `json:"restoredFrom,omitempty"`
	// Object is the object as stored after the write. ListRevisions leaves it out.
	Object json.RawMessage `json:"object,omitempty"`

	// The key of the object. TenantID is the tenant's stable ID; on an append,
	// empty takes the call's tenant scope.
	TenantID  string `json:"-"`
	Kind      string `json:"-"`
	Namespace string `json:"-"`
	Name      string `json:"-"`
}

// RevisionQuery selects the revisions of one object.
type RevisionQuery struct {
	// TenantID narrows a platform-scoped query to one tenant. A tenant-scoped
	// call is always limited to its own tenant.
	TenantID  string
	Kind      string
	Namespace string
	Name      string
	// Before is a cursor: only revisions numbered below it. Zero starts at the newest.
	Before int
	Limit  int
}

// objectKey is the WHERE clause that selects one object's rows; $1 is the tenant.
const objectKey = `tenant_id IS NOT DISTINCT FROM NULLIF($1, '') AND kind = $2 AND namespace = $3 AND name = $4`

// AppendRevision stores rev as the object's next revision and returns it with
// its number. keep, when positive, is how many of the newest revisions the object
// keeps; older ones are pruned in the same transaction, except the revision the
// object's current content equals.
//
// A save whose content equals the object's latest revision is not stored: the
// latest revision is returned and stored is false. A delete is always stored.
// Writers of one object take turns on an advisory lock, so numbers never repeat.
func (s *Store) AppendRevision(ctx context.Context, rev Revision, keep int) (out Revision, stored bool, err error) {
	if rev.Action == "" || rev.Kind == "" || rev.Name == "" || len(rev.Object) == 0 {
		return Revision{}, false, fmt.Errorf("%w: action, kind, name and object are required", ErrInvalidRevision)
	}
	if len(rev.Object) > MaxRevisionObjectBytes {
		return Revision{}, false, fmt.Errorf("%w: the object is %d bytes, over the %d byte limit", ErrInvalidRevision, len(rev.Object), MaxRevisionObjectBytes)
	}
	if len(rev.Summary) > MaxRevisionSummaryBytes {
		rev.Summary = strings.ToValidUTF8(rev.Summary[:MaxRevisionSummaryBytes], "")
	}
	err = s.scoped(ctx, func(tx dbtx, scope string) error {
		tid, err := rowTenant(scope, rev.TenantID)
		if err != nil {
			return err
		}
		rev.TenantID = tid
		key := tid + "|" + rev.Kind + "|" + rev.Namespace + "|" + rev.Name
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, key); err != nil {
			return err
		}
		var (
			prevNum    sql.NullInt64
			prevAction string
			equal      bool
		)
		err = tx.QueryRowContext(ctx, `SELECT revision, action, object = $5::jsonb FROM revisions WHERE `+objectKey+` ORDER BY revision DESC LIMIT 1`,
			tid, rev.Kind, rev.Namespace, rev.Name, string(rev.Object)).Scan(&prevNum, &prevAction, &equal)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if prevNum.Valid && equal && rev.Action != AuditDelete && prevAction != AuditDelete {
			out, err = getRevision(ctx, tx, scope, RevisionQuery{TenantID: tid, Kind: rev.Kind, Namespace: rev.Namespace, Name: rev.Name}, int(prevNum.Int64))
			return err
		}
		rev.Revision = int(prevNum.Int64) + 1
		err = tx.QueryRowContext(ctx, `
		INSERT INTO revisions (tenant_id, kind, namespace, name, revision, action, user_id, summary, resource_version, restored_from, object)
		VALUES (NULLIF($1, ''), $2, $3, $4, $5, $6, $7, $8, $9, $10, $11::jsonb)
		RETURNING created_at`,
			tid, rev.Kind, rev.Namespace, rev.Name, rev.Revision, rev.Action, rev.UserID, rev.Summary, rev.ResourceVersion, rev.RestoredFrom, string(rev.Object),
		).Scan(&rev.CreatedAt)
		if err != nil {
			return err
		}
		stored = true
		out = rev
		if keep <= 0 {
			return nil
		}
		// Keep the newest `keep` rows, and the newest row whose content equals
		// the object just written (the live object's). That row is the one just
		// inserted, so it always survives; a delete leaves no live object to protect.
		_, err = tx.ExecContext(ctx, `
		DELETE FROM revisions WHERE `+objectKey+` AND revision <= $5
		AND revision IS DISTINCT FROM (
			SELECT max(revision) FROM revisions WHERE `+objectKey+` AND $7::text <> 'delete' AND object = $6::jsonb)`,
			tid, rev.Kind, rev.Namespace, rev.Name, rev.Revision-keep, string(rev.Object), rev.Action)
		return err
	})
	if err != nil {
		return Revision{}, false, fmt.Errorf("storing revision: %w", err)
	}
	return out, stored, nil
}

// ListRevisions returns the object's revisions without their objects, newest first.
func (s *Store) ListRevisions(ctx context.Context, q RevisionQuery) ([]Revision, error) {
	out := []Revision{}
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		var args []any
		arg := func(v any) string {
			args = append(args, v)
			return fmt.Sprintf("$%d", len(args))
		}
		where := []string{"kind = " + arg(q.Kind), "namespace = " + arg(q.Namespace), "name = " + arg(q.Name)}
		if cond, ok := tenantFilter(scope, q.TenantID, arg); ok {
			where = append(where, cond)
		}
		if q.Before > 0 {
			where = append(where, "revision < "+arg(q.Before))
		}
		limit := q.Limit
		if limit <= 0 || limit > 1000 {
			limit = 100
		}
		rows, err := tx.QueryContext(ctx, `SELECT revision, action, user_id, created_at, summary, resource_version, restored_from, COALESCE(tenant_id, '')
			FROM revisions WHERE `+strings.Join(where, " AND ")+` ORDER BY revision DESC LIMIT `+arg(limit), args...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			r := Revision{Kind: q.Kind, Namespace: q.Namespace, Name: q.Name}
			var restored sql.NullInt64
			if err := rows.Scan(&r.Revision, &r.Action, &r.UserID, &r.CreatedAt, &r.Summary, &r.ResourceVersion, &restored, &r.TenantID); err != nil {
				return fmt.Errorf("scanning revision: %w", err)
			}
			if restored.Valid {
				n := int(restored.Int64)
				r.RestoredFrom = &n
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("listing revisions: %w", err)
	}
	return out, nil
}

// GetRevision returns revision n of an object with its object, or
// ErrRevisionNotFound.
func (s *Store) GetRevision(ctx context.Context, q RevisionQuery, n int) (Revision, error) {
	var out Revision
	err := s.scoped(ctx, func(tx dbtx, scope string) (err error) {
		out, err = getRevision(ctx, tx, scope, q, n)
		return err
	})
	if errors.Is(err, ErrRevisionNotFound) {
		return Revision{}, err
	}
	if err != nil {
		return Revision{}, fmt.Errorf("reading revision: %w", err)
	}
	return out, nil
}

func getRevision(ctx context.Context, tx dbtx, scope string, q RevisionQuery, n int) (Revision, error) {
	var args []any
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	where := []string{"kind = " + arg(q.Kind), "namespace = " + arg(q.Namespace), "name = " + arg(q.Name), "revision = " + arg(n)}
	if cond, ok := tenantFilter(scope, q.TenantID, arg); ok {
		where = append(where, cond)
	}
	r := Revision{Kind: q.Kind, Namespace: q.Namespace, Name: q.Name}
	var restored sql.NullInt64
	var obj []byte
	err := tx.QueryRowContext(ctx, `SELECT revision, action, user_id, created_at, summary, resource_version, restored_from, COALESCE(tenant_id, ''), object
		FROM revisions WHERE `+strings.Join(where, " AND ")+` ORDER BY id DESC LIMIT 1`, args...).
		Scan(&r.Revision, &r.Action, &r.UserID, &r.CreatedAt, &r.Summary, &r.ResourceVersion, &restored, &r.TenantID, &obj)
	if errors.Is(err, sql.ErrNoRows) {
		return Revision{}, ErrRevisionNotFound
	}
	if err != nil {
		return Revision{}, err
	}
	if restored.Valid {
		v := int(restored.Int64)
		r.RestoredFrom = &v
	}
	r.Object = obj
	return r, nil
}
