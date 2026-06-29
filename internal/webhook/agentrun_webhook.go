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

// Package webhook provides the validating admission webhook for agent-orc CRDs.
// The webhook is registered at /validate-agentorc-io-v1alpha1-agentrun and
// /validate-agentorc-io-v1alpha1-agent. It enforces:
//
//   - Image registry allowlist: all ociRef fields must use an approved registry prefix.
//   - Required field validation beyond what kubebuilder markers express.
//   - OCI signature check hook point (actual Cosign verification deferred to the
//     AgentRun controller to avoid blocking admission for long).
//
// The webhook uses failurePolicy: Fail for AgentRun (if we can't validate, don't run)
// and failurePolicy: Ignore for other resources (avoid blocking unrelated workloads).
package webhook

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	agentorcv1alpha1 "github.com/floppyfish14/agent-orc/api/v1alpha1"
)

// AgentRunValidator validates AgentRun resources on admission.
type AgentRunValidator struct {
	Client            client.Client
	Decoder           admission.Decoder
	AllowedRegistries []string // empty = allow all
}

// +kubebuilder:webhook:path=/validate-agentorc-agentorc-io-v1alpha1-agentrun,mutating=false,failurePolicy=fail,sideEffects=None,groups=agentorc.agentorc.io,resources=agentruns,verbs=create;update,versions=v1alpha1,name=vagentrun.kb.io,admissionReviewVersions=v1

// Handle implements admission.Handler.
func (v *AgentRunValidator) Handle(ctx context.Context, req admission.Request) admission.Response {
	var run agentorcv1alpha1.AgentRun
	if err := v.Decoder.DecodeRaw(req.Object, &run); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	// Skip validation for objects being deleted (e.g. finalizer removal updates).
	if run.DeletionTimestamp != nil {
		return admission.Allowed("ok")
	}

	if err := v.validate(ctx, &run); err != nil {
		return admission.Denied(err.Error())
	}
	return admission.Allowed("ok")
}

func (v *AgentRunValidator) validate(ctx context.Context, run *agentorcv1alpha1.AgentRun) error {
	if run.Spec.AgentRef == "" {
		return fmt.Errorf("%s", "spec.agentRef is required")
	}
	if run.Spec.Input == "" {
		return fmt.Errorf("%s", "spec.input is required")
	}

	// Verify the referenced Agent exists.
	var agent agentorcv1alpha1.Agent
	if err := v.Client.Get(ctx, client.ObjectKey{
		Name:      run.Spec.AgentRef,
		Namespace: run.Namespace,
	}, &agent); err != nil {
		return fmt.Errorf("%s", fmt.Sprintf("agent %q not found in namespace %q: %v", run.Spec.AgentRef, run.Namespace, err))
	}

	// Check the agent's OCI image against the registry allowlist.
	if err := v.checkRegistry(agent.Spec.Runtime.OCIRef); err != nil {
		return fmt.Errorf("%s", fmt.Sprintf("agent runtime image rejected: %v", err))
	}

	return nil
}

// AgentValidator validates Agent resources on admission.
type AgentValidator struct {
	Client            client.Client
	Decoder           admission.Decoder
	AllowedRegistries []string // empty = allow all
}

// +kubebuilder:webhook:path=/validate-agentorc-agentorc-io-v1alpha1-agent,mutating=false,failurePolicy=fail,sideEffects=None,groups=agentorc.agentorc.io,resources=agents,verbs=create;update,versions=v1alpha1,name=vagent.kb.io,admissionReviewVersions=v1

// Handle implements admission.Handler.
func (v *AgentValidator) Handle(ctx context.Context, req admission.Request) admission.Response {
	var agent agentorcv1alpha1.Agent
	if err := v.Decoder.DecodeRaw(req.Object, &agent); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	if err := v.validate(ctx, &agent); err != nil {
		return admission.Denied(err.Error())
	}
	return admission.Allowed("ok")
}

func (v *AgentValidator) validate(_ context.Context, agent *agentorcv1alpha1.Agent) error {
	if agent.Spec.Runtime.OCIRef == "" {
		return fmt.Errorf("%s", "spec.runtime.ociRef is required")
	}
	if agent.Spec.ModelSelectorRef == "" {
		return fmt.Errorf("%s", "spec.modelSelectorRef is required")
	}
	if err := v.checkRegistry(agent.Spec.Runtime.OCIRef); err != nil {
		return fmt.Errorf("%s", fmt.Sprintf("agent runtime image rejected: %v", err))
	}
	return nil
}

// ToolValidator validates Tool resources on admission.
type ToolValidator struct {
	Client            client.Client
	Decoder           admission.Decoder
	AllowedRegistries []string
}

// +kubebuilder:webhook:path=/validate-agentorc-agentorc-io-v1alpha1-tool,mutating=false,failurePolicy=fail,sideEffects=None,groups=agentorc.agentorc.io,resources=tools,verbs=create;update,versions=v1alpha1,name=vtool.kb.io,admissionReviewVersions=v1

// Handle implements admission.Handler.
func (v *ToolValidator) Handle(_ context.Context, req admission.Request) admission.Response {
	var tool agentorcv1alpha1.Tool
	if err := v.Decoder.DecodeRaw(req.Object, &tool); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	if tool.Spec.OCIRef != "" {
		if err := v.checkRegistry(tool.Spec.OCIRef); err != nil {
			return admission.Denied(fmt.Sprintf("tool OCI image rejected: %v", err))
		}
	}
	return admission.Allowed("ok")
}

// checkRegistry verifies an OCI reference against the configured allowlist.
// If AllowedRegistries is empty, all registries are allowed.
func (v *AgentRunValidator) checkRegistry(ociRef string) error {
	return checkRegistryAllowlist(ociRef, v.AllowedRegistries)
}

func (v *AgentValidator) checkRegistry(ociRef string) error {
	return checkRegistryAllowlist(ociRef, v.AllowedRegistries)
}

func (v *ToolValidator) checkRegistry(ociRef string) error {
	return checkRegistryAllowlist(ociRef, v.AllowedRegistries)
}

func checkRegistryAllowlist(ociRef string, allowedRegistries []string) error {
	if len(allowedRegistries) == 0 {
		return nil // no restriction
	}
	for _, allowed := range allowedRegistries {
		// Ensure we match on a path boundary to prevent "gcr.io/my" from
		// matching "gcr.io/my-evil-org/image". Append "/" if the allowed
		// prefix doesn't already end with one, then check for exact match
		// or prefix+"/".
		if ociRef == allowed || strings.HasPrefix(ociRef, allowed+"/") ||
			(strings.HasSuffix(allowed, "/") && strings.HasPrefix(ociRef, allowed)) {
			return nil
		}
	}
	return fmt.Errorf("image %q does not match any allowed registry prefix: %v", ociRef, allowedRegistries)
}

// SetupAgentRunWebhook registers the AgentRun validating webhook with the manager.
func SetupAgentRunWebhook(mgr ctrl.Manager, allowedRegistries []string) error {
	decoder := admission.NewDecoder(mgr.GetScheme())
	mgr.GetWebhookServer().Register("/validate-agentorc-agentorc-io-v1alpha1-agentrun",
		&admission.Webhook{
			Handler: &AgentRunValidator{
				Client:            mgr.GetClient(),
				Decoder:           decoder,
				AllowedRegistries: allowedRegistries,
			},
		},
	)
	return nil
}

// SetupAgentWebhook registers the Agent validating webhook with the manager.
func SetupAgentWebhook(mgr ctrl.Manager, allowedRegistries []string) error {
	decoder := admission.NewDecoder(mgr.GetScheme())
	mgr.GetWebhookServer().Register("/validate-agentorc-agentorc-io-v1alpha1-agent",
		&admission.Webhook{
			Handler: &AgentValidator{
				Client:            mgr.GetClient(),
				Decoder:           decoder,
				AllowedRegistries: allowedRegistries,
			},
		},
	)
	return nil
}

// SetupToolWebhook registers the Tool validating webhook with the manager.
func SetupToolWebhook(mgr ctrl.Manager, allowedRegistries []string) error {
	decoder := admission.NewDecoder(mgr.GetScheme())
	mgr.GetWebhookServer().Register("/validate-agentorc-agentorc-io-v1alpha1-tool",
		&admission.Webhook{
			Handler: &ToolValidator{
				Client:            mgr.GetClient(),
				Decoder:           decoder,
				AllowedRegistries: allowedRegistries,
			},
		},
	)
	return nil
}

// Ensure the runtime import is referenced to satisfy the Go import checker.
var _ runtime.Object
