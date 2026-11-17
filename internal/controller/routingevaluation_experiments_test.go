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

	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

// spec.evaluation.includeExperiments is off unless the benchmark sets it, so the
// source is asked for shadow samples only and existing findings do not change.
func TestEvaluation_IncludeExperimentsIsOffByDefault(t *testing.T) {
	w := newEvalWorld(t, nil)
	w.reconcile(t)
	if w.src.calls == 0 || w.src.last.IncludeExperiments {
		t.Fatalf("query = %+v, calls %d: experiments must not be included by default", w.src.last, w.src.calls)
	}
	if recs := w.recs(t); len(recs) != 1 || recs[0].Spec.Detector != "overspend" {
		t.Errorf("recommendations = %+v", recs)
	}
}

func TestEvaluation_IncludeExperimentsReachesTheSource(t *testing.T) {
	w := newEvalWorld(t, func(objs *[]client.Object) {
		for _, o := range *objs {
			if b, ok := o.(*agentorcav1alpha1.RoutingBenchmark); ok {
				b.Spec.Evaluation = &agentorcav1alpha1.EvaluationSpec{IncludeExperiments: true}
			}
		}
	})
	w.reconcile(t)
	if !w.src.last.IncludeExperiments {
		t.Fatalf("query = %+v, want IncludeExperiments", w.src.last)
	}
	// The observations are the same shadow samples, so the finding is the same
	// and names no experiment samples.
	recs := w.recs(t)
	if len(recs) != 1 || recs[0].Spec.Detector != "overspend" {
		t.Fatalf("recommendations = %+v", recs)
	}
}
