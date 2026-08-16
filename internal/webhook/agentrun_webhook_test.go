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

package webhook

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentorcv1alpha1 "github.com/floppyfish14/agent-orc/api/v1alpha1"
	"github.com/floppyfish14/agent-orc/internal/security"
)

func TestCheckRegistryAllowlist(t *testing.T) {
	tests := []struct {
		name              string
		ociRef            string
		allowedRegistries []string
		wantErr           bool
	}{
		{name: "empty allowlist allows anything", ociRef: "evil.io/malware:latest", allowedRegistries: nil, wantErr: false},
		{name: "exact match", ociRef: "gcr.io/my-project", allowedRegistries: []string{"gcr.io/my-project"}, wantErr: false},
		{name: "prefix with sub-path", ociRef: "gcr.io/my-project/image:v1", allowedRegistries: []string{"gcr.io/my-project"}, wantErr: false},
		{name: "bypass: prefix must not match across path boundary", ociRef: "gcr.io/my-evil-org/image:v1", allowedRegistries: []string{"gcr.io/my"}, wantErr: true},
		{name: "bypass: similar project name", ociRef: "gcr.io/my-project-evil/image:v1", allowedRegistries: []string{"gcr.io/my-project"}, wantErr: true},
		{name: "allowed prefix with trailing slash", ociRef: "gcr.io/my-project/image:v1", allowedRegistries: []string{"gcr.io/my-project/"}, wantErr: false},
		{name: "trailing slash does not match different prefix", ociRef: "gcr.io/my-project-evil/image:v1", allowedRegistries: []string{"gcr.io/my-project/"}, wantErr: true},
		{name: "registry-only prefix", ociRef: "docker.io/library/nginx:latest", allowedRegistries: []string{"docker.io"}, wantErr: false},
		{name: "no match among multiple registries", ociRef: "evil.io/malware:latest", allowedRegistries: []string{"gcr.io/my-project", "docker.io/library"}, wantErr: true},
		{name: "match second entry in list", ociRef: "docker.io/library/nginx:latest", allowedRegistries: []string{"gcr.io/my-project", "docker.io/library"}, wantErr: false},
		{name: "deeply nested image under allowed prefix", ociRef: "gcr.io/my-project/team/service/image:v2", allowedRegistries: []string{"gcr.io/my-project"}, wantErr: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkRegistryAllowlist(tt.ociRef, tt.allowedRegistries)
			if (err != nil) != tt.wantErr {
				t.Errorf("checkRegistryAllowlist(%q, %v) error = %v, wantErr %v",
					tt.ociRef, tt.allowedRegistries, err, tt.wantErr)
			}
		})
	}
}

// --- SecurityContextOverride gating tests ---

// baseAgent returns an Agent that passes the baseline (non-override) validation.
func baseAgent(ns string, withGuardrail bool) agentorcv1alpha1.Agent {
	a := agentorcv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "red-pwnbox", Namespace: ns},
		Spec: agentorcv1alpha1.AgentSpec{
			Runtime:          agentorcv1alpha1.AgentRuntime{OCIRef: "python:3.12-slim"},
			ModelSelectorRef: "poolside-strategist",
		},
	}
	if withGuardrail {
		a.Spec.GuardrailPolicyRef = "arena-bounds"
	}
	return a
}

func newValidator(nsName string, labelled bool) *AgentValidator {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}}
	if labelled {
		ns.Labels = map[string]string{
			security.LabelEnablePrivilegedPods: security.PrivilegedPodsAllowedValue,
		}
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ns).Build()
	return &AgentValidator{Client: cl}
}

func ptrTo[T any](v T) *T { return &v }

func TestAgentValidatorSecurityContextOverride(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name      string
		labelled  bool
		guardrail bool
		override  *agentorcv1alpha1.PodSecurityOverride
		wantErr   bool
		errSubstr string
	}{
		{name: "no override: allowed even without label/guardrail", labelled: false, guardrail: false, override: nil, wantErr: false},
		{name: "override without namespace label is denied", labelled: false, guardrail: true, override: &agentorcv1alpha1.PodSecurityOverride{Privileged: true}, wantErr: true, errSubstr: "not opted in"},
		{name: "override without guardrail policy is denied", labelled: true, guardrail: false, override: &agentorcv1alpha1.PodSecurityOverride{Privileged: true}, wantErr: true, errSubstr: "guardrailPolicyRef is empty"},
		{name: "privileged + addCapabilities both set is denied", labelled: true, guardrail: true, override: &agentorcv1alpha1.PodSecurityOverride{Privileged: true, AddCapabilities: []string{"NET_ADMIN"}}, wantErr: true, errSubstr: "mutually exclusive"},
		{name: "runAsUser=0 without privileged is denied", labelled: true, guardrail: true, override: &agentorcv1alpha1.PodSecurityOverride{AddCapabilities: []string{"NET_ADMIN"}, RunAsUser: ptrTo[int64](0)}, wantErr: true, errSubstr: "requires privileged"},
		{name: "labelled + guardrail + privileged is allowed", labelled: true, guardrail: true, override: &agentorcv1alpha1.PodSecurityOverride{Privileged: true}, wantErr: false},
		{name: "labelled + guardrail + NET_ADMIN cap is allowed", labelled: true, guardrail: true, override: &agentorcv1alpha1.PodSecurityOverride{AddCapabilities: []string{"NET_ADMIN"}}, wantErr: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := newValidator("red-team", tt.labelled)
			agent := baseAgent("red-team", tt.guardrail)
			agent.Spec.Runtime.SecurityContextOverride = tt.override

			err := v.validate(ctx, &agent)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validate() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr && tt.errSubstr != "" && !strings.Contains(err.Error(), tt.errSubstr) {
				t.Fatalf("expected error containing %q, got %q", tt.errSubstr, err)
			}
		})
	}
}

// namespace lookup failure path: pointing an override at a namespace the fake
// client does not know about returns an error (never silently allowed).
func TestAgentValidatorOverrideMissingNamespace(t *testing.T) {
	v := newValidator("red-team", true)
	agent := baseAgent("does-not-exist", true)
	agent.Spec.Runtime.SecurityContextOverride = &agentorcv1alpha1.PodSecurityOverride{Privileged: true}

	err := v.validate(context.Background(), &agent)
	if err == nil {
		t.Fatal("expected an error when namespace lookup fails, got nil")
	}
	if !strings.Contains(err.Error(), "namespace") {
		t.Fatalf("expected a namespace error, got %q", err)
	}
}
