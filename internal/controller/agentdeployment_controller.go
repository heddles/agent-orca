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
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentorcv1alpha1 "github.com/floppyfish14/agent-orc/api/v1alpha1"
	"github.com/floppyfish14/agent-orc/internal/podbuilder"
	"github.com/floppyfish14/agent-orc/internal/router"
	"github.com/floppyfish14/agent-orc/internal/security"
	"github.com/floppyfish14/agent-orc/internal/state"
)

// AgentDeploymentReconciler reconciles an AgentDeployment object.
type AgentDeploymentReconciler struct {
	client.Client
	Scheme           *runtime.Scheme
	Recorder         record.EventRecorder
	K8s              kubernetes.Interface
	ModelRouterImage string
	StateConfig      state.Config
	CloudProvider    security.CloudProvider
	// TokenReviewerClusterRole is the ClusterRole that grants "create tokenreviews".
	TokenReviewerClusterRole string
	// OperatorAPIURL is the base URL of the operator's internal API server.
	// Injected into the router config so the model-router can call RAG, handoff, etc.
	OperatorAPIURL string
}

// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=agentorc.agentorc.io,resources=agentdeployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agentorc.agentorc.io,resources=agentdeployments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentorc.agentorc.io,resources=agentdeployments/finalizers,verbs=update
// +kubebuilder:rbac:groups=agentorc.agentorc.io,resources=agents,verbs=get;list
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=serviceaccounts,verbs=get;list;watch;create;update;patch

// Reconcile implements the reconciliation loop for AgentDeployment.
func (r *AgentDeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := slog.With("agentdeployment", req.NamespacedName)

	var deployment agentorcv1alpha1.AgentDeployment
	if err := r.Get(ctx, req.NamespacedName, &deployment); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		log.Error("failed to fetch AgentDeployment", "error", err)
		return ctrl.Result{}, err
	}
	// Snapshot for status patch — avoids optimistic concurrency conflicts
	// that occur when concurrent reconciliations race on status updates.
	statusBase := deployment.DeepCopy()

	// Set initial phase if not set
	if deployment.Status.Phase == "" {
		deployment.Status.Phase = agentorcv1alpha1.AgentDeploymentPhaseCreating
		deployment.Status.LastUpdateTime = &metav1.Time{Time: time.Now()}
	}

	// Verify the referenced Agent exists
	var agent agentorcv1alpha1.Agent
	if err := r.Get(ctx, types.NamespacedName{
		Name:      deployment.Spec.AgentRef,
		Namespace: deployment.Namespace,
	}, &agent); err != nil {
		log.Error("referenced Agent not found", "agent", deployment.Spec.AgentRef)
		deployment.Status.Phase = agentorcv1alpha1.AgentDeploymentPhaseFailed
		deployment.Status.Message = fmt.Sprintf("Agent %s not found", deployment.Spec.AgentRef)
		_ = r.Status().Patch(ctx, &deployment, client.MergeFrom(statusBase))
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	saName := security.AgentSAName(agent.Name)
	if agent.Spec.ServiceAccountRef != nil {
		saName = agent.Spec.ServiceAccountRef.Name
	}

	// Resolve MCP sidecar image volumes (must run before buildDeploymentRouterConfig
	// so the binary path rewrites are available for the router config).
	mcpBinVolumes, mcpBinMounts, binPathRewrites, err := podbuilder.ResolveMCPSidecarVolumes(ctx, r.Client, deployment.Namespace, &agent)
	if err != nil {
		deployment.Status.Phase = agentorcv1alpha1.AgentDeploymentPhaseFailed
		deployment.Status.Message = fmt.Sprintf("resolving MCP sidecar volumes: %v", err)
		_ = r.Status().Patch(ctx, &deployment, client.MergeFrom(statusBase))
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Resolve tool secret volumes (MCP envFrom secrets).
	toolSecretVolumes, toolSecretMounts, err := podbuilder.ResolveToolSecretVolumes(ctx, r.Client, deployment.Namespace, &agent)
	if err != nil {
		deployment.Status.Phase = agentorcv1alpha1.AgentDeploymentPhaseFailed
		deployment.Status.Message = fmt.Sprintf("resolving tool secret volumes: %v", err)
		_ = r.Status().Patch(ctx, &deployment, client.MergeFrom(statusBase))
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Resolve ModelSelector + tools → build router config.
	routerCfg, err := r.buildDeploymentRouterConfig(ctx, &deployment, &agent, saName, binPathRewrites)
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("router config dependency not ready, retrying", "error", err)
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		deployment.Status.Phase = agentorcv1alpha1.AgentDeploymentPhaseFailed
		deployment.Status.Message = fmt.Sprintf("building router config: %v", err)
		_ = r.Status().Patch(ctx, &deployment, client.MergeFrom(statusBase))
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Ensure router config ConfigMap.
	routerConfigHash, err := r.ensureDeploymentRouterConfigMap(ctx, &deployment, routerCfg)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Ensure per-deployment token Secret.
	tokenSecretName, err := r.ensureDeploymentTokenSecret(ctx, &deployment, saName)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Ensure TokenReviewer ClusterRoleBinding so the model-router sidecar can validate tokens.
	if err := r.ensureDeploymentTokenReviewerBinding(ctx, &deployment, saName); err != nil {
		return ctrl.Result{}, err
	}

	// Resolve provider secret volumes.
	providerVolumes, providerMounts, err := podbuilder.ResolveProviderVolumes(ctx, r.Client, deployment.Namespace, &agent)
	if err != nil {
		deployment.Status.Phase = agentorcv1alpha1.AgentDeploymentPhaseFailed
		deployment.Status.Message = fmt.Sprintf("resolving provider volumes: %v", err)
		_ = r.Status().Patch(ctx, &deployment, client.MergeFrom(statusBase))
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Create or update the underlying Kubernetes Deployment.
	if err := r.reconcileDeployment(ctx, &deployment, &agent, saName, tokenSecretName, routerConfigHash, providerVolumes, providerMounts, toolSecretVolumes, toolSecretMounts, mcpBinVolumes, mcpBinMounts); err != nil {
		log.Error("failed to reconcile Deployment", "error", err)
		deployment.Status.Phase = agentorcv1alpha1.AgentDeploymentPhaseFailed
		deployment.Status.Message = err.Error()
		_ = r.Status().Patch(ctx, &deployment, client.MergeFrom(statusBase))
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Sync pod status from underlying Deployment
	if err := r.syncPodStatus(ctx, &deployment); err != nil {
		log.Error("failed to sync pod status", "error", err)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, err
	}

	// Reconcile warm pod pool for fast chat dispatch.
	if deployment.Spec.WarmPoolSize > 0 {
		if err := r.reconcileWarmPool(ctx, &deployment, &agent, saName, routerConfigHash, routerCfg, providerVolumes, providerMounts, toolSecretVolumes, toolSecretMounts, mcpBinVolumes, mcpBinMounts); err != nil {
			log.Error("failed to reconcile warm pool", "error", err)
			// Non-fatal: continue with status update.
		}
	} else {
		deployment.Status.WarmPoolReady = 0
	}

	// Check if we should pause due to too many failures
	restartPolicy := deployment.Spec.RestartPolicy
	if restartPolicy != nil && restartPolicy.MaxConsecutiveFailures > 0 {
		if deployment.Status.ConsecutiveFailures >= restartPolicy.MaxConsecutiveFailures {
			deployment.Status.Phase = agentorcv1alpha1.AgentDeploymentPhasePaused
			deployment.Status.Message = fmt.Sprintf(
				"Paused after %d consecutive failures (max: %d)",
				deployment.Status.ConsecutiveFailures,
				restartPolicy.MaxConsecutiveFailures,
			)
			_ = r.Status().Patch(ctx, &deployment, client.MergeFrom(statusBase))
			return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
		}
	}

	// Update phase to Running if pods are ready
	if deployment.Status.ReadyReplicas > 0 {
		deployment.Status.Phase = agentorcv1alpha1.AgentDeploymentPhaseRunning
		deployment.Status.Message = ""
	}

	deployment.Status.LastUpdateTime = &metav1.Time{Time: time.Now()}
	if err := r.Status().Patch(ctx, &deployment, client.MergeFrom(statusBase)); err != nil {
		log.Error("failed to update status", "error", err)
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// reconcileDeployment creates or updates the underlying K8s Deployment with the
// model-router sidecar, provider volumes, token secret, and security hardening.
func (r *AgentDeploymentReconciler) reconcileDeployment(
	ctx context.Context,
	agentDeploy *agentorcv1alpha1.AgentDeployment,
	agent *agentorcv1alpha1.Agent,
	saName string,
	tokenSecretName string,
	routerConfigHash string,
	providerVolumes []corev1.Volume,
	providerMounts []corev1.VolumeMount,
	toolSecretVolumes []corev1.Volume,
	toolSecretMounts []corev1.VolumeMount,
	mcpBinVolumes []corev1.Volume,
	mcpBinMounts []corev1.VolumeMount,
) error {
	deploymentName := agentDeploy.Name
	namespace := agentDeploy.Namespace

	// Get or create the Kubernetes Deployment
	var k8sDeploy appsv1.Deployment
	k8sDeployKey := types.NamespacedName{Name: deploymentName, Namespace: namespace}

	isNew := false
	if err := r.Get(ctx, k8sDeployKey, &k8sDeploy); err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		isNew = true
	}

	// Build the deployment spec
	replicas := agentDeploy.Spec.Replicas
	if replicas == nil {
		one := int32(1)
		replicas = &one
	}

	selector := map[string]string{
		"app.kubernetes.io/name":      "agent-orc",
		"app.kubernetes.io/component": "agent-deployment",
		"agentdeployment.agentorc.io": agentDeploy.Name,
	}

	labels := map[string]string{
		"app.kubernetes.io/name":      "agent-orc",
		"app.kubernetes.io/component": "agent-deployment",
		"agentdeployment.agentorc.io": agentDeploy.Name,
		"agent.agentorc.io":           agent.Name,
		security.LabelManagedBy:       security.ManagedByValue,
	}

	inputSourceType := agentorcv1alpha1.InputSourceChat
	if agentDeploy.Spec.InputSource != nil {
		inputSourceType = agentDeploy.Spec.InputSource.Type
	}

	agentEnv := []corev1.EnvVar{
		{Name: "AGENTORC_AGENT", Value: agent.Name},
		{Name: "AGENTORC_NAMESPACE", Value: namespace},
		{Name: "AGENTORC_INPUT_SOURCE", Value: string(inputSourceType)},
	}

	pod := podbuilder.Build(podbuilder.PodConfig{
		Namespace: namespace,
		Labels:    labels,
		Annotations: map[string]string{
			"agentorc.io/router-config-hash": routerConfigHash,
		},
		Agent:             agent,
		AgentEnv:          agentEnv,
		TokenSecretName:   tokenSecretName,
		ServiceAccount:    saName,
		RestartPolicy:     corev1.RestartPolicyAlways,
		ModelRouterImage:  r.ModelRouterImage,
		RouterConfigName:  "agentorc-deploy-" + agentDeploy.Name,
		ProviderVolumes:   providerVolumes,
		ProviderMounts:    providerMounts,
		ToolSecretVolumes: toolSecretVolumes,
		ToolSecretMounts:  toolSecretMounts,
		MCPBinVolumes:     mcpBinVolumes,
		MCPBinMounts:      mcpBinMounts,
		CloudProvider:     r.CloudProvider,
	})

	if isNew {
		k8sDeploy = appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      deploymentName,
				Namespace: namespace,
				Labels:    labels,
				OwnerReferences: []metav1.OwnerReference{
					*metav1.NewControllerRef(agentDeploy, agentorcv1alpha1.GroupVersion.WithKind("AgentDeployment")),
				},
			},
			Spec: appsv1.DeploymentSpec{
				Replicas: replicas,
				Selector: &metav1.LabelSelector{MatchLabels: selector},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: pod.ObjectMeta,
					Spec:       pod.Spec,
				},
			},
		}
		if err := r.Create(ctx, &k8sDeploy); err != nil {
			return fmt.Errorf("creating Deployment: %w", err)
		}
	} else {
		// Update existing deployment spec. Setting the router-config-hash annotation
		// on the pod template ensures a rollout when the router config changes even
		// if the pod spec itself is unchanged (e.g. only ModelSelector weights changed).
		// Use Patch instead of Update to avoid optimistic concurrency conflicts when
		// the Deployment controller modifies the object concurrently.
		deployBase := k8sDeploy.DeepCopy()
		k8sDeploy.Spec.Replicas = replicas
		k8sDeploy.Spec.Template.ObjectMeta.Labels = pod.Labels
		k8sDeploy.Spec.Template.ObjectMeta.Annotations = pod.Annotations
		k8sDeploy.Spec.Template.Spec = pod.Spec
		if err := r.Patch(ctx, &k8sDeploy, client.MergeFrom(deployBase)); err != nil {
			return fmt.Errorf("updating Deployment: %w", err)
		}
	}

	return nil
}

// syncPodStatus reads the underlying Deployment's status and updates AgentDeployment.
func (r *AgentDeploymentReconciler) syncPodStatus(
	ctx context.Context,
	agentDeploy *agentorcv1alpha1.AgentDeployment,
) error {
	var k8sDeploy appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{
		Name:      agentDeploy.Name,
		Namespace: agentDeploy.Namespace,
	}, &k8sDeploy); err != nil {
		return err
	}

	// Sync replica counts
	agentDeploy.Status.ReadyReplicas = k8sDeploy.Status.ReadyReplicas
	agentDeploy.Status.AvailableReplicas = k8sDeploy.Status.AvailableReplicas

	// List pods for this deployment
	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(agentDeploy.Namespace),
		client.MatchingLabels{
			"agentdeployment.agentorc.io": agentDeploy.Name,
		},
	); err != nil {
		return err
	}

	agentDeploy.Status.PodNames = make([]string, 0, len(pods.Items))
	var consecutiveFailures int
	for _, pod := range pods.Items {
		agentDeploy.Status.PodNames = append(agentDeploy.Status.PodNames, pod.Name)
		// Check if pod is in failed state
		if pod.Status.Phase == corev1.PodFailed {
			consecutiveFailures++
			agentDeploy.Status.LastFailureTime = &metav1.Time{Time: time.Now()}
		}
	}

	// Update consecutive failures count (resets if any pod is successful)
	if consecutiveFailures > 0 && consecutiveFailures == len(pods.Items) {
		agentDeploy.Status.ConsecutiveFailures = consecutiveFailures
	} else if len(pods.Items) > 0 {
		agentDeploy.Status.ConsecutiveFailures = 0
	}

	return nil
}

// buildDeploymentRouterConfig resolves ModelSelector + Tools for an AgentDeployment.
func (r *AgentDeploymentReconciler) buildDeploymentRouterConfig(
	ctx context.Context,
	deploy *agentorcv1alpha1.AgentDeployment,
	agent *agentorcv1alpha1.Agent,
	saName string,
	binPathRewrites map[string]string,
) (*router.Config, error) {
	var selector agentorcv1alpha1.ModelSelector
	if err := r.Get(ctx, client.ObjectKey{Name: agent.Spec.ModelSelectorRef, Namespace: deploy.Namespace}, &selector); err != nil {
		return nil, fmt.Errorf("getting ModelSelector %q: %w", agent.Spec.ModelSelectorRef, err)
	}

	var providers []router.ProviderConfig
	seenProviders := make(map[string]bool)
	for _, pw := range selector.Spec.Providers {
		var mp agentorcv1alpha1.ModelProvider
		if err := r.Get(ctx, client.ObjectKey{Name: pw.Name, Namespace: deploy.Namespace}, &mp); err != nil {
			return nil, fmt.Errorf("getting ModelProvider %q: %w", pw.Name, err)
		}
		seenProviders[pw.Name] = true
		apiKeyFile := fmt.Sprintf("%s/%s/api-key", podbuilder.ProviderSecretsDir, pw.Name)
		providers = append(providers, router.ProviderConfig{
			Name:               pw.Name,
			LiteLLMModel:       mp.Spec.LiteLLMModel,
			APIKeyFile:         apiKeyFile,
			Weight:             pw.Weight,
			Capabilities:       mp.Spec.Capabilities,
			ContextWindow:      mp.Spec.Constraints.ContextWindow,
			MaxRequestTokens:   mp.Spec.Constraints.MaxRequestTokens,
			LatencyProfile:     mp.Spec.LatencyProfile,
			CostPerInputToken:  parseFloat(mp.Spec.Constraints.CostPerMillionInputTokens) / 1e6,
			CostPerOutputToken: parseFloat(mp.Spec.Constraints.CostPerMillionOutputTokens) / 1e6,
			BaseURL:            mp.Spec.BaseURL,
			RoutingHint:        pw.RoutingHint,
		})
	}
	// Load fallback chain providers with Weight=0 so tryFallback can find them
	// but they are excluded from weighted primary selection.
	for _, name := range selector.Spec.FallbackChain {
		if seenProviders[name] {
			continue
		}
		seenProviders[name] = true
		var mp agentorcv1alpha1.ModelProvider
		if err := r.Get(ctx, client.ObjectKey{Name: name, Namespace: deploy.Namespace}, &mp); err != nil {
			if apierrors.IsNotFound(err) {
				// Provider is listed in the fallback chain but not deployed (e.g. disabled
				// in the model-providers chart). Skip it rather than blocking the deployment.
				if r.Recorder != nil {
					r.Recorder.Eventf(deploy, corev1.EventTypeWarning, "FallbackProviderSkipped",
						"fallback provider %q not deployed; skipped from chain", name)
				}
				continue
			}
			return nil, fmt.Errorf("getting fallback ModelProvider %q: %w", name, err)
		}
		apiKeyFile := fmt.Sprintf("%s/%s/api-key", podbuilder.ProviderSecretsDir, name)
		providers = append(providers, router.ProviderConfig{
			Name:               name,
			LiteLLMModel:       mp.Spec.LiteLLMModel,
			APIKeyFile:         apiKeyFile,
			Weight:             0,
			Capabilities:       mp.Spec.Capabilities,
			ContextWindow:      mp.Spec.Constraints.ContextWindow,
			MaxRequestTokens:   mp.Spec.Constraints.MaxRequestTokens,
			LatencyProfile:     mp.Spec.LatencyProfile,
			CostPerInputToken:  parseFloat(mp.Spec.Constraints.CostPerMillionInputTokens) / 1e6,
			CostPerOutputToken: parseFloat(mp.Spec.Constraints.CostPerMillionOutputTokens) / 1e6,
			BaseURL:            mp.Spec.BaseURL,
		})
	}

	var toolDefs []router.ToolDefinition
	var mcpServers []router.MCPServerConfig
	seenMCPServers := make(map[string]bool)
	// Builtin tools are injected automatically and don't exist as Tool CRDs
	builtinTools := map[string]bool{
		"_spawn": true, "_handoff": true, "_clarify": true, "_done": true,
		"_fail": true, "_emit_event": true, "_rag_ingest": true, "_rag_search": true,
		"_write_state": true, "_read_state": true, "_list_state": true, "_delete_state": true,
		"_memory_store": true, "_mcp_read_resource": true, "_propose_step": true,
		"_propose_fix": true, "_confirm_fix": true, "_create_workflow": true,
	}
	for _, toolName := range agent.Spec.Tools {
		// Skip builtin tools - they're injected separately in the router
		if builtinTools[toolName] {
			continue
		}
		var tool agentorcv1alpha1.Tool
		if err := r.Get(ctx, client.ObjectKey{Name: toolName, Namespace: deploy.Namespace}, &tool); err != nil {
			return nil, fmt.Errorf("getting Tool %q: %w", toolName, err)
		}
		var params json.RawMessage
		if tool.Spec.Schema != nil && tool.Spec.Schema.Input != nil {
			params = tool.Spec.Schema.Input.Raw
		}
		desc := ""
		if tool.Spec.Schema != nil {
			desc = tool.Spec.Schema.Description
		}
		backendType := string(tool.Spec.Type)
		if backendType == "" {
			backendType = "regular"
		}
		backendRef := tool.Spec.OCIRef
		if tool.Spec.Type == agentorcv1alpha1.ToolTypeAgent {
			backendRef = tool.Spec.AgentRef
		}
		if tool.Spec.Type == agentorcv1alpha1.ToolTypeMCP && tool.Spec.MCPConfig != nil {
			serverName := tool.Labels[LabelMCPServer]
			if serverName == "" || !seenMCPServers[serverName] {
				if serverName != "" {
					seenMCPServers[serverName] = true
				}
				mcpCfg := router.MCPServerConfig{
					Name:      toolName,
					Transport: tool.Spec.MCPConfig.Transport,
					URL:       tool.Spec.MCPConfig.URL,
				}
				if serverName != "" {
					var mcpServer agentorcv1alpha1.MCPServer
					if err := r.Get(ctx, client.ObjectKey{Name: serverName, Namespace: deploy.Namespace}, &mcpServer); err == nil {
						mcpCfg.AllowApps = mcpServer.Spec.AllowApps
					}
				}
				args := tool.Spec.MCPConfig.Args
				if tool.Spec.MCPConfig.Transport == "stdio" && len(args) > 0 {
					mcpCfg.Cmd = args[0]
					mcpCfg.Args = args[1:]
					// Rewrite absolute paths to point into the image volume mount.
					if mountDir, ok := binPathRewrites[toolName]; ok {
						if strings.HasPrefix(mcpCfg.Cmd, "/") {
							mcpCfg.Cmd = mountDir + mcpCfg.Cmd
						}
						for i, a := range mcpCfg.Args {
							if strings.HasPrefix(a, "/") {
								mcpCfg.Args[i] = mountDir + a
							}
						}
					}
				}
				envKeys := make([]string, 0, len(tool.Spec.MCPConfig.Env))
				for k := range tool.Spec.MCPConfig.Env {
					envKeys = append(envKeys, k)
				}
				slices.Sort(envKeys)
				for _, k := range envKeys {
					mcpCfg.Env = append(mcpCfg.Env, k+"="+tool.Spec.MCPConfig.Env[k])
				}
				// Resolve secret-backed env vars to file paths.
				for _, ev := range tool.Spec.MCPConfig.EnvFrom {
					if ev.ValueFrom != nil && ev.ValueFrom.SecretKeyRef != nil {
						ref := ev.ValueFrom.SecretKeyRef
						filePath := podbuilder.ToolSecretFilePath(toolName, ref.Name, ref.Key)
						mcpCfg.EnvFiles = append(mcpCfg.EnvFiles, router.EnvFileMapping{
							Name:     ev.Name,
							FilePath: filePath,
						})
					} else if ev.Value != "" {
						mcpCfg.Env = append(mcpCfg.Env, ev.Name+"="+ev.Value)
					}
				}
				mcpServers = append(mcpServers, mcpCfg)
			}
		}
		toolDefs = append(toolDefs, router.ToolDefinition{
			Name:        toolName,
			Description: desc,
			Parameters:  params,
			BackendType: backendType,
			BackendRef:  backendRef,
			Command:     tool.Spec.Command,
			Args:        tool.Spec.Args,
		})
	}

	// Inject builtin tools.
	toolDefs = append(toolDefs, router.ToolDefinition{
		Name:        "_handoff",
		Description: "Transfer control to another agent with the accumulated conversation context. Use this when the task is better handled by a different specialist agent.",
		Parameters:  []byte(`{"type":"object","properties":{"targetAgent":{"type":"string","description":"Name of the Agent CRD to hand off to"},"contextSummary":{"type":"string","description":"Brief summary of work done so far to pass as input to the successor agent"}},"required":["targetAgent"]}`),
		BackendType: "builtin",
	})
	toolDefs = append(toolDefs, router.ToolDefinition{
		Name:        "_clarify",
		Description: "Ask the human user a clarifying question when you need more information to proceed. The workflow will pause until the human responds. Only use this for genuinely ambiguous situations where you cannot make a reasonable assumption.",
		Parameters:  []byte(`{"type":"object","properties":{"question":{"type":"string","description":"The question to ask the human user"}},"required":["question"]}`),
		BackendType: "builtin",
	})
	toolDefs = append(toolDefs, router.ToolDefinition{
		Name:        "_done",
		Description: "Signal successful task completion with a structured result. Use this when the task is fully complete and you have a final answer or output to return. The run will transition to Succeeded and no further LLM calls will be made.",
		Parameters:  []byte(`{"type":"object","properties":{"output":{"type":"string","description":"The final output or result of the task"},"summary":{"type":"string","description":"A brief human-readable summary of what was accomplished"}},"required":["output"]}`),
		BackendType: "builtin",
	})
	toolDefs = append(toolDefs, router.ToolDefinition{
		Name:        "_fail",
		Description: "Signal an unrecoverable failure with an explanation. Use this when the task cannot be completed and proceeding further would not help. The run will transition to Failed.",
		Parameters:  []byte(`{"type":"object","properties":{"reason":{"type":"string","description":"Clear explanation of why the task failed"},"retryable":{"type":"boolean","description":"Whether the failure might succeed with a retry","default":false}},"required":["reason"]}`),
		BackendType: "builtin",
	})
	toolDefs = append(toolDefs, router.ToolDefinition{
		Name:        "_spawn",
		Description: "Launch a sub-agent to handle a subtask and wait for its result. Unlike _handoff (which transfers control away), _spawn delegates work and returns the result to you so you can continue. Use for parallelizable subtasks or specialized delegation.",
		Parameters:  []byte(`{"type":"object","properties":{"agentRef":{"type":"string","description":"Name of the Agent CRD to run"},"input":{"type":"string","description":"Task description to pass to the sub-agent"},"timeoutSeconds":{"type":"integer","description":"Maximum seconds to wait for the sub-agent (default: 300)","default":300}},"required":["agentRef","input"]}`),
		BackendType: "builtin",
	})
	toolDefs = append(toolDefs, router.ToolDefinition{
		Name:        "_emit_event",
		Description: "Publish a named event for observability and loose coupling. Events are visible in Kubernetes (kubectl describe) and the run trace stream. Use to signal milestones or state changes.",
		Parameters:  []byte(`{"type":"object","properties":{"eventType":{"type":"string","description":"Short PascalCase event type name (e.g. DocumentProcessed, ValidationFailed)"},"message":{"type":"string","description":"Human-readable event message"}},"required":["eventType","message"]}`),
		BackendType: "builtin",
	})
	toolDefs = append(toolDefs, router.ToolDefinition{
		Name:        "_create_workflow",
		Description: "Create and execute a deterministic AgentWorkflow with multiple steps. The workflow controller handles sequencing and you receive all step outputs when complete. Use for evaluation pipelines or multi-step tasks requiring guaranteed execution order.",
		Parameters:  []byte(`{"type":"object","properties":{"name":{"type":"string","description":"Unique workflow name"},"description":{"type":"string"},"steps":{"type":"array","description":"Array of step objects with name, agentRef, input, dependsOn","items":{"type":"object","properties":{"name":{"type":"string"},"agentRef":{"type":"string"},"input":{"type":"string"},"dependsOn":{"type":"array","items":{"type":"string"}},"condition":{"type":"string"},"timeout":{"type":"object"}},"required":["name","agentRef","input"]}},"budgetCap":{"type":"object","properties":{"total":{"type":"string"}}},"timeout":{"type":"object"},"onStepFailure":{"type":"string","enum":["stop","continue"]}},"required":["name","steps"]}`),
		BackendType: "builtin",
	})

	// Resolve KnowledgeBases from Agent spec and inject _rag tools.
	var kbConfigs []router.KnowledgeBaseConfig
	for _, kbName := range agent.Spec.KnowledgeBases {
		var kb agentorcv1alpha1.KnowledgeBase
		if err := r.Get(ctx, client.ObjectKey{Name: kbName, Namespace: deploy.Namespace}, &kb); err != nil {
			return nil, fmt.Errorf("getting KnowledgeBase %q: %w", kbName, err)
		}
		if kb.Spec.AllowedAgents != nil && !slices.Contains(kb.Spec.AllowedAgents, agent.Name) {
			return nil, fmt.Errorf("agent %q is not in KnowledgeBase %q allowedAgents", agent.Name, kbName)
		}
		if !kb.Status.Ready {
			return nil, fmt.Errorf("KnowledgeBase %q is not ready", kbName)
		}
		var embMS agentorcv1alpha1.ModelSelector
		if err := r.Get(ctx, client.ObjectKey{Name: kb.Spec.Embedding.ModelSelectorRef, Namespace: deploy.Namespace}, &embMS); err != nil {
			return nil, fmt.Errorf("getting embedding ModelSelector for KB %q: %w", kbName, err)
		}
		if len(embMS.Spec.Providers) == 0 {
			return nil, fmt.Errorf("embedding ModelSelector %q for KB %q has no providers", kb.Spec.Embedding.ModelSelectorRef, kbName)
		}
		embPW := embMS.Spec.Providers[0]
		var embMP agentorcv1alpha1.ModelProvider
		if err := r.Get(ctx, client.ObjectKey{Name: embPW.Name, Namespace: deploy.Namespace}, &embMP); err != nil {
			return nil, fmt.Errorf("getting embedding ModelProvider %q: %w", embPW.Name, err)
		}
		dims := kb.Status.EmbeddingDimensions
		kbConfigs = append(kbConfigs, router.KnowledgeBaseConfig{
			Name:                  kbName,
			VectorStoreURL:        kb.Status.VectorStoreURL,
			CollectionName:        kb.Status.CollectionName,
			EmbeddingProviderName: embPW.Name,
			EmbeddingModel:        embMP.Spec.LiteLLMModel,
			Dimensions:            dims,
		})
	}
	if len(kbConfigs) > 0 {
		kbNames := make([]string, len(kbConfigs))
		for i, kb := range kbConfigs {
			kbNames[i] = kb.Name
		}
		kbEnum, _ := json.Marshal(kbNames)
		kbList := strings.Join(kbNames, ", ")

		searchParams := fmt.Sprintf(`{"type":"object","properties":{"knowledgeBase":{"type":"string","enum":%s,"description":"Name of the KnowledgeBase to search"},"query":{"type":"string","minLength":1,"description":"The specific search terms derived from the user's question. Must be non-empty."},"topK":{"type":"integer","default":5,"description":"Number of results to return"}},"required":["knowledgeBase","query"]}`, kbEnum)
		ingestParams := fmt.Sprintf(`{"type":"object","properties":{"knowledgeBase":{"type":"string","enum":%s,"description":"Name of the KnowledgeBase"},"documents":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"},"content":{"type":"string"},"metadata":{"type":"object"}},"required":["id","content"]}}},"required":["knowledgeBase","documents"]}`, kbEnum)

		toolDefs = append(toolDefs, router.ToolDefinition{
			Name: "_rag_search",
			Description: fmt.Sprintf("Search the knowledge base for relevant documents when the user's question is specifically about topics that may be documented there. "+
				"Do NOT call this for general knowledge questions you can answer directly. "+
				"Only call it when the answer likely requires project-specific or domain-specific information stored in the knowledge base. "+
				"Available knowledge bases: %s", kbList),
			Parameters:  json.RawMessage(searchParams),
			BackendType: "builtin",
		})
		toolDefs = append(toolDefs, router.ToolDefinition{
			Name:        "_rag_ingest",
			Description: fmt.Sprintf("Ingest documents into a knowledge base at runtime. Available knowledge bases: %s", kbList),
			Parameters:  json.RawMessage(ingestParams),
			BackendType: "builtin",
		})
	}

	// Resolve long-term memory KB and inject _memory_store tool.
	var longTermMemory router.LongTermMemoryConfig
	if agent.Spec.Memory != nil && agent.Spec.Memory.LongTermMemoryRef != "" {
		ltmKBName := agent.Spec.Memory.LongTermMemoryRef
		var ltmKB agentorcv1alpha1.KnowledgeBase
		if err := r.Get(ctx, client.ObjectKey{Name: ltmKBName, Namespace: deploy.Namespace}, &ltmKB); err != nil || !ltmKB.Status.Ready {
			slog.Warn("long-term memory KnowledgeBase not ready; proceeding without it",
				"knowledgeBase", ltmKBName, "deployment", deploy.Name)
		} else {
			var ltmEmbMS agentorcv1alpha1.ModelSelector
			if err := r.Get(ctx, client.ObjectKey{Name: ltmKB.Spec.Embedding.ModelSelectorRef, Namespace: deploy.Namespace}, &ltmEmbMS); err == nil && len(ltmEmbMS.Spec.Providers) > 0 {
				ltmPW := ltmEmbMS.Spec.Providers[0]
				var ltmMP agentorcv1alpha1.ModelProvider
				if err := r.Get(ctx, client.ObjectKey{Name: ltmPW.Name, Namespace: deploy.Namespace}, &ltmMP); err == nil {
					dims := ltmKB.Status.EmbeddingDimensions
					longTermMemory = router.LongTermMemoryConfig{
						Enabled:               true,
						KBName:                ltmKBName,
						VectorStoreURL:        ltmKB.Status.VectorStoreURL,
						CollectionName:        ltmKB.Status.CollectionName,
						EmbeddingProviderName: ltmPW.Name,
						EmbeddingModel:        ltmMP.Spec.LiteLLMModel,
						Dimensions:            dims,
					}
					toolDefs = append(toolDefs, router.ToolDefinition{
						Name:        "_memory_store",
						Description: "Persist a fact or piece of information to long-term memory so it can be recalled in future sessions.",
						Parameters:  []byte(`{"type":"object","properties":{"fact":{"type":"string","description":"The fact or information to remember"},"tags":{"type":"array","items":{"type":"string"},"description":"Optional tags for categorization"}},"required":["fact"]}`),
						BackendType: "builtin",
					})
				}
			}
		}
	}

	var budget float64
	if selector.Spec.BudgetCap != nil {
		budget = parseFloat(selector.Spec.BudgetCap.PerRun)
	}

	threshold := 0.6
	if selector.Spec.MetaRouter != nil && selector.Spec.MetaRouter.Threshold != "" {
		threshold = parseFloat(selector.Spec.MetaRouter.Threshold)
	}
	metaRouterProvider := ""
	if selector.Spec.MetaRouter != nil {
		metaRouterProvider = selector.Spec.MetaRouter.ProviderRef
	}

	checkpointEvery := 1
	if agent.Spec.Memory != nil {
		checkpointEvery = agent.Spec.Memory.CheckpointEvery
	}

	cfg := &router.Config{
		RunName:                deploy.Name,
		RunNamespace:           deploy.Namespace,
		AgentSAName:            saName,
		DisableClarify:         agent.Spec.DisableClarify,
		Providers:              providers,
		Strategy:               selector.Spec.Strategy,
		CapabilityRouting:      selector.Spec.CapabilityRouting,
		FallbackChain:          selector.Spec.FallbackChain,
		MetaRouterProviderName: metaRouterProvider,
		MetaRouterThreshold:    threshold,
		BudgetPerRunUSD:        budget,
		ToolDefinitions:        toolDefs,
		MCPServers:             mcpServers,
		CheckpointEvery:        checkpointEvery,
		CheckpointKey:          fmt.Sprintf("agentorc/deployments/%s/state", deploy.Name),
		StateConfig:            r.StateConfig,
		KubeAPIURL:             "https://kubernetes.default.svc",
		OperatorAPIURL:         r.OperatorAPIURL,
		SystemPrompt:           agent.Spec.SystemPrompt,
		KnowledgeBases:         kbConfigs,
		LongTermMemory:         longTermMemory,
	}



	// GuardrailPolicy CR: wire agent's guardrailPolicyRef into cfg.Guardrails.
	if agent.Spec.GuardrailPolicyRef != "" {
		mergeGuardrailPolicyIntoRouterConfig(ctx, r.Client, deploy.Namespace, agent.Spec.GuardrailPolicyRef, cfg)
	}

	return cfg, nil
}

// ensureDeploymentRouterConfigMap creates or updates the ConfigMap with the router config.
func (r *AgentDeploymentReconciler) ensureDeploymentRouterConfigMap(
	ctx context.Context,
	deploy *agentorcv1alpha1.AgentDeployment,
	cfg *router.Config,
) (string, error) {
	data, err := json.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("marshaling router config: %w", err)
	}

	sum := sha256.Sum256(data)
	configHash := hex.EncodeToString(sum[:])

	cmName := "agentorc-deploy-" + deploy.Name
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmName,
			Namespace: deploy.Namespace,
			Labels: map[string]string{
				security.LabelManagedBy: security.ManagedByValue,
			},
		},
		Data: map[string]string{
			podbuilder.RouterConfigKey: string(data),
		},
	}
	if err := ctrl.SetControllerReference(deploy, cm, r.Scheme); err != nil {
		return "", fmt.Errorf("setting configmap owner ref: %w", err)
	}

	// Try create; if it exists, update.
	if err := r.Create(ctx, cm); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return "", fmt.Errorf("creating router configmap: %w", err)
		}
		var existing corev1.ConfigMap
		if err := r.Get(ctx, types.NamespacedName{Name: cmName, Namespace: deploy.Namespace}, &existing); err != nil {
			return "", fmt.Errorf("getting existing router configmap: %w", err)
		}
		cmBase := existing.DeepCopy()
		existing.Data = cm.Data
		if err := r.Patch(ctx, &existing, client.MergeFrom(cmBase)); err != nil {
			return "", fmt.Errorf("updating router configmap: %w", err)
		}
	}
	return configHash, nil
}

// ensureDeploymentTokenSecret creates the per-deployment Secret with OPENAI_BASE_URL and OPENAI_API_KEY.
func (r *AgentDeploymentReconciler) ensureDeploymentTokenSecret(
	ctx context.Context,
	deploy *agentorcv1alpha1.AgentDeployment,
	saName string,
) (string, error) {
	secretName := "agentorc-deploy-" + deploy.Name + podbuilder.TokenSecretSuffix
	var existing corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: deploy.Namespace}, &existing); err == nil {
		return secretName, nil // already exists
	}

	expirySeconds := int64(900)
	tr := &authv1.TokenRequest{
		Spec: authv1.TokenRequestSpec{
			Audiences:         []string{security.ModelRouterTokenAudience},
			ExpirationSeconds: ptr.To(expirySeconds),
		},
	}
	result, err := r.K8s.CoreV1().ServiceAccounts(deploy.Namespace).CreateToken(ctx, saName, tr, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("creating SA token for deployment %s: %w", deploy.Name, err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: deploy.Namespace,
			Labels: map[string]string{
				security.LabelManagedBy: security.ManagedByValue,
			},
		},
		StringData: map[string]string{
			"OPENAI_BASE_URL": "http://localhost:8080/v1",
			"OPENAI_API_KEY":  result.Status.Token,
		},
	}
	if err := ctrl.SetControllerReference(deploy, secret, r.Scheme); err != nil {
		return "", fmt.Errorf("setting secret owner ref: %w", err)
	}
	if err := r.Create(ctx, secret); err != nil && !apierrors.IsAlreadyExists(err) {
		return "", fmt.Errorf("creating token secret: %w", err)
	}
	return secretName, nil
}

// ensureDeploymentTokenReviewerBinding creates a ClusterRoleBinding so the model-router
// sidecar can validate SA tokens via the TokenReview API.
func (r *AgentDeploymentReconciler) ensureDeploymentTokenReviewerBinding(
	ctx context.Context,
	deploy *agentorcv1alpha1.AgentDeployment,
	saName string,
) error {
	if r.TokenReviewerClusterRole == "" {
		return nil
	}
	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: "agentorc-deploy-" + deploy.Name,
			Labels: map[string]string{
				security.LabelManagedBy: security.ManagedByValue,
			},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     r.TokenReviewerClusterRole,
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      saName,
				Namespace: deploy.Namespace,
			},
		},
	}
	if err := r.Create(ctx, crb); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating token-reviewer ClusterRoleBinding: %w", err)
	}
	return nil
}

const (
	labelWarmPool     = "agentorc.io/warm-pool"
	labelWarmStatus   = "agentorc.io/warm-status"
	warmStatusIdle    = "idle"
	warmStatusClaimed = "claimed"
)

// reconcileWarmPool ensures the pre-warmed pod pool is at the desired size.
func (r *AgentDeploymentReconciler) reconcileWarmPool(
	ctx context.Context,
	deploy *agentorcv1alpha1.AgentDeployment,
	agent *agentorcv1alpha1.Agent,
	saName string,
	routerConfigHash string,
	routerCfg *router.Config,
	providerVolumes []corev1.Volume,
	providerMounts []corev1.VolumeMount,
	toolSecretVolumes []corev1.Volume,
	toolSecretMounts []corev1.VolumeMount,
	mcpBinVolumes []corev1.Volume,
	mcpBinMounts []corev1.VolumeMount,
) error {
	// Ensure deployment-scoped RBAC for warm pods.
	if err := r.ensureWarmPoolRBAC(ctx, deploy, saName); err != nil {
		return fmt.Errorf("ensuring warm pool RBAC: %w", err)
	}

	// Ensure the warm router ConfigMap exists.
	if err := r.ensureWarmRouterConfigMap(ctx, deploy, agent, routerCfg); err != nil {
		return fmt.Errorf("ensuring warm router configmap: %w", err)
	}

	// Delete any completed claimed warm pods. Claimed pods are owned by the AgentDeployment
	// (not the AgentRun), so they are not automatically GC'd when a run finishes. For
	// non-HTTP mode agents the process exits naturally, leaving the pod in Succeeded/Failed;
	// clean those up here before re-evaluating the idle pool.
	var claimedPods corev1.PodList
	if err := r.List(ctx, &claimedPods,
		client.InNamespace(deploy.Namespace),
		client.MatchingLabels{
			labelWarmPool:   deploy.Name,
			labelWarmStatus: warmStatusClaimed,
		},
	); err != nil {
		return fmt.Errorf("listing claimed warm pods: %w", err)
	}
	for i := range claimedPods.Items {
		p := &claimedPods.Items[i]
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			slog.Info("cleaning up completed claimed warm pod", "pod", p.Name, "phase", p.Status.Phase)
			_ = r.Delete(ctx, p)
		}
	}

	// List idle warm pods.
	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(deploy.Namespace),
		client.MatchingLabels{
			labelWarmPool:   deploy.Name,
			labelWarmStatus: warmStatusIdle,
		},
	); err != nil {
		return fmt.Errorf("listing warm pods: %w", err)
	}

	// Delete any warm pods that have completed, failed, or whose SA token has expired.
	// The token is minted at warm pod creation with a 1-hour expiry; pods older than
	// that will fail authentication when claimed, so replace them proactively.
	// Track ready pods (model-router startup probe passed) separately from starting pods.
	// Only ready pods can be claimed; starting pods still count toward the total to avoid
	// creating more pods than necessary while a replacement warms up.
	const warmTokenMaxAge = 50 * time.Minute // recycle before the 1h token expires
	var readyPods, startingPods []*corev1.Pod
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			_ = r.Delete(ctx, p)
			continue
		}
		if time.Since(p.CreationTimestamp.Time) > warmTokenMaxAge {
			slog.Info("recycling warm pod with expiring token",
				"pod", p.Name, "age", time.Since(p.CreationTimestamp.Time).Round(time.Second))
			_ = r.Delete(ctx, p)
			continue
		}
		// Delete warm pods whose config is stale (e.g. agent spec, router config,
		// or provider weights changed). This ensures a Deployment rollout also
		// refreshes the warm pool. Pods without the annotation are treated as stale
		// (they predate this feature).
		if podHash := p.Annotations["agentorc.io/router-config-hash"]; podHash != routerConfigHash {
			slog.Info("recycling warm pod with stale config",
				"pod", p.Name, "podHash", podHash, "currentHash", routerConfigHash)
			_ = r.Delete(ctx, p)
			continue
		}
		modelRouterReady := false
		for _, cs := range p.Status.InitContainerStatuses {
			if cs.Name == "model-router" && cs.Ready {
				modelRouterReady = true
				break
			}
		}
		if modelRouterReady {
			readyPods = append(readyPods, p)
		} else {
			startingPods = append(startingPods, p)
		}
	}

	// Scale down excess idle warm pods when the pool is larger than desired.
	desired := deploy.Spec.WarmPoolSize
	readyCount := len(readyPods)
	startingCount := len(startingPods)
	totalExisting := readyCount + startingCount

	if excess := totalExisting - desired; excess > 0 {
		// Delete oldest ready pods first (closest to token expiry, fungible).
		slices.SortFunc(readyPods, func(a, b *corev1.Pod) int {
			return a.CreationTimestamp.Time.Compare(b.CreationTimestamp.Time)
		})
		deleted := 0
		for deleted < excess && len(readyPods) > 0 {
			p := readyPods[0]
			readyPods = readyPods[1:]
			slog.Info("scaling down warm pool: deleting excess pod",
				"deployment", deploy.Name, "pod", p.Name, "desired", desired, "excess", excess)
			if err := r.Delete(ctx, p); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("deleting excess warm pod %s: %w", p.Name, err)
			}
			deleted++
		}
		for deleted < excess && len(startingPods) > 0 {
			p := startingPods[0]
			startingPods = startingPods[1:]
			slog.Info("scaling down warm pool: deleting excess starting pod",
				"deployment", deploy.Name, "pod", p.Name, "desired", desired, "excess", excess)
			if err := r.Delete(ctx, p); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("deleting excess warm pod %s: %w", p.Name, err)
			}
			deleted++
		}
		readyCount = len(readyPods)
		startingCount = len(startingPods)
	}

	// Create warm pods up to the desired pool size.
	// Each warm pod gets its own freshly-minted SA token so it is never blocked
	// by the deployment-level secret's 15-minute expiry.
	totalExisting = readyCount + startingCount
	for i := totalExisting; i < desired; i++ {
		warmTokenName, err := r.createWarmPodTokenSecret(ctx, deploy, saName)
		if err != nil {
			return fmt.Errorf("creating warm pod token secret: %w", err)
		}
		pod := r.buildWarmPod(deploy, agent, saName, warmTokenName, routerConfigHash, providerVolumes, providerMounts, toolSecretVolumes, toolSecretMounts, mcpBinVolumes, mcpBinMounts)
		if err := ctrl.SetControllerReference(deploy, pod, r.Scheme); err != nil {
			return fmt.Errorf("setting warm pod owner ref: %w", err)
		}
		if err := r.Create(ctx, pod); err != nil {
			return fmt.Errorf("creating warm pod: %w", err)
		}
		slog.Info("created warm pod", "deployment", deploy.Name, "pod", pod.Name)
		startingCount++
	}

	deploy.Status.WarmPoolReady = readyCount
	return nil
}

// ensureWarmPoolRBAC creates the deployment-scoped Role and RoleBinding for warm pods.
func (r *AgentDeploymentReconciler) ensureWarmPoolRBAC(
	ctx context.Context,
	deploy *agentorcv1alpha1.AgentDeployment,
	saName string,
) error {
	role := security.BuildDeploymentRole(deploy.Name, deploy.Namespace)
	if err := ctrl.SetControllerReference(deploy, role, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, role); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating deployment Role: %w", err)
	}

	rb := security.BuildDeploymentRoleBinding(deploy.Name, saName, deploy.Namespace)
	if err := ctrl.SetControllerReference(deploy, rb, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, rb); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating deployment RoleBinding: %w", err)
	}
	return nil
}

// ensureWarmRouterConfigMap creates the ConfigMap used by warm pods (WarmMode: true).
func (r *AgentDeploymentReconciler) ensureWarmRouterConfigMap(
	ctx context.Context,
	deploy *agentorcv1alpha1.AgentDeployment,
	agent *agentorcv1alpha1.Agent,
	baseCfg *router.Config,
) error {
	// Build warm config from the deployment config.
	warmCfg := *baseCfg
	warmCfg.WarmMode = true
	warmCfg.ChatMode = true
	// Set HTTPInput so it starts after claiming.
	port := agent.Spec.Runtime.InputPort
	if port == 0 {
		port = 8000
	}
	path := agent.Spec.Runtime.InputPath
	if path == "" {
		path = "/invoke"
	}
	warmCfg.HTTPInput = router.HTTPInputConfig{
		Enabled: true,
		Port:    port,
		Path:    path,
		// RunName and Input are empty; they will be set at claim time via ClaimRun().
	}

	data, err := json.Marshal(warmCfg)
	if err != nil {
		return fmt.Errorf("marshaling warm router config: %w", err)
	}

	cmName := "agentorc-warm-" + deploy.Name
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmName,
			Namespace: deploy.Namespace,
			Labels: map[string]string{
				security.LabelManagedBy: security.ManagedByValue,
			},
		},
		Data: map[string]string{
			podbuilder.RouterConfigKey: string(data),
		},
	}
	if err := ctrl.SetControllerReference(deploy, cm, r.Scheme); err != nil {
		return err
	}

	if err := r.Create(ctx, cm); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("creating warm router configmap: %w", err)
		}
		// Update existing.
		var existing corev1.ConfigMap
		if err := r.Get(ctx, types.NamespacedName{Name: cmName, Namespace: deploy.Namespace}, &existing); err != nil {
			return err
		}
		cmBase := existing.DeepCopy()
		existing.Data = cm.Data
		if err := r.Patch(ctx, &existing, client.MergeFrom(cmBase)); err != nil {
			return fmt.Errorf("updating warm router configmap: %w", err)
		}
	}
	return nil
}

// buildWarmPod constructs an idle warm pod for the given AgentDeployment.
// Built via the shared podbuilder package so all agent pod types use one codepath.
func (r *AgentDeploymentReconciler) buildWarmPod(
	deploy *agentorcv1alpha1.AgentDeployment,
	agent *agentorcv1alpha1.Agent,
	saName string,
	tokenSecretName string,
	routerConfigHash string,
	providerVolumes []corev1.Volume,
	providerMounts []corev1.VolumeMount,
	toolSecretVolumes []corev1.Volume,
	toolSecretMounts []corev1.VolumeMount,
	mcpBinVolumes []corev1.Volume,
	mcpBinMounts []corev1.VolumeMount,
) *corev1.Pod {
	inputSourceType := agentorcv1alpha1.InputSourceChat
	if deploy.Spec.InputSource != nil {
		inputSourceType = deploy.Spec.InputSource.Type
	}

	agentEnv := []corev1.EnvVar{
		{Name: "AGENTORC_AGENT", Value: agent.Name},
		{Name: "AGENTORC_NAMESPACE", Value: deploy.Namespace},
		{Name: "AGENTORC_INPUT_SOURCE", Value: string(inputSourceType)},
	}

	return podbuilder.Build(podbuilder.PodConfig{
		GenerateName: "warm-" + deploy.Name + "-",
		Namespace:    deploy.Namespace,
		Labels: map[string]string{
			"app.kubernetes.io/name":      "agent-orc",
			"app.kubernetes.io/component": "warm-pod",
			"agentdeployment.agentorc.io": deploy.Name,
			"agent.agentorc.io":           agent.Name,
			labelWarmPool:                 deploy.Name,
			labelWarmStatus:               warmStatusIdle,
			security.LabelManagedBy:       security.ManagedByValue,
		},
		Annotations: map[string]string{
			"agentorc.io/router-config-hash": routerConfigHash,
		},
		Agent:            agent,
		AgentEnv:         agentEnv,
		TokenSecretName:  tokenSecretName,
		ServiceAccount:   saName,
		RestartPolicy:    corev1.RestartPolicyNever,
		ModelRouterImage: r.ModelRouterImage,
		RouterConfigName: "agentorc-warm-" + deploy.Name,
		RouterExtraPorts: []corev1.ContainerPort{
			{Name: "warm-mgmt", ContainerPort: 9090, Protocol: corev1.ProtocolTCP},
		},
		RouterProbePort:   9090,
		ProviderVolumes:   providerVolumes,
		ProviderMounts:    providerMounts,
		ToolSecretVolumes: toolSecretVolumes,
		ToolSecretMounts:  toolSecretMounts,
		MCPBinVolumes:     mcpBinVolumes,
		MCPBinMounts:      mcpBinMounts,
		CloudProvider:     r.CloudProvider,
	})
}

// createWarmPodTokenSecret mints a fresh SA token for a single warm pod.
// Unlike the deployment-level token secret (created once, 15-min expiry), this is
// created fresh per warm pod so the token is never stale at the time the pod starts.
func (r *AgentDeploymentReconciler) createWarmPodTokenSecret(
	ctx context.Context,
	deploy *agentorcv1alpha1.AgentDeployment,
	saName string,
) (string, error) {
	expirySeconds := int64(3600) // 1 hour — warm pods are claimed and complete well within this
	tr := &authv1.TokenRequest{
		Spec: authv1.TokenRequestSpec{
			Audiences:         []string{security.ModelRouterTokenAudience},
			ExpirationSeconds: ptr.To(expirySeconds),
		},
	}
	result, err := r.K8s.CoreV1().ServiceAccounts(deploy.Namespace).CreateToken(ctx, saName, tr, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("creating SA token for warm pod: %w", err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "agentorc-warm-" + deploy.Name + "-token-",
			Namespace:    deploy.Namespace,
			Labels: map[string]string{
				labelWarmPool:           deploy.Name,
				security.LabelManagedBy: security.ManagedByValue,
			},
		},
		StringData: map[string]string{
			"OPENAI_BASE_URL": "http://localhost:8080/v1",
			"OPENAI_API_KEY":  result.Status.Token,
		},
	}
	if err := ctrl.SetControllerReference(deploy, secret, r.Scheme); err != nil {
		return "", err
	}
	if err := r.Create(ctx, secret); err != nil {
		return "", fmt.Errorf("creating warm pod token secret: %w", err)
	}
	return secret.Name, nil
}

// agentDeploymentsForAgent maps an Agent change to the AgentDeployments that reference it.
func (r *AgentDeploymentReconciler) agentDeploymentsForAgent(ctx context.Context, obj client.Object) []reconcile.Request {
	agent := obj.(*agentorcv1alpha1.Agent)
	var list agentorcv1alpha1.AgentDeploymentList
	if err := r.List(ctx, &list, client.InNamespace(agent.Namespace)); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, d := range list.Items {
		if d.Spec.AgentRef == agent.Name {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&d)})
		}
	}
	return reqs
}

// agentDeploymentsForModelSelector maps a ModelSelector change to affected AgentDeployments.
// The link is indirect: AgentDeployment → Agent → ModelSelector.
func (r *AgentDeploymentReconciler) agentDeploymentsForModelSelector(ctx context.Context, obj client.Object) []reconcile.Request {
	ms := obj.(*agentorcv1alpha1.ModelSelector)
	var deployList agentorcv1alpha1.AgentDeploymentList
	if err := r.List(ctx, &deployList, client.InNamespace(ms.Namespace)); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, d := range deployList.Items {
		var agent agentorcv1alpha1.Agent
		if err := r.Get(ctx, types.NamespacedName{Name: d.Spec.AgentRef, Namespace: d.Namespace}, &agent); err != nil {
			continue
		}
		if agent.Spec.ModelSelectorRef == ms.Name {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&d)})
		}
	}
	return reqs
}

// agentDeploymentsForModelProvider maps a ModelProvider change to affected AgentDeployments.
// The link is: AgentDeployment → Agent → ModelSelector → ModelProvider.
func (r *AgentDeploymentReconciler) agentDeploymentsForModelProvider(ctx context.Context, obj client.Object) []reconcile.Request {
	mp := obj.(*agentorcv1alpha1.ModelProvider)
	// Find ModelSelectors that reference this provider.
	var selectorList agentorcv1alpha1.ModelSelectorList
	if err := r.List(ctx, &selectorList, client.InNamespace(mp.Namespace)); err != nil {
		return nil
	}
	affectedSelectors := map[string]bool{}
	for _, ms := range selectorList.Items {
		for _, pw := range ms.Spec.Providers {
			if pw.Name == mp.Name {
				affectedSelectors[ms.Name] = true
				break
			}
		}
		if ms.Spec.MetaRouter != nil && ms.Spec.MetaRouter.ProviderRef == mp.Name {
			affectedSelectors[ms.Name] = true
		}
	}
	if len(affectedSelectors) == 0 {
		return nil
	}
	// Find AgentDeployments whose Agent uses one of those selectors.
	var deployList agentorcv1alpha1.AgentDeploymentList
	if err := r.List(ctx, &deployList, client.InNamespace(mp.Namespace)); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, d := range deployList.Items {
		var agent agentorcv1alpha1.Agent
		if err := r.Get(ctx, types.NamespacedName{Name: d.Spec.AgentRef, Namespace: d.Namespace}, &agent); err != nil {
			continue
		}
		if affectedSelectors[agent.Spec.ModelSelectorRef] {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&d)})
		}
	}
	return reqs
}

// agentDeploymentsForTool maps a Tool change to AgentDeployments whose Agent references it.
func (r *AgentDeploymentReconciler) agentDeploymentsForTool(ctx context.Context, obj client.Object) []reconcile.Request {
	tool := obj.(*agentorcv1alpha1.Tool)
	// Find Agents that list this tool.
	var agentList agentorcv1alpha1.AgentList
	if err := r.List(ctx, &agentList, client.InNamespace(tool.Namespace)); err != nil {
		return nil
	}
	affectedAgents := map[string]bool{}
	for _, ag := range agentList.Items {
		for _, t := range ag.Spec.Tools {
			if t == tool.Name {
				affectedAgents[ag.Name] = true
				break
			}
		}
	}
	if len(affectedAgents) == 0 {
		return nil
	}
	var deployList agentorcv1alpha1.AgentDeploymentList
	if err := r.List(ctx, &deployList, client.InNamespace(tool.Namespace)); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, d := range deployList.Items {
		if affectedAgents[d.Spec.AgentRef] {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&d)})
		}
	}
	return reqs
}

// agentDeploymentsForKnowledgeBase maps a KnowledgeBase change to AgentDeployments whose Agent references it.
func (r *AgentDeploymentReconciler) agentDeploymentsForKnowledgeBase(ctx context.Context, obj client.Object) []reconcile.Request {
	kb := obj.(*agentorcv1alpha1.KnowledgeBase)
	var agentList agentorcv1alpha1.AgentList
	if err := r.List(ctx, &agentList, client.InNamespace(kb.Namespace)); err != nil {
		return nil
	}
	affectedAgents := map[string]bool{}
	for _, ag := range agentList.Items {
		for _, k := range ag.Spec.KnowledgeBases {
			if k == kb.Name {
				affectedAgents[ag.Name] = true
				break
			}
		}
	}
	if len(affectedAgents) == 0 {
		return nil
	}
	var deployList agentorcv1alpha1.AgentDeploymentList
	if err := r.List(ctx, &deployList, client.InNamespace(kb.Namespace)); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, d := range deployList.Items {
		if affectedAgents[d.Spec.AgentRef] {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&d)})
		}
	}
	return reqs
}

// SetupWithManager sets up the controller with the Manager.
func (r *AgentDeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Recorder = mgr.GetEventRecorderFor("agentdeployment")
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentorcv1alpha1.AgentDeployment{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Pod{}).
		Watches(&agentorcv1alpha1.Agent{}, handler.EnqueueRequestsFromMapFunc(r.agentDeploymentsForAgent)).
		Watches(&agentorcv1alpha1.ModelSelector{}, handler.EnqueueRequestsFromMapFunc(r.agentDeploymentsForModelSelector)).
		Watches(&agentorcv1alpha1.ModelProvider{}, handler.EnqueueRequestsFromMapFunc(r.agentDeploymentsForModelProvider)).
		Watches(&agentorcv1alpha1.Tool{}, handler.EnqueueRequestsFromMapFunc(r.agentDeploymentsForTool)).
		Watches(&agentorcv1alpha1.KnowledgeBase{}, handler.EnqueueRequestsFromMapFunc(r.agentDeploymentsForKnowledgeBase)).
		Complete(r)
}
