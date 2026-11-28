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

package dispatch

import (
	"context"
	"log/slog"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/manager"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/dispatch/decision"
	"github.com/heddles/agent-orca/internal/tenancy"
)

const (
	warmInterval = 30 * time.Second
	// warmState is what the warming question is about.
	warmState = "warm-up"
	// retention is how long a decision record is kept.
	retention     = 90 * 24 * time.Hour
	pruneInterval = 24 * time.Hour
)

// Pruner deletes old decision records. A *postgresql.Store with the platform
// scope is one.
type Pruner interface {
	PruneDispatchDecisions(ctx context.Context, before time.Time) (int64, error)
}

// Warm asks the local decider about each tenant's agents, so that it has
// prepared them before a real request arrives: preparing a new set can take
// longer than a request waits. The decider keeps what it prepared, so asking
// again costs little, and it also covers a restarted decider. Only the local
// decider is asked: nothing is sent out of the cluster and nothing is recorded.
//
// What is warmed is the tenant's whole set. A request narrowed further, as from
// a chat channel, is prepared when it first arrives.
func (d *Dispatcher) Warm(ctx context.Context) {
	if d.Local == nil {
		return
	}
	var list agentorcav1alpha1.TenantConfigList
	if err := d.Client.List(ctx, &list); err != nil {
		slog.Warn("dispatch: warming, listing tenants", "err", err)
		return
	}
	for i := range list.Items {
		tc := &list.Items[i]
		set := settingsOf(tc)
		id := tenancy.TenantIDOf(tc)
		if id == "" || !set.enabled || set.decider != agentorcav1alpha1.DispatchDeciderLocal || ctx.Err() != nil {
			continue
		}
		pool, err := d.candidates(ctx, Request{Tenant: Tenant{
			ID: id, Name: tc.Name, Namespaces: tc.Spec.AllowedNamespaces, AllowedAgents: tc.Spec.AllowedAgents,
		}})
		if err != nil || len(pool.routable) < 2 || len(pool.routable) > decision.MaxOptions {
			continue
		}
		question, err := d.question(ctx, pool)
		if err != nil {
			slog.Warn("dispatch: warming, describing agents", "tenant", id, "err", err)
			continue
		}
		// A timeout is expected for a new set: preparing it carries on.
		if _, err := d.Local.Decide(ctx, decision.Request{
			Tenant: id, State: warmState, Questions: map[string]decision.Question{questionID: question},
		}); err != nil {
			slog.Debug("dispatch: warming", "tenant", id, "err", err)
		}
	}
}

// Upkeep is the manager runnable that keeps dispatch ready: it warms the local
// decider and drops old records. It wants leader election, the default, so one
// operator does it.
func (d *Dispatcher) Upkeep(pruner Pruner) manager.Runnable {
	return manager.RunnableFunc(func(ctx context.Context) error {
		tick := time.NewTicker(warmInterval)
		defer tick.Stop()
		var pruned time.Time
		for {
			d.Warm(ctx)
			if pruner != nil && d.now().Sub(pruned) >= pruneInterval {
				if n, err := pruner.PruneDispatchDecisions(ctx, d.now().Add(-retention)); err != nil {
					slog.Warn("dispatch: pruning old decisions", "err", err)
				} else {
					pruned = d.now()
					if n > 0 {
						slog.Info("dispatch: pruned old decisions", "count", n)
					}
				}
			}
			select {
			case <-ctx.Done():
				return nil
			case <-tick.C:
			}
		}
	})
}
