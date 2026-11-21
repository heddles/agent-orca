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
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/connections"
	"github.com/heddles/agent-orca/internal/schedule"
	"github.com/heddles/agent-orca/internal/security"
	"github.com/heddles/agent-orca/internal/tenancy"
)

const (
	parentRunName = "parent"
	tokServerName = "github"
)

type tokMut = func(*agentorcav1alpha1.Agent, *agentorcav1alpha1.AgentRun, *agentorcav1alpha1.MCPServer)

// invoker makes the fixture's server an invoker-mode server, runs created by who,
// and the agent unpublished (so a fallback to the maintainer would show).
func invoker(who string, more ...tokMut) []tokMut {
	return append([]tokMut{func(_ *agentorcav1alpha1.Agent, r *agentorcav1alpha1.AgentRun, s *agentorcav1alpha1.MCPServer) {
		s.Spec.Auth.OAuth.ConnectionMode = agentorcav1alpha1.MCPConnectionInvoker
		if who != "" {
			r.Annotations = map[string]string{AnnotationCreatedBy: who}
		}
	}}, more...)
}

func TestMCPTokenInvoker_UsesTheRunsCreatedByNotTheMaintainerNorTheBody(t *testing.T) {
	f := newTokFixture(t, invoker(connTestUser)...)
	rec := f.call(tokCaller, `{"server":"github","user":"mallory","userId":"mallory","createdBy":"mallory"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["identity"] != tokMaintainer+" (invoker)" { // the fake broker's account label
		t.Errorf("identity = %v", out["identity"])
	}
	if len(f.broker.reqs) != 1 || f.broker.reqs[0].UserID != connTestUser || f.broker.reqs[0].TenantID != isoA {
		t.Errorf("request = %+v; want alice, never the maintainer (%s) or mallory", f.broker.reqs, tokMaintainer)
	}
}

func TestMCPTokenInvoker_FailsClosedWithConnectionRequired(t *testing.T) {
	notAUser := func(l map[string]string) tokMut {
		return func(_ *agentorcav1alpha1.Agent, r *agentorcav1alpha1.AgentRun, _ *agentorcav1alpha1.MCPServer) {
			maps.Copy(r.Labels, l)
		}
	}
	child := func(annotation string) tokMut {
		return func(_ *agentorcav1alpha1.Agent, r *agentorcav1alpha1.AgentRun, _ *agentorcav1alpha1.MCPServer) {
			r.Spec.ParentRunRef = parentRunName
			if annotation != "" {
				r.Annotations[AnnotationInvokerFrom] = annotation
			}
		}
	}
	for _, c := range []struct {
		name string
		by   string
		more tokMut
	}{
		{"missing created-by", "", nil},
		{"a scheduled run", schedule.Actor("team-a", "nightly"), nil},
		{"a trigger run", TriggerActorPrefix + "trg_1", nil},
		{"a widget run", WidgetActorPrefix + "key_1", nil},
		{"a machine caller", "machine:acme", nil},
		{"an unauthenticated actor", anonymousActor, nil},
		{"an external API task", connTestUser, notAUser(map[string]string{"agentorca.io/external-task": "true"})},
		{"a workflow step", connTestUser, notAUser(map[string]string{"agentorca.io/workflow": "wf", "agentorca.io/workflow-step": "s1"})},
		{"an evaluation run", connTestUser, notAUser(map[string]string{tenancy.LabelEvalArm: "a"})},
		{"a child run with no propagation", connTestUser, child("")},
		{"a child run propagated from some other run", connTestUser, child("someone-else")},
	} {
		t.Run(c.name, func(t *testing.T) {
			muts := invoker(c.by)
			if c.more != nil {
				muts = append(muts, c.more)
			}
			f := newTokFixture(t, muts...)
			rec := f.call(tokCaller, `{"server":"github"}`)
			if rec.Code != http.StatusConflict || tokError(t, rec) != connections.CodeRequired {
				t.Fatalf("status %d code %q: %s", rec.Code, tokError(t, rec), rec.Body)
			}
			if len(f.broker.reqs) != 0 || strings.Contains(rec.Body.String(), tokAccess) {
				t.Errorf("no connection may be used: %+v", f.broker.reqs)
			}
		})
	}
}

func TestMCPTokenInvoker_AnotherUsersConnectionIsNeverUsed(t *testing.T) {
	// Bob started the run; only Bob's row may be asked for, whoever else is connected.
	f := newTokFixture(t, invoker(connOtherUser)...)
	f.call(tokCaller, `{"server":"github"}`)
	for _, r := range f.broker.reqs {
		if r.UserID != connOtherUser {
			t.Errorf("asked for %q", r.UserID)
		}
	}
	// With no connection of Bob's the broker says so, and that is the answer: no retry as anyone else.
	f = newTokFixture(t, invoker(connOtherUser)...)
	f.broker.err = &connections.TokenError{Code: connections.CodeRequired, Message: "the account has not been connected"}
	rec := f.call(tokCaller, `{"server":"github"}`)
	if rec.Code != http.StatusConflict || tokError(t, rec) != connections.CodeRequired || len(f.broker.reqs) != 1 {
		t.Errorf("status %d code %q calls %d", rec.Code, tokError(t, rec), len(f.broker.reqs))
	}
}

func TestMCPTokenInvoker_KeepsTheExistingChecks(t *testing.T) {
	for name, m := range map[string]tokMut{
		"another agent's pod": nil,
		"allowedAgents": func(_ *agentorcav1alpha1.Agent, _ *agentorcav1alpha1.AgentRun, s *agentorcav1alpha1.MCPServer) {
			s.Spec.AllowedAgents = []string{"someone-else"}
		},
		"agent uses server": func(a *agentorcav1alpha1.Agent, _ *agentorcav1alpha1.AgentRun, _ *agentorcav1alpha1.MCPServer) {
			a.Spec.Tools = nil
		},
		"terminal run": func(_ *agentorcav1alpha1.Agent, r *agentorcav1alpha1.AgentRun, _ *agentorcav1alpha1.MCPServer) {
			r.Status.Phase = agentorcav1alpha1.AgentRunPhaseFailed
		},
		"tenant": func(_ *agentorcav1alpha1.Agent, r *agentorcav1alpha1.AgentRun, _ *agentorcav1alpha1.MCPServer) {
			r.Labels[tenancy.LabelTenantID] = isoB
		},
	} {
		t.Run(name, func(t *testing.T) {
			muts := invoker(connTestUser)
			caller := tokCaller
			if m == nil {
				caller = "system:serviceaccount:" + tokNS + ":" + security.AgentSAName("someone-else")
			} else {
				muts = append(muts, m)
			}
			f := newTokFixture(t, muts...)
			if rec := f.call(caller, `{"server":"github"}`); rec.Code == http.StatusOK || len(f.broker.reqs) != 0 {
				t.Errorf("status %d, broker calls %d", rec.Code, len(f.broker.reqs))
			}
		})
	}
}

// A propagated child keeps the original invoker, and the callee's own checks apply.
func TestMCPTokenInvoker_AgentAsToolKeepsTheOriginalInvoker(t *testing.T) {
	f := newTokFixture(t, invoker(connTestUser, func(_ *agentorcav1alpha1.Agent, r *agentorcav1alpha1.AgentRun, _ *agentorcav1alpha1.MCPServer) {
		r.Spec.ParentRunRef = parentRunName
		r.Annotations[AnnotationInvokerFrom] = parentRunName
	})...)
	rec := f.call(tokCaller, `{"server":"github"}`)
	if rec.Code != http.StatusOK || len(f.broker.reqs) != 1 || f.broker.reqs[0].UserID != connTestUser {
		t.Fatalf("status %d: %s %+v", rec.Code, rec.Body, f.broker.reqs)
	}
	// The callee's own allowedAgents still applies.
	f = newTokFixture(t, invoker(connTestUser, func(_ *agentorcav1alpha1.Agent, r *agentorcav1alpha1.AgentRun, s *agentorcav1alpha1.MCPServer) {
		r.Spec.ParentRunRef = parentRunName
		r.Annotations[AnnotationInvokerFrom] = parentRunName
		s.Spec.AllowedAgents = []string{"the-caller-only"}
	})...)
	if rec := f.call(tokCaller, `{"server":"github"}`); rec.Code != http.StatusForbidden || len(f.broker.reqs) != 0 {
		t.Errorf("callee not in allowedAgents: status %d", rec.Code)
	}
}

func TestInvokerOf(t *testing.T) {
	run := func(by string) *agentorcav1alpha1.AgentRun {
		return &agentorcav1alpha1.AgentRun{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{AnnotationCreatedBy: by}}}
	}
	if u, _ := invokerOf(run("alice@example.com")); u != "alice@example.com" {
		t.Errorf("a person = %q", u)
	}
	if u, why := invokerOf(run("schedule:ns/x")); u != "" || why == "" {
		t.Errorf("schedule = %q, %q", u, why)
	}
}

// Propagation: createAgentRun copies the parent's invoker only for the parent's own
// agent's pod, drops anything the body claimed, and never copies a non-user.
func TestCreateAgentRun_PropagatesTheInvokerToChildrenExplicitly(t *testing.T) {
	parentRun := func(by string) tokMut {
		return func(_ *agentorcav1alpha1.Agent, r *agentorcav1alpha1.AgentRun, _ *agentorcav1alpha1.MCPServer) {
			if by == "" {
				r.Annotations = nil
				return
			}
			r.Annotations = map[string]string{AnnotationCreatedBy: by}
		}
	}
	create := func(f *tokFixture, caller, body string) client.ObjectKey {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/agentrun/team-a", strings.NewReader(body))
		f.s.createAgentRun(rec, req, caller, tokNS)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status %d: %s", rec.Code, rec.Body)
		}
		var out agentorcav1alpha1.AgentRun
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return client.ObjectKey{Namespace: tokNS, Name: out.Name}
	}
	get := func(f *tokFixture, k client.ObjectKey) *agentorcav1alpha1.AgentRun {
		var r agentorcav1alpha1.AgentRun
		if err := f.s.crdClient.Get(context.Background(), k, &r); err != nil {
			t.Fatal(err)
		}
		return &r
	}
	// run-1 (the fixture's run) is the parent.
	child := `{"metadata":{"name":"c1","annotations":{"agentorca.io/created-by":"mallory","agentorca.io/invoker-from-run":"run-1"}},"spec":{"agentRef":"reporter","input":"x","parentRunRef":"run-1"}}`

	f := newTokFixture(t, parentRun(connTestUser))
	c := get(f, create(f, tokCaller, child))
	if c.Annotations[AnnotationCreatedBy] != connTestUser || c.Annotations[AnnotationInvokerFrom] != routingTestRun {
		t.Errorf("child annotations = %v; want alice from run-1, not the body's mallory", c.Annotations)
	}
	if u, _ := invokerOf(c); u != connTestUser {
		t.Errorf("invokerOf(child) = %q", u)
	}

	// A pod that is not the parent's agent's cannot make a child inherit.
	other := "system:serviceaccount:" + tokNS + ":" + security.AgentSAName("someone-else")
	f = newTokFixture(t, parentRun(connTestUser))
	c = get(f, create(f, other, child))
	if c.Annotations[AnnotationCreatedBy] != "" || c.Annotations[AnnotationInvokerFrom] != "" {
		t.Errorf("foreign caller got annotations %v", c.Annotations)
	}

	// A parent with no person behind it (scheduled, no created-by) has nothing to propagate.
	for _, by := range []string{"", schedule.Actor("team-a", "n"), TriggerActorPrefix + "t"} {
		f = newTokFixture(t, parentRun(by))
		c = get(f, create(f, tokCaller, child))
		if c.Annotations[AnnotationCreatedBy] != "" {
			t.Errorf("parent %q: child annotations %v", by, c.Annotations)
		}
	}
}
