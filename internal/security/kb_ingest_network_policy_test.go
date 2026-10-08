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

package security

import (
	"testing"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

func TestBuildIngestNetworkPolicy_Basic(t *testing.T) {
	np := BuildIngestNetworkPolicy("my-kb", "default", nil)

	if np.Name != "agentorca-kb-ingest-my-kb" {
		t.Errorf("unexpected name: %s", np.Name)
	}
	if np.Namespace != "default" {
		t.Errorf("unexpected namespace: %s", np.Namespace)
	}
	if np.Spec.PodSelector.MatchLabels[LabelKBIngest] != "my-kb" {
		t.Errorf("pod selector: got %q want %q",
			np.Spec.PodSelector.MatchLabels[LabelKBIngest], "my-kb")
	}

	// Ingress must be selected but empty — ingestion Jobs never accept
	// inbound connections.
	hasIngressType, hasEgressType := false, false
	for _, pt := range np.Spec.PolicyTypes {
		switch pt {
		case "Ingress":
			hasIngressType = true
		case "Egress":
			hasEgressType = true
		}
	}
	if !hasIngressType || !hasEgressType {
		t.Errorf("expected Ingress and Egress policy types, got %v", np.Spec.PolicyTypes)
	}
	if len(np.Spec.Ingress) != 0 {
		t.Errorf("expected no ingress rules, got %d", len(np.Spec.Ingress))
	}

	// 443 + 6443 (K8s API) + 80 (HTTP) + same-namespace + DNS UDP + DNS TCP = 6.
	if len(np.Spec.Egress) != 6 {
		t.Fatalf("expected 6 egress rules, got %d", len(np.Spec.Egress))
	}

	// The same-namespace rule selects all pods in the namespace on any port.
	foundSameNS := false
	for _, rule := range np.Spec.Egress {
		if len(rule.To) == 1 && rule.To[0].PodSelector != nil &&
			len(rule.To[0].PodSelector.MatchLabels) == 0 {
			foundSameNS = true
			if len(rule.Ports) != 0 {
				t.Errorf("same-namespace rule should allow all ports, got %v", rule.Ports)
			}
		}
	}
	if !foundSameNS {
		t.Error("missing same-namespace egress rule")
	}
}

func TestBuildIngestNetworkPolicy_ExtraEgress(t *testing.T) {
	extra := []agentorcav1alpha1.EgressRule{
		{Host: "minio.internal", Port: 9000, Protocol: "TCP"},
	}
	np := BuildIngestNetworkPolicy("s3-kb", "default", extra)

	// 6 base rules + 1 extra = 7.
	if len(np.Spec.Egress) != 7 {
		t.Fatalf("expected 7 egress rules, got %d", len(np.Spec.Egress))
	}

	foundExtra := false
	for _, rule := range np.Spec.Egress {
		for _, port := range rule.Ports {
			if port.Port != nil && port.Port.IntVal == 9000 {
				foundExtra = true
			}
		}
	}
	if !foundExtra {
		t.Error("extra egress rule on port 9000 not found")
	}
}
