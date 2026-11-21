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
	"strings"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/schedule"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// AnnotationInvokerFrom marks a child run whose created-by was copied from its
// parent by the operator (never taken from a request body). Its value is the
// parent run's name. It is what lets an agent-as-tool call keep the ORIGINAL
// invoker's connection: without it a child run has no invoker.
const AnnotationInvokerFrom = "agentorca.io/invoker-from-run"

// nonUserActorPrefixes start a created-by value that is not a person: a
// schedule, an external trigger, a chat widget key, a machine caller (service
// account or client credentials, see actorOf), the unauthenticated actor.
var nonUserActorPrefixes = []string{
	schedule.ActorPrefix, TriggerActorPrefix, WidgetActorPrefix, "machine:", anonymousActor,
}

// Run labels of runs that are never started by a person even if something
// stamped a user on them: workflow steps, external-API tasks, evaluation arms.
var nonUserRunLabels = []string{
	"agentorca.io/workflow", "agentorca.io/external-task", tenancy.LabelEvalArm,
}

// invokerOf is the user whose connection an invoker-mode server uses for run: the
// run's created-by annotation, read from the object, and only when it names a
// real UI user. The second result is why not, for the log; empty when ok. It
// never falls back to any other identity.
func invokerOf(run *agentorcav1alpha1.AgentRun) (user, why string) {
	by := run.Annotations[AnnotationCreatedBy]
	if by == "" {
		return "", "the run has no invoker (created-by is empty)"
	}
	for _, p := range nonUserActorPrefixes {
		if strings.HasPrefix(by, p) {
			return "", "the run was not started by a person"
		}
	}
	for _, l := range nonUserRunLabels {
		if _, ok := run.Labels[l]; ok {
			return "", "the run is a workflow step, an external task or an evaluation run"
		}
	}
	if run.Spec.ParentRunRef != "" && run.Annotations[AnnotationInvokerFrom] != run.Spec.ParentRunRef {
		return "", "a child run has no invoker unless its parent's was propagated"
	}
	return by, ""
}

// propagateInvoker stamps child with its parent's invoker when parent has one.
// A created-by or propagation annotation the request carried is dropped first:
// the pod that asked for the child must not be able to name an identity.
func propagateInvoker(child, parent *agentorcav1alpha1.AgentRun) {
	delete(child.Annotations, AnnotationCreatedBy)
	delete(child.Annotations, AnnotationInvokerFrom)
	if parent == nil {
		return
	}
	user, _ := invokerOf(parent)
	if user == "" {
		return
	}
	if child.Annotations == nil {
		child.Annotations = map[string]string{}
	}
	child.Annotations[AnnotationCreatedBy] = user
	child.Annotations[AnnotationInvokerFrom] = parent.Name
}
