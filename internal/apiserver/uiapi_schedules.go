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
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/schedule"
	"github.com/heddles/agent-orca/internal/security"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// Scheduled runs in the UI API (docs/schedules.md). Plain create, update and
// delete go through /api/resources/agentschedules, which gives ownership,
// server-side apply, the resourceVersion check and audit for free. What is not
// plain CRUD lives here:
//
//	GET  /api/schedules                         summary list
//	GET  /api/schedules/preview?cron=&tz=&count= the next firings, as the builder types
//	GET  /api/schedules/{ns}/{name}/runs        history, newest first, paged
//	POST /api/schedules/{ns}/{name}/suspend     runner; UI-owned schedules only
//	POST /api/schedules/{ns}/{name}/resume      runner; also clears a Degraded breaker
//	POST /api/schedules/{ns}/{name}/run         runner; "Run now", same quotas
//	GET  /api/notification-channels             builder; channel names and types, read-only

// Kind names and route words used more than once.
const (
	kindAgentWorkflow = "AgentWorkflow"
	kindAgentRun      = "AgentRun"
	kindAgentSchedule = "AgentSchedule"

	triggerManual   = "manual"
	triggerSchedule = "schedule"

	stateSuspended  = "suspended"
	outcomeFailed   = "failed"
	schedActionRuns = "runs"
)

func (s *UIServer) registerScheduleRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/schedules", s.handleListSchedules)
	mux.HandleFunc("/api/schedules/", s.handleScheduleAction)
	mux.HandleFunc("/api/notification-channels", s.handleNotificationChannels)
}

// SetScheduleMinInterval sets the minimum interval the preview reports against.
func (s *UIServer) SetScheduleMinInterval(d time.Duration) { s.scheduleMinInterval = d }

func (s *UIServer) minInterval() time.Duration {
	if s.scheduleMinInterval > 0 {
		return s.scheduleMinInterval
	}
	return schedule.DefaultMinInterval
}

// scheduleResult is the latest outcome of a schedule.
type scheduleResult struct {
	// Status is succeeded, failed, skipped or none.
	Status string `json:"status"`
	At     string `json:"at,omitempty"`
	Reason string `json:"reason,omitempty"`
	Run    string `json:"run,omitempty"`
}

type scheduleSkipView struct {
	Time    string `json:"time"`
	Reason  string `json:"reason"`
	Message string `json:"message,omitempty"`
}

// scheduleSummary is one schedule in GET /api/schedules.
type scheduleSummary struct {
	Name              string `json:"name"`
	Namespace         string `json:"namespace"`
	Target            string `json:"target"`
	TargetKind        string `json:"targetKind"` // agent or workflowTemplate
	Schedule          string `json:"schedule"`
	TimeZone          string `json:"timeZone"`
	ConcurrencyPolicy string `json:"concurrencyPolicy"`
	Suspended         bool   `json:"suspended"`
	// State is active, suspended, degraded, budgetExhausted, targetMissing or invalid.
	State       string `json:"state"`
	StateReason string `json:"stateReason,omitempty"`
	// NextRun is absolute (UTC); NextRunLocal is in the schedule's own zone.
	NextRun             string             `json:"nextRun,omitempty"`
	NextRunLocal        string             `json:"nextRunLocal,omitempty"`
	LastRun             string             `json:"lastRun,omitempty"`
	LastResult          scheduleResult     `json:"lastResult"`
	SpentTodayUSD       string             `json:"spentTodayUSD,omitempty"`
	SpentMonthUSD       string             `json:"spentMonthUSD,omitempty"`
	ConsecutiveFailures int32              `json:"consecutiveFailures"`
	Active              []string           `json:"active"`
	RecentSkips         []scheduleSkipView `json:"recentSkips"`
	Ownership           ownershipView      `json:"ownership"`
}

func utcTime(t *metav1.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// scheduleState is the one word and plain sentence a user sees.
func scheduleState(sc *agentorcav1alpha1.AgentSchedule) (state, reason string) {
	cond := func(t string) *metav1.Condition { return meta.FindStatusCondition(sc.Status.Conditions, t) }
	on := func(t string) bool { return meta.IsStatusConditionTrue(sc.Status.Conditions, t) }
	switch {
	case cond(agentorcav1alpha1.ScheduleConditionReady) != nil && !on(agentorcav1alpha1.ScheduleConditionReady):
		return "invalid", cond(agentorcav1alpha1.ScheduleConditionReady).Message
	case sc.Spec.Suspend:
		return stateSuspended, "Paused: no new runs start until it is resumed."
	case on(agentorcav1alpha1.ScheduleConditionDegraded):
		return "degraded", cond(agentorcav1alpha1.ScheduleConditionDegraded).Message
	case on(agentorcav1alpha1.ScheduleConditionBudgetExhausted):
		return "budgetExhausted", cond(agentorcav1alpha1.ScheduleConditionBudgetExhausted).Message
	case on(agentorcav1alpha1.ScheduleConditionTargetMissing):
		return "targetMissing", cond(agentorcav1alpha1.ScheduleConditionTargetMissing).Message
	}
	return "active", ""
}

func scheduleTarget(sc *agentorcav1alpha1.AgentSchedule) (name, kind string) {
	if sc.Spec.WorkflowTemplateRef != "" {
		return sc.Spec.WorkflowTemplateRef, "workflowTemplate"
	}
	return sc.Spec.AgentRef, "agent"
}

// childOutcome is the state of a run or workflow a schedule made.
func childOutcome(obj client.Object) (status string, at time.Time) {
	switch o := obj.(type) {
	case *agentorcav1alpha1.AgentRun:
		switch o.Status.Phase {
		case agentorcav1alpha1.AgentRunPhaseSucceeded, agentorcav1alpha1.AgentRunPhaseHandedOff:
			status = "succeeded"
		case agentorcav1alpha1.AgentRunPhaseFailed:
			status = outcomeFailed
		default:
			return "", time.Time{}
		}
		if o.Status.CompletionTime != nil {
			return status, o.Status.CompletionTime.Time
		}
		return status, o.CreationTimestamp.Time
	case *agentorcav1alpha1.AgentWorkflow:
		switch o.Status.Phase {
		case agentorcav1alpha1.AgentWorkflowPhaseSucceeded:
			status = "succeeded"
		case agentorcav1alpha1.AgentWorkflowPhaseFailed, agentorcav1alpha1.AgentWorkflowPhaseCancelled:
			status = outcomeFailed
		default:
			return "", time.Time{}
		}
		if o.Status.CompletionTime != nil {
			return status, o.Status.CompletionTime.Time
		}
		return status, o.CreationTimestamp.Time
	}
	return "", time.Time{}
}

// summarize builds the summary of sc; finished are the runs and workflows
// labelled for it.
func (s *UIServer) summarize(r *http.Request, sc *agentorcav1alpha1.AgentSchedule, finished []client.Object) scheduleSummary {
	target, tk := scheduleTarget(sc)
	state, reason := scheduleState(sc)
	zone := sc.Spec.TimeZone
	if zone == "" {
		zone = "UTC"
	}
	out := scheduleSummary{
		Name: sc.Name, Namespace: sc.Namespace, Target: target, TargetKind: tk,
		Schedule: sc.Spec.Schedule, TimeZone: zone, ConcurrencyPolicy: sc.Spec.ConcurrencyPolicy,
		Suspended: sc.Spec.Suspend, State: state, StateReason: reason,
		NextRun: utcTime(sc.Status.NextScheduleTime), LastRun: utcTime(sc.Status.LastScheduleTime),
		SpentTodayUSD: sc.Status.SpentTodayUSD, SpentMonthUSD: sc.Status.SpentMonthUSD,
		ConsecutiveFailures: sc.Status.ConsecutiveFailures,
		Active:              []string{}, RecentSkips: []scheduleSkipView{}, LastResult: scheduleResult{Status: "none"},
		Ownership: s.ownershipOf(r.Context(), sc, kindAgentSchedule),
	}
	if sc.Status.NextScheduleTime != nil {
		if loc, err := time.LoadLocation(zone); err == nil {
			out.NextRunLocal = sc.Status.NextScheduleTime.In(loc).Format(time.RFC3339)
		}
	}
	for _, a := range sc.Status.Active {
		out.Active = append(out.Active, a.Name)
	}
	for _, k := range sc.Status.RecentSkips {
		out.RecentSkips = append(out.RecentSkips, scheduleSkipView{Time: utcTime(&k.Time), Reason: k.Reason, Message: k.Message})
	}
	// The latest outcome: the newest finished run, or a later skip.
	var at time.Time
	for _, c := range finished {
		if st, t := childOutcome(c); st != "" && t.After(at) {
			at = t
			out.LastResult = scheduleResult{Status: st, At: t.UTC().Format(time.RFC3339), Run: c.GetName()}
		}
	}
	for _, k := range sc.Status.RecentSkips {
		if k.Time.After(at) {
			at = k.Time.Time
			out.LastResult = scheduleResult{Status: "skipped", At: utcTime(&k.Time), Reason: k.Reason + ": " + k.Message}
		}
	}
	return out
}

// scheduleChildren lists the runs and workflows labelled for a schedule
// namespace, grouped by schedule name.
func (s *UIServer) scheduleChildren(r *http.Request, ns string) (map[string][]client.Object, error) {
	out := map[string][]client.Object{}
	var runs agentorcav1alpha1.AgentRunList
	if err := listCRDsMultiNamespace(r.Context(), s.crdClient, &runs, ns, client.HasLabels{schedule.LabelSchedule}); err != nil {
		return nil, err
	}
	for i := range runs.Items {
		out[runs.Items[i].Namespace+"/"+runs.Items[i].Labels[schedule.LabelSchedule]] = append(out[runs.Items[i].Namespace+"/"+runs.Items[i].Labels[schedule.LabelSchedule]], &runs.Items[i])
	}
	var wfs agentorcav1alpha1.AgentWorkflowList
	if err := listCRDsMultiNamespace(r.Context(), s.crdClient, &wfs, ns, client.HasLabels{schedule.LabelSchedule}); err != nil {
		return nil, err
	}
	for i := range wfs.Items {
		k := wfs.Items[i].Namespace + "/" + wfs.Items[i].Labels[schedule.LabelSchedule]
		out[k] = append(out[k], &wfs.Items[i])
	}
	return out, nil
}

// handleListSchedules answers GET /api/schedules.
func (s *UIServer) handleListSchedules(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var list agentorcav1alpha1.AgentScheduleList
	ns := r.URL.Query().Get("namespace")
	if err := listCRDsMultiNamespace(r.Context(), s.crdClient, &list, ns); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	children, err := s.scheduleChildren(r, ns)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]scheduleSummary, 0, len(list.Items))
	for i := range list.Items {
		sc := &list.Items[i]
		out = append(out, s.summarize(r, sc, children[sc.Namespace+"/"+sc.Name]))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	jsonResponse(w, out)
}

// handleScheduleAction routes /api/schedules/preview and /api/schedules/{ns}/{name}/{action}.
func (s *UIServer) handleScheduleAction(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/schedules/"), "/"), "/")
	if len(parts) == 1 && parts[0] == "preview" {
		s.handleSchedulePreview(w, r)
		return
	}
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" {
		http.NotFound(w, r)
		return
	}
	ns, name, action := parts[0], parts[1], parts[2]
	want := http.MethodPost
	if action == schedActionRuns {
		want = http.MethodGet
	}
	if action != schedActionRuns && action != "suspend" && action != "resume" && action != actionRun {
		http.NotFound(w, r)
		return
	}
	if r.Method != want {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var sc agentorcav1alpha1.AgentSchedule
	if err := s.crdClient.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &sc); err != nil {
		notFoundOr(w, err)
		return
	}
	if !canRead(r.Context(), &sc, kindAgentSchedule) || !namespaceAllowed(r.Context(), ns) {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	switch action {
	case schedActionRuns:
		s.handleScheduleRuns(w, r, &sc)
	case "suspend":
		s.setSuspend(w, r, &sc, true)
	case "resume":
		s.setSuspend(w, r, &sc, false)
	case actionRun:
		s.runScheduleNow(w, r, &sc)
	}
}

// handleSchedulePreview answers GET /api/schedules/preview?cron=&tz=&count=.
func (s *UIServer) handleSchedulePreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	cs, err := schedule.Parse(q.Get("cron"), q.Get("tz"))
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	count := 5
	if v, err := strconv.Atoi(q.Get("count")); err == nil && v > 0 {
		count = min(v, 20)
	}
	type firing struct {
		UTC   string `json:"utc"`
		Local string `json:"local"`
	}
	now := time.Now()
	runs := []firing{}
	for _, t := range cs.NextN(now, count) {
		runs = append(runs, firing{UTC: t.UTC().Format(time.RFC3339), Local: t.In(cs.Location()).Format(time.RFC3339)})
	}
	gap := cs.MinInterval(now)
	jsonResponse(w, map[string]any{
		"timeZone":        cs.Location().String(),
		"nextRuns":        runs,
		"minInterval":     s.minInterval().String(),
		"shortestGap":     gap.String(),
		"tooFrequent":     gap != 0 && gap < s.minInterval(),
		"shortestGapSecs": int64(gap.Seconds()),
	})
}

// scheduleRun is one run or workflow in the history.
type scheduleRun struct {
	Name         string `json:"name"`
	Namespace    string `json:"namespace"`
	Kind         string `json:"kind"`
	Trigger      string `json:"trigger"` // schedule or manual
	ScheduledFor string `json:"scheduledFor,omitempty"`
	CreatedBy    string `json:"createdBy,omitempty"`
	Phase        string `json:"phase"`
	SpendUSD     string `json:"spendUSD,omitempty"`
	CreatedAt    string `json:"createdAt"`
	Completed    string `json:"completionTime,omitempty"`
}

const scheduleRunsMaxLimit = 100

// handleScheduleRuns answers GET /api/schedules/{ns}/{name}/runs?limit=&before=.
func (s *UIServer) handleScheduleRuns(w http.ResponseWriter, r *http.Request, sc *agentorcav1alpha1.AgentSchedule) {
	kids, err := s.scheduleChildren(r, sc.Namespace)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	limit := 20
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 {
		limit = min(v, scheduleRunsMaxLimit)
	}
	var before time.Time
	if v := r.URL.Query().Get("before"); v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			jsonError(w, "before must be an RFC 3339 time", http.StatusBadRequest)
			return
		}
		before = t
	}
	objs := kids[sc.Namespace+"/"+sc.Name]
	sort.Slice(objs, func(i, j int) bool {
		a, b := objs[i].GetCreationTimestamp().Time, objs[j].GetCreationTimestamp().Time
		if !a.Equal(b) {
			return a.After(b)
		}
		return objs[i].GetName() > objs[j].GetName()
	})
	out := []scheduleRun{}
	next := ""
	for _, o := range objs {
		created := o.GetCreationTimestamp().Time
		if !before.IsZero() && !created.Before(before) {
			continue
		}
		if len(out) == limit {
			next = out[len(out)-1].CreatedAt
			break
		}
		sr := scheduleRun{
			Name: o.GetName(), Namespace: o.GetNamespace(), Trigger: triggerSchedule,
			ScheduledFor: o.GetAnnotations()[schedule.AnnotationScheduledFor], CreatedBy: o.GetAnnotations()[schedule.AnnotationCreatedBy],
			CreatedAt: created.UTC().Format(time.RFC3339Nano),
		}
		if !strings.HasPrefix(sr.CreatedBy, schedule.ActorPrefix) {
			sr.Trigger = triggerManual
		}
		switch v := o.(type) {
		case *agentorcav1alpha1.AgentRun:
			sr.Kind, sr.Phase, sr.SpendUSD = kindAgentRun, string(v.Status.Phase), v.Status.SpendUSD
			sr.Completed = utcTime(v.Status.CompletionTime)
		case *agentorcav1alpha1.AgentWorkflow:
			sr.Kind, sr.Phase, sr.SpendUSD = kindAgentWorkflow, string(v.Status.Phase), v.Status.TotalSpendUSD
			sr.Completed = utcTime(v.Status.CompletionTime)
		}
		out = append(out, sr)
	}
	jsonResponse(w, map[string]any{"runs": out, "nextBefore": next})
}

// setSuspend handles suspend and resume. Both are a UI edit of spec.suspend:
// the same checks as a PUT (UI-owned only, tenant, ownership webhook), so a
// Git-owned schedule is suspended in Git, and the answer says so. Resuming a
// Degraded schedule also stamps the resume annotation the controller watches.
func (s *UIServer) setSuspend(w http.ResponseWriter, r *http.Request, sc *agentorcav1alpha1.AgentSchedule, suspend bool) {
	if !requireRole(w, r, tenancy.RoleRunner) {
		return
	}
	if !s.authorizeWrite(w, r, sc, kindAgentSchedule) {
		return
	}
	if o := tenancy.Detect(sc, s.ownDetect); o.Owner != tenancy.OwnerUI {
		word := "resumed"
		if suspend {
			word = "suspended"
		}
		writeConflict(w, fmt.Sprintf("AgentSchedule %s/%s is managed in Git and cannot be %s here: change spec.suspend in Git, or use Duplicate to make a UI-owned copy",
			sc.Namespace, sc.Name, word), sc.ResourceVersion)
		return
	}
	degraded := meta.IsStatusConditionTrue(sc.Status.Conditions, agentorcav1alpha1.ScheduleConditionDegraded)
	if sc.Spec.Suspend == suspend && (suspend || !degraded) {
		jsonResponse(w, map[string]string{"status": "unchanged"})
		return
	}
	u, err := toUIBody(sc)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_ = unstructured.SetNestedField(u.Object, suspend, "spec", "suspend")
	action, word := postgresql.AuditSuspend, "suspended"
	if !suspend {
		action, word = postgresql.AuditResume, "resumed"
		a := u.GetAnnotations()
		if a == nil {
			a = map[string]string{}
		}
		a[schedule.AnnotationResumedAt] = time.Now().UTC().Format(time.RFC3339)
		u.SetAnnotations(a)
	}
	info := crdKinds["agentschedules"]
	s.applyUpdate(w, r, info, sc.Namespace, sc.Name, resourceBody{obj: u, resourceVersion: sc.ResourceVersion}, updateAudit{
		action: action, summary: fmt.Sprintf("%s AgentSchedule %s/%s", word, sc.Namespace, sc.Name),
	})
}

// toUIBody turns a stored object into the body an editor would send back:
// no status, no server-owned metadata.
func toUIBody(obj client.Object) (*unstructured.Unstructured, error) {
	cp, ok := obj.DeepCopyObject().(client.Object)
	if !ok {
		return nil, fmt.Errorf("%T is not a client.Object", obj)
	}
	cp.SetManagedFields(nil)
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(cp)
	if err != nil {
		return nil, err
	}
	u := &unstructured.Unstructured{Object: m}
	delete(m, "status")
	if md, ok := m["metadata"].(map[string]any); ok {
		for _, f := range uiMetadataDropped {
			delete(md, f)
		}
	}
	u.SetAPIVersion(agentorcav1alpha1.GroupVersion.String())
	u.SetKind(kindAgentSchedule)
	return u, nil
}

// runScheduleNow handles POST .../run: a normal run (or workflow) with the
// schedule's settings, started by the caller. It is labelled for the schedule
// but not owned by it, so it is not counted in the schedule's history limits,
// failure streak or spend caps, and it passes the tenant's quotas like any run.
func (s *UIServer) runScheduleNow(w http.ResponseWriter, r *http.Request, sc *agentorcav1alpha1.AgentSchedule) {
	if !requireRole(w, r, tenancy.RoleRunner) {
		return
	}
	ctx := r.Context()
	tenant, _ := TenantFromContext(ctx)
	if tenant != nil {
		var rl *RateLimiter
		if s.externalAuth != nil {
			rl = s.externalAuth.RateLimiter()
		}
		if qerr, ok := enforceQuotas(ctx, s.crdClient, rl, tenant).(*QuotaError); ok && qerr != nil {
			if qerr.RetryAfter > 0 {
				w.Header().Set("Retry-After", strconv.Itoa(int(qerr.RetryAfter.Seconds())))
			}
			jsonError(w, qerr.Message, qerr.Code)
			return
		}
	}
	zone, err := schedule.Parse(sc.Spec.Schedule, sc.Spec.TimeZone)
	if err != nil {
		jsonError(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	now := time.Now().In(zone.Location())
	by := actorOf(ctx)
	labels := map[string]string{security.LabelManagedBy: security.ManagedByValue, schedule.LabelSchedule: sc.Name}
	for _, k := range []string{tenancy.LabelTenantID, tenancy.LabelTenantName} {
		if v := sc.Labels[k]; v != "" {
			labels[k] = v
		}
	}
	ann := map[string]string{schedule.AnnotationCreatedBy: by, schedule.AnnotationScheduledFor: now.Format(time.RFC3339)}
	om := metav1.ObjectMeta{GenerateName: sc.Name + "-m-", Namespace: sc.Namespace, Labels: labels, Annotations: ann}

	var created client.Object
	var kind string
	if sc.Spec.WorkflowTemplateRef != "" {
		var tpl agentorcav1alpha1.AgentWorkflow
		if err := s.crdClient.Get(ctx, client.ObjectKey{Namespace: sc.Namespace, Name: sc.Spec.WorkflowTemplateRef}, &tpl); err != nil || tpl.Spec.Mode != agentorcav1alpha1.WorkflowModeTemplate {
			jsonError(w, "the schedule's workflow template is missing", http.StatusConflict)
			return
		}
		if tenant != nil {
			for _, st := range tpl.Spec.Steps {
				if st.AgentRef != "" && !tenantCanAccessAgent(tenant, st.AgentRef) {
					jsonError(w, fmt.Sprintf("agent %q is not allowed for this tenant", st.AgentRef), http.StatusForbidden)
					return
				}
			}
		}
		params := map[string]string{}
		for k, v := range sc.Spec.Parameters {
			params[k] = schedule.Substitute(v, sc.Name, now)
		}
		created = &agentorcav1alpha1.AgentWorkflow{ObjectMeta: om, Spec: agentorcav1alpha1.AgentWorkflowSpec{
			FromTemplate: &agentorcav1alpha1.WorkflowFromTemplate{Name: tpl.Name, Parameters: params},
		}}
		kind = "AgentWorkflow"
	} else {
		var agent agentorcav1alpha1.Agent
		if err := s.crdClient.Get(ctx, client.ObjectKey{Namespace: sc.Namespace, Name: sc.Spec.AgentRef}, &agent); err != nil {
			if apierrors.IsNotFound(err) {
				jsonError(w, "the schedule's agent is missing", http.StatusConflict)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if tenant != nil && !tenantCanAccessAgent(tenant, agent.Name) {
			jsonError(w, "agent not allowed for tenant", http.StatusForbidden)
			return
		}
		run := &agentorcav1alpha1.AgentRun{ObjectMeta: om, Spec: agentorcav1alpha1.AgentRunSpec{
			AgentRef: sc.Spec.AgentRef, Input: schedule.Substitute(sc.Spec.Input, sc.Name, now),
			Timeout: sc.Spec.Timeout.DeepCopy(), Safeguards: sc.Spec.Safeguards.DeepCopy(),
		}}
		if b := sc.Spec.Budget; b != nil {
			run.Spec.Budget = b.DeepCopy()
			if run.Spec.Budget.OnExceeded == "" {
				run.Spec.Budget.OnExceeded = agentorcav1alpha1.RunBudgetFail
			}
		}
		if res := sc.Spec.Result; res != nil {
			run.Spec.Egress, run.Spec.Callbacks = res.Egress.DeepCopy(), res.Callbacks.DeepCopy()
		}
		created, kind = run, "AgentRun"
	}
	if err := s.crdClient.Create(ctx, created); err != nil {
		writeAPIError(w, "creating the run", err)
		return
	}
	s.recordAudit(w, r, auditEventFor(postgresql.AuditRequest, kind, created.GetNamespace(), created.GetName(), created.GetResourceVersion(),
		fmt.Sprintf("ran AgentSchedule %s/%s now", sc.Namespace, sc.Name)))
	w.WriteHeader(http.StatusAccepted)
	jsonResponse(w, map[string]string{"kind": kind, "name": created.GetName(), "namespace": created.GetNamespace()})
}

// scheduleWriteGate applies the role rule the CRUD path cannot express: a
// schedule that sends results to an egress sink with credentials needs the
// admin role, as for MCPServer and Tool. Keeping a secretRef an admin already
// set is allowed. It answers 403 and returns false when the caller may not write.
func scheduleWriteGate(w http.ResponseWriter, r *http.Request, body *unstructured.Unstructured, existing *agentorcav1alpha1.AgentSchedule) bool {
	ref, found, _ := unstructured.NestedMap(body.Object, "spec", "result", "egress", "secretRef")
	if !found || len(ref) == 0 || hasRole(r.Context(), tenancy.RoleAdmin) {
		return true
	}
	if existing != nil && existing.Spec.Result != nil && existing.Spec.Result.Egress != nil && existing.Spec.Result.Egress.SecretRef != nil {
		old := existing.Spec.Result.Egress.SecretRef
		sameNS := ref["namespace"] == old.Namespace || (ref["namespace"] == nil && old.Namespace == "")
		if ref["name"] == old.Name && ref["key"] == old.Key && sameNS {
			return true
		}
	}
	http.Error(w, `{"error":"sending results to a sink with credentials (result.egress.secretRef) needs the admin role in your tenant"}`, http.StatusForbidden)
	return false
}
