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
	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/router"
)

// defaultHindsightBankTemplate gives each Agent one bank, shared by its runs
// and deployments.
const defaultHindsightBankTemplate = "{namespace}--{agent-name}"

// hindsightConfig is the model-router's hindsight settings for an Agent.
// Hindsight is opt-in: it runs only when the Agent sets spec.memory.hindsight
// (enabled defaults to true there). Recall and retain on every turn add
// latency and spend to every LLM call, and retaining a one-shot run's turns
// would carry its untrusted input into later runs. defaultURL is the
// operator's configured endpoint, used when the Agent names none.
func hindsightConfig(agent *agentorcav1alpha1.Agent, defaultURL string) router.HindsightConfig {
	if agent.Spec.Memory == nil || agent.Spec.Memory.Hindsight == nil || !agent.Spec.Memory.Hindsight.Enabled {
		return router.HindsightConfig{}
	}
	h := agent.Spec.Memory.Hindsight
	cfg := router.HindsightConfig{
		Enabled:           true,
		URL:               h.URL,
		BankIDTemplate:    h.BankIDTemplate,
		RecallBudget:      h.RecallBudget,
		RetainOnEveryTurn: h.RetainOnEveryTurn,
	}
	if cfg.URL == "" {
		cfg.URL = defaultURL
	}
	if cfg.BankIDTemplate == "" {
		cfg.BankIDTemplate = defaultHindsightBankTemplate
	}
	if cfg.RecallBudget <= 0 {
		cfg.RecallBudget = 5
	}
	return cfg
}
