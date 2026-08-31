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
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentorcav1alpha1 "github.com/floppyfish14/agent-orca/api/v1alpha1"
	"github.com/floppyfish14/agent-orca/internal/egress"
	"github.com/floppyfish14/agent-orca/internal/podbuilder"
	"github.com/floppyfish14/agent-orca/internal/postgresql"
	"github.com/floppyfish14/agent-orca/internal/router"
	"github.com/floppyfish14/agent-orca/internal/security"
	"github.com/floppyfish14/agent-orca/internal/state"
)

const (
	agentRunFinalizer = "agentorca.io/agentrun-cleanup"
)

// AgentRunReconciler reconciles a AgentRun object.
type AgentRunReconciler struct {
	client.Client
	Scheme           *runtime.Scheme
	Recorder         record.EventRecorder
	K8s              kubernetes.Interface
	Dynamic          dynamic.Interface
	ModelRouterImage string
	StateConfig      state.Config
	StateStore       state.Store
	CloudProvider    security.CloudProvider
	// PostgresStore is the optional PostgreSQL archival store. When non-nil,
	// terminal-phase AgentRuns are snapshotted here for long-term retention
	// and historical querying by the UI.
	PostgresStore *postgresql.Store
	// TokenReviewerClusterRole is the name of the ClusterRole that grants
	// "create tokenreviews" — injected at startup from the Helm release name.
	// The agentrun controller binds each agent SA to this role so the
	// model-router sidecar can call the TokenReview API.
	TokenReviewerClusterRole string
	// OperatorAPIURL is the base URL of the operator's internal API server,
	// injected from the OPERATOR_API_URL env var. Written into every router
	// ConfigMap so sidecars can call /agentrun endpoints for handoff and child runs.
	// Example: "http://agent-orca-internal-api.agent-orca-system.svc.cluster.local:8082"
	OperatorAPIURL string
	// LLMRequestTimeout is the per-LLM-request timeout written into every router
	// config (default 1h). Injected from the LLM_REQUEST_TIMEOUT env var.
	LLMRequestTimeout time.Duration
}

// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=agentorca.agentorca.io,resources=agentruns,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agentorca.agentorca.io,resources=agentruns/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentorca.agentorca.io,resources=agentruns/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=pods/log,verbs=get
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=serviceaccounts/token,verbs=create
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings;clusterrolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cilium.io,resources=tracingpoliciesnamespaced,verbs=get;list;watch;create;update;patch;delete

func (r *AgentRunReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var run agentorcav1alpha1.AgentRun
	if err := r.Get(ctx, req.NamespacedName, &run); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Handle deletion first — even terminal runs must have their finalizer removed.
	if !run.DeletionTimestamp.IsZero() {
		return r.handleRunDeletion(ctx, &run)
	}

	base := run.DeepCopy()
	_ = base // used below for status patches

	// Terminal — ensure cleanup has run then stop reconciling.
	if isTerminal(run.Status.Phase) {
		return r.ensureCleanup(ctx, &run)
	}

	// Ensure finalizer.
	if !controllerutil.ContainsFinalizer(&run, agentRunFinalizer) {
		controllerutil.AddFinalizer(&run, agentRunFinalizer)
		return ctrl.Result{Requeue: true}, r.Update(ctx, &run)
	}

	// Fetch the referenced Agent.
	var agent agentorcav1alpha1.Agent
	if err := r.Get(ctx, client.ObjectKey{Name: run.Spec.AgentRef, Namespace: run.Namespace}, &agent); err != nil {
		if errors.IsNotFound(err) {
			return r.failRun(ctx, &run, fmt.Sprintf("agent %q not found", run.Spec.AgentRef))
		}
		return ctrl.Result{}, err
	}

	saName := security.AgentSAName(agent.Name)
	if agent.Spec.ServiceAccountRef != nil {
		saName = agent.Spec.ServiceAccountRef.Name
	}

	switch run.Status.Phase {
	case "", agentorcav1alpha1.AgentRunPhasePending:
		return r.startRun(ctx, &run, &agent, saName)
	case agentorcav1alpha1.AgentRunPhaseRunning:
		return r.checkProgress(ctx, &run, &agent)
	case agentorcav1alpha1.AgentRunPhaseWaitingForInput:
		return r.checkClarifyTimeout(ctx, &run)
	}

	logger.V(1).Info("agentrun reconciled", "phase", run.Status.Phase)
	return ctrl.Result{}, nil
}

// startRun creates all per-run resources and spawns the agent pod.
func (r *AgentRunReconciler) startRun(ctx context.Context, run *agentorcav1alpha1.AgentRun, agent *agentorcav1alpha1.Agent, saName string) (ctrl.Result, error) { //nolint:gocyclo

	logger := log.FromContext(ctx)
	logger.Info("starting AgentRun", "run", run.Name)

	// Mark Pending immediately so we don't re-enter startRun on requeue.
	if run.Status.Phase == "" {
		if err := r.patchPhase(ctx, run, agentorcav1alpha1.AgentRunPhasePending); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// 1a. Resolve MCP sidecar image volumes (must run before buildRouterConfig
	// so the binary path rewrites are available for the router config).
	mcpBinVolumes, mcpBinMounts, binPathRewrites, err := podbuilder.ResolveMCPSidecarVolumes(ctx, r.Client, run.Namespace, agent)
	if err != nil {
		return r.failRun(ctx, run, fmt.Sprintf("resolving MCP sidecar volumes: %v", err))
	}

	// 1b. Resolve ModelSelector + tools → build router config.
	routerCfg, toolEgressRules, hasAgentTools, err := r.buildRouterConfig(ctx, run, agent, saName, binPathRewrites)
	if err != nil {
		if errors.IsNotFound(err) {
			// Referenced resource (ModelSelector, ModelProvider, Tool) doesn't exist yet —
			// requeue and wait rather than permanently failing the run.
			logger.Info("router config dependency not ready, retrying", "err", err)
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		return r.failRun(ctx, run, fmt.Sprintf("building router config: %v", err))
	}

	// Determine topology once for use across all steps.
	splitPod := agent.Spec.NetworkIsolation != nil && agent.Spec.NetworkIsolation.SplitPod

	// 2. Create per-run RBAC.
	if err := r.ensureRole(ctx, run); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureRoleBinding(ctx, run, saName); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureTokenReviewerBinding(ctx, run, saName); err != nil {
		return ctrl.Result{}, err
	}

	// 3. Create NetworkPolicy (or two policies in split-pod topology).
	hasKnowledgeBases := len(agent.Spec.KnowledgeBases) > 0
	if splitPod {
		if err := r.ensureSplitPodNetworkPolicies(ctx, run, toolEgressRules); err != nil {
			return ctrl.Result{}, err
		}
	} else {
		if err := r.ensureNetworkPolicy(ctx, run, toolEgressRules, hasAgentTools, hasKnowledgeBases); err != nil {
			return ctrl.Result{}, err
		}
	}

	// 4. Create TracingPolicy (optional — skipped if Tetragon not installed).
	if err := r.ensureTracingPolicy(ctx, run); err != nil {
		// Tetragon is optional; log and continue.
		logger.V(1).Info("skipping TracingPolicy (Tetragon not available)", "err", err)
	}

	// 5. Create RouterConfig ConfigMap.
	if err := r.ensureRouterConfigMap(ctx, run, routerCfg); err != nil {
		return ctrl.Result{}, err
	}

	// 6. Create per-run SA token Secret (via TokenRequest API).
	// In split-pod topology, OPENAI_BASE_URL points at the router Service.
	routerServiceURL := "http://localhost:8080/v1"
	if splitPod {
		routerServiceURL = "http://agentorca-router-" + run.Name + "." + run.Namespace + ":8080/v1"
	}
	tokenSecretName, err := r.ensureTokenSecret(ctx, run, saName, routerServiceURL)
	if err != nil {
		return ctrl.Result{}, err
	}

	// 7. Resolve provider secrets for volume mounts.
	providerVolumes, providerMounts, err := podbuilder.ResolveProviderVolumes(ctx, r.Client, run.Namespace, agent)
	if err != nil {
		return r.failRun(ctx, run, fmt.Sprintf("resolving provider volumes: %v", err))
	}

	// 7b. Resolve tool secret volumes (MCP envFrom, auth, and secretRefs).
	toolSecretVolumes, toolSecretMounts, err := podbuilder.ResolveToolSecretVolumes(ctx, r.Client, run.Namespace, agent)
	if err != nil {
		return r.failRun(ctx, run, fmt.Sprintf("resolving tool secret volumes: %v", err))
	}

	// 8. Build and create the agent pod (or claim a pre-warmed pod if available).
	// In split-pod topology, also create the router Service and router pod.

	// Record this run in its deployment's run index so the model-router (and the
	// _search_history tool) can enumerate prior runs for THIS deployment without
	// scanning the whole Redis keyspace. Best-effort and non-fatal: a missed index
	// entry just narrows the recall of prior-turn search.
	r.recordRunInDeploymentIndex(ctx, run)
	var podName string
	var routerPodName string

	if splitPod {
		// Split-pod: create router Service first, then router pod, then agent pod.
		// Warm pool is not supported in split-pod topology (combined pod only).
		if err := r.ensureRouterService(ctx, run); err != nil {
			return ctrl.Result{}, err
		}

		// Router pod: model-router as standalone container with provider/tool mounts.
		routerBaseURL := "http://agentorca-router-" + run.Name + "." + run.Namespace + ":8080"
		routerPod := r.buildRouterPod(run, agent, saName, providerVolumes, providerMounts, toolSecretVolumes, toolSecretMounts, mcpBinVolumes, mcpBinMounts)
		if err := ctrl.SetControllerReference(run, routerPod, r.Scheme); err != nil {
			return r.failRun(ctx, run, fmt.Sprintf("setting router pod owner ref: %v", err))
		}
		if err := r.Create(ctx, routerPod); err != nil && !errors.IsAlreadyExists(err) {
			return ctrl.Result{}, fmt.Errorf("creating router pod: %w", err)
		}
		routerPodName = routerPod.Name

		// Agent pod: agent container only, pointing OPENAI_BASE_URL at the router Service.
		agentPod := r.buildAgentOnlyPod(run, agent, saName, tokenSecretName, routerBaseURL)
		if err := ctrl.SetControllerReference(run, agentPod, r.Scheme); err != nil {
			return r.failRun(ctx, run, fmt.Sprintf("setting agent pod owner ref: %v", err))
		}
		if err := r.Create(ctx, agentPod); err != nil && !errors.IsAlreadyExists(err) {
			return ctrl.Result{}, fmt.Errorf("creating agent pod: %w", err)
		}
		podName = agentPod.Name
	} else {
		if claimedPod, claimErr := r.claimWarmPod(ctx, run, agent); claimErr != nil {
			logger.Error(claimErr, "warm pod claim failed, falling back to new pod")
		} else if claimedPod != "" {
			logger.Info("claimed warm pod", "pod", claimedPod)
			podName = claimedPod
		}

		if podName == "" {
			// A prior reconcile may have bound this run to a warm pod even though
			// the claim POST response was lost (e.g. client-side timeout). Reuse
			// that warm pod instead of spawning a second agent pod, which would
			// create a split-brain two agents for one run.
			if reuse := r.findClaimedWarmPod(ctx, run); reuse != "" {
				podName = reuse
				logger.Info("reusing warm pod already bound to run", "pod", podName, "run", run.Name)
			}
		}
		if podName == "" {
			pod := r.buildAgentPod(run, agent, saName, tokenSecretName, providerVolumes, providerMounts, toolSecretVolumes, toolSecretMounts, mcpBinVolumes, mcpBinMounts)
			if err := ctrl.SetControllerReference(run, pod, r.Scheme); err != nil {
				return r.failRun(ctx, run, fmt.Sprintf("setting pod owner ref: %v", err))
			}
			if err := r.Create(ctx, pod); err != nil && !errors.IsAlreadyExists(err) {
				return ctrl.Result{}, fmt.Errorf("creating agent pod: %w", err)
			}
			podName = pod.Name
		}
	}

	// 9. Update status to Running with initial routing decision.
	patch := client.MergeFrom(run.DeepCopy())
	run.Status.Phase = agentorcav1alpha1.AgentRunPhaseRunning
	run.Status.PodName = podName
	run.Status.RouterPodName = routerPodName
	now := metav1.Now()
	run.Status.StartTime = &now
	run.Status.CheckpointRef = fmt.Sprintf("agentorca/runs/%s/state", run.Name)
	run.Status.InputMode = agent.Spec.Runtime.InputMode

	// Record the initial routing configuration on first start only (not restarts).
	// Only include providers with weight > 0 — weight=0 providers are reserved for
	// meta-router escalation and error fallback, not random selection, so listing them
	// here as routing decisions would be misleading.
	if run.Status.RestartCount == 0 {
		for _, p := range routerCfg.Providers {
			if p.Weight <= 0 {
				continue
			}
			run.Status.RoutingDecisions = append(run.Status.RoutingDecisions, agentorcav1alpha1.RoutingDecision{
				Model:      p.LiteLLMModel,
				Provider:   p.Name,
				Strategy:   routerCfg.Strategy,
				Reason:     fmt.Sprintf("configured provider (weight=%d)", p.Weight),
				Confidence: fmt.Sprintf("%.2f", float64(p.Weight)/100.0),
				Timestamp:  &now,
			})
		}
	}

	if err := r.Status().Patch(ctx, run, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("patching run status to Running: %w", err)
	}
	logger.Info("agent pod started", "pod", podName)
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// checkProgress polls the agent pod and advances the AgentRun phase when complete.
func (r *AgentRunReconciler) checkProgress(ctx context.Context, run *agentorcav1alpha1.AgentRun, agent *agentorcav1alpha1.Agent) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if run.Status.PodName == "" {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	var pod corev1.Pod
	if err := r.Get(ctx, client.ObjectKey{Name: run.Status.PodName, Namespace: run.Namespace}, &pod); err != nil {
		if errors.IsNotFound(err) {
			// Pod was deleted externally. If this is an http-mode run that already
			// succeeded (we deleted the pod ourselves), there's nothing to do.
			if run.Status.InputMode == "http" && run.Status.Phase == agentorcav1alpha1.AgentRunPhaseSucceeded { //nolint:goconst

				return ctrl.Result{}, nil
			}
			// Otherwise treat as failure and possibly retry.
			return r.handlePodFailure(ctx, run, agent, "pod not found")
		}
		return ctrl.Result{}, err
	}

	// HTTP-mode agents are long-running servers that never exit on their own.
	// Poll the state store for output instead of waiting for PodSucceeded.
	if run.Status.InputMode == "http" && pod.Status.Phase == corev1.PodRunning && r.StateStore != nil {
		httpOut, err := r.StateStore.LoadHTTPOutput(ctx, run.Name)
		if err == nil && httpOut != "" {
			result, err := r.handlePodSuccess(ctx, run, &pod)
			// Only destroy ONE-SHOT pods here so they don't linger as Running.
			// WARM pods must be left alive: ensureCleanup -> reconcileRunPodOnTerminal
			// (run on the terminal reconcile) returns them to the idle pool so they
			// survive across chat turns and long-trajectory tasks. Deleting warm
			// pods here — as the old unconditional delete did — defeated reuse and
			// could murder an in-progress session, since the pod is removed before
			// the return-to-idle logic ever runs.
			if err == nil && shouldDeletePodOnCompletion(&pod) {
				_ = r.Delete(ctx, &pod)
			}
			return result, err
		}
	}

	switch pod.Status.Phase {
	case corev1.PodSucceeded:
		return r.handlePodSuccess(ctx, run, &pod)
	case corev1.PodFailed:
		return r.handlePodFailure(ctx, run, agent, podFailureReason(&pod))
	default:
		logger.V(2).Info("pod still running", "pod", pod.Name, "phase", pod.Status.Phase)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
}

func (r *AgentRunReconciler) handlePodSuccess(ctx context.Context, run *agentorcav1alpha1.AgentRun, pod *corev1.Pod) (ctrl.Result, error) {
	// Re-fetch the run to handle the race where the model-router set WaitingForInput
	// via the internal API just before the pod exited. If the phase is already
	// WaitingForInput, the pod exit is expected — don't transition to Succeeded.
	var latest agentorcav1alpha1.AgentRun
	if err := r.Get(ctx, client.ObjectKeyFromObject(run), &latest); err == nil {
		if latest.Status.Phase == agentorcav1alpha1.AgentRunPhaseWaitingForInput {
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		// Explicit _done/_fail: the model-router already set the terminal phase via
		// the internal API. The pod exit is expected — skip re-processing.
		if latest.Status.Phase == agentorcav1alpha1.AgentRunPhaseSucceeded ||
			latest.Status.Phase == agentorcav1alpha1.AgentRunPhaseFailed {
			return ctrl.Result{}, nil
		}
		// Safeguard trip: the model-router set LoopDetected via the internal API
		// just before returning 410 Gone to the agent process. Fail the run.
		if latest.Status.LoopDetected != nil {
			return r.failRun(ctx, run, "safeguard: "+latest.Status.LoopDetected.Reason)
		}
	}

	// Collect stdout from the agent container.
	logs, err := r.K8s.CoreV1().Pods(run.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
		Container: "agent",
	}).DoRaw(ctx)
	if err != nil {
		logs = []byte("(log collection failed)")
	}

	output := string(logs)
	if run.Status.InputMode == "http" {
		if httpOut, err := r.StateStore.LoadHTTPOutput(ctx, run.Name); err == nil && httpOut != "" {
			output = httpOut
		}
	}
	raw := truncate(output, 4096)
	finalOutput := truncate(output, 10240)

	// Load spend from the state store (persisted by the model-router on each LLM call).
	spendUSD := r.loadSpendFromStore(ctx, run.Name)

	// Safety net: if the output looks like a question directed at the user (e.g.
	// "Please specify the data source..."), the LLM failed to call _clarify.
	// Intercept here and set WaitingForInput instead of Succeeded so the human
	// gets a chance to answer. This is the last reliable intercept point — it
	// works regardless of streaming, provider, or agent framework.
	if looksLikeClarifyQuestion(finalOutput) {
		logger := ctrl.LoggerFrom(ctx)
		logger.Info("output looks like a clarifying question, setting WaitingForInput", "run", run.Name)

		patch := client.MergeFrom(run.DeepCopy())
		now := metav1.Now()
		run.Status.Phase = agentorcav1alpha1.AgentRunPhaseWaitingForInput
		run.Status.ClarifyQuestion = finalOutput
		run.Status.ClarifyAnswer = ""
		run.Status.WaitingSince = &now
		run.Status.SpendUSD = spendUSD
		run.Status.PodName = ""
		if err := r.Status().Patch(ctx, run, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("patching WaitingForInput status: %w", err)
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	patch := client.MergeFrom(run.DeepCopy())
	run.Status.Phase = agentorcav1alpha1.AgentRunPhaseSucceeded
	run.Status.Output = finalOutput
	run.Status.RawOutput = raw
	run.Status.SpendUSD = spendUSD
	now := metav1.Now()
	run.Status.CompletionTime = &now
	if err := r.Status().Patch(ctx, run, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("patching succeeded status: %w", err)
	}

	// Fire completion callback if configured.
	r.fireCallback(ctx, run)

	// Publish result to egress sink if configured.
	r.fireEgress(ctx, run)

	// Archive terminal-phase run to PostgreSQL for historical querying.
	r.maybeArchiveRun(ctx, run)

	return ctrl.Result{}, nil
}

// looksLikeClarifyQuestion returns true if the text appears to be a question or
// request for user input rather than a substantive answer. Used as a safety net
// when the LLM fails to call the _clarify tool and instead outputs a question as text.
func looksLikeClarifyQuestion(text string) bool {
	trimmed := strings.TrimSpace(text)
	if len(trimmed) == 0 {
		return false
	}
	lower := strings.ToLower(trimmed)

	// Strong signals: imperative requests for user input, regardless of punctuation.
	strongPhrases := []string{
		"please specify", "please provide", "please clarify",
		"please confirm", "please share", "please indicate",
		"please let me know", "i need to know",
		"i need you to", "i need more information",
		"to proceed, i need", "before i can proceed",
		"could you provide", "could you specify", "could you clarify",
		"can you provide", "can you specify", "can you clarify",
	}
	for _, phrase := range strongPhrases {
		if strings.Contains(lower, phrase) {
			return true
		}
	}

	// Weaker signals: ends with "?" and contains a question phrase.
	if strings.HasSuffix(trimmed, "?") {
		questionPhrases := []string{
			"could you", "can you", "would you", "do you",
			"what would you", "which one", "what is the",
			"would you like me to", "shall i",
			"what specific", "where should", "how would you like",
		}
		for _, phrase := range questionPhrases {
			if strings.Contains(lower, phrase) {
				return true
			}
		}
	}

	return false
}

func (r *AgentRunReconciler) handlePodFailure(ctx context.Context, run *agentorcav1alpha1.AgentRun, agent *agentorcav1alpha1.Agent, reason string) (ctrl.Result, error) { //nolint:unparam

	// Enrich the failure reason with container logs before the pod is deleted.
	if run.Status.PodName != "" {
		reason = r.enrichFailureReason(ctx, run.Namespace, run.Status.PodName, reason)
	}

	maxRetries := 3
	if run.Spec.RestartPolicy != nil {
		maxRetries = run.Spec.RestartPolicy.MaxRetries
	}

	if run.Status.RestartCount < maxRetries {
		// Delete the failed pod and re-trigger startRun by resetting to Pending.
		if run.Status.PodName != "" {
			_ = r.K8s.CoreV1().Pods(run.Namespace).Delete(ctx, run.Status.PodName, metav1.DeleteOptions{})
		}
		patch := client.MergeFrom(run.DeepCopy())
		run.Status.RestartCount++
		run.Status.LastRestartReason = reason
		run.Status.PodName = ""
		run.Status.Phase = agentorcav1alpha1.AgentRunPhasePending
		// Set checkpoint so the model-router sidecar resumes from last state.
		run.Status.CheckpointRef = fmt.Sprintf("agentorca/runs/%s/state", run.Name)
		if err := r.Status().Patch(ctx, run, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("patching restart status: %w", err)
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	return r.failRun(ctx, run, fmt.Sprintf("exceeded max retries (%d): %s", maxRetries, reason))
}

// buildRouterConfig resolves ModelSelector + Tools and returns a router.Config JSON.
func (r *AgentRunReconciler) buildRouterConfig( //nolint:gocyclo

	ctx context.Context,
	run *agentorcav1alpha1.AgentRun,
	agent *agentorcav1alpha1.Agent,
	saName string,
	binPathRewrites map[string]string,
) (*router.Config, []agentorcav1alpha1.EgressRule, bool, error) {
	// Resolve ModelSelector.
	var selector agentorcav1alpha1.ModelSelector
	if err := r.Get(ctx, client.ObjectKey{Name: agent.Spec.ModelSelectorRef, Namespace: run.Namespace}, &selector); err != nil {
		return nil, nil, false, fmt.Errorf("getting ModelSelector %q: %w", agent.Spec.ModelSelectorRef, err)
	}

	var providers []router.ProviderConfig
	seenProviders := make(map[string]bool)
	for _, pw := range selector.Spec.Providers {
		var mp agentorcav1alpha1.ModelProvider
		if err := r.Get(ctx, client.ObjectKey{Name: pw.Name, Namespace: run.Namespace}, &mp); err != nil {
			return nil, nil, false, fmt.Errorf("getting ModelProvider %q: %w", pw.Name, err)
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
		var mp agentorcav1alpha1.ModelProvider
		if err := r.Get(ctx, client.ObjectKey{Name: name, Namespace: run.Namespace}, &mp); err != nil {
			if errors.IsNotFound(err) {
				// Provider is listed in the fallback chain but not deployed (e.g. disabled
				// in the model-providers chart). Skip it rather than blocking the run.
				if r.Recorder != nil {
					r.Recorder.Eventf(run, corev1.EventTypeWarning, "FallbackProviderSkipped",
						"fallback provider %q not deployed; skipped from chain", name)
				}
				continue
			}
			return nil, nil, false, fmt.Errorf("getting fallback ModelProvider %q: %w", name, err)
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

	// Resolve Tools.
	var toolDefs []router.ToolDefinition
	var egressRules []agentorcav1alpha1.EgressRule

	// Add egress rules for model provider endpoints.
	for _, p := range providers {
		if port := providerPort(p.BaseURL, p.LiteLLMModel); port != 443 {
			egressRules = append(egressRules, agentorcav1alpha1.EgressRule{Port: int32(port), Protocol: "TCP"})
		}
	}
	var hasAgentTools bool
	var mcpServers []router.MCPServerConfig
	seenMCPServers := make(map[string]bool) // dedup MCPServer-managed tools by server name
	// Builtin tools are injected automatically and don't exist as Tool CRDs
	builtinTools := map[string]bool{
		"_spawn": true, "_handoff": true, "_clarify": true, "_done": true,
		"_fail": true, "_emit_event": true, "_rag_ingest": true, "_rag_search": true,
		"_write_state": true, "_read_state": true, "_list_state": true, "_delete_state": true,
		"_memory_store": true, "_mcp_read_resource": true, "_propose_step": true,
		"_propose_fix": true, "_confirm_fix": true, "_create_workflow": true, "_list_resources": true,
		"_search_history": true,
	}
	for _, toolName := range agent.Spec.Tools {
		// Skip builtin tools - they're injected separately in the router
		if builtinTools[toolName] {
			continue
		}
		var tool agentorcav1alpha1.Tool
		if err := r.Get(ctx, client.ObjectKey{Name: toolName, Namespace: run.Namespace}, &tool); err != nil {
			return nil, nil, false, fmt.Errorf("getting Tool %q: %w", toolName, err)
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
		if tool.Spec.Type == agentorcav1alpha1.ToolTypeAgent {
			backendRef = tool.Spec.AgentRef
			hasAgentTools = true
		}
		if tool.Spec.Type == agentorcav1alpha1.ToolTypeMCP && tool.Spec.MCPConfig != nil {
			// MCPServer-managed tools share the same MCP server connection.
			// Deduplicate by the MCPServer name so we only connect once per server.
			if serverName := tool.Labels[LabelMCPServer]; serverName != "" && seenMCPServers[serverName] {
				// Already created an MCPServerConfig for this MCPServer.
			} else {
				if serverName != "" {
					seenMCPServers[serverName] = true
				}
				mcpCfg := router.MCPServerConfig{
					Name:      toolName,
					Transport: tool.Spec.MCPConfig.Transport,
					URL:       tool.Spec.MCPConfig.URL,
				}
				if serverName != "" {
					var mcpServer agentorcav1alpha1.MCPServer
					if err := r.Get(ctx, client.ObjectKey{Name: serverName, Namespace: run.Namespace}, &mcpServer); err == nil {
						mcpCfg.AllowApps = mcpServer.Spec.AllowApps
					}
				}
				args := tool.Spec.MCPConfig.Args
				if tool.Spec.MCPConfig.Transport == "stdio" && len(args) > 0 { //nolint:goconst

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
				// Resolve auth config to file paths.
				if auth := tool.Spec.MCPConfig.Auth; auth != nil {
					if auth.BearerToken != nil {
						ref := auth.BearerToken
						mcpCfg.AuthHeaderFiles = append(mcpCfg.AuthHeaderFiles, router.AuthHeaderFile{
							HeaderName: "Authorization",
							FilePath:   podbuilder.ToolSecretFilePath(toolName, ref.Name, ref.Key),
							Prefix:     "Bearer ",
						})
					}
					if auth.APIKey != nil {
						ref := &auth.APIKey.SecretKeyRef
						headerName := auth.APIKey.HeaderName
						if headerName == "" {
							headerName = "X-API-Key"
						}
						mcpCfg.AuthHeaderFiles = append(mcpCfg.AuthHeaderFiles, router.AuthHeaderFile{
							HeaderName: headerName,
							FilePath:   podbuilder.ToolSecretFilePath(toolName, ref.Name, ref.Key),
						})
					}
					for _, h := range auth.Headers {
						ref := &h.SecretKeyRef
						mcpCfg.AuthHeaderFiles = append(mcpCfg.AuthHeaderFiles, router.AuthHeaderFile{
							HeaderName: h.Name,
							FilePath:   podbuilder.ToolSecretFilePath(toolName, ref.Name, ref.Key),
						})
					}
				}
				mcpServers = append(mcpServers, mcpCfg)
			}
		}
		// Populate secret refs for pod-type tools.
		var secretRefs []router.ToolSecretRef
		for _, sr := range tool.Spec.SecretRefs {
			secretRefs = append(secretRefs, router.ToolSecretRef{
				SecretName: sr.Name,
				MountPath:  sr.MountPath,
			})
		}
		toolDefs = append(toolDefs, router.ToolDefinition{
			Name:        toolName,
			Description: desc,
			Parameters:  params,
			BackendType: backendType,
			BackendRef:  backendRef,
			SecretRefs:  secretRefs,
			Command:     tool.Spec.Command,
			Args:        tool.Spec.Args,
		})
		egressRules = append(egressRules, tool.Spec.NetworkEgress...)
	}

	// Inject built-in tools so the LLM can see and invoke them.
	// Child runs (spawned via agent-as-tool) have a restricted set:
	//   - _handoff: meaningless — transfers control away; child must return a value to the caller
	//   - _clarify: deadlocks — blocks waiting for a human who has no channel into a child run
	//   - _emit_event: causes model confusion — LLMs conflate "notify" semantics with emitting an
	//     event and treat it as task completion instead of doing the actual work
	//   - _spawn: omitted for simplicity; child runs are expected to be leaf workers
	// Top-level runs get the full set.
	isChildRun := run.Spec.ParentRunRef != ""
	if !isChildRun {
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
			Name:        "_search_history",
			Description: "Search prior chat turns for this deployment when the information you need is not in your current context. The model-router looks through the local warm-pod cache (an emptyDir mirroring checkpoints) first, then the shared Redis store, scoped to this deployment's prior runs. Use this BEFORE asking the human via _clarify when the answer may already exist in a previous conversation. If found=false, the human must be asked.",
			Parameters:  []byte(`{"type":"object","properties":{"query":{"type":"string","minLength":1,"description":"The specific facts or topics to search prior conversation turns for. Use concrete terms, not vague phrases."},"topK":{"type":"integer","default":3,"description":"Maximum number of matching turn snippets to return"}},"required":["query"]}`),
			BackendType: "builtin",
		})
	}
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
		Name:        "_create_workflow",
		Description: "Create and execute a deterministic AgentWorkflow with multiple steps. The workflow controller handles sequencing and you receive all step outputs when complete. Use for evaluation pipelines or multi-step tasks requiring guaranteed execution order.",
		Parameters:  []byte(`{"type":"object","properties":{"name":{"type":"string","description":"Unique workflow name"},"description":{"type":"string"},"steps":{"type":"array","description":"Array of step objects with name, agentRef, input, dependsOn","items":{"type":"object","properties":{"name":{"type":"string"},"agentRef":{"type":"string"},"input":{"type":"string"},"dependsOn":{"type":"array","items":{"type":"string"}},"condition":{"type":"string"},"timeout":{"type":"object"}},"required":["name","agentRef","input"]}},"budgetCap":{"type":"object","properties":{"total":{"type":"string"}}},"timeout":{"type":"object"},"onStepFailure":{"type":"string","enum":["stop","continue"]}},"required":["name","steps"]}`),
		BackendType: "builtin",
	})

	// _list_resources: allows agents to discover their available resources
	toolDefs = append(toolDefs, router.ToolDefinition{
		Name:        "_list_resources",
		Description: "List all available MCP servers, KnowledgeBases, and tools for this agent. Use this to discover what resources you can access before attempting to use them. Returns a JSON object with mcpServers, knowledgeBases, and tools arrays.",
		Parameters:  []byte(`{"type":"object","properties":{}}`),
		BackendType: "builtin",
	})

	// Resolve KnowledgeBases from Agent spec.
	var kbConfigs []router.KnowledgeBaseConfig
	for _, kbName := range agent.Spec.KnowledgeBases {
		var kb agentorcav1alpha1.KnowledgeBase
		if err := r.Get(ctx, client.ObjectKey{Name: kbName, Namespace: run.Namespace}, &kb); err != nil {
			return nil, nil, false, fmt.Errorf("getting KnowledgeBase %q: %w", kbName, err)
		}
		if kb.Spec.AllowedAgents != nil && !slices.Contains(kb.Spec.AllowedAgents, agent.Name) {
			return nil, nil, false, fmt.Errorf("agent %q is not in KnowledgeBase %q allowedAgents", agent.Name, kbName)
		}
		if !kb.Status.Ready {
			// Surface the KB's own failure reason so the run/agent-deployment
			// status explains *why* the KB isn't ready.
			kbReason := kb.Status.Message
			if kbReason == "" {
				kbReason = firstConditionMessage(kb.Status.Conditions)
			}
			if kbReason != "" {
				return nil, nil, false, fmt.Errorf("KnowledgeBase %q is not ready: %s", kbName, kbReason)
			}
			return nil, nil, false, fmt.Errorf("KnowledgeBase %q is not ready", kbName)
		}
		// Resolve the embedding ModelSelector to get the provider's model string.
		var embMS agentorcav1alpha1.ModelSelector
		if err := r.Get(ctx, client.ObjectKey{Name: kb.Spec.Embedding.ModelSelectorRef, Namespace: run.Namespace}, &embMS); err != nil {
			return nil, nil, false, fmt.Errorf("getting embedding ModelSelector for KB %q: %w", kbName, err)
		}
		if len(embMS.Spec.Providers) == 0 {
			return nil, nil, false, fmt.Errorf("embedding ModelSelector %q for KB %q has no providers", kb.Spec.Embedding.ModelSelectorRef, kbName)
		}
		embPW := embMS.Spec.Providers[0]
		var embMP agentorcav1alpha1.ModelProvider
		if err := r.Get(ctx, client.ObjectKey{Name: embPW.Name, Namespace: run.Namespace}, &embMP); err != nil {
			return nil, nil, false, fmt.Errorf("getting embedding ModelProvider %q: %w", embPW.Name, err)
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
		// Build enum list of available KB names so the LLM knows which KBs exist.
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

	// Budget cap.
	var budget float64
	if selector.Spec.BudgetCap != nil {
		budget = parseFloat(selector.Spec.BudgetCap.PerRun)
	}

	// Meta-router threshold.
	threshold := 0.6
	if selector.Spec.MetaRouter != nil && selector.Spec.MetaRouter.Threshold != "" {
		threshold = parseFloat(selector.Spec.MetaRouter.Threshold)
	}
	metaRouterProvider := ""
	if selector.Spec.MetaRouter != nil {
		metaRouterProvider = selector.Spec.MetaRouter.ProviderRef
	}

	checkpointEvery := 1
	resumeKey := ""
	if agent.Spec.Memory != nil {
		checkpointEvery = agent.Spec.Memory.CheckpointEvery
	}
	if run.Status.CheckpointRef != "" && run.Status.RestartCount > 0 {
		// Restart recovery: resume from this run's own last checkpoint.
		resumeKey = run.Status.CheckpointRef
	} else if run.Spec.PriorRunRef != "" {
		// New turn in a chat session: load the previous run's accumulated history
		// so the model-router starts with full multi-turn context.
		resumeKey = fmt.Sprintf("agentorca/runs/%s/state", run.Spec.PriorRunRef)
	}

	// Resolve safeguards from AgentRun spec.
	var safeguards router.RouterSafeguards
	if run.Spec.Safeguards != nil {
		s := run.Spec.Safeguards
		safeguards = router.RouterSafeguards{
			MaxConsecutiveNoopTurns: s.MaxConsecutiveNoopTurns,
			MinSubstantiveTokens:    s.MinSubstantiveTokens,
			MaxRepeatedToolCalls:    s.MaxRepeatedToolCalls,
			ToolFrequencyCap:        s.ToolFrequencyCap,
			ToolExecutionTimeoutSec: s.ToolExecutionTimeoutSec,
		}
	}

	// Resolve episodic memory config from Agent spec.
	var episodicMemory router.EpisodicMemoryConfig
	var longTermMemory router.LongTermMemoryConfig
	if agent.Spec.Memory != nil && agent.Spec.Memory.EpisodicSummaryEvery > 0 {
		summaryMSRef := agent.Spec.Memory.SummaryModelSelectorRef
		if summaryMSRef == "" {
			summaryMSRef = agent.Spec.ModelSelectorRef
		}
		var summaryMS agentorcav1alpha1.ModelSelector
		if err := r.Get(ctx, client.ObjectKey{Name: summaryMSRef, Namespace: run.Namespace}, &summaryMS); err != nil {
			return nil, nil, false, fmt.Errorf("getting summary ModelSelector %q: %w", summaryMSRef, err)
		}
		if len(summaryMS.Spec.Providers) > 0 {
			sumPW := summaryMS.Spec.Providers[0]
			var sumMP agentorcav1alpha1.ModelProvider
			if err := r.Get(ctx, client.ObjectKey{Name: sumPW.Name, Namespace: run.Namespace}, &sumMP); err != nil {
				return nil, nil, false, fmt.Errorf("getting summary ModelProvider %q: %w", sumPW.Name, err)
			}
			episodicMemory = router.EpisodicMemoryConfig{
				SummaryEvery:        agent.Spec.Memory.EpisodicSummaryEvery,
				SummaryProviderName: sumPW.Name,
				SummaryModel:        sumMP.Spec.LiteLLMModel,
			}
		}
	}

	// Resolve long-term memory KB from Agent spec.
	if agent.Spec.Memory != nil && agent.Spec.Memory.LongTermMemoryRef != "" {
		ltmKBName := agent.Spec.Memory.LongTermMemoryRef
		var ltmKB agentorcav1alpha1.KnowledgeBase
		var ltmErr error
		if err := r.Get(ctx, client.ObjectKey{Name: ltmKBName, Namespace: run.Namespace}, &ltmKB); err != nil {
			ltmErr = err
		} else if !ltmKB.Status.Ready {
			ltmErr = fmt.Errorf("not ready: %s", firstNonEmpty(ltmKB.Status.Message, firstConditionMessage(ltmKB.Status.Conditions)))
		}
		if ltmErr != nil {
			// Degrade gracefully: log a warning and proceed without long-term memory.
			log.FromContext(ctx).Info("long-term memory KnowledgeBase not ready; proceeding without it",
				"knowledgeBase", ltmKBName, "run", run.Name, "error", ltmErr)
		} else {
			// Use the pinned provider from status if available; fall back to Providers[0].
			ltmEmbProviderName := ltmKB.Status.EmbeddingModelProvider
			if ltmEmbProviderName == "" {
				var ltmEmbMS agentorcav1alpha1.ModelSelector
				if err := r.Get(ctx, client.ObjectKey{Name: ltmKB.Spec.Embedding.ModelSelectorRef, Namespace: run.Namespace}, &ltmEmbMS); err == nil && len(ltmEmbMS.Spec.Providers) > 0 {
					ltmEmbProviderName = ltmEmbMS.Spec.Providers[0].Name
				}
			}
			if ltmEmbProviderName != "" {
				var ltmMP agentorcav1alpha1.ModelProvider
				if err := r.Get(ctx, client.ObjectKey{Name: ltmEmbProviderName, Namespace: run.Namespace}, &ltmMP); err == nil {
					dims := ltmKB.Status.EmbeddingDimensions
					longTermMemory = router.LongTermMemoryConfig{
						Enabled:               true,
						KBName:                ltmKBName,
						VectorStoreURL:        ltmKB.Status.VectorStoreURL,
						CollectionName:        ltmKB.Status.CollectionName,
						EmbeddingProviderName: ltmEmbProviderName,
						EmbeddingModel:        ltmMP.Spec.LiteLLMModel,
						Dimensions:            dims,
					}
					// Inject _memory_store tool so the LLM can explicitly persist facts.
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

	cfg := &router.Config{
		RunName:                run.Name,
		RunNamespace:           run.Namespace,
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
		CheckpointKey:          fmt.Sprintf("agentorca/runs/%s/state", run.Name),
		ResumeCheckpointKey:    resumeKey,
		StateConfig:            r.StateConfig,
		KubeAPIURL:             "https://kubernetes.default.svc",
		OperatorAPIURL:         r.OperatorAPIURL,
		LLMRequestTimeout:      r.LLMRequestTimeout,
		SystemPrompt:           agent.Spec.SystemPrompt,
		ChatMode:               agent.Spec.Runtime.InputMode == "http" || agent.Spec.Runtime.InputMode == "chat",
		WorkflowName:           run.Labels["agentorca.io/workflow"],
		DeploymentName:         run.Labels["agentorca.io/deployment"],
		KnowledgeBases:         kbConfigs,
		Safeguards:             safeguards,
		EpisodicMemory:         episodicMemory,
		LongTermMemory:         longTermMemory,
	}
	if agent.Spec.Runtime.InputMode == "http" {
		port := agent.Spec.Runtime.InputPort
		if port == 0 {
			port = 8000
		}
		path := agent.Spec.Runtime.InputPath
		if path == "" {
			path = "/invoke"
		}
		cfg.HTTPInput = router.HTTPInputConfig{
			Enabled: true,
			Input:   run.Spec.Input,
			Port:    port,
			Path:    path,
			RunName: run.Name,
		}
	}

	// GuardrailPolicy CR: wire agent's guardrailPolicyRef into cfg.Guardrails.
	if agent.Spec.GuardrailPolicyRef != "" {
		mergeGuardrailPolicyIntoRouterConfig(ctx, r.Client, run.Namespace, agent.Spec.GuardrailPolicyRef, cfg)
	}

	return cfg, egressRules, hasAgentTools, nil
}

// ensureRole creates the per-run Role if it doesn't exist.
func (r *AgentRunReconciler) ensureRole(ctx context.Context, run *agentorcav1alpha1.AgentRun) error {
	role := security.BuildRunRole(run)
	if err := ctrl.SetControllerReference(run, role, r.Scheme); err != nil {
		return fmt.Errorf("setting role owner ref: %w", err)
	}
	if err := r.Create(ctx, role); err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("creating role: %w", err)
	}
	return nil
}

// ensureRoleBinding creates the per-run RoleBinding if it doesn't exist.
func (r *AgentRunReconciler) ensureRoleBinding(ctx context.Context, run *agentorcav1alpha1.AgentRun, saName string) error {
	rb := security.BuildRunRoleBinding(run, saName, run.Namespace)
	if err := ctrl.SetControllerReference(run, rb, r.Scheme); err != nil {
		return fmt.Errorf("setting rolebinding owner ref: %w", err)
	}
	if err := r.Create(ctx, rb); err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("creating rolebinding: %w", err)
	}
	return nil
}

// ensureTokenReviewerBinding creates a ClusterRoleBinding that grants the agent SA
// the "create tokenreviews" permission so the model-router sidecar can validate tokens.
// The binding is named "agentorca-run-<run-name>" and is cleaned up after the run.
func (r *AgentRunReconciler) ensureTokenReviewerBinding(ctx context.Context, run *agentorcav1alpha1.AgentRun, saName string) error {
	if r.TokenReviewerClusterRole == "" {
		return nil // skip if not configured (e.g. tests)
	}
	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: "agentorca-run-" + run.Name,
			Labels: map[string]string{
				security.LabelAgentRunName: security.SafeLabelValue(run.Name),
				security.LabelManagedBy:    security.ManagedByValue,
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
				Namespace: run.Namespace,
			},
		},
	}
	if err := r.Create(ctx, crb); err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("creating token-reviewer ClusterRoleBinding: %w", err)
	}
	return nil
}

// ensureNetworkPolicy creates the per-run NetworkPolicy if it doesn't exist.
func (r *AgentRunReconciler) ensureNetworkPolicy(ctx context.Context, run *agentorcav1alpha1.AgentRun, toolEgressRules []agentorcav1alpha1.EgressRule, hasAgentTools bool, hasKnowledgeBases bool) error { //nolint:unparam

	np := security.BuildNetworkPolicy(run, run.Namespace, toolEgressRules, r.StateConfig.Backend == "redis")
	if err := ctrl.SetControllerReference(run, np, r.Scheme); err != nil {
		return fmt.Errorf("setting networkpolicy owner ref: %w", err)
	}
	if err := r.Create(ctx, np); err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("creating networkpolicy: %w", err)
	}
	return nil
}

// ensureSplitPodNetworkPolicies creates two NetworkPolicies for split-pod topology:
//   - Router pod policy: full egress (providers, K8s API, Redis, tools) + ingress from agent pod
//   - Agent pod policy: egress restricted to router pod ports 8080/8082 + DNS only
func (r *AgentRunReconciler) ensureSplitPodNetworkPolicies(ctx context.Context, run *agentorcav1alpha1.AgentRun, toolEgressRules []agentorcav1alpha1.EgressRule) error {
	routerNP := security.BuildRouterPodNetworkPolicy(run, run.Namespace, toolEgressRules, r.StateConfig.Backend == "redis")
	if err := ctrl.SetControllerReference(run, routerNP, r.Scheme); err != nil {
		return fmt.Errorf("setting router networkpolicy owner ref: %w", err)
	}
	if err := r.Create(ctx, routerNP); err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("creating router networkpolicy: %w", err)
	}

	agentNP := security.BuildAgentPodNetworkPolicy(run, run.Namespace)
	if err := ctrl.SetControllerReference(run, agentNP, r.Scheme); err != nil {
		return fmt.Errorf("setting agent networkpolicy owner ref: %w", err)
	}
	if err := r.Create(ctx, agentNP); err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("creating agent networkpolicy: %w", err)
	}
	return nil
}

// ensureRouterService creates the ClusterIP Service that exposes the router pod in split-pod
// topology. The agent pod reaches the model-router via this Service rather than localhost.
func (r *AgentRunReconciler) ensureRouterService(ctx context.Context, run *agentorcav1alpha1.AgentRun) error {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agentorca-router-" + run.Name,
			Namespace: run.Namespace,
			Labels: map[string]string{
				security.LabelAgentRunName: security.SafeLabelValue(run.Name),
				security.LabelManagedBy:    security.ManagedByValue,
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				security.LabelAgentRunName: security.SafeLabelValue(run.Name),
				security.LabelComponent:    security.LabelComponentRouter,
			},
			Ports: []corev1.ServicePort{
				{Name: "openai", Port: 8080, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt(8080)},
				{Name: "gemini", Port: 8082, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt(8082)},
			},
			Type: corev1.ServiceTypeClusterIP,
		},
	}
	if err := ctrl.SetControllerReference(run, svc, r.Scheme); err != nil {
		return fmt.Errorf("setting router service owner ref: %w", err)
	}
	if err := r.Create(ctx, svc); err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("creating router service: %w", err)
	}
	return nil
}

// buildRouterPod constructs a standalone router Pod for split-pod topology.
// The router runs as a regular container (not a sidecar init container).
func (r *AgentRunReconciler) buildRouterPod(
	run *agentorcav1alpha1.AgentRun,
	agent *agentorcav1alpha1.Agent,
	saName string,
	providerVolumes []corev1.Volume,
	providerMounts []corev1.VolumeMount,
	toolSecretVolumes []corev1.Volume,
	toolSecretMounts []corev1.VolumeMount,
	mcpBinVolumes []corev1.Volume,
	mcpBinMounts []corev1.VolumeMount,
) *corev1.Pod {
	return podbuilder.BuildRouterOnly(podbuilder.PodConfig{
		PodName:   "agentorca-router-" + run.Name,
		Namespace: run.Namespace,
		Labels: map[string]string{
			security.LabelAgentRunName: security.SafeLabelValue(run.Name),
			security.LabelManagedBy:    security.ManagedByValue,
			security.LabelComponent:    security.LabelComponentRouter,
			"agentorca.io/agent":       agent.Name,
		},
		Agent:             agent,
		ServiceAccount:    saName,
		RestartPolicy:     corev1.RestartPolicyNever,
		ModelRouterImage:  r.ModelRouterImage,
		RouterConfigName:  "agentorca-run-" + run.Name,
		ProviderVolumes:   providerVolumes,
		ProviderMounts:    providerMounts,
		ToolSecretVolumes: toolSecretVolumes,
		ToolSecretMounts:  toolSecretMounts,
		MCPBinVolumes:     mcpBinVolumes,
		MCPBinMounts:      mcpBinMounts,
		CloudProvider:     r.CloudProvider,
	})
}

// buildAgentOnlyPod constructs the agent Pod for split-pod topology.
// The pod has only the agent container; no model-router sidecar.
func (r *AgentRunReconciler) buildAgentOnlyPod(
	run *agentorcav1alpha1.AgentRun,
	agent *agentorcav1alpha1.Agent,
	saName string,
	tokenSecretName string,
	routerBaseURL string,
) *corev1.Pod {
	timeout := 5 * time.Minute
	if run.Spec.Timeout != nil {
		timeout = run.Spec.Timeout.Duration
	}

	agentEnv := []corev1.EnvVar{
		{Name: "AGENTORC_RUN_ID", Value: run.Name},
		{Name: "AGENTORC_TIMEOUT_SEC", Value: fmt.Sprintf("%d", int(timeout.Seconds()))},
	}
	if agent.Spec.Runtime.InputMode != "http" {
		agentEnv = append(agentEnv, corev1.EnvVar{Name: "AGENTORC_INPUT", Value: run.Spec.Input})
	}
	if wf, ok := run.Labels["agentorca.io/workflow"]; ok && wf != "" {
		agentEnv = append(agentEnv, corev1.EnvVar{Name: "AGENTORC_WORKFLOW_NAME", Value: wf})
	}
	if dep, ok := run.Labels["agentorca.io/deployment"]; ok && dep != "" {
		agentEnv = append(agentEnv, corev1.EnvVar{Name: "AGENTORC_DEPLOYMENT_NAME", Value: dep})
	}

	var readinessProbe *corev1.Probe
	if agent.Spec.Runtime.InputMode == "http" {
		port := agent.Spec.Runtime.InputPort
		if port == 0 {
			port = 8000
		}
		readinessProbe = &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt(port)},
			},
			InitialDelaySeconds: 2,
			PeriodSeconds:       2,
			FailureThreshold:    30,
		}
	}

	return podbuilder.BuildAgentOnly(podbuilder.PodConfig{
		PodName:   "agentorca-run-" + run.Name,
		Namespace: run.Namespace,
		Labels: map[string]string{
			security.LabelAgentRunName: security.SafeLabelValue(run.Name),
			security.LabelManagedBy:    security.ManagedByValue,
			security.LabelComponent:    security.LabelComponentAgent,
			"agentorca.io/agent":       agent.Name,
		},
		Agent:               agent,
		AgentEnv:            agentEnv,
		TokenSecretName:     tokenSecretName,
		ServiceAccount:      saName,
		RestartPolicy:       corev1.RestartPolicyNever,
		RouterBaseURL:       routerBaseURL,
		AgentReadinessProbe: readinessProbe,
		CloudProvider:       r.CloudProvider,
	})
}

// ensureTracingPolicy creates the Tetragon TracingPolicy if Tetragon is installed.
func (r *AgentRunReconciler) ensureTracingPolicy(ctx context.Context, run *agentorcav1alpha1.AgentRun) error {
	if r.Dynamic == nil {
		return fmt.Errorf("dynamic client not configured")
	}
	tp := security.BuildTracingPolicy(run.Name, run.Namespace)
	_, err := r.Dynamic.Resource(security.TetragonGVR).Namespace(run.Namespace).Create(
		ctx, tp, metav1.CreateOptions{},
	)
	if err != nil && !errors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// ensureRouterConfigMap creates or updates the ConfigMap with the router.Config JSON.
func (r *AgentRunReconciler) ensureRouterConfigMap(ctx context.Context, run *agentorcav1alpha1.AgentRun, cfg *router.Config) error {
	data, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshaling router config: %w", err)
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agentorca-run-" + run.Name,
			Namespace: run.Namespace,
			Labels: map[string]string{
				security.LabelAgentRunName: security.SafeLabelValue(run.Name),
				security.LabelManagedBy:    security.ManagedByValue,
			},
		},
		Data: map[string]string{
			podbuilder.RouterConfigKey: string(data),
		},
	}
	if err := ctrl.SetControllerReference(run, cm, r.Scheme); err != nil {
		return fmt.Errorf("setting configmap owner ref: %w", err)
	}
	if err := r.Create(ctx, cm); err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("creating router configmap: %w", err)
	}
	return nil
}

// ensureTokenSecret creates a per-run Secret containing the projected SA token via TokenRequest.
// routerBaseURL is the model-router URL injected as OPENAI_BASE_URL into the agent container.
// In combined-pod topology it is "http://localhost:8080/v1"; in split-pod topology it is the
// router Service URL (e.g. "http://agentorca-router-<run>.<ns>:8080/v1").
func (r *AgentRunReconciler) ensureTokenSecret(ctx context.Context, run *agentorcav1alpha1.AgentRun, saName string, routerBaseURL string) (string, error) {
	secretName := "agentorca-run-" + run.Name + podbuilder.TokenSecretSuffix
	var existing corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: run.Namespace}, &existing); err == nil {
		return secretName, nil // already exists
	}

	// Call the Kubernetes TokenRequest API to get a bound SA token.
	expirySeconds := int64(900) // 15 minutes; token-refresher sidecar handles rotation
	tr := &authv1.TokenRequest{
		Spec: authv1.TokenRequestSpec{
			Audiences:         []string{security.ModelRouterTokenAudience},
			ExpirationSeconds: ptr.To(expirySeconds),
		},
	}
	result, err := r.K8s.CoreV1().ServiceAccounts(run.Namespace).CreateToken(ctx, saName, tr, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("creating SA token for run %s: %w", run.Name, err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: run.Namespace,
			Labels: map[string]string{
				security.LabelAgentRunName: security.SafeLabelValue(run.Name),
				security.LabelManagedBy:    security.ManagedByValue,
			},
		},
		StringData: map[string]string{
			// These env vars are injected into the agent container via envFrom.secretRef.
			// All OpenAI-compatible frameworks read them automatically.
			"OPENAI_BASE_URL": routerBaseURL,
			"OPENAI_API_KEY":  result.Status.Token,
		},
	}
	if err := ctrl.SetControllerReference(run, secret, r.Scheme); err != nil {
		return "", fmt.Errorf("setting secret owner ref: %w", err)
	}
	if err := r.Create(ctx, secret); err != nil && !errors.IsAlreadyExists(err) {
		return "", fmt.Errorf("creating token secret: %w", err)
	}
	return secretName, nil
}

// buildAgentPod constructs the Pod spec for an AgentRun.
// The pod has two containers: the agent runtime and the model-router sidecar.
// Built via the shared podbuilder package so all agent pod types use one codepath.
func (r *AgentRunReconciler) buildAgentPod(
	run *agentorcav1alpha1.AgentRun,
	agent *agentorcav1alpha1.Agent,
	saName string,
	tokenSecretName string,
	providerVolumes []corev1.Volume,
	providerMounts []corev1.VolumeMount,
	toolSecretVolumes []corev1.Volume,
	toolSecretMounts []corev1.VolumeMount,
	mcpBinVolumes []corev1.Volume,
	mcpBinMounts []corev1.VolumeMount,
) *corev1.Pod {
	// Determine timeout.
	timeout := 5 * time.Minute
	if run.Spec.Timeout != nil {
		timeout = run.Spec.Timeout.Duration
	}

	// Agent container env vars injected by the operator.
	agentEnv := []corev1.EnvVar{
		{Name: "AGENTORC_RUN_ID", Value: run.Name},
		{Name: "AGENTORC_TIMEOUT_SEC", Value: fmt.Sprintf("%d", int(timeout.Seconds()))},
	}
	if agent.Spec.Runtime.InputMode != "http" {
		agentEnv = append(agentEnv, corev1.EnvVar{Name: "AGENTORC_INPUT", Value: run.Spec.Input})
	}
	// Inject workflow/deployment context so the model-router can scope shared state keys.
	if wf, ok := run.Labels["agentorca.io/workflow"]; ok && wf != "" {
		agentEnv = append(agentEnv, corev1.EnvVar{Name: "AGENTORC_WORKFLOW_NAME", Value: wf})
	}
	if dep, ok := run.Labels["agentorca.io/deployment"]; ok && dep != "" {
		agentEnv = append(agentEnv, corev1.EnvVar{Name: "AGENTORC_DEPLOYMENT_NAME", Value: dep})
	}

	// Build readiness probe for http-mode agents.
	var readinessProbe *corev1.Probe
	if agent.Spec.Runtime.InputMode == "http" {
		port := agent.Spec.Runtime.InputPort
		if port == 0 {
			port = 8000
		}
		readinessProbe = &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt(port)},
			},
			InitialDelaySeconds: 2,
			PeriodSeconds:       2,
			FailureThreshold:    30,
		}
	}

	// Agent-runtime secret refs (e.g. HTB OpenVPN config): mounted into the agent
	// container only. This is a pure transform of Agent.spec.runtime.secretRefs.
	agentSecretVolumes, agentSecretMounts := podbuilder.ResolveAgentSecretRefs(agent)

	return podbuilder.Build(podbuilder.PodConfig{
		PodName:   "agentorca-run-" + run.Name,
		Namespace: run.Namespace,
		Labels: map[string]string{
			security.LabelAgentRunName: security.SafeLabelValue(run.Name),
			security.LabelManagedBy:    security.ManagedByValue,
			"agentorca.io/agent":       agent.Name,
		},
		Agent:               agent,
		AgentEnv:            agentEnv,
		TokenSecretName:     tokenSecretName,
		ServiceAccount:      saName,
		RestartPolicy:       corev1.RestartPolicyNever,
		ModelRouterImage:    r.ModelRouterImage,
		RouterConfigName:    "agentorca-run-" + run.Name,
		AgentReadinessProbe: readinessProbe,
		ProviderVolumes:     providerVolumes,
		ProviderMounts:      providerMounts,
		ToolSecretVolumes:   toolSecretVolumes,
		ToolSecretMounts:    toolSecretMounts,
		MCPBinVolumes:       mcpBinVolumes,
		MCPBinMounts:        mcpBinMounts,
		AgentSecretVolumes:  agentSecretVolumes,
		AgentSecretMounts:   agentSecretMounts,
		CloudProvider:       r.CloudProvider,
	})
}

// ensureCleanup deletes per-run resources owned by the AgentRun on terminal state.
// Resources with ownerReferences are garbage-collected automatically; this handles
// any resources without owner references (e.g. TracingPolicy via dynamic client).
func (r *AgentRunReconciler) ensureCleanup(ctx context.Context, run *agentorcav1alpha1.AgentRun) (ctrl.Result, error) {
	if r.Dynamic != nil {
		tpName := security.TracingPolicyName(run.Name)
		_ = r.Dynamic.Resource(security.TetragonGVR).Namespace(run.Namespace).Delete(
			ctx, tpName, metav1.DeleteOptions{},
		)
	}
	// ClusterRoleBinding is cluster-scoped so it cannot have a namespace-scoped ownerRef;
	// delete it explicitly.
	if r.TokenReviewerClusterRole != "" {
		crb := &rbacv1.ClusterRoleBinding{}
		crb.Name = "agentorca-run-" + run.Name
		_ = r.Delete(ctx, crb)
	}
	// Reconcile the run's pod on terminal state. One-shot AgentRun pods are deleted
	// (existing behavior). Warm pods — owned by an AgentDeployment, so ownerRef GC
	// won't fire — are REUSED by default: returned to the idle pool so the next chat
	// run can claim them, unless the deployment's maxRequestsPerPod cap has been
	// reached, in which case the pod is recycled. (HTTP-mode warm pods are long-running
	// servers that never exit on their own, so they must be handled explicitly here.)
	r.reconcileRunPodOnTerminal(ctx, run)
	// In split-pod topology, also delete the router pod explicitly.
	// The router Service is owned by the AgentRun and GC'd automatically.
	if run.Status.RouterPodName != "" {
		routerPod := &corev1.Pod{}
		routerPod.Name = run.Status.RouterPodName
		routerPod.Namespace = run.Namespace
		_ = r.Delete(ctx, routerPod)
	}
	// Cascade cancellation to child runs when this run was cancelled.
	// Child runs are identified by the label "agentorca.io/parent-run" set by the
	// executor at child-run creation time.
	reason := run.Status.LastRestartReason + " " + run.Status.FailureReason
	if run.Status.Phase == agentorcav1alpha1.AgentRunPhaseFailed && strings.Contains(reason, "cancelled") {
		var childRuns agentorcav1alpha1.AgentRunList
		if err := r.List(ctx, &childRuns,
			client.InNamespace(run.Namespace),
			client.MatchingLabels{"agentorca.io/parent-run": run.Name},
		); err == nil {
			for i := range childRuns.Items {
				child := &childRuns.Items[i]
				if isTerminal(child.Status.Phase) {
					continue
				}
				patch := client.MergeFrom(child.DeepCopy())
				now := metav1.Now()
				child.Status.Phase = agentorcav1alpha1.AgentRunPhaseFailed
				child.Status.FailureReason = "cancelled: parent run cancelled"
				child.Status.CompletionTime = &now
				logger := log.FromContext(ctx)
				if pErr := r.Status().Patch(ctx, child, patch); pErr != nil {
					logger.Error(pErr, "failed to cancel child run", "child", child.Name)
				} else {
					logger.Info("cascaded cancellation to child run", "child", child.Name, "parent", run.Name)
				}
				// Signal via Redis for immediate effect on the child's model-router.
				if r.StateStore != nil {
					_ = r.StateStore.SignalCancel(ctx, child.Namespace, child.Name)
				}
			}
		}
	}

	// Role, RoleBinding, NetworkPolicy, ConfigMap, Secret are owned by the AgentRun
	// and garbage-collected automatically via ownerReferences.

	// Re-archive terminal runs to PostgreSQL on every terminal reconcile. This is
	// the restart/back-fill path: a run that went terminal just before a crash may
	// never have completed the fire-and-forget archive in handlePodSuccess, and
	// ensureCleanup previously didn't re-archive, so such runs vanished from the
	// history view after a controller restart. ArchiveRun is an idempotent UPSERT
	// (keyed by namespace/name), so this is a cheap no-op for runs already
	// archived and a durable back-fill for the ones that were lost.
	if r.PostgresStore != nil && postgresql.IsTerminalPhase(run.Status.Phase) {
		archiveCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		traceJSON := r.loadTraceEventsForArchive(archiveCtx, run)
		if err := r.PostgresStore.ArchiveRun(archiveCtx, run, traceJSON); err != nil {
			logger := log.FromContext(ctx)
			logger.Error(err, "failed to re-archive terminal run on restart", "run", run.Name, "ns", run.Namespace)
		}
	}

	return ctrl.Result{}, nil
}

// shouldDeletePodOnCompletion reports whether a pod should be destroyed when its
// HTTP/chat run reaches completion (output detected by the controller). One-shot
// pods (no warm-pool label) are torn down so they don't linger as Running.
//
// WARM pods are intentionally preserved: they are returned to the idle pool by
// reconcileRunPodOnTerminal on the terminal reconcile so they survive across chat
// turns and long-trajectory tasks. checkProgress must NOT delete them here, or the
// pod is removed before the return-to-idle logic ever runs — which destroyed warm
// pods on every completion and could murder an in-progress session.
func shouldDeletePodOnCompletion(pod *corev1.Pod) bool {
	return pod.Labels[labelWarmPool] == ""
}

// reconcileRunPodOnTerminal handles the AgentRun's pod when the run reaches a terminal
// state. One-shot AgentRun pods are deleted (existing behavior). Warm pods — owned by
// an AgentDeployment run, so ownerReference GC will not fire — are REUSED by default:
// returned to the idle pool so the next chat run can claim them, unless the owning
// deployment's maxRequestsPerPod cap has been reached, in which case the pod is recycled.
// The model-router's ClaimRun is re-entrant (it resets per-run state on each claim), so a
// returned idle pod is safe to re-claim.
func (r *AgentRunReconciler) reconcileRunPodOnTerminal(ctx context.Context, run *agentorcav1alpha1.AgentRun) {
	logger := log.FromContext(ctx)
	if run.Status.PodName == "" {
		return
	}
	var pod corev1.Pod
	if err := r.Get(ctx, client.ObjectKey{Namespace: run.Namespace, Name: run.Status.PodName}, &pod); err != nil {
		// Pod already gone — nothing to return; fall back to a best-effort delete of the
		// (now missing) name to preserve prior behavior.
		_ = r.Delete(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name: run.Status.PodName, Namespace: run.Namespace,
		}})
		return
	}
	if shouldDeletePodOnCompletion(&pod) {
		// One-shot AgentRun pod: delete as before.
		_ = r.Delete(ctx, &pod)
		return
	}

	// Warm pod: reuse it unless the owning deployment's request cap is reached.
	maxRequests := 0
	if deployName := run.Labels["agentorca.io/deployment"]; deployName != "" {
		var dep agentorcav1alpha1.AgentDeployment
		if err := r.Get(ctx, client.ObjectKey{Namespace: run.Namespace, Name: deployName}, &dep); err == nil {
			maxRequests = dep.Spec.MaxRequestsPerPod
		}
	}
	served := warmRequestCount(&pod)

	if warmPodOverCap(&pod, maxRequests) {
		logger.Info("recycling warm pod: request cap reached",
			"pod", pod.Name, "served", served, "cap", maxRequests)
		_ = r.Delete(ctx, &pod)
		return
	}

	// Reuse: return the pod to the idle pool so it can be claimed by the next run.
	patch := client.MergeFrom(pod.DeepCopy())
	pod.Labels[labelWarmStatus] = warmStatusIdle
	delete(pod.Labels, "agentorca.io/run")
	if err := r.Patch(ctx, &pod, patch); err != nil {
		logger.Error(err, "failed to return warm pod to idle; deleting as fallback", "pod", pod.Name)
		_ = r.Delete(ctx, &pod)
	}
}

// warmRequestCount parses the warm-requests label of a pod into an int (0 if absent or
// unparseable).
func warmRequestCount(pod *corev1.Pod) int {
	n, ok := pod.Labels[labelWarmRequests]
	if !ok {
		return 0
	}
	cnt, err := strconv.Atoi(n)
	if err != nil {
		return 0
	}
	return cnt
}

// warmPodOverCap reports whether a warm pod has reached its owning deployment's
// maxRequestsPerPod quota and should be recycled. maxRequests<=0 means "reuse
// indefinitely" (no cap), so it returns false.
func warmPodOverCap(pod *corev1.Pod, maxRequests int) bool {
	if maxRequests <= 0 {
		return false
	}
	return warmRequestCount(pod) >= maxRequests
}

// handleRunDeletion cleans up before removing the finalizer.
func (r *AgentRunReconciler) handleRunDeletion(ctx context.Context, run *agentorcav1alpha1.AgentRun) (ctrl.Result, error) {
	if _, err := r.ensureCleanup(ctx, run); err != nil {
		return ctrl.Result{}, err
	}
	controllerutil.RemoveFinalizer(run, agentRunFinalizer)
	return ctrl.Result{}, r.Update(ctx, run)
}

// failRun sets the AgentRun to Failed with a reason message.
func (r *AgentRunReconciler) failRun(ctx context.Context, run *agentorcav1alpha1.AgentRun, reason string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Error(nil, "failing AgentRun", "reason", reason)

	patch := client.MergeFrom(run.DeepCopy())
	run.Status.Phase = agentorcav1alpha1.AgentRunPhaseFailed
	run.Status.SpendUSD = r.loadSpendFromStore(ctx, run.Name)
	now := metav1.Now()
	run.Status.CompletionTime = &now
	run.Status.LastRestartReason = reason
	if err := r.Status().Patch(ctx, run, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("patching failed status: %w", err)
	}

	// Fire failure callback if configured.
	r.fireCallback(ctx, run)

	// Publish result to egress sink if configured.
	r.fireEgress(ctx, run)

	// Archive terminal-phase run to PostgreSQL for historical querying.
	r.maybeArchiveRun(ctx, run)

	return ctrl.Result{}, nil
}

// maybeArchiveRun snapshots a terminal-phase AgentRun to PostgreSQL if the
// archival store is configured. Uses a non-blocking goroutine so DB latency
// never delays reconciliation. Re-archiving is idempotent (UPSERT by name).
// The run's Redis token stream is read at archival time so the full execution
// trace (tokens, tool calls, tool results, etc.) is durable in PostgreSQL and
// viewable from the history tab even after the Redis stream expires.
func (r *AgentRunReconciler) maybeArchiveRun(ctx context.Context, run *agentorcav1alpha1.AgentRun) {
	if r.PostgresStore == nil || !postgresql.IsTerminalPhase(run.Status.Phase) {
		return
	}
	go func() {
		archiveCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		traceJSON := r.loadTraceEventsForArchive(archiveCtx, run)
		if err := r.PostgresStore.ArchiveRun(archiveCtx, run, traceJSON); err != nil {
			log.Log.Error(err, "failed to archive run to PostgreSQL", "run", run.Name, "ns", run.Namespace)
		}
	}()
}

// loadTraceEventsForArchive reads the run's trace events from the Redis token
// stream (including child-run streams) and synthesises modelSelected/finalOutput
// events from the run status, producing a JSON array ready for archival.
// Returns nil when the state store is unavailable so the caller stores NULL
// in the trace_events column (preserving any previously archived trace on
// idempotent re-upserts).
func (r *AgentRunReconciler) loadTraceEventsForArchive(ctx context.Context, run *agentorcav1alpha1.AgentRun) []byte {
	if r.StateStore == nil {
		return nil
	}

	var entries []state.TraceEntry

	// Read the parent run's trace events from its Redis token stream.
	streamKey := "tokens:" + run.Namespace + ":" + run.Name
	parentEvents, err := r.StateStore.ReadTraceEvents(ctx, streamKey)
	if err != nil {
		log.Log.Error(err, "failed to read trace events for archival", "run", run.Name, "ns", run.Namespace)
		return nil
	}
	entries = parentEvents

	// Read each child run's trace events (agent-as-tool), tagging them so the
	// UI can distinguish the child's activity (mirrors RunView's child-stream
	// subscription behaviour).
	for _, childName := range run.Status.ChildRunRefs {
		childKey := "tokens:" + run.Namespace + ":" + childName
		childEvents, err := r.StateStore.ReadTraceEvents(ctx, childKey)
		if err != nil {
			continue
		}
		for i := range childEvents {
			childEvents[i].ChildRunName = childName
		}
		entries = append(entries, childEvents...)
	}

	// Synthesise modelSelected events from the run's routing decisions. The
	// model-router emits these as SSE events at stream time (the stream handler
	// itself constructs them from the CRD), but they are NOT written to the
	// Redis token stream — so we reconstruct them here for archival parity.
	for _, rd := range run.Status.RoutingDecisions {
		if strings.HasPrefix(rd.Reason, "configured provider") {
			continue
		}
		conf := 0.0
		_, _ = fmt.Sscanf(rd.Confidence, "%f", &conf)
		eventJSON, _ := json.Marshal(map[string]any{
			"type":       "modelSelected",
			"model":      rd.Model,
			"reason":     fmt.Sprintf("[%s] %s — %s", rd.Strategy, rd.Provider, rd.Reason),
			"confidence": conf,
		})
		ts := ""
		if rd.Timestamp != nil {
			ts = rd.Timestamp.UTC().Format(time.RFC3339)
		}
		entries = append(entries, state.TraceEntry{
			Event: json.RawMessage(eventJSON),
			TS:    ts,
		})
	}

	// Synthesise a finalOutput event from the run's captured output so the
	// archive's trace ends with the consolidated result (matching the live
	// stream handler, which emits finalOutput from accumulated tokens).
	if run.Status.Output != "" {
		eventJSON, _ := json.Marshal(map[string]string{
			"type":   "finalOutput",
			"output": run.Status.Output,
		})
		ts := ""
		if run.Status.CompletionTime != nil {
			ts = run.Status.CompletionTime.UTC().Format(time.RFC3339)
		}
		entries = append(entries, state.TraceEntry{
			Event: json.RawMessage(eventJSON),
			TS:    ts,
		})
	}

	if len(entries) == 0 {
		return nil
	}

	// Stable sort by timestamp so events appear in chronological order even
	// when parent + child streams are merged. Timeless entries sink to the end.
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].TS == "" && entries[j].TS == "" {
			return false
		}
		if entries[i].TS == "" {
			return false
		}
		if entries[j].TS == "" {
			return true
		}
		return entries[i].TS < entries[j].TS
	})

	// Re-assign sequential IDs after merging/sorting.
	for i := range entries {
		entries[i].ID = i
	}

	traceJSON, _ := json.Marshal(entries)
	return traceJSON
}

// fireCallback sends an HTTP POST to the configured callback URL when an AgentRun
// reaches a terminal phase (Succeeded or Failed). This is fire-and-forget with a
// 10-second timeout; failures are logged as Kubernetes Events on the run.
func (r *AgentRunReconciler) fireCallback(ctx context.Context, run *agentorcav1alpha1.AgentRun) {
	if run.Spec.Callbacks == nil {
		return
	}

	var callbackURL string
	switch run.Status.Phase {
	case agentorcav1alpha1.AgentRunPhaseSucceeded:
		callbackURL = run.Spec.Callbacks.OnComplete
	case agentorcav1alpha1.AgentRunPhaseFailed:
		callbackURL = run.Spec.Callbacks.OnFailed
	default:
		return
	}
	if callbackURL == "" {
		return
	}

	payload := map[string]any{
		"taskId":    run.Name,
		"agent":     run.Spec.AgentRef,
		"phase":     string(run.Status.Phase),
		"output":    run.Status.Output,
		"spendUSD":  run.Status.SpendUSD,
		"namespace": run.Namespace,
	}
	if run.Status.FailureReason != "" {
		payload["failureReason"] = run.Status.FailureReason
	}
	if run.Status.CompletionTime != nil {
		payload["completedAt"] = run.Status.CompletionTime.Format(time.RFC3339)
	}

	body, err := json.Marshal(payload)
	if err != nil {
		log.FromContext(ctx).Error(err, "marshaling callback payload", "run", run.Name)
		return
	}

	// Fire in a goroutine so we don't block reconciliation.
	go func() {
		callbackCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		// Resolve the per-callback HMAC signing secret (if configured). The secret
		// name is recorded by the external API at submission time under the
		// `agentorca.io/callback-secret` annotation. The Secret must contain a
		// `hmac-key` key whose value is the shared HMAC key. If absent or
		// unreadable the callback is still delivered (unsigned) so existing
		// customers are not broken — signing is opt-in via the shared secret.
		var signingKey []byte
		if secretName := run.Annotations["agentorca.io/callback-secret"]; secretName != "" {
			signingKey = r.resolveCallbackKey(callbackCtx, run.Namespace, secretName, run.Name)
		}

		resp, err := r.deliverCallback(callbackCtx, callbackURL, body, signingKey)
		if err != nil {
			log.Log.Error(err, "callback delivery failed", "run", run.Name, "url", callbackURL)
			return
		}
		if resp != nil {
			_ = resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				log.Log.Info("callback delivered", "run", run.Name, "url", callbackURL, "status", resp.StatusCode)
			} else {
				log.Log.Error(nil, "callback returned non-2xx", "run", run.Name, "url", callbackURL, "status", resp.StatusCode)
			}
		}
	}()
}

// resolveCallbackKey reads the shared HMAC key from the tenant's Secret. Returns
// nil (and logs) when the secret is missing/unreadable so delivery proceeds
// unsigned rather than failing the run. errors.IsNotFound is the expected
// "not configured" case and is therefore logged at debug, not error.
func (r *AgentRunReconciler) resolveCallbackKey(ctx context.Context, namespace, secretName, runName string) []byte {
	secret, err := r.K8s.CoreV1().Secrets(namespace).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			log.Log.V(1).Info("callback signing secret not found; delivering unsigned callback",
				"run", runName, "secret", secretName)
		} else {
			log.Log.Error(err, "fetching callback signing secret; delivering unsigned callback",
				"run", runName, "secret", secretName)
		}
		return nil
	}
	if data := secret.Data["hmac-key"]; len(data) > 0 {
		return data
	}
	log.Log.Info("callback signing secret found but missing hmac-key key; delivering unsigned callback",
		"run", runName, "secret", secretName)
	return nil
}

// deliverCallback builds and sends a single callback POST. When signingKey is
// non-empty the request is signed with HMAC-SHA256 over the body and carries an
// X-Agentorc-Timestamp header so receivers can verify authenticity and guard
// against replay. Returns the HTTP response (caller must close Body) and any
// transport error.
func (r *AgentRunReconciler) deliverCallback(ctx context.Context, callbackURL string, body []byte, signingKey []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, callbackURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	if len(signingKey) > 0 {
		mac := hmac.New(sha256.New, signingKey)
		mac.Write(body)
		req.Header.Set("X-Agentorc-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		req.Header.Set("X-Agentorc-Timestamp", strconv.FormatInt(time.Now().Unix(), 10))
	}

	return http.DefaultClient.Do(req)
}

// patchPhase is a helper to patch only the phase field.
func (r *AgentRunReconciler) patchPhase(ctx context.Context, run *agentorcav1alpha1.AgentRun, phase agentorcav1alpha1.AgentRunPhase) error {
	patch := client.MergeFrom(run.DeepCopy())
	run.Status.Phase = phase
	return r.Status().Patch(ctx, run, patch)
}

// checkClarifyTimeout enforces a maximum waiting time when an AgentRun is paused
// for human input. If the human does not respond within 30 minutes, the run is failed.
func (r *AgentRunReconciler) checkClarifyTimeout(ctx context.Context, run *agentorcav1alpha1.AgentRun) (ctrl.Result, error) {
	const maxWait = 30 * time.Minute
	if run.Status.WaitingSince != nil && time.Since(run.Status.WaitingSince.Time) > maxWait {
		return r.failRun(ctx, run, "clarification timeout: no human response within 30 minutes")
	}
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// isTerminal returns true if the phase is a terminal state.
func isTerminal(phase agentorcav1alpha1.AgentRunPhase) bool {
	return phase == agentorcav1alpha1.AgentRunPhaseSucceeded ||
		phase == agentorcav1alpha1.AgentRunPhaseFailed ||
		phase == agentorcav1alpha1.AgentRunPhaseHandedOff
}

// enrichFailureReason appends the tail of the agent and model-router container
// logs to the failure reason so that upstream errors (e.g. provider 400/502
// responses) are visible in the AgentRun status instead of only showing the
// exit code.
func (r *AgentRunReconciler) enrichFailureReason(ctx context.Context, namespace, podName, reason string) string {
	tailLines := int64(10)
	var parts []string
	parts = append(parts, reason)

	for _, container := range []string{"agent", "model-router"} {
		logs, err := r.K8s.CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{
			Container: container,
			TailLines: &tailLines,
		}).DoRaw(ctx)
		if err != nil || len(logs) == 0 {
			continue
		}
		snippet := truncate(strings.TrimSpace(string(logs)), 1024)
		if snippet != "" {
			parts = append(parts, fmt.Sprintf("[%s] %s", container, snippet))
		}
	}

	if len(parts) == 1 {
		return reason
	}
	return strings.Join(parts, "\n")
}

// loadSpendFromStore reads the cumulative spend for a run from the state store.
// Returns "" if the store is unavailable or no spend was recorded.
func (r *AgentRunReconciler) loadSpendFromStore(ctx context.Context, runName string) string {
	if r.StateStore == nil {
		return ""
	}
	key := fmt.Sprintf("agentorca/runs/%s/state", runName)
	usd, err := r.StateStore.LoadSpend(ctx, key)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to load spend from state store", "run", runName, "key", key)
		return ""
	}
	if usd == 0 {
		return ""
	}
	return fmt.Sprintf("%.6f", usd)
}

// recordRunInDeploymentIndex writes a marker KV entry under the deployment's run
// index so the model-router can enumerate this deployment's prior runs
// (agentorca/deployments/<dep>/runs:<runName>) for warm-pool history retrieval.
// This is best-effort: if the state store is unavailable the run is simply not
// indexed (search recall is narrowed), which is never a run-failure condition.
func (r *AgentRunReconciler) recordRunInDeploymentIndex(ctx context.Context, run *agentorcav1alpha1.AgentRun) {
	dep := run.Labels["agentorca.io/deployment"]
	if dep == "" || r.StateStore == nil {
		return
	}
	scope := fmt.Sprintf("agentorca/deployments/%s/runs", dep)
	ttl := int64(21600) // 6h default beyond the run; mirrors run-checkpoint residency
	if r.StateConfig.TTLSeconds > 0 {
		ttl = int64(r.StateConfig.TTLSeconds)
	}
	if err := r.StateStore.SaveKV(ctx, scope, run.Name, []byte("1"), time.Duration(ttl)*time.Second); err != nil {
		log.FromContext(ctx).V(1).Info("failed to record run in deployment index (non-fatal)", "run", run.Name, "deployment", dep, "err", err)
	}
}

// podFailureReason extracts a human-readable failure reason from a failed pod.
func podFailureReason(pod *corev1.Pod) string {
	// Check the agent main container first.
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == "agent" && cs.State.Terminated != nil {
			return fmt.Sprintf("exit code %d: %s", cs.State.Terminated.ExitCode, cs.State.Terminated.Message)
		}
	}
	// Check init containers (e.g. model-router native sidecar).
	for _, cs := range pod.Status.InitContainerStatuses {
		if cs.State.Terminated != nil && cs.State.Terminated.ExitCode != 0 {
			return fmt.Sprintf("init container %q failed: exit code %d: %s", cs.Name, cs.State.Terminated.ExitCode, cs.State.Terminated.Message)
		}
	}
	if pod.Status.Message != "" {
		return pod.Status.Message
	}
	return "unknown failure"
}

// truncate limits a string to at most n bytes.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// claimWarmPod finds an idle warm pod for this run's AgentDeployment, POSTs /v1/claim-run
// to it, and returns the pod name. Returns ("", nil) if no warm pod is available (caller
// should fall through to normal pod creation).
func (r *AgentRunReconciler) claimWarmPod(ctx context.Context, run *agentorcav1alpha1.AgentRun, agent *agentorcav1alpha1.Agent) (podName string, err error) {
	logger := log.FromContext(ctx)

	// Bounding the claim POST prevents a hung/unreachable warm-mgmt port from
	// stalling reconciliation long enough to race a fresh-pod fallback and cause
	// split-brain (warm pod and fresh pod both serving one run).
	const warmClaimTimeout = 15 * time.Second
	claimClient := &http.Client{Timeout: warmClaimTimeout}

	// Only applicable to http-mode agents backed by an AgentDeployment.
	if agent.Spec.Runtime.InputMode != "http" {
		logger.V(1).Info("warm pod claim skipped: agent is not http input mode", "agent", agent.Name)
		return "", nil
	}
	deployName := run.Labels["agentorca.io/deployment"]
	if deployName == "" {
		logger.V(1).Info("warm pod claim skipped: run has no agentorca.io/deployment label (created outside a chat/deployment path)")
		return "", nil
	}

	// Find an idle warm pod for this deployment.
	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(run.Namespace),
		client.MatchingLabels{
			labelWarmPool:   deployName,
			labelWarmStatus: warmStatusIdle,
		},
	); err != nil {
		return "", fmt.Errorf("listing warm pods: %w", err)
	}
	if len(pods.Items) == 0 {
		logger.Info("warm pod claim skipped: no idle warm pods for deployment", "deployment", deployName)
		return "", nil
	}

	// Find the first pod with a running model-router and a reachable IP.
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Status.PodIP == "" || pod.DeletionTimestamp != nil {
			logger.V(1).Info("warm pod claim skipped: pod not schedulable/being deleted", "pod", pod.Name)
			continue
		}
		// Only claim pods whose init container (model-router) is running.
		modelRouterReady := false
		for _, cs := range pod.Status.InitContainerStatuses {
			if cs.Name == "model-router" && cs.Ready { //nolint:goconst

				modelRouterReady = true
				break
			}
		}
		if !modelRouterReady {
			logger.Info("warm pod claim skipped: model-router sidecar not ready",
				"pod", pod.Name, "podIP", pod.Status.PodIP)
			continue
		}

		// Determine prior run ref for checkpoint resumption.
		priorRunRef := ""
		if run.Spec.PriorRunRef != "" {
			priorRunRef = run.Spec.PriorRunRef
		}

		// POST /v1/claim-run to the warm pod's management port.
		payload := router.WarmRunInput{
			RunName:     run.Name,
			Input:       run.Spec.Input,
			PriorRunRef: priorRunRef,
		}
		body, _ := json.Marshal(payload)
		claimURL := fmt.Sprintf("http://%s:9090/v1/claim-run", pod.Status.PodIP)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, claimURL, bytes.NewReader(body))
		if err != nil {
			continue
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := claimClient.Do(req)
		if err != nil {
			// Pod may not be fully ready yet; try the next one.
			logger.V(1).Info("warm pod claim: claim-run POST failed, trying next", "pod", pod.Name, "error", err)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			logger.Info("warm pod claim: claim-run rejected by pod, trying next", "pod", pod.Name, "status", resp.StatusCode)
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			continue
		}

		// Drain and close the success body before mutating the pod.
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()

		// Mark the pod as claimed so other runs don't pick it up. Also bump the
		// per-pod served-request counter used by maxRequestsPerPod recycling.
		patch := client.MergeFrom(pod.DeepCopy())
		pod.Labels[labelWarmStatus] = warmStatusClaimed
		pod.Labels["agentorca.io/run"] = security.SafeLabelValue(run.Name)
		pod.Labels[labelWarmRequests] = strconv.Itoa(warmRequestCount(pod) + 1)
		if err := r.Patch(ctx, pod, patch); err != nil {
			// Non-fatal — worst case another run also tries to claim this pod (the
			// /v1/claim-run endpoint is idempotent for the same run name).
			logger.V(1).Info("warm pod claim: failed to mark pod claimed", "pod", pod.Name, "error", err)
		}

		return pod.Name, nil
	}

	return "", nil
}

// findClaimedWarmPod returns the name of a warm pod that has already bound this run
// (via its agentorca.io/run label). This catches the race where /v1/claim-run succeeded
// on the warm pod (so it is already serving the run) but the operator's claim POST
// response was lost/retried — instead of spawning a second agent pod (split-brain),
// we reuse the warm pod that owns the run.
func (r *AgentRunReconciler) findClaimedWarmPod(ctx context.Context, run *agentorcav1alpha1.AgentRun) string {
	deployName := run.Labels["agentorca.io/deployment"]
	if deployName == "" {
		return ""
	}
	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(run.Namespace),
		client.MatchingLabels{
			labelWarmPool:      deployName,
			"agentorca.io/run": security.SafeLabelValue(run.Name),
		},
	); err != nil {
		return ""
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.DeletionTimestamp != nil {
			continue
		}
		// A non-terminating warm pod carrying this run's label owns the run.
		return p.Name
	}
	return ""
}

// parseFloat converts a USD decimal string to float64.
func parseFloat(s string) float64 {
	if s == "" {
		return 0
	}
	var f float64
	_, _ = fmt.Sscanf(s, "%f", &f)
	return f
}

// providerPort extracts the port from a ModelProvider's baseURL. Falls back to 443
// for HTTPS providers and 80 for HTTP. If baseURL is empty, returns 443 (cloud APIs).
func providerPort(baseURL, litellmModel string) int { //nolint:unparam

	if baseURL != "" {
		if u, err := url.Parse(baseURL); err == nil && u.Port() != "" {
			if p, err := strconv.Atoi(u.Port()); err == nil {
				return p
			}
		}
	}
	// Default: cloud APIs use HTTPS/443.
	return 443
}

// agentRunsForAgent maps an Agent change to AgentRuns that reference it and are not yet running.
// Already-running pods cannot benefit from a config update without a restart, so we skip them.
func (r *AgentRunReconciler) agentRunsForAgent(ctx context.Context, obj client.Object) []reconcile.Request {
	agent := obj.(*agentorcav1alpha1.Agent)
	var list agentorcav1alpha1.AgentRunList
	if err := r.List(ctx, &list, client.InNamespace(agent.Namespace)); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, run := range list.Items {
		if run.Spec.AgentRef != agent.Name {
			continue
		}
		switch run.Status.Phase {
		case agentorcav1alpha1.AgentRunPhasePending, agentorcav1alpha1.AgentRunPhaseWaitingForInput:
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&run)})
		}
	}
	return reqs
}

// agentRunsForModelSelector maps a ModelSelector change to pending/waiting AgentRuns.
func (r *AgentRunReconciler) agentRunsForModelSelector(ctx context.Context, obj client.Object) []reconcile.Request {
	ms := obj.(*agentorcav1alpha1.ModelSelector)
	var list agentorcav1alpha1.AgentRunList
	if err := r.List(ctx, &list, client.InNamespace(ms.Namespace)); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, run := range list.Items {
		switch run.Status.Phase {
		case agentorcav1alpha1.AgentRunPhasePending, agentorcav1alpha1.AgentRunPhaseWaitingForInput:
		default:
			continue
		}
		var agent agentorcav1alpha1.Agent
		if err := r.Get(ctx, types.NamespacedName{Name: run.Spec.AgentRef, Namespace: run.Namespace}, &agent); err != nil {
			continue
		}
		if agent.Spec.ModelSelectorRef == ms.Name {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&run)})
		}
	}
	return reqs
}

// SetupWithManager sets up the controller with the Manager.
func (r *AgentRunReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Recorder = mgr.GetEventRecorderFor("agentrun") //nolint:staticcheck
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentorcav1alpha1.AgentRun{}).
		Owns(&corev1.Pod{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.Secret{}).
		Owns(&corev1.Service{}).
		Owns(&rbacv1.Role{}).
		Owns(&rbacv1.RoleBinding{}).
		Watches(&agentorcav1alpha1.Agent{}, handler.EnqueueRequestsFromMapFunc(r.agentRunsForAgent)).
		Watches(&agentorcav1alpha1.ModelSelector{}, handler.EnqueueRequestsFromMapFunc(r.agentRunsForModelSelector)).
		Named("agentrun").
		Complete(r)
}

// fireEgress publishes the final run result to the configured egress sink.
// This is fire-and-forget: the publisher is created, the result is published,
// and any errors are logged + recorded as metrics. Egress is independent of
// webhook callbacks — both can be configured simultaneously.
func (r *AgentRunReconciler) fireEgress(ctx context.Context, run *agentorcav1alpha1.AgentRun) {
	if run.Spec.Egress == nil {
		return
	}

	// Build the egress result payload from the run's terminal status.
	result := agentorcav1alpha1.EgressResult{
		RunID:         run.Name,
		Agent:         run.Spec.AgentRef,
		Phase:         string(run.Status.Phase),
		Output:        run.Status.Output,
		SpendUSD:      run.Status.SpendUSD,
		FailureReason: run.Status.FailureReason,
		Tenant:        run.Labels["agentorca.io/tenant"],
	}
	if run.Status.CompletionTime != nil {
		result.CompletedAt = run.Status.CompletionTime.Format(time.RFC3339)
	}
	// Include original metadata annotations as the result's metadata map.
	for k, v := range run.Annotations {
		if strings.HasPrefix(k, "agentorca.io/meta-") {
			if result.Metadata == nil {
				result.Metadata = make(map[string]string)
			}
			result.Metadata[strings.TrimPrefix(k, "agentorca.io/meta-")] = v
		}
	}

	publisher, err := egress.NewPublisher(ctx, *run.Spec.Egress, r.Client, run.Namespace)
	if err != nil {
		log.Log.Error(err, "egress: failed to create publisher", "run", run.Name, "type", run.Spec.Egress.Type)
		return
	}
	defer func() { _ = publisher.Close() }()

	egress.PublishAndRecord(ctx, publisher, result)
}
