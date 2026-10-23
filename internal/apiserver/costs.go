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
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/postgresql"
)

// costArchive is the part of the PostgreSQL archive the cost endpoint reads.
type costArchive interface {
	QueryCostRows(ctx context.Context, q postgresql.CostQuery) ([]postgresql.CostRow, error)
}

// untrackedSampleSize caps the run names returned in untrackedSample.
const untrackedSampleSize = 10

// unknownModel buckets spend from runs with no routing decision.
const unknownModel = "unknown"

// costEntry is one run, from the live cluster or the archive, in the shape the
// aggregation needs.
type costEntry struct {
	key      string // namespace/name, used to de-duplicate live vs archived
	agent    string
	phase    string
	model    string
	spend    float64
	terminal bool
	// incomplete marks a terminal run whose spend could not be captured
	// (CostCapture=Incomplete). Its spend is only a lower bound.
	incomplete bool
	when       time.Time
}

// costDay is one day's spend, split by run outcome (the run phase).
type costDay struct {
	Date string `json:"date"`
	// USD matches the {date, usd} shape the UI's CostData.byDay already declares.
	USD       string            `json:"usd"`
	ByOutcome map[string]string `json:"byOutcome"`
}

// costResponse is the /api/costs payload.
type costResponse struct {
	TotalUSD string            `json:"totalUSD"`
	ByAgent  map[string]string `json:"byAgent"`
	// ByModel attributes each run's whole spend to the model of its last routing
	// decision, so a run that fell back mid-way is billed to the model it ended
	// on. Per-call attribution needs the router's own counters.
	ByModel map[string]string `json:"byModel"`
	ByDay   []costDay         `json:"byDay"`
	// ByOutcome is spend by run phase (Succeeded, Failed, Running, ...).
	ByOutcome map[string]string `json:"byOutcome"`
	// UntrackedCount is the number of finished runs whose spend could not be
	// captured. They are not counted as $0: see UntrackedSample.
	UntrackedCount  int      `json:"untrackedCount"`
	UntrackedSample []string `json:"untrackedSample"`
	// InFlightUSD is spend recorded on runs that have not finished.
	InFlightUSD string `json:"inFlightUSD"`
	// AvgRunUSD is the mean spend of finished runs whose spend was captured.
	AvgRunUSD string `json:"avgRunUSD"`
	// FailedRunUSD is spend on runs that ended Failed.
	FailedRunUSD string `json:"failedRunUSD"`
	// InFlightUnscraped counts unfinished runs whose model-router could not be
	// read, so their live spend is missing from InFlightUSD and the total.
	InFlightUnscraped int `json:"inFlightUnscraped,omitempty"`
	// ArchiveUnavailable is set when the run archive is configured but could not
	// be read, so the totals cover live runs only.
	ArchiveUnavailable bool `json:"archiveUnavailable,omitempty"`
}

func usd(v float64) string { return fmt.Sprintf("%.4f", v) }

// aggregateCosts folds entries into the response. entries are expected newest
// first so untrackedSample lists the most recent offenders.
func aggregateCosts(entries []costEntry) costResponse {
	var (
		total, inFlight, trackedSpend float64
		trackedRuns                   int
		byAgent                       = map[string]float64{}
		byModel                       = map[string]float64{}
		byOutcome                     = map[string]float64{}
		dayTotal                      = map[string]float64{}
		dayOutcome                    = map[string]map[string]float64{}
	)
	out := costResponse{UntrackedSample: []string{}}
	for _, e := range entries {
		total += e.spend
		byAgent[e.agent] += e.spend

		model := e.model
		if model == "" {
			model = unknownModel
		}
		byModel[model] += e.spend

		outcome := e.phase
		if outcome == "" {
			outcome = string(agentorcav1alpha1.AgentRunPhasePending)
		}
		byOutcome[outcome] += e.spend

		day := e.when.UTC().Format("2006-01-02")
		dayTotal[day] += e.spend
		if dayOutcome[day] == nil {
			dayOutcome[day] = map[string]float64{}
		}
		dayOutcome[day][outcome] += e.spend

		switch {
		case !e.terminal:
			inFlight += e.spend
		case e.incomplete:
			out.UntrackedCount++
			if len(out.UntrackedSample) < untrackedSampleSize {
				out.UntrackedSample = append(out.UntrackedSample, e.key)
			}
		default:
			trackedSpend += e.spend
			trackedRuns++
		}
	}

	out.TotalUSD = usd(total)
	out.InFlightUSD = usd(inFlight)
	out.FailedRunUSD = usd(byOutcome[string(agentorcav1alpha1.AgentRunPhaseFailed)])
	avg := 0.0
	if trackedRuns > 0 {
		avg = trackedSpend / float64(trackedRuns)
	}
	out.AvgRunUSD = usd(avg)

	out.ByAgent = stringifyCosts(byAgent)
	out.ByModel = stringifyCosts(byModel)
	out.ByOutcome = stringifyCosts(byOutcome)

	out.ByDay = make([]costDay, 0, len(dayTotal))
	for day, v := range dayTotal {
		out.ByDay = append(out.ByDay, costDay{Date: day, USD: usd(v), ByOutcome: stringifyCosts(dayOutcome[day])})
	}
	sort.Slice(out.ByDay, func(i, j int) bool { return out.ByDay[i].Date < out.ByDay[j].Date })
	return out
}

func stringifyCosts(m map[string]float64) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = usd(v)
	}
	return out
}

// entryFromRun converts a live AgentRun.
func entryFromRun(run *agentorcav1alpha1.AgentRun) costEntry {
	e := costEntry{
		key:      run.Namespace + "/" + run.Name,
		agent:    run.Spec.AgentRef,
		phase:    string(run.Status.Phase),
		spend:    parseFloatCost(run.Status.SpendUSD),
		terminal: postgresql.IsTerminalPhase(run.Status.Phase),
		when:     run.CreationTimestamp.Time,
	}
	if n := len(run.Status.RoutingDecisions); n > 0 {
		e.model = run.Status.RoutingDecisions[n-1].Model
	}
	switch {
	case run.Status.CompletionTime != nil:
		e.when = run.Status.CompletionTime.Time
	case run.Status.StartTime != nil:
		e.when = run.Status.StartTime.Time
	}
	// Mirrors the controller's CostCapture condition (False = spend unreadable).
	for _, c := range run.Status.Conditions {
		if c.Type == "CostCapture" {
			e.incomplete = c.Status == "False"
		}
	}
	return e
}

// entryFromArchive converts an archived run row.
func entryFromArchive(r postgresql.CostRow) costEntry {
	return costEntry{
		key:        r.Namespace + "/" + r.Name,
		agent:      r.AgentRef,
		phase:      r.Phase,
		model:      r.Model,
		spend:      parseFloatCost(r.SpendUSD),
		terminal:   postgresql.IsTerminalPhase(agentorcav1alpha1.AgentRunPhase(r.Phase)),
		incomplete: r.CostIncomplete,
		when:       r.When,
	}
}

// mergeCostEntries combines live and archived runs. A run still present in the
// cluster is also in the archive once it finished; the live copy wins so a
// run is never counted twice. Entries are returned newest first.
func mergeCostEntries(live, archived []costEntry) []costEntry {
	seen := make(map[string]bool, len(live))
	out := make([]costEntry, 0, len(live)+len(archived))
	for _, e := range live {
		seen[e.key] = true
		out = append(out, e)
	}
	for _, e := range archived {
		if !seen[e.key] {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].when.After(out[j].when) })
	return out
}

func (s *UIServer) costArchiveSource() costArchive {
	if s.costArchive != nil {
		return s.costArchive
	}
	if s.pgStore != nil {
		return s.pgStore
	}
	return nil
}

func (s *UIServer) handleCosts(w http.ResponseWriter, r *http.Request) {
	ns := r.URL.Query().Get("namespace")
	runFilter := r.URL.Query().Get("run")
	deployment := r.URL.Query().Get("deployment")

	var since *time.Time
	if d, err := strconv.Atoi(r.URL.Query().Get("days")); err == nil && d > 0 {
		t := time.Now().AddDate(0, 0, -d)
		since = &t
	}

	var extra []client.ListOption
	if deployment != "" {
		extra = append(extra, client.MatchingLabels{"agentorca.io/deployment": deployment})
	}
	var list agentorcav1alpha1.AgentRunList
	if err := listCRDsMultiNamespace(r.Context(), s.crdClient, &list, ns, extra...); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	live := make([]costEntry, 0, len(list.Items))
	var unfinished []*agentorcav1alpha1.AgentRun
	for i := range list.Items {
		run := &list.Items[i]
		if runFilter != "" && run.Name != runFilter {
			continue
		}
		e := entryFromRun(run)
		if since != nil && e.when.Before(*since) {
			continue
		}
		live = append(live, e)
		if isUnfinished(run) {
			unfinished = append(unfinished, run)
		}
	}
	// Unfinished runs have no stamped spend yet: read it from their routers.
	unscraped := s.applyInFlightSpend(r.Context(), live, unfinished)

	var archived []costEntry
	archiveFailed := false
	if arch := s.costArchiveSource(); arch != nil {
		q := postgresql.CostQuery{Deployment: deployment, Run: runFilter, Since: since}
		skip := false
		if tenant, ok := TenantFromContext(r.Context()); ok && tenant != nil {
			// An empty namespace filter means "all", so a tenant with no
			// authorized namespaces must not reach the query at all.
			q.Namespaces = tenantNamespaces(tenant)
			skip = len(q.Namespaces) == 0
		} else if ns != "" {
			q.Namespaces = []string{ns}
		}
		if !skip {
			rows, err := arch.QueryCostRows(r.Context(), q)
			if err != nil {
				// Live data is still useful; say the archive part is missing
				// instead of failing the whole dashboard.
				slog.Error("querying archived run costs", "err", err)
				archiveFailed = true
			}
			for _, row := range rows {
				archived = append(archived, entryFromArchive(row))
			}
		}
	}

	resp := aggregateCosts(mergeCostEntries(live, archived))
	resp.ArchiveUnavailable = archiveFailed
	resp.InFlightUnscraped = unscraped
	jsonResponse(w, resp)
}
