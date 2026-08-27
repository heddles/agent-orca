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
)

func TestSanitizeKBError(t *testing.T) {
	tests := []struct {
		name string
		err  string
		want string
	}{
		{"empty", "", ""},
		{
			"DNS resolution failure (no such host)",
			`dial tcp: lookup ollama-embed.agent-orca-system.svc on 10.96.0.10:53: no such host`,
			"embedding model provider is unreachable (DNS resolution failed)",
		},
		{
			"DNS resolution failure (name resolution failure)",
			`name resolution failure`,
			"embedding model provider is unreachable (DNS resolution failed)",
		},
		{
			"DNS resolution failure (cannot resolve)",
			`cannot resolve host`,
			"embedding model provider is unreachable (DNS resolution failed)",
		},
		{
			"connection refused",
			`dial tcp: connect: connection refused`,
			"embedding model provider is unreachable (connection refused)",
		},
		{
			"i/o timeout",
			`dial tcp: i/o timeout`,
			"embedding model provider is unreachable (timed out)",
		},
		{
			"deadline exceeded",
			`context deadline exceeded`,
			"embedding model provider is unreachable (timed out)",
		},
		{
			"HTTP 401",
			`HTTP 401 Unauthorized`,
			"embedding model provider authentication failed",
		},
		{
			"HTTP 403",
			`HTTP 403 Forbidden`,
			"embedding model provider access denied",
		},
		{
			"HTTP 404",
			`HTTP 404 Not Found`,
			"embedding model provider endpoint not found",
		},
		{
			"HTTP 500",
			`HTTP 500 Internal Server Error`,
			"embedding model provider returned a server error",
		},
		{
			"forbidden",
			`pods is forbidden`,
			"access denied: operator lacks permissions to read a required secret or resource",
		},
		{
			"not found",
			`secret not found`,
			"a required Kubernetes resource was not found",
		},
		{
			"fallback strips IPs",
			`error at 10.96.0.10:53`,
			"error at <host>",
		},
		{
			"fallback strips internal DNS (svc)",
			`error at ollama-embed.agent-orca-system.svc`,
			"error at <host>",
		},
		{
			"fallback strips internal DNS (svc.cluster.local)",
			`error at ollama-embed.agent-orca-system.svc.cluster.local`,
			"error at <host>",
		},
		{
			"fallback strips quotes",
			`some "quoted" error`,
			"some quoted error",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeKBError(tt.err)
			if got != tt.want {
				t.Errorf("sanitizeKBError(%q) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}

func TestSanitizeKBErrorPreservesOperationContext(t *testing.T) {
	// Verify the call-site pattern: operation prefix + sanitized error
	rawErr := `dial tcp: lookup ollama-embed.agent-orca-system.svc on 10.96.0.10:53: no such host`
	sanitized := sanitizeKBError(rawErr)
	result := "discovering embedding dimension: " + sanitized
	want := "discovering embedding dimension: embedding model provider is unreachable (DNS resolution failed)"
	if result != want {
		t.Errorf("combined message = %q, want %q", result, want)
	}
}

func TestFirstConditionMessage(t *testing.T) {
	tests := []struct {
		name       string
		conditions []metav1.Condition
		want       string
	}{
		{"nil conditions", nil, ""},
		{"empty", []metav1.Condition{}, ""},
		{
			"ready false returns message",
			[]metav1.Condition{{
				Type:    "Ready",
				Status:  metav1.ConditionFalse,
				Message: "Qdrant pod failed to start",
			}},
			"Qdrant pod failed to start",
		},
		{
			"qdrant upgrading false (UpToDate) is NOT returned",
			[]metav1.Condition{
				{Type: "QdrantUpgrading", Status: metav1.ConditionFalse, Reason: "UpToDate", Message: "Qdrant is at target version 1.17.1"},
			},
			"",
		},
		{
			"ready false takes priority over upgrade condition",
			[]metav1.Condition{
				{Type: "QdrantUpgrading", Status: metav1.ConditionFalse, Reason: "UpToDate", Message: "Qdrant is at target version 1.17.1"},
				{Type: "Ready", Status: metav1.ConditionFalse, Reason: "NotReady", Message: "Qdrant pod crashed"},
			},
			"Qdrant pod crashed",
		},
		{
			"ready true returns empty",
			[]metav1.Condition{{
				Type:   "Ready",
				Status: metav1.ConditionTrue,
			}},
			"",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := firstConditionMessage(tt.conditions)
			if got != tt.want {
				t.Errorf("firstConditionMessage() = %q, want %q", got, tt.want)
			}
		})
	}
}
