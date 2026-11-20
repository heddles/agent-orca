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

package webhook

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/schedule"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// AgentScheduleValidator checks what the CRD schema cannot: the cron
// expression and time zone, a minimum interval, that the target exists in the
// schedule's own namespace and is allowed for its tenant, and that an unattended
// schedule has a spending ceiling. Cross-tenant references and the tenant label
// are the isolation webhook's job; this one adds the schedule-specific rules.
//
// A suspend or resume (the only change is spec.suspend) is always allowed, so a
// person can stop a schedule whose target has since gone away.
type AgentScheduleValidator struct {
	Decoder admission.Decoder
	Client  client.Reader
	// MinInterval is the shortest gap between ticks a schedule may have; zero
	// means schedule.DefaultMinInterval.
	MinInterval time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// +kubebuilder:webhook:path=/validate-agentorca-agentorca-io-v1alpha1-agentschedule,mutating=false,failurePolicy=fail,sideEffects=None,groups=agentorca.agentorca.io,resources=agentschedules,verbs=create;update,versions=v1alpha1,name=vagentschedule.kb.io,admissionReviewVersions=v1

// Handle implements admission.Handler.
func (v *AgentScheduleValidator) Handle(ctx context.Context, req admission.Request) admission.Response {
	var s agentorcav1alpha1.AgentSchedule
	if err := v.Decoder.DecodeRaw(req.Object, &s); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	if !s.DeletionTimestamp.IsZero() {
		return admission.Allowed("ok")
	}
	var old *agentorcav1alpha1.AgentSchedule
	if req.Operation == admissionv1.Update && len(req.OldObject.Raw) > 0 {
		old = &agentorcav1alpha1.AgentSchedule{}
		if err := v.Decoder.DecodeRaw(req.OldObject, old); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
		if onlySuspendChanged(old, &s) {
			return admission.Allowed("ok")
		}
	}
	warnings, err := v.validate(ctx, &s, old)
	if err != nil {
		var denied *deniedError
		if asDenied(err, &denied) {
			return admission.Denied(denied.Error()).WithWarnings(warnings...)
		}
		return admission.Errored(http.StatusInternalServerError, err)
	}
	return admission.Allowed("ok").WithWarnings(warnings...)
}

// deniedError is a rule violation, as opposed to a failure to evaluate the rules.
type deniedError struct{ msg string }

func (e *deniedError) Error() string { return e.msg }

func asDenied(err error, target **deniedError) bool {
	d, ok := err.(*deniedError) //nolint:errorlint // only ever returned unwrapped
	if ok {
		*target = d
	}
	return ok
}

func deny(format string, args ...any) error { return &deniedError{fmt.Sprintf(format, args...)} }

// onlySuspendChanged reports whether the update changes nothing but spec.suspend.
func onlySuspendChanged(old, cur *agentorcav1alpha1.AgentSchedule) bool {
	spec := cur.Spec.DeepCopy()
	spec.Suspend = old.Spec.Suspend
	return equality.Semantic.DeepEqual(old.Spec, *spec)
}

func (v *AgentScheduleValidator) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}

func (v *AgentScheduleValidator) validate(ctx context.Context, s, old *agentorcav1alpha1.AgentSchedule) (warnings []string, err error) {
	spec := &s.Spec
	if len(s.Name) > schedule.MaxNameLength {
		return nil, deny("the name is %d characters; a schedule's name is at most %d because each run is named <schedule>-<unix time>", len(s.Name), schedule.MaxNameLength)
	}
	if (spec.AgentRef == "") == (spec.WorkflowTemplateRef == "") {
		return nil, deny("set exactly one of spec.agentRef and spec.workflowTemplateRef")
	}

	cs, perr := schedule.Parse(spec.Schedule, spec.TimeZone)
	if perr != nil {
		return nil, deny("spec.schedule: %v", perr)
	}
	minInterval := v.MinInterval
	if minInterval <= 0 {
		minInterval = schedule.DefaultMinInterval
	}
	if gap := cs.MinInterval(v.now()); gap != 0 && gap < minInterval {
		return nil, deny("spec.schedule %q fires every %s at its closest; the minimum interval is %s", spec.Schedule, gap, minInterval)
	}

	if err := validateRunBudget(spec.Budget); err != nil {
		return nil, deny("%v", err)
	}
	if b := spec.Budget; b != nil && b.OnExceeded == agentorcav1alpha1.RunBudgetPause {
		warnings = append(warnings, "spec.budget.onExceeded is pause: a scheduled run that reaches its cap waits for an approval nobody is expecting. Use fail unless an approver group watches for it.")
	}
	if c := spec.Spend; c != nil {
		for field, val := range map[string]string{"maxUSDPerDay": c.MaxUSDPerDay, "maxUSDPerMonth": c.MaxUSDPerMonth} {
			if val == "" {
				continue
			}
			if f, perr := strconv.ParseFloat(val, 64); perr != nil || f <= 0 {
				return nil, deny("spec.spend.%s must be a positive amount, got %q", field, val)
			}
		}
	}
	if spec.WorkflowTemplateRef != "" && spec.ConcurrencyPolicy == agentorcav1alpha1.ScheduleConcurrencyReplace {
		return nil, deny("spec.concurrencyPolicy Replace needs a run to cancel: a workflow cannot be cancelled, so use Forbid or Allow with workflowTemplateRef")
	}

	// The checks that read the cluster run on create and when the target or the
	// ceiling changes; an unrelated edit does not re-litigate them.
	if old != nil && old.Spec.AgentRef == spec.AgentRef && old.Spec.WorkflowTemplateRef == spec.WorkflowTemplateRef &&
		equality.Semantic.DeepEqual(old.Spec.Budget, spec.Budget) && equality.Semantic.DeepEqual(old.Spec.Spend, spec.Spend) {
		return warnings, nil
	}

	template, terr := v.checkTarget(ctx, s)
	if terr != nil {
		return warnings, terr
	}
	tenant, terr := v.tenantOf(ctx, s)
	if terr != nil {
		return warnings, terr
	}
	if tenant != nil {
		for _, name := range targetAgents(spec, template) {
			if !tenancy.AgentAllowed(tenant.Spec.AllowedAgents, name) {
				return warnings, deny("agent %q is not in the allowed agents of tenant %q", name, tenant.Name)
			}
		}
	}
	budget := ""
	if tenant != nil {
		budget = tenant.Spec.BudgetPerDayUSD
	}
	if !schedule.HasCeiling(spec, budget, template) {
		return warnings, deny("an unattended schedule needs a spending ceiling: set spec.budget (per run) or spec.spend, " +
			"or ask a tenant admin to set budgetPerDayUSD on the TenantConfig")
	}
	return warnings, nil
}

// checkTarget confirms the agent or template exists in the schedule's namespace.
// It returns the template for a workflow schedule.
func (v *AgentScheduleValidator) checkTarget(ctx context.Context, s *agentorcav1alpha1.AgentSchedule) (*agentorcav1alpha1.AgentWorkflow, error) {
	if s.Spec.AgentRef != "" {
		var a agentorcav1alpha1.Agent
		if err := v.Client.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: s.Spec.AgentRef}, &a); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, deny("agent %q was not found in namespace %q: a schedule can run only an agent of its own namespace", s.Spec.AgentRef, s.Namespace)
			}
			return nil, fmt.Errorf("reading agent %q: %w", s.Spec.AgentRef, err)
		}
		return nil, nil
	}
	var wf agentorcav1alpha1.AgentWorkflow
	if err := v.Client.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: s.Spec.WorkflowTemplateRef}, &wf); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, deny("workflow template %q was not found in namespace %q", s.Spec.WorkflowTemplateRef, s.Namespace)
		}
		return nil, fmt.Errorf("reading workflow template %q: %w", s.Spec.WorkflowTemplateRef, err)
	}
	if wf.Spec.Mode != agentorcav1alpha1.WorkflowModeTemplate {
		return nil, deny("AgentWorkflow %q is not a template: it needs spec.mode: template", wf.Name)
	}
	return &wf, nil
}

// tenantOf finds the tenant that owns the schedule: the one its tenant-id label
// names, else the only one that claims its namespace. nil means none (a
// platform-scoped schedule, or a namespace no tenant claims).
func (v *AgentScheduleValidator) tenantOf(ctx context.Context, s *agentorcav1alpha1.AgentSchedule) (*agentorcav1alpha1.TenantConfig, error) {
	var tcs agentorcav1alpha1.TenantConfigList
	if err := v.Client.List(ctx, &tcs); err != nil {
		return nil, fmt.Errorf("listing TenantConfigs: %w", err)
	}
	return tenancy.TenantConfigFor(tcs.Items, s), nil
}

// targetAgents lists the agents a schedule would run: its agentRef, or the
// agents of the template's steps.
func targetAgents(spec *agentorcav1alpha1.AgentScheduleSpec, template *agentorcav1alpha1.AgentWorkflow) []string {
	if spec.AgentRef != "" {
		return []string{spec.AgentRef}
	}
	if template == nil {
		return nil
	}
	var out []string
	for _, st := range template.Spec.Steps {
		if st.AgentRef != "" && !slices.Contains(out, st.AgentRef) {
			out = append(out, st.AgentRef)
		}
	}
	return out
}

// SetupAgentScheduleWebhook registers the AgentSchedule validating webhook.
func SetupAgentScheduleWebhook(mgr ctrl.Manager, minInterval time.Duration) error {
	mgr.GetWebhookServer().Register("/validate-agentorca-agentorca-io-v1alpha1-agentschedule",
		&admission.Webhook{Handler: &AgentScheduleValidator{
			Decoder: admission.NewDecoder(mgr.GetScheme()), Client: mgr.GetClient(), MinInterval: minInterval,
		}})
	return nil
}
