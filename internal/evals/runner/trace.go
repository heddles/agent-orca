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

package runner

import (
	"context"
	"encoding/json"
	"time"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/evals/graders"
	"github.com/heddles/agent-orca/internal/postgresql"
)

// traceGrace is how long after a run ends the runner waits for the run's trace
// to reach the archive before it grades the run without one. The operator
// archives a run (with the trace read from the state store) soon after it ends,
// so the wait is normally one tick.
const traceGrace = 2 * time.Minute

// traceEventToolCall is the type of the trace event the model-router writes when
// the agent calls a tool (internal/router dispatchToolCall).
const traceEventToolCall = "toolCall"

// runTrace is what the archive says the run did.
type runTrace struct {
	// Known is true when the archive held a trace, so ToolCalls is the complete
	// list (possibly empty), not a guess.
	Known     bool
	ToolCalls []string
}

// toolCallNames lists, in order and repeats kept, the tools the run's own agent
// called, from the archived trace (a JSON array of {id, event, ts, childRunName}).
// Calls made inside a child run (agent as tool) belong to the child and are not
// listed. A trace that cannot be read is not known.
func toolCallNames(raw []byte) (names []string, known bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var entries []struct {
		Event        json.RawMessage `json:"event"`
		ChildRunName string          `json:"childRunName"`
	}
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, false
	}
	names = []string{}
	for _, e := range entries {
		if e.ChildRunName != "" {
			continue
		}
		var ev struct {
			Type string `json:"type"`
			Name string `json:"name"`
		}
		if json.Unmarshal(e.Event, &ev) != nil || ev.Type != traceEventToolCall || ev.Name == "" {
			continue
		}
		if len(names) >= postgresql.MaxEvalResultToolCalls {
			break
		}
		names = append(names, ev.Name)
	}
	return names, true
}

// traceOf reads the run's archived trace. A run that is not archived yet, or has
// no trace in its archive, is not known.
func (r *Runner) traceOf(ctx context.Context, run *agentorcav1alpha1.AgentRun) (runTrace, error) {
	arch, err := r.store.GetRun(ctx, run.Namespace, run.Name)
	if err != nil {
		return runTrace{}, err
	}
	if arch == nil {
		return runTrace{}, nil
	}
	names, known := toolCallNames(arch.TraceEventsJSON)
	return runTrace{Known: known, ToolCalls: names}, nil
}

// traceExpired reports whether the grace for a run's trace has run out.
func (r *Runner) traceExpired(run *agentorcav1alpha1.AgentRun) bool {
	if run.Status.CompletionTime == nil {
		return true
	}
	return r.cfg.Now().Sub(run.Status.CompletionTime.Time) > traceGrace
}

// needsTrace reports whether a grader reads the trace of a run.
func needsTrace(gs []postgresql.EvalGrader) bool {
	for _, g := range gs {
		if g.Kind == graders.KindToolCalled || g.Kind == graders.KindToolNotCalled {
			return true
		}
	}
	return false
}

// traceGrade is the verdict of a trace grader whose trace is not known: an error
// grade, never a pass (a "not called" grader would otherwise pass on nothing).
func traceGrade(g postgresql.EvalGrader) postgresql.EvalGrade {
	return postgresql.EvalGrade{GraderID: g.ID, Status: postgresql.GradeError,
		Detail: "the run's trace was not archived, so the tools it called are not known"}
}
