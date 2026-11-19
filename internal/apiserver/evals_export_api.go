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
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// GET /api/evals/datasets/{id}/export: the dataset's live cases as JSON lines
// (builder, audited). Data moves out by export, not by Git write-back
// (docs/proposals/evaluations.md, decision 2).

// exportLine is one line of an export: the shape POST .../cases accepts as JSON
// lines, so an export can be imported into another dataset or tenant. Source
// runs and users are left out: they are tenant-local pointers.
type exportLine struct {
	Input         string   `json:"input"`
	Reference     string   `json:"reference,omitempty"`
	ReferenceKind string   `json:"referenceKind,omitempty"`
	Tags          []string `json:"tags,omitempty"`
	Weight        float64  `json:"weight,omitempty"`
}

// exportEvalDataset needs builder (case text is user data) and is audited with
// the count, never the text.
func (s *UIServer) exportEvalDataset(w http.ResponseWriter, r *http.Request, datasetID int64) {
	store, ok := s.evalPreflight(w, r, tenancy.RoleBuilder)
	if !ok {
		return
	}
	d, ok := s.loadDataset(w, r, store, datasetID)
	if !ok {
		return
	}
	// The first page is read before anything is written, so a failure is a clean error.
	ctx := pgScope(r.Context())
	page, err := store.ListEvalCases(ctx, datasetID, postgresql.EvalCaseQuery{Limit: postgresql.MaxEvalCasesPerPage + 1})
	if err != nil {
		evalFail(w, "exporting the dataset", err)
		return
	}
	s.evalAudit(w, r, postgresql.AuditExport, d.Namespace, d.Name, d.Version,
		fmt.Sprintf("exported %d %s as JSON lines; version %d", d.CaseCount, plural(d.CaseCount, "case", "cases"), d.Version))
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-v%d.jsonl"`, d.Name, d.Version))
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	for {
		more := len(page) > postgresql.MaxEvalCasesPerPage
		if more {
			page = page[:postgresql.MaxEvalCasesPerPage]
		}
		for _, c := range page {
			line := exportLine{Input: c.Input, Reference: c.Reference, ReferenceKind: c.ReferenceKind, Tags: c.Tags, Weight: c.Weight}
			if err := enc.Encode(line); err != nil {
				return
			}
		}
		if !more {
			return
		}
		page, err = store.ListEvalCases(ctx, datasetID, postgresql.EvalCaseQuery{After: page[len(page)-1].ID, Limit: postgresql.MaxEvalCasesPerPage + 1})
		if err != nil {
			slog.Error("exporting a dataset: reading the next page", "err", err)
			return
		}
	}
}
