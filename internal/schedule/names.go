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

package schedule

import (
	"fmt"
	"strings"
	"time"
)

// What a scheduled run carries. The label agentorca.io/trigger is taken (it
// holds the ID of a webhook trigger), so a scheduled run is marked by the
// schedule label alone.
const (
	// LabelSchedule names the AgentSchedule that created a run or workflow.
	LabelSchedule = "agentorca.io/schedule"
	// AnnotationCreatedBy is who started a run. For a scheduled run it is
	// ActorPrefix plus namespace/name: a schedule has no invoker.
	AnnotationCreatedBy = "agentorca.io/created-by"
	// ActorPrefix starts the created-by value of a scheduled run.
	ActorPrefix = "schedule:"
	// AnnotationScheduledFor is the tick, in the schedule's zone, a run was
	// created for.
	AnnotationScheduledFor = "agentorca.io/scheduled-for"
	// AnnotationAccounted marks a finished child the schedule has counted
	// (failures, spend), so pruning it later loses nothing.
	AnnotationAccounted = "agentorca.io/schedule-accounted"
	// AnnotationResumedAt is set (RFC 3339) by the API when a person resumes a
	// Degraded schedule; the controller clears the breaker when it is newer than
	// the moment the schedule degraded.
	AnnotationResumedAt = "agentorca.io/schedule-resumed-at"

	// SystemUser is the audit user ID of a change the schedule controller makes
	// on its own.
	SystemUser = "system:agentschedule"

	// MaxNameLength is the longest AgentSchedule name: the label value limit is 63
	// and a run is named <schedule>-<unix seconds>.
	MaxNameLength = 52
)

// Defaults of spec fields left unset.
const (
	DefaultStartingDeadline = time.Hour
	DefaultHistoryLimit     = 7
	// DefaultMinInterval is the shortest gap between ticks a schedule may have.
	DefaultMinInterval = 5 * time.Minute
	// DefaultMaxConsecutiveFailures stops a schedule whose runs keep failing.
	DefaultMaxConsecutiveFailures = 5
)

// Actor is the created-by value of a run the schedule namespace/name created.
func Actor(namespace, name string) string { return ActorPrefix + namespace + "/" + name }

// RunName is the deterministic name of the run for a tick. The tick is the
// scheduled instant, not the time the run was created, so a repeated reconcile
// or a second operator replica creates the same object and the second create
// fails with AlreadyExists instead of firing twice.
func RunName(schedule string, tick time.Time) string {
	return fmt.Sprintf("%s-%d", schedule, tick.Unix())
}

// Substitute replaces {{schedule.time}}, {{schedule.date}} and {{schedule.name}}
// in s. Nothing else is interpreted, and the replacement is a single pass over
// the original text, so a value cannot introduce another placeholder.
func Substitute(s, name string, tick time.Time) string {
	if !strings.Contains(s, "{{") {
		return s
	}
	var pairs []string
	for _, form := range []string{"{{schedule.time}}", "{{ schedule.time }}"} {
		pairs = append(pairs, form, tick.Format(time.RFC3339))
	}
	for _, form := range []string{"{{schedule.date}}", "{{ schedule.date }}"} {
		pairs = append(pairs, form, tick.Format(time.DateOnly))
	}
	for _, form := range []string{"{{schedule.name}}", "{{ schedule.name }}"} {
		pairs = append(pairs, form, name)
	}
	return strings.NewReplacer(pairs...).Replace(s)
}
