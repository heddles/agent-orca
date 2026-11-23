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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// RevisionReader reads a saved revision of an Agent (postgresql.Store does).
type RevisionReader interface {
	GetRevision(ctx context.Context, q postgresql.RevisionQuery, n int) (postgresql.Revision, error)
}

// ArmError is a problem with an arm that the person who asked for the
// experiment can fix: an agent or revision that does not exist, or a hash that
// no longer matches its revision.
type ArmError struct{ Msg string }

func (e *ArmError) Error() string { return e.Msg }

// SpecHash is the content hash of an Agent spec: the SHA-256 of its canonical
// JSON, in hex. A revision and the live object hash the same way, so an arm
// that ran "the live agent" and one that ran its saved revision are comparable.
func SpecHash(spec agentorcav1alpha1.AgentSpec) (string, error) {
	b, err := json.Marshal(spec)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// ResolvedArm is an arm with the spec it runs and the hash of that spec.
type ResolvedArm struct {
	Arm  postgresql.EvalArm
	Spec agentorcav1alpha1.AgentSpec
}

// ResolveArms finds the spec each arm runs. An arm with a revision reads it from
// the revision history of the tenant (kind Agent) and, when the arm carries a
// hash, refuses it if the stored spec no longer hashes to that; an arm with no
// revision reads the live Agent and hashes it now. The returned arms have Hash
// set. An ArmError names what the caller got wrong.
func ResolveArms(ctx context.Context, kube client.Reader, revs RevisionReader, tenantID, namespace string, arms []postgresql.EvalArm) ([]ResolvedArm, error) {
	out := make([]ResolvedArm, 0, len(arms))
	for _, arm := range arms {
		var spec agentorcav1alpha1.AgentSpec
		if len(arm.Spec) > 0 {
			// A draft from the builder's check: the spec is the arm, and nothing
			// is read from the cluster. Its references are checked like any arm's.
			if err := json.Unmarshal(arm.Spec, &spec); err != nil {
				return nil, &ArmError{Msg: fmt.Sprintf("arm %q: the draft cannot be read: %v", arm.Label, err)}
			}
		} else if arm.Revision > 0 {
			if revs == nil {
				return nil, &ArmError{Msg: "revisions need the PostgreSQL store"}
			}
			rev, err := revs.GetRevision(ctx, postgresql.RevisionQuery{TenantID: tenantID, Kind: KindAgent, Namespace: namespace, Name: arm.Agent}, arm.Revision)
			if errors.Is(err, postgresql.ErrRevisionNotFound) {
				return nil, &ArmError{Msg: fmt.Sprintf("arm %q: agent %q has no revision %d", arm.Label, arm.Agent, arm.Revision)}
			}
			if err != nil {
				return nil, err
			}
			var a agentorcav1alpha1.Agent
			if err := json.Unmarshal(rev.Object, &a); err != nil {
				return nil, &ArmError{Msg: fmt.Sprintf("arm %q: revision %d of %q cannot be read: %v", arm.Label, arm.Revision, arm.Agent, err)}
			}
			if rev.Action == postgresql.AuditDelete {
				return nil, &ArmError{Msg: fmt.Sprintf("arm %q: revision %d of %q records a delete", arm.Label, arm.Revision, arm.Agent)}
			}
			spec = a.Spec
		} else {
			var live agentorcav1alpha1.Agent
			if err := kube.Get(ctx, client.ObjectKey{Namespace: namespace, Name: arm.Agent}, &live); err != nil {
				if k8serrors.IsNotFound(err) {
					return nil, &ArmError{Msg: fmt.Sprintf("arm %q: agent %q does not exist in namespace %s", arm.Label, arm.Agent, namespace)}
				}
				return nil, err
			}
			if live.Labels[tenancy.LabelTenantID] != "" && live.Labels[tenancy.LabelTenantID] != tenantID {
				return nil, &ArmError{Msg: fmt.Sprintf("arm %q: agent %q does not exist in namespace %s", arm.Label, arm.Agent, namespace)}
			}
			spec = live.Spec
		}
		h, err := SpecHash(spec)
		if err != nil {
			return nil, err
		}
		if arm.Hash != "" && arm.Hash != h {
			return nil, &ArmError{Msg: fmt.Sprintf("arm %q: agent %q changed since it was chosen (its hash is no longer %s)", arm.Label, arm.Agent, arm.Hash[:12])}
		}
		arm.Hash = h
		out = append(out, ResolvedArm{Arm: arm, Spec: spec})
	}
	return out, nil
}

// armObject is the hidden Agent an arm runs as: the resolved spec, under the
// name eval-<experiment>-<arm>, with a one-provider ModelSelector of the same
// name for a model arm. The copy is not the live agent, so it has no tools
// beyond what the spec names, and carries labels that hide it from listings.
func armObjects(e postgresql.EvalExperiment, tenantName string, a ResolvedArm) (*agentorcav1alpha1.Agent, *agentorcav1alpha1.ModelSelector) {
	name := agentNameOf(e.ID, a.Arm.Label)
	labels := objectLabels(e, tenantName)
	labels[tenancy.LabelEvalArm] = a.Arm.Label
	agent := &agentorcav1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: e.Namespace, Labels: labels,
			Annotations: map[string]string{tenancy.AnnotationOwner: tenancy.OwnerUI, "agentorca.io/eval-source": a.Arm.Agent},
		},
		Spec: *a.Spec.DeepCopy(),
	}
	var sel *agentorcav1alpha1.ModelSelector
	if a.Arm.ModelProvider != "" {
		agent.Spec.ModelSelectorRef = name
		sel = &agentorcav1alpha1.ModelSelector{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: e.Namespace, Labels: copyMap(labels),
				Annotations: map[string]string{tenancy.AnnotationOwner: tenancy.OwnerUI},
			},
			Spec: agentorcav1alpha1.ModelSelectorSpec{
				Strategy:  "rule-based",
				Providers: []agentorcav1alpha1.ProviderWeight{{Name: a.Arm.ModelProvider, Weight: 100}},
			},
		}
	}
	return agent, sel
}

func copyMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	maps.Copy(out, m)
	return out
}

// MissingArmRefs checks what the arms' agents name, and what a model arm's
// provider is, against the workspace: the "use a template" check. It returns
// every gap, each prefixed with the arm that has it.
func MissingArmRefs(ctx context.Context, kube client.Reader, e postgresql.EvalExperiment, arms []ResolvedArm, readable func(client.Object, string) bool) ([]ArmMissing, error) {
	var out []ArmMissing
	for _, a := range arms {
		agent, sel := armObjects(e, "", a)
		checker := &RefChecker{Reader: kube, Target: e.Namespace, Readable: readable}
		if sel != nil {
			// The selector is made by the runner; its one provider is checked below.
			checker.InSet = map[string]bool{KindModelSelector + "/" + sel.Name: true}
			gaps, err := checker.Check(ctx, sel)
			if err != nil {
				return nil, err
			}
			for _, g := range gaps {
				out = append(out, ArmMissing{Arm: a.Arm.Label, MissingRef: g})
			}
		}
		gaps, err := checker.Check(ctx, agent)
		if err != nil {
			return nil, err
		}
		for _, g := range gaps {
			out = append(out, ArmMissing{Arm: a.Arm.Label, MissingRef: g})
		}
	}
	return out, nil
}

// ArmMissing is a missing reference and the arm whose agent has it.
type ArmMissing struct {
	Arm string
	MissingRef
}

// Describe is the message of a missing reference.
func (m ArmMissing) Describe(namespace string) string {
	s := fmt.Sprintf("arm %q: %s %q does not exist in namespace %s", m.Arm, m.Kind, m.Name, namespace)
	if len(m.Alternatives) > 0 {
		s += " (it has " + strings.Join(m.Alternatives, ", ") + ")"
	}
	return s
}

// readableBy is the reference check's Readable for an experiment's tenant.
func readableBy(e postgresql.EvalExperiment, tenantName string) func(client.Object, string) bool {
	caller := tenancy.Caller{TenantID: e.TenantID, TenantName: tenantName, Namespaces: []string{e.Namespace}}
	return func(obj client.Object, kind string) bool {
		return tenancy.CheckAccess(caller, obj, kind) != tenancy.AccessNone
	}
}

// prepare gets an experiment ready to run: it checks the tenant's daily budget,
// resolves and checks the arms, and makes their objects. It returns the arms with
// their hashes. A problem the experiment cannot get past is an experimentError.
func (r *Runner) prepare(ctx context.Context, e postgresql.EvalExperiment, lim tenantLimits) ([]postgresql.EvalArm, error) {
	if lim.DailyUSD > 0 {
		used, err := r.todaySpend(ctx, e.TenantID)
		if err != nil {
			return nil, err
		}
		if left := lim.DailyUSD - used; left < e.BudgetUSD {
			return nil, failf("the tenant's daily budget has $%.4f left, less than the experiment's budget of $%.4f", max(left, 0), e.BudgetUSD)
		}
	}
	resolved, err := ResolveArms(ctx, r.kube, r.store, e.TenantID, e.Namespace, e.Arms)
	if err != nil {
		var ae *ArmError
		if errors.As(err, &ae) {
			return nil, failf("%s", ae.Msg)
		}
		return nil, err
	}
	gaps, err := MissingArmRefs(ctx, r.kube, e, resolved, readableBy(e, lim.Name))
	if err != nil {
		return nil, err
	}
	if len(gaps) > 0 {
		msgs := make([]string, 0, len(gaps))
		for _, g := range gaps {
			msgs = append(msgs, g.Describe(e.Namespace))
		}
		return nil, failf("%s", strings.Join(msgs, "; "))
	}
	arms := make([]postgresql.EvalArm, len(resolved))
	for i, a := range resolved {
		arms[i] = a.Arm
	}
	checked := e
	checked.Arms = arms
	if err := r.checkJudges(ctx, checked, lim); err != nil {
		return nil, err
	}
	if err := r.materialise(ctx, e, lim.Name, resolved); err != nil {
		return nil, err
	}
	return arms, nil
}

// materialise creates, or brings up to date, the Agent and ModelSelector of each arm.
func (r *Runner) materialise(ctx context.Context, e postgresql.EvalExperiment, tenantName string, arms []ResolvedArm) error {
	for _, a := range arms {
		agent, sel := armObjects(e, tenantName, a)
		if sel != nil {
			if err := r.upsert(ctx, e, KindModelSelector, sel, &agentorcav1alpha1.ModelSelector{}, func(cur client.Object) { cur.(*agentorcav1alpha1.ModelSelector).Spec = sel.Spec }); err != nil {
				return err
			}
		}
		if err := r.upsert(ctx, e, KindAgent, agent, &agentorcav1alpha1.Agent{}, func(cur client.Object) { cur.(*agentorcav1alpha1.Agent).Spec = agent.Spec }); err != nil {
			return err
		}
	}
	return nil
}

// upsert creates want, or updates the existing object of that name when it
// belongs to this experiment. An object of the same name that does not (a
// user's own eval-... agent) is a failure of the experiment, never overwritten.
func (r *Runner) upsert(ctx context.Context, e postgresql.EvalExperiment, kind string, want, cur client.Object, setSpec func(client.Object)) error {
	err := r.kube.Get(ctx, client.ObjectKeyFromObject(want), cur)
	if k8serrors.IsNotFound(err) {
		return r.kube.Create(ctx, want)
	}
	if err != nil {
		return err
	}
	if cur.GetLabels()[tenancy.LabelEvalExperiment] != tenancy.ExperimentLabel(e.ID) {
		return failf("%s %q already exists and is not part of this experiment", kind, want.GetName())
	}
	setSpec(cur)
	return r.kube.Update(ctx, cur)
}

// ensureArmObjects recreates an arm's objects that were deleted while the
// experiment ran. It reuses the hashes stored when the experiment started.
func (r *Runner) ensureArmObjects(ctx context.Context, e postgresql.EvalExperiment, tenantName string) error {
	for _, arm := range e.Arms {
		var a agentorcav1alpha1.Agent
		err := r.kube.Get(ctx, client.ObjectKey{Namespace: e.Namespace, Name: agentNameOf(e.ID, arm.Label)}, &a)
		if err == nil {
			continue
		}
		if !k8serrors.IsNotFound(err) {
			return err
		}
		resolved, rerr := ResolveArms(ctx, r.kube, r.store, e.TenantID, e.Namespace, []postgresql.EvalArm{arm})
		if rerr != nil {
			var ae *ArmError
			if errors.As(rerr, &ae) {
				return failf("%s", ae.Msg)
			}
			return rerr
		}
		if err := r.materialise(ctx, e, tenantName, resolved); err != nil {
			return err
		}
	}
	return nil
}
