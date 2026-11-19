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

package routingbench

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// experimentBatch is batch, marked as projected from an evaluation experiment.
func experimentBatch(agent, category string, n, wins, losses int) []Sample {
	s := batch(agent, category, n, wins, losses, 0)
	for i := range s {
		s[i].FromExperiment = true
	}
	return s
}

// Without experiment samples (the includeExperiments flag off) the findings
// carry no experiment count and serialise as they always did.
func TestAnalyzeWithoutExperimentSamplesIsUnchanged(t *testing.T) {
	ev := Observations{
		Samples: batch("a", stageC, 100, 1, 1, 0),
		Volumes: []Volume{{Agent: "a", Category: stageC, Provider: strong, Calls: 300}},
	}
	fs := Analyze(ev, clusterOf(ruleSelector(), "a"), window, Options{})
	if len(fs) == 0 {
		t.Fatal("no findings")
	}
	for _, f := range fs {
		if f.Evidence.ExperimentSamples != 0 {
			t.Errorf("%s: experiment samples = %d", f.Detector, f.Evidence.ExperimentSamples)
		}
	}
	if b, _ := json.Marshal(fs); strings.Contains(string(b), "experimentSamples") {
		t.Errorf("findings JSON names experiment samples: %s", b)
	}
}

// Experiment samples add win-rate evidence for their own category, are counted
// in the finding's evidence, and leave the findings of other categories alone.
func TestAnalyzeCountsExperimentSamplesAndKeepsOtherFindings(t *testing.T) {
	base := Observations{
		Samples: batch("a", stageC, 100, 1, 1, 0),
		Volumes: []Volume{{Agent: "a", Category: stageC, Provider: strong, Calls: 300}},
	}
	want := Analyze(base, clusterOf(ruleSelector(), "a"), window, Options{})

	with := Observations{Samples: append(slices.Clone(base.Samples), experimentBatch("a", "tag:billing", 200, 1, 1)...),
		Volumes: append(slices.Clone(base.Volumes), Volume{Agent: "a", Category: "tag:billing", Provider: strong, Calls: 90})}
	got := Analyze(with, clusterOf(ruleSelector(), "a"), window, Options{})
	var fromExperiment int
	var others []Finding
	for _, f := range got {
		if f.Category == "tag:billing" {
			fromExperiment++
			if f.Evidence.ExperimentSamples != 200 || f.Evidence.Samples != 200 {
				t.Errorf("experiment finding evidence = %+v", f.Evidence)
			}
			if f.Action != nil {
				if _, err := BuildPatch(ruleSelector(), *f.Action); err == nil {
					t.Error("a tag category must not yield a patch: only stages can be routed")
				}
			}
			continue
		}
		others = append(others, f)
	}
	if fromExperiment == 0 {
		t.Fatalf("no finding from the experiment samples: %+v", got)
	}
	a, _ := json.Marshal(want)
	b, _ := json.Marshal(others)
	if string(a) != string(b) {
		t.Errorf("the findings of other categories changed:\nwant %s\n got %s", a, b)
	}
}

// Experiment pairs carry no routing score, so they stay out of the call list the
// threshold and routing-lift detectors read.
func TestAnalyzeKeepsExperimentSamplesOutOfTheScoreDetectors(t *testing.T) {
	sel := compositeSelector()
	sel.CapabilityRouting = map[string]string{"capable": strong, "efficient": weak}
	shadow := Observations{Samples: batch("a", stageC, 40, 5, 5, 0)}
	with := Observations{Samples: append(slices.Clone(shadow.Samples), experimentBatch("a", stageC, 40, 30, 0)...)}
	want := Analyze(shadow, clusterOf(sel, "a"), window, Options{})
	got := Analyze(with, clusterOf(sel, "a"), window, Options{})
	for _, f := range got {
		if f.Detector == DetectorThreshold || f.Detector == DetectorStrategySwitch {
			if !slices.ContainsFunc(want, func(w Finding) bool { return w.Detector == f.Detector }) {
				t.Errorf("an experiment-only signal reached %s: %+v", f.Detector, f)
			}
		}
	}
}
