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
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentorcav1alpha1 "github.com/floppyfish14/agent-orca/api/v1alpha1"
)

// AgentSAName returns the deterministic ServiceAccount name for a managed Agent SA.
// The SA is stable (Agent-scoped, not run-scoped) to support cloud IAM bindings.
func AgentSAName(agentName string) string {
	return "agentorca-agent-" + agentName
}

// RunRoleName returns the deterministic Role name for a per-run RBAC Role.
func RunRoleName(runName string) string {
	return "agentorca-run-" + runName
}

// RunRoleBindingName returns the deterministic RoleBinding name for a per-run binding.
func RunRoleBindingName(runName string) string {
	return "agentorca-run-" + runName
}

// BuildRunRole constructs a minimal Role for a specific AgentRun.
// The role allows the agent pod to read its own AgentRun object and create events.
func BuildRunRole(run *agentorcav1alpha1.AgentRun) *rbacv1.Role {
	return &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      RunRoleName(run.Name),
			Namespace: run.Namespace,
			Labels: map[string]string{
				LabelAgentRunName: SafeLabelValue(run.Name),
				LabelManagedBy:    ManagedByValue,
			},
		},
		Rules: []rbacv1.PolicyRule{
			{
				// Allow the agent pod to read its own AgentRun status.
				APIGroups:     []string{"agentorca.agentorca.io"},
				Resources:     []string{"agentruns"},
				ResourceNames: []string{run.Name},
				Verbs:         []string{"get"},
			},
			{
				// Allow the agent pod to create Kubernetes events for observability.
				APIGroups: []string{""},
				Resources: []string{"events"},
				Verbs:     []string{"create", "patch"},
			},
			{
				// Allow the model-router executor to create/delete/read ephemeral
				// tool pods for executionMode:pod tools (e.g. code-runner).
				APIGroups: []string{""},
				Resources: []string{"pods"},
				Verbs:     []string{"create", "delete", "get", "list"},
			},
			{
				// Allow reading tool pod logs to return stdout/stderr to the LLM.
				APIGroups: []string{""},
				Resources: []string{"pods/log"},
				Verbs:     []string{"get"},
			},
		},
	}
}

// DeploymentRoleName returns the deterministic Role name for a deployment-scoped RBAC Role.
// This role is used by warm pods, which are not scoped to a single run.
func DeploymentRoleName(deploymentName string) string {
	return "agentorca-deploy-" + deploymentName
}

// DeploymentRoleBindingName returns the deterministic RoleBinding name for a deployment-scoped binding.
func DeploymentRoleBindingName(deploymentName string) string {
	return "agentorca-deploy-" + deploymentName
}

// BuildDeploymentRole constructs a Role scoped to an AgentDeployment for use by warm pods.
// It grants the same permissions as BuildRunRole but is stable across runs.
func BuildDeploymentRole(deploymentName, namespace string) *rbacv1.Role {
	return &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      DeploymentRoleName(deploymentName),
			Namespace: namespace,
			Labels: map[string]string{
				LabelManagedBy: ManagedByValue,
			},
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{"agentorca.agentorca.io"},
				Resources: []string{"agentruns"},
				Verbs:     []string{"get"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"events"},
				Verbs:     []string{"create", "patch"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"pods"},
				Verbs:     []string{"create", "delete", "get", "list"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"pods/log"},
				Verbs:     []string{"get"},
			},
		},
	}
}

// BuildDeploymentRoleBinding constructs a RoleBinding that grants the deployment-scoped Role
// to the Agent's ServiceAccount, used by warm pods.
func BuildDeploymentRoleBinding(deploymentName, saName, namespace string) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      DeploymentRoleBindingName(deploymentName),
			Namespace: namespace,
			Labels: map[string]string{
				LabelManagedBy: ManagedByValue,
			},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     DeploymentRoleName(deploymentName),
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      saName,
				Namespace: namespace,
			},
		},
	}
}

// BuildRunRoleBinding constructs a RoleBinding that grants the per-run Role to the
// Agent's stable ServiceAccount. This is what scopes each run's access without
// requiring a new SA per run.
func BuildRunRoleBinding(run *agentorcav1alpha1.AgentRun, saName, namespace string) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      RunRoleBindingName(run.Name),
			Namespace: namespace,
			Labels: map[string]string{
				LabelAgentRunName: SafeLabelValue(run.Name),
				LabelManagedBy:    ManagedByValue,
			},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     RunRoleName(run.Name),
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      saName,
				Namespace: namespace,
			},
		},
	}
}
