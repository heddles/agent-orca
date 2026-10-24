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
	"fmt"
	"strconv"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

// validateRunBudget rejects budgets the router could not enforce. The CRD
// schema checks the number format; this checks the values.
func validateRunBudget(b *agentorcav1alpha1.RunBudget) error {
	if b == nil {
		return nil
	}
	if v, err := strconv.ParseFloat(b.MaxUSD, 64); err != nil || v <= 0 {
		return fmt.Errorf("spec.budget.maxUSD must be a positive amount, got %q", b.MaxUSD)
	}
	if b.RaiseByUSD != "" {
		if v, err := strconv.ParseFloat(b.RaiseByUSD, 64); err != nil || v <= 0 {
			return fmt.Errorf("spec.budget.raiseByUSD must be a positive amount, got %q", b.RaiseByUSD)
		}
	}
	if b.PauseTimeout != nil && b.PauseTimeout.Duration <= 0 {
		return fmt.Errorf("spec.budget.pauseTimeout must be positive")
	}
	if b.OnExceeded != "" && b.OnExceeded != agentorcav1alpha1.RunBudgetFail && b.OnExceeded != agentorcav1alpha1.RunBudgetPause {
		return fmt.Errorf("spec.budget.onExceeded must be %q or %q", agentorcav1alpha1.RunBudgetFail, agentorcav1alpha1.RunBudgetPause)
	}
	if len(b.ApproverGroups) > 0 && b.OnExceeded != agentorcav1alpha1.RunBudgetPause {
		return fmt.Errorf("spec.budget.approverGroups only applies with onExceeded: pause")
	}
	return nil
}
