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
	"context"
	"fmt"
	"net/http"

	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/evals/graders"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// checkExperimentJudges checks what the dataset's llm graders need before an
// experiment is queued: a pairwise grader needs exactly two arms, and the
// ModelProvider a grader names must exist in the workspace (or the platform
// catalog) and be one the caller may use. It returns cautions for the response:
// a judge that is also an arm's model favours its own output.
func (s *UIServer) checkExperimentJudges(w http.ResponseWriter, r *http.Request, e postgresql.EvalExperiment, gs []postgresql.EvalGrader) ([]string, bool) {
	var warnings []string
	for _, g := range gs {
		if g.Kind != graders.KindLLM {
			continue
		}
		cfg, err := graders.ParseLLM(g.Config)
		if err != nil {
			jsonError(w, fmt.Sprintf("grader %q: %v", g.Name, err), http.StatusBadRequest)
			return nil, false
		}
		if cfg.Mode == graders.ModePairwise && len(e.Arms) != 2 {
			jsonError(w, fmt.Sprintf("grader %q compares two arms, and the experiment has %d", g.Name, len(e.Arms)), http.StatusBadRequest)
			return nil, false
		}
		if s.crdClient != nil && !s.judgeProviderUsable(r.Context(), e.Namespace, cfg.ProviderRef) {
			jsonError(w, fmt.Sprintf("grader %q: ModelProvider %q does not exist in namespace %s or the platform catalog", g.Name, cfg.ProviderRef, e.Namespace), http.StatusUnprocessableEntity)
			return nil, false
		}
		for _, a := range e.Arms {
			if a.ModelProvider == cfg.ProviderRef {
				warnings = append(warnings, fmt.Sprintf("the judge of grader %q is the model of arm %q and will favour its own output", g.Name, a.Label))
			}
		}
	}
	return warnings, true
}

// judgeProviderUsable reports whether the caller may use the ModelProvider a judge names.
func (s *UIServer) judgeProviderUsable(ctx context.Context, namespace, name string) bool {
	for _, ns := range append([]string{namespace}, tenancy.PlatformNamespaces()...) {
		var mp agentorcav1alpha1.ModelProvider
		if err := s.crdClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &mp); err == nil && canRead(ctx, &mp, "ModelProvider") {
			return true
		}
	}
	return false
}
