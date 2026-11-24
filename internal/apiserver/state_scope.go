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
	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/runoutput"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/heddles/agent-orca/internal/state"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// runState returns the state store scoped to the tenant that owns obj (a run),
// the same view the controller and the model-router use for that run. Every
// API read or write of a run's Redis keys (token stream, checkpoint, answer,
// cancel flag, cached MCP apps) goes through it, so all three processes agree
// on the key: the tenant-prefixed one, with the old unprefixed key still read
// as a fallback (see state.ForTenant). It follows state.PrefixEnabled, so the
// escape hatch AGENTORCA_STATE_TENANT_PREFIX=false turns all of them off
// together.
func runState(store state.Store, obj client.Object) state.Store {
	return state.ForTenantIfEnabled(store, tenantIDOf(obj))
}

// tenantIDOf is the tenant ID on obj's agentorca.io/tenant-id label.
func tenantIDOf(obj client.Object) string {
	if obj == nil {
		return ""
	}
	return obj.GetLabels()[tenancy.LabelTenantID]
}

// outputOf is a run's answer. It is kept in the state store, not in the run's
// status (see runoutput); a run from before that, or one the store could not
// take, still has it in status.output. store may be nil.
func outputOf(ctx context.Context, store state.Store, run *agentorcav1alpha1.AgentRun) string {
	if store == nil {
		return run.Status.Output
	}
	return runoutput.Of(ctx, runState(store, run), run)
}

// scopedStore is runState that stays nil when there is no store.
func scopedStore(store state.Store, obj client.Object) state.Store {
	if store == nil {
		return nil
	}
	return runState(store, obj)
}
