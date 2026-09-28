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
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

// CloudProvider identifies the detected or configured cloud environment.
type CloudProvider string

const (
	CloudProviderGCP   CloudProvider = "gcp"
	CloudProviderAWS   CloudProvider = "aws"
	CloudProviderAzure CloudProvider = "azure"
	CloudProviderNone  CloudProvider = "none"
)

// Cloud provider SA annotation keys.
const (
	annotGKEWorkloadIdentity = "iam.gke.io/gcp-service-account"
	annotEKSRoleARN          = "eks.amazonaws.com/role-arn"
	annotAzureClientID       = "azure.workload.identity/client-id"

	labelAzureWorkloadIdentity = "azure.workload.identity/use"
)

// +kubebuilder:rbac:groups="",resources=nodes,verbs=list

// DetectCloudProvider inspects node labels to determine the active cloud provider.
// Returns CloudProviderNone if detection fails or if the cluster is on-premises.
func DetectCloudProvider(ctx context.Context, c client.Client) CloudProvider {
	logger := log.FromContext(ctx)

	var nodeList corev1.NodeList
	if err := c.List(ctx, &nodeList, &client.ListOptions{Limit: 1}); err != nil {
		logger.V(1).Info("cloud provider detection: could not list nodes", "err", err)
		return CloudProviderNone
	}
	if len(nodeList.Items) == 0 {
		return CloudProviderNone
	}

	labels := nodeList.Items[0].Labels
	switch {
	case labelExists(labels, "cloud.google.com/gke-nodepool"):
		logger.V(1).Info("detected cloud provider: GCP (GKE)")
		return CloudProviderGCP
	case labelExists(labels, "eks.amazonaws.com/nodegroup"):
		logger.V(1).Info("detected cloud provider: AWS (EKS)")
		return CloudProviderAWS
	case labelExists(labels, "kubernetes.azure.com/agentpool"):
		logger.V(1).Info("detected cloud provider: Azure (AKS)")
		return CloudProviderAzure
	default:
		logger.V(1).Info("cloud provider: none detected (on-prem or unknown)")
		return CloudProviderNone
	}
}

// ApplyCloudAuthAnnotations annotates the ServiceAccount with the cloud-provider-specific
// identity binding declared in the Agent's cloudAuth field.
// The caller is responsible for updating the SA via the Kubernetes API.
func ApplyCloudAuthAnnotations(sa *corev1.ServiceAccount, cloudAuth *agentorcav1alpha1.CloudAuthSpec, provider CloudProvider) {
	if cloudAuth == nil {
		return
	}
	if sa.Annotations == nil {
		sa.Annotations = make(map[string]string)
	}
	if sa.Labels == nil {
		sa.Labels = make(map[string]string)
	}

	switch provider {
	case CloudProviderGCP:
		if cloudAuth.GCP != nil && cloudAuth.GCP.ServiceAccount != "" {
			sa.Annotations[annotGKEWorkloadIdentity] = cloudAuth.GCP.ServiceAccount
		}
	case CloudProviderAWS:
		if cloudAuth.AWS != nil && cloudAuth.AWS.RoleARN != "" {
			sa.Annotations[annotEKSRoleARN] = cloudAuth.AWS.RoleARN
		}
	case CloudProviderAzure:
		if cloudAuth.Azure != nil && cloudAuth.Azure.ClientID != "" {
			sa.Annotations[annotAzureClientID] = cloudAuth.Azure.ClientID
		}
	}
}

// ApplyAzurePodLabel adds the Azure Workload Identity label to a pod when running on AKS.
// The label triggers the Azure Identity webhook which injects the federated token.
func ApplyAzurePodLabel(pod *corev1.Pod, provider CloudProvider) {
	if provider != CloudProviderAzure {
		return
	}
	if pod.Labels == nil {
		pod.Labels = make(map[string]string)
	}
	pod.Labels[labelAzureWorkloadIdentity] = "true"
}

// BuildManagedServiceAccount constructs the stable Agent SA managed by the operator.
func BuildManagedServiceAccount(agentName, namespace string) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      AgentSAName(agentName),
			Namespace: namespace,
			Labels: map[string]string{
				LabelManagedBy:       ManagedByValue,
				"agentorca.io/agent": agentName,
			},
		},
	}
}

func labelExists(labels map[string]string, key string) bool {
	_, ok := labels[key]
	return ok
}
