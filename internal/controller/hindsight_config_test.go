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

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/router"
)

func TestHindsightConfig(t *testing.T) {
	const defaultURL = "http://hindsight-api.hindsight.svc:8888"
	withMemory := func(h *agentorcav1alpha1.HindsightMemoryConfig) *agentorcav1alpha1.Agent {
		return &agentorcav1alpha1.Agent{Spec: agentorcav1alpha1.AgentSpec{
			Memory: &agentorcav1alpha1.AgentMemoryConfig{Hindsight: h},
		}}
	}
	tests := []struct {
		name  string
		agent *agentorcav1alpha1.Agent
		want  router.HindsightConfig
	}{
		{"no memory block is off", &agentorcav1alpha1.Agent{}, router.HindsightConfig{}},
		{"memory without hindsight is off", withMemory(nil), router.HindsightConfig{}},
		{"explicitly disabled is off", withMemory(&agentorcav1alpha1.HindsightMemoryConfig{Enabled: false}),
			router.HindsightConfig{}},
		{"enabled gets the defaults", withMemory(&agentorcav1alpha1.HindsightMemoryConfig{Enabled: true}),
			router.HindsightConfig{Enabled: true, URL: defaultURL, BankIDTemplate: "{namespace}--{agent-name}", RecallBudget: 5}},
		{"agent settings win", withMemory(&agentorcav1alpha1.HindsightMemoryConfig{
			Enabled: true, URL: "http://h:1", BankIDTemplate: "{namespace}--{deployment}", RecallBudget: 9, RetainOnEveryTurn: true,
		}), router.HindsightConfig{
			Enabled: true, URL: "http://h:1", BankIDTemplate: "{namespace}--{deployment}", RecallBudget: 9, RetainOnEveryTurn: true,
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hindsightConfig(tt.agent, defaultURL); got != tt.want {
				t.Errorf("config = %+v\nwant %+v", got, tt.want)
			}
		})
	}
}
