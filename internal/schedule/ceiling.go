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

package schedule

import (
	"strconv"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

// positive reports whether s is a decimal greater than zero.
func positive(s string) bool {
	v, err := strconv.ParseFloat(s, 64)
	return err == nil && v > 0
}

// HasCeiling reports whether an unattended schedule has something that stops
// its spend: a per-run budget, a schedule spend cap, a workflow template's
// budget cap, or a daily budget on the tenant. A schedule without one is
// refused, because an unattended run with no ceiling is exactly the failure
// scheduling must not allow. tenantBudgetPerDay is the tenant's
// budgetPerDayUSD, "" when there is no tenant. template is the workflow
// template for a workflowTemplateRef schedule, otherwise nil.
func HasCeiling(spec *agentorcav1alpha1.AgentScheduleSpec, tenantBudgetPerDay string, template *agentorcav1alpha1.AgentWorkflow) bool {
	if b := spec.Budget; b != nil && positive(b.MaxUSD) {
		return true
	}
	if c := spec.Spend; c != nil && (positive(c.MaxUSDPerDay) || positive(c.MaxUSDPerMonth)) {
		return true
	}
	if template != nil && template.Spec.BudgetCap != nil && positive(template.Spec.BudgetCap.Total) {
		return true
	}
	return positive(tenantBudgetPerDay)
}

// SpendCaps returns the schedule's day and month caps in USD; zero means none.
func SpendCaps(spec *agentorcav1alpha1.AgentScheduleSpec) (day, month float64) {
	if c := spec.Spend; c != nil {
		day, _ = strconv.ParseFloat(c.MaxUSDPerDay, 64)
		month, _ = strconv.ParseFloat(c.MaxUSDPerMonth, 64)
	}
	return max(day, 0), max(month, 0)
}
