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

package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

func envMap(run *agentorcav1alpha1.AgentRun) map[string]string {
	out := map[string]string{}
	for _, e := range agentRunEnv(run, &agentorcav1alpha1.Agent{}) {
		out[e.Name] = e.Value
	}
	return out
}

func TestAgentRunEnvCarriesThePriorTurn(t *testing.T) {
	run := &agentorcav1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: "turn-2"},
		Spec:       agentorcav1alpha1.AgentRunSpec{PriorRunRef: "turn-1"},
	}
	if got := envMap(run)["AGENTORC_PRIOR_RUN_ID"]; got != "turn-1" {
		t.Errorf("AGENTORC_PRIOR_RUN_ID = %q, want turn-1", got)
	}
}

func TestAgentRunEnvOmitsPriorTurnOnAFirstTurn(t *testing.T) {
	run := &agentorcav1alpha1.AgentRun{ObjectMeta: metav1.ObjectMeta{Name: "turn-1"}}
	if got, ok := envMap(run)["AGENTORC_PRIOR_RUN_ID"]; ok {
		t.Errorf("AGENTORC_PRIOR_RUN_ID = %q on a first turn, want unset", got)
	}
}
