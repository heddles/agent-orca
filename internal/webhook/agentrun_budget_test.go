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
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

func TestValidateRunBudget(t *testing.T) {
	pause := agentorcav1alpha1.RunBudgetPause
	for name, tc := range map[string]struct {
		budget  *agentorcav1alpha1.RunBudget
		wantErr string
	}{
		"none":               {nil, ""},
		"fail by default":    {&agentorcav1alpha1.RunBudget{MaxUSD: "1.50"}, ""},
		"pause with options": {&agentorcav1alpha1.RunBudget{MaxUSD: "5", OnExceeded: pause, RaiseByUSD: "2", PauseTimeout: &metav1.Duration{Duration: time.Hour}, ApproverGroups: []string{"finance"}}, ""},
		"zero cap":           {&agentorcav1alpha1.RunBudget{MaxUSD: "0"}, "maxUSD must be a positive amount"},
		"zero decimal cap":   {&agentorcav1alpha1.RunBudget{MaxUSD: "0.00"}, "maxUSD must be a positive amount"},
		"zero raise":         {&agentorcav1alpha1.RunBudget{MaxUSD: "1", RaiseByUSD: "0"}, "raiseByUSD must be a positive amount"},
		"negative timeout":   {&agentorcav1alpha1.RunBudget{MaxUSD: "1", PauseTimeout: &metav1.Duration{Duration: -time.Second}}, "pauseTimeout must be positive"},
		"unknown action":     {&agentorcav1alpha1.RunBudget{MaxUSD: "1", OnExceeded: "stop"}, "onExceeded must be"},
		"groups need pause":  {&agentorcav1alpha1.RunBudget{MaxUSD: "1", ApproverGroups: []string{"finance"}}, "approverGroups only applies with onExceeded: pause"},
	} {
		err := validateRunBudget(tc.budget)
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("%s: unexpected error %v", name, err)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("%s: error %v, want one containing %q", name, err, tc.wantErr)
		}
	}
}
