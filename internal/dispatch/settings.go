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

package dispatch

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/dispatch/decision"
	"github.com/heddles/agent-orca/internal/dispatch/llmdecider"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/quota"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// settings are a tenant's spec.dispatch, with the defaults filled in.
type settings struct {
	enabled bool
	decider string
	// selector is the ModelSelector a model decides with, when one is named.
	selectorNamespace, selector string
	escalate                    bool
	defaultAgent                string
	threshold                   float64
	budgetPerDayUSD             float64
}

func settingsOf(tc *agentorcav1alpha1.TenantConfig) settings {
	s := settings{enabled: true, decider: agentorcav1alpha1.DispatchDeciderLocal, threshold: DefaultThreshold}
	if v, err := strconv.ParseFloat(tc.Spec.BudgetPerDayUSD, 64); err == nil && v > 0 {
		s.budgetPerDayUSD = v
	}
	spec := tc.Spec.Dispatch
	if spec == nil {
		return s
	}
	if spec.Enabled != nil {
		s.enabled = *spec.Enabled
	}
	if spec.Decider != "" {
		s.decider = spec.Decider
	}
	if ref := spec.ModelSelectorRef; ref != nil {
		s.selector, s.selectorNamespace = ref.Name, ref.Namespace
		if s.selectorNamespace == "" && len(tc.Spec.AllowedNamespaces) > 0 {
			s.selectorNamespace = tc.Spec.AllowedNamespaces[0]
		}
	}
	s.escalate = spec.Escalate
	s.defaultAgent = spec.DefaultAgent
	if p := spec.MinConfidencePercent; p != nil {
		s.threshold = max(floor, min(float64(*p)/100, 1))
	}
	return s
}

// settings reads the settings of the caller's TenantConfig, found by tenant ID
// because a name can be reused.
func (d *Dispatcher) settings(ctx context.Context, t Tenant) (settings, error) {
	var list agentorcav1alpha1.TenantConfigList
	if err := d.Client.List(ctx, &list); err != nil {
		return settings{}, fmt.Errorf("reading the tenant's configuration: %w", err)
	}
	for i := range list.Items {
		if t.ID != "" && tenancy.TenantIDOf(&list.Items[i]) == t.ID {
			return settingsOf(&list.Items[i]), nil
		}
	}
	return settings{}, fmt.Errorf("%w: the caller has no TenantConfig", ErrOff)
}

// available reports why a tenant has no way to have an agent decided, if it has
// none.
func (d *Dispatcher) available(s settings) error {
	switch {
	case !s.enabled:
		return fmt.Errorf("%w: it is turned off for this tenant", ErrOff)
	case s.decider == agentorcav1alpha1.DispatchDeciderModelSelector:
		if s.selector == "" {
			return fmt.Errorf("%w: the modelSelector decider needs spec.dispatch.modelSelectorRef", ErrOff)
		}
	case d.Local == nil && (!s.escalate || s.selector == ""):
		// A tenant on the local decider never has requests sent out of the
		// cluster unless it asked for that with escalate.
		return fmt.Errorf("%w: no local decision service is deployed", ErrOff)
	}
	return nil
}

// decider is the tenant's: the local one, a model of its ModelSelector, or the
// local one with the model behind it.
func (d *Dispatcher) decider(t Tenant, s settings) decision.Decider {
	judge := &selectorDecider{d: d, tenant: t, settings: s}
	switch {
	case s.decider == agentorcav1alpha1.DispatchDeciderModelSelector:
		return judge
	case !s.escalate || s.selector == "":
		return d.Local
	case d.Local == nil:
		return judge
	}
	return decision.Escalating{First: d.Local, Second: judge, Threshold: s.threshold}
}

// selectorDecider asks a model of the tenant's ModelSelector. It resolves the
// selector, and reads provider credentials, only when it is asked.
type selectorDecider struct {
	d        *Dispatcher
	tenant   Tenant
	settings settings
}

func (s *selectorDecider) Decide(ctx context.Context, req decision.Request) (decision.Response, error) {
	caller := tenancy.Caller{TenantID: s.tenant.ID, TenantName: s.tenant.Name, Namespaces: s.tenant.Namespaces}
	models, err := llmdecider.SelectorModels(ctx, s.d.Client, s.d.Providers, caller,
		s.settings.selectorNamespace, s.settings.selector)
	if err != nil {
		slog.Warn("dispatch: the tenant's decision ModelSelector", "tenant", s.tenant.ID, "err", err)
		return decision.Response{Provider: llmdecider.ProviderModelSelector}, err
	}
	return llmdecider.Chooser{Models: models, Guard: s.d.budgetGuard(ctx, s.tenant, s.settings)}.Decide(ctx, req)
}

// budgetGuard allows a decision call while the tenant's spend today, on runs and
// on decision calls, leaves room for it. When the spend cannot be read it does
// not. Calls made at the same moment can each pass, so the budget can be
// overshot by about a call for each.
func (d *Dispatcher) budgetGuard(ctx context.Context, t Tenant, s settings) func(float64) bool {
	if s.budgetPerDayUSD <= 0 {
		return nil
	}
	return func(pending float64) bool {
		now := d.now()
		spent, err := quota.SumTodaySpend(ctx, d.Client, t.Name, t.ID, now)
		if err == nil && d.Records != nil {
			var calls float64
			calls, err = d.Records.DispatchSpendSince(postgresql.WithTenant(ctx, t.ID), t.ID, now.UTC().Truncate(24*time.Hour))
			spent += calls
		}
		if err != nil {
			slog.Error("dispatch: reading the tenant's spend", "tenant", t.ID, "err", err)
			return false
		}
		return spent+pending < s.budgetPerDayUSD
	}
}
