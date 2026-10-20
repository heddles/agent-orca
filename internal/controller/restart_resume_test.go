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

func TestResumeCheckpointKey(t *testing.T) {
	own := "agentorca/runs/run-1/state"
	cases := []struct {
		name     string
		prior    string
		ckptRef  string
		restarts int
		want     string
	}{
		{"fresh run", "", "", 0, ""},
		{"first start records ref but has nothing to resume", "", own, 0, ""},
		{"chat turn resumes the prior run", "prev", own, 0, "agentorca/runs/prev/state"},
		{"restart resumes its own checkpoint", "", own, 1, own},
		{"restarted chat turn resumes its own progress, not the prior run", "prev", own, 2, own},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run := &agentorcav1alpha1.AgentRun{
				ObjectMeta: metav1.ObjectMeta{Name: "run-1"},
				Spec:       agentorcav1alpha1.AgentRunSpec{PriorRunRef: tc.prior},
				Status:     agentorcav1alpha1.AgentRunStatus{CheckpointRef: tc.ckptRef, RestartCount: tc.restarts},
			}
			if got := resumeCheckpointKey(run); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// Regression for #37: the warm-pod claim payload must carry the same resume
// key as the cold-start router config, so a restarted run that re-claims a
// warm pod resumes its own checkpoint instead of replaying from its input.
func TestWarmRunInput_RestartCarriesOwnCheckpoint(t *testing.T) {
	run := &agentorcav1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: "acp-senior-programmer-df66x"},
		Spec:       agentorcav1alpha1.AgentRunSpec{Input: "Show me the open PRs", PriorRunRef: "acp-senior-programmer-4kk8x"},
		Status: agentorcav1alpha1.AgentRunStatus{
			CheckpointRef: "agentorca/runs/acp-senior-programmer-df66x/state",
			RestartCount:  1,
		},
	}
	in := warmRunInput(run)
	if !in.Restart {
		t.Fatal("a restarted run's claim must carry the explicit Restart flag")
	}
	if in.ResumeCheckpointKey != run.Status.CheckpointRef {
		t.Fatalf("restart claim must resume the run's own checkpoint, got %q", in.ResumeCheckpointKey)
	}
	if in.RunName != run.Name || in.Input != run.Spec.Input {
		t.Fatalf("identity/input not carried: %+v", in)
	}
	// PriorRunRef stays set for routers that predate ResumeCheckpointKey.
	if in.PriorRunRef != run.Spec.PriorRunRef {
		t.Fatalf("PriorRunRef must be kept for older routers, got %q", in.PriorRunRef)
	}

	run.Status.RestartCount = 0
	if warmRunInput(run).Restart {
		t.Fatal("a first claim is not a restart")
	}
	if got := warmRunInput(run).ResumeCheckpointKey; got != "agentorca/runs/acp-senior-programmer-4kk8x/state" {
		t.Fatalf("first claim of a chat turn must resume the prior run, got %q", got)
	}
}

func TestFallbackCheckpointKey(t *testing.T) {
	run := &agentorcav1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1"},
		Spec:       agentorcav1alpha1.AgentRunSpec{PriorRunRef: "prev"},
	}
	if got := fallbackCheckpointKey(run); got != "" {
		t.Fatalf("first start resumes the prior run directly; want no fallback, got %q", got)
	}
	run.Status.RestartCount = 1
	if got := fallbackCheckpointKey(run); got != "agentorca/runs/prev/state" {
		t.Fatalf("restarted chat turn must fall back to the prior run, got %q", got)
	}
	run.Spec.PriorRunRef = ""
	if got := fallbackCheckpointKey(run); got != "" {
		t.Fatalf("no prior run, no fallback; got %q", got)
	}
}
