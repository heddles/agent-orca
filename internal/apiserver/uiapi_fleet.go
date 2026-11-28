package apiserver

import (
	"context"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// Fleet overview: GET /api/fleet answers "how is every agent doing" in one
// call. Everything is listed once and grouped in memory, so the cost does not
// grow with the number of agents.

// Fleet windows. The window sets how far back runs and spend are counted.
const (
	fleetWindow24h = "24h"
	fleetWindow7d  = "7d"
)

// Health values.
const (
	healthHealthy  = "healthy"
	healthDegraded = "degraded"
	healthFailing  = "failing"
	healthUnknown  = "unknown"
)

// Condition types the health reads, and the approval status the fleet counts.
const (
	condReady          = "Ready"
	condAvailable      = "Available"
	fleetPendingStatus = "pending"
)

// fleetBudget is the spend in the window against the budgets that apply. A
// budget is null when none is set. Budgets are daily limits, scaled to the
// window (a 7d window allows seven days of budget).
type fleetBudget struct {
	USD             float64  `json:"usd"`
	AgentBudgetUSD  *float64 `json:"agentBudgetUSD"`
	TenantBudgetUSD *float64 `json:"tenantBudgetUSD"`
}

type fleetHealth struct {
	Status string `json:"status"`
	Reason string `json:"reason"`
}

// fleetRow is one agent in GET /api/fleet.
type fleetRow struct {
	Name       string      `json:"name"`
	Namespace  string      `json:"namespace"`
	Maintainer string      `json:"maintainer"`
	Published  bool        `json:"published"`
	Deployment string      `json:"deployment,omitempty"`
	Owner      string      `json:"owner"`
	Health     fleetHealth `json:"health"`
	// RunCount counts every run in the window. ErrorRate is failed runs over
	// finished runs, null when none finished. P95DurationSeconds is null when
	// no finished run has both a start and a completion time.
	RunCount           int         `json:"runCount"`
	ErrorRate          *float64    `json:"errorRate"`
	P95DurationSeconds *float64    `json:"p95DurationSeconds"`
	Spend              fleetBudget `json:"spend"`
	PendingApprovals   int         `json:"pendingApprovals"`
	// LastEvalScore is filled in once evaluations exist (Stage U7).
	LastEvalScore *float64 `json:"lastEvalScore"`
}

// fleetResponse is the body of GET /api/fleet.
type fleetResponse struct {
	Window string `json:"window"`
	Since  string `json:"since"`
	// TenantSpendUSD is the spend of every run in the window, including runs of
	// agents the list does not show.
	TenantSpendUSD  float64    `json:"tenantSpendUSD"`
	TenantBudgetUSD *float64   `json:"tenantBudgetUSD"`
	Agents          []fleetRow `json:"agents"`
}

func (s *UIServer) registerFleetRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/fleet", s.handleFleet)
}

// fleetOwner is the agent's owner as the fleet shows it: the same answer the
// ownership field on every other UI API object gives (ui, git or unknown),
// including Argo CD, Flux and Helm tracking metadata and not only the owner
// annotation.
func (s *UIServer) fleetOwner(a *agentorcav1alpha1.Agent) string {
	return tenancy.Detect(a, s.ownDetect).Owner
}

// fleetHealthOf derives an agent's health from its own and its deployment's
// status. An unpublished agent has no running part, so it can only be judged
// by its conditions.
//
//	failing   the deployment phase is Failed, or a Ready or Available
//	          condition is False
//	degraded  the deployment is paused or has consecutive pod failures, or any
//	          other condition is False
//	healthy   the deployment is Running, or a Ready condition is True and
//	          nothing is wrong
//	unknown   no deployment and no conditions, or the deployment has not
//	          reported a state yet
func fleetHealthOf(a *agentorcav1alpha1.Agent, d *agentorcav1alpha1.AgentDeployment) fleetHealth {
	conds := slices.Clone(a.Status.Conditions)
	if d != nil {
		conds = append(conds, d.Status.Conditions...)
	}
	var ready, degradedBy string
	for _, c := range conds {
		switch {
		case c.Status == metav1.ConditionFalse && (c.Type == condReady || c.Type == condAvailable):
			return fleetHealth{healthFailing, c.Type + " is false: " + conditionWhy(c)}
		case c.Status == metav1.ConditionFalse && degradedBy == "":
			degradedBy = c.Type + " is false: " + conditionWhy(c)
		case c.Status == metav1.ConditionTrue && c.Type == "Ready":
			ready = condReady
		}
	}
	if d != nil {
		switch {
		case d.Status.Phase == agentorcav1alpha1.AgentDeploymentPhaseFailed:
			return fleetHealth{healthFailing, "the deployment failed" + messageSuffix(d.Status.Message)}
		case d.Status.Phase == agentorcav1alpha1.AgentDeploymentPhasePaused:
			return fleetHealth{healthDegraded, "the deployment is paused"}
		case d.Status.ConsecutiveFailures > 0:
			return fleetHealth{healthDegraded, strconv.Itoa(d.Status.ConsecutiveFailures) + " consecutive pod failures"}
		}
	}
	if degradedBy != "" {
		return fleetHealth{healthDegraded, degradedBy}
	}
	if d != nil && d.Status.Phase == agentorcav1alpha1.AgentDeploymentPhaseRunning {
		return fleetHealth{healthHealthy, "the deployment is running"}
	}
	if ready != "" {
		return fleetHealth{healthHealthy, "ready"}
	}
	if d != nil {
		return fleetHealth{healthUnknown, "the deployment has not reported a state yet"}
	}
	return fleetHealth{healthUnknown, "no deployment and no status reported"}
}

func conditionWhy(c metav1.Condition) string {
	if c.Message != "" {
		return c.Message
	}
	return c.Reason
}

func messageSuffix(m string) string {
	if m == "" {
		return ""
	}
	return ": " + m
}

// fleetRun is one run in the window, from the cluster or the archive.
type fleetRun struct {
	key      string // namespace/name
	agent    string // namespace/agent name
	phase    string
	spend    float64
	when     time.Time
	duration *time.Duration
}

func fleetRunFromLive(run *agentorcav1alpha1.AgentRun) fleetRun {
	e := entryFromRun(run)
	r := fleetRun{key: e.key, agent: run.Namespace + "/" + run.Spec.AgentRef, phase: e.phase, spend: e.spend, when: e.when}
	if st, ct := run.Status.StartTime, run.Status.CompletionTime; st != nil && ct != nil {
		d := ct.Sub(st.Time)
		r.duration = &d
	}
	return r
}

func fleetRunFromArchive(row postgresql.CostRow) fleetRun {
	e := entryFromArchive(row)
	r := fleetRun{key: e.key, agent: row.Namespace + "/" + row.AgentRef, phase: e.phase, spend: e.spend, when: e.when}
	if row.Started != nil && row.Completed != nil {
		d := row.Completed.Sub(*row.Started)
		r.duration = &d
	}
	return r
}

// fleetStats is what the runs of one agent add up to.
type fleetStats struct {
	runs, finished, failed int
	spend                  float64
	durations              []float64
}

// aggregateFleetRuns groups runs in the window by agent. A run counts once even
// when it is both live and archived: live wins, as in the cost dashboard.
func aggregateFleetRuns(live, archived []fleetRun, since time.Time) (map[string]*fleetStats, float64) {
	seen := make(map[string]bool, len(live))
	byAgent := map[string]*fleetStats{}
	var total float64
	add := func(r fleetRun) {
		if r.when.Before(since) {
			return
		}
		st := byAgent[r.agent]
		if st == nil {
			st = &fleetStats{}
			byAgent[r.agent] = st
		}
		st.runs++
		st.spend += r.spend
		total += r.spend
		if postgresql.IsTerminalPhase(agentorcav1alpha1.AgentRunPhase(r.phase)) {
			st.finished++
			if r.phase == string(agentorcav1alpha1.AgentRunPhaseFailed) {
				st.failed++
			}
			if r.duration != nil && *r.duration >= 0 {
				st.durations = append(st.durations, r.duration.Seconds())
			}
		}
	}
	for _, r := range live {
		seen[r.key] = true
		add(r)
	}
	for _, r := range archived {
		if !seen[r.key] {
			add(r)
		}
	}
	return byAgent, total
}

// percentile95 is the nearest-rank 95th percentile of vals, which it sorts.
func percentile95(vals []float64) *float64 {
	if len(vals) == 0 {
		return nil
	}
	sort.Float64s(vals)
	rank := max(int(math.Ceil(0.95*float64(len(vals)))), 1)
	v := vals[rank-1]
	return &v
}

func parseBudget(s string) *float64 {
	if s == "" {
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v <= 0 {
		return nil
	}
	return &v
}

func scaleBudget(v *float64, days float64) *float64 {
	if v == nil {
		return nil
	}
	out := *v * days
	return &out
}

func (s *UIServer) handleFleet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	window, days := fleetWindow24h, 1.0
	switch q := r.URL.Query().Get("window"); q {
	case "", fleetWindow24h:
	case fleetWindow7d:
		window, days = q, 7
	default:
		http.Error(w, `{"error":"window must be 24h or 7d"}`, http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	ns := r.URL.Query().Get("namespace")
	since := time.Now().Add(-time.Duration(days*24) * time.Hour)

	var agents agentorcav1alpha1.AgentList
	var deployments agentorcav1alpha1.AgentDeploymentList
	var runs agentorcav1alpha1.AgentRunList
	var selectors agentorcav1alpha1.ModelSelectorList
	if err := listCRDsMultiNamespace(ctx, s.crdClient, &agents, ns); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := listCRDsMultiNamespace(ctx, s.crdClient, &deployments, ns); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := listCRDsMultiNamespace(ctx, s.crdClient, &runs, ns); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := listCRDsMultiNamespace(ctx, s.crdClient, &selectors, ns); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var tools agentorcav1alpha1.ToolList
	if err := listCRDsMultiNamespace(ctx, s.crdClient, &tools, ns); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	have, err := s.listAgentRefs(ctx, ns, tools.Items)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	live := make([]fleetRun, 0, len(runs.Items))
	agentOfRun := make(map[string]string, len(runs.Items))
	for i := range runs.Items {
		fr := fleetRunFromLive(&runs.Items[i])
		agentOfRun[fr.key] = fr.agent
		live = append(live, fr)
	}
	archived := s.fleetArchivedRuns(ctx, ns, since)
	byAgent, tenantSpend := aggregateFleetRuns(live, archived, since)
	pending := s.fleetPendingApprovals(ctx, ns, agentOfRun)

	depByAgent := map[string]*agentorcav1alpha1.AgentDeployment{}
	for i := range deployments.Items {
		d := &deployments.Items[i]
		key := d.Namespace + "/" + d.Spec.AgentRef
		if _, dup := depByAgent[key]; !dup {
			depByAgent[key] = d
		}
	}
	perDay := map[string]*float64{}
	for i := range selectors.Items {
		sel := &selectors.Items[i]
		if sel.Spec.BudgetCap != nil {
			perDay[sel.Namespace+"/"+sel.Name] = parseBudget(sel.Spec.BudgetCap.PerDay)
		}
	}

	var tenantBudget *float64
	if t, ok := TenantFromContext(ctx); ok && t != nil {
		tenantBudget = scaleBudget(parseBudget(t.BudgetPerDayUSD), days)
	}

	rows := make([]fleetRow, 0, len(agents.Items))
	for i := range agents.Items {
		a := &agents.Items[i]
		key := a.Namespace + "/" + a.Name
		dep := depByAgent[key]
		reason, _ := s.unpublishedReason(a, dep != nil, have.lookup(a.Namespace))
		row := fleetRow{
			Name: a.Name, Namespace: a.Namespace, Maintainer: agentMaintainer(a),
			Published: reason == "", Owner: s.fleetOwner(a), Health: fleetHealthOf(a, dep),
			PendingApprovals: pending[key],
			Spend: fleetBudget{
				AgentBudgetUSD:  scaleBudget(perDay[a.Namespace+"/"+a.Spec.ModelSelectorRef], days),
				TenantBudgetUSD: tenantBudget,
			},
		}
		if dep != nil {
			row.Deployment = dep.Name
		}
		if st := byAgent[key]; st != nil {
			row.RunCount = st.runs
			row.Spend.USD = st.spend
			if st.finished > 0 {
				rate := float64(st.failed) / float64(st.finished)
				row.ErrorRate = &rate
			}
			row.P95DurationSeconds = percentile95(st.durations)
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Namespace != rows[j].Namespace {
			return rows[i].Namespace < rows[j].Namespace
		}
		return rows[i].Name < rows[j].Name
	})
	jsonResponse(w, fleetResponse{
		Window: window, Since: since.UTC().Format(time.RFC3339),
		TenantSpendUSD: tenantSpend, TenantBudgetUSD: tenantBudget, Agents: rows,
	})
}

// fleetNamespaces narrows the namespaces a store query may read to the
// caller's tenant and the optional namespace filter. ok is false when the
// query must not run at all (a tenant with no namespaces, or a filter outside
// them). namespaces is empty for an unrestricted caller without a filter.
func fleetNamespaces(ctx context.Context, ns string) (namespaces []string, ok bool) {
	tenant, has := TenantFromContext(ctx)
	if !has || tenant == nil {
		if ns != "" {
			return []string{ns}, true
		}
		return nil, true
	}
	namespaces = tenantNamespaces(tenant)
	if len(namespaces) == 0 {
		return nil, false
	}
	if ns != "" {
		if !slices.Contains(namespaces, ns) {
			return nil, false
		}
		return []string{ns}, true
	}
	return namespaces, true
}

// fleetArchivedRuns reads the archived runs in the window through the scoped
// store. A failing archive is logged and the fleet shows live runs only.
func (s *UIServer) fleetArchivedRuns(ctx context.Context, ns string, since time.Time) []fleetRun {
	arch := s.costArchiveSource()
	if arch == nil {
		return nil
	}
	namespaces, ok := fleetNamespaces(ctx, ns)
	if !ok {
		return nil
	}
	rows, err := arch.QueryCostRows(pgScope(ctx), postgresql.CostQuery{Since: &since, Namespaces: namespaces})
	if err != nil {
		slog.Error("querying archived runs for the fleet", "err", err)
		return nil
	}
	out := make([]fleetRun, 0, len(rows))
	for _, row := range rows {
		out = append(out, fleetRunFromArchive(row))
	}
	return out
}

// fleetPendingApprovals counts pending approvals per agent. An approval names
// its run, so the run's agent comes from the runs already listed.
func (s *UIServer) fleetPendingApprovals(ctx context.Context, ns string, agentOfRun map[string]string) map[string]int {
	out := map[string]int{}
	store := s.approvalStore()
	if store == nil {
		return out
	}
	namespaces, ok := fleetNamespaces(ctx, ns)
	if !ok {
		return out
	}
	list, err := store.ListApprovals(pgScope(ctx), postgresql.ApprovalQuery{Status: fleetPendingStatus, Namespaces: namespaces})
	if err != nil {
		slog.Error("listing approvals for the fleet", "err", err)
		return out
	}
	for _, ap := range list {
		if agent, ok := agentOfRun[ap.Namespace+"/"+ap.RunName]; ok {
			out[agent]++
		}
	}
	return out
}
