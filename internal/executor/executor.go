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

// Package executor provides tool execution for the model-router sidecar. The router
// invokes [Executor.Dispatch] in-process (no separate HTTP listener). The executor dispatches to the correct backend:
//
//   - regular (pod):     spawns a fresh pod per invocation via the Kubernetes API
//   - regular (sidecar): forwards to a sidecar container via unix socket
//   - agent:             creates a child AgentRun CRD and waits for completion
//   - mcp:               calls the MCP client (running in the model-router sidecar)
//   - wasm:              runs a WASM module embedded in the executor process
package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcv1alpha1 "github.com/floppyfish14/agent-orc/api/v1alpha1"
	"github.com/floppyfish14/agent-orc/internal/security"
	"github.com/floppyfish14/agent-orc/internal/state"
)

// ToolExecuteRequest is the JSON body sent to localhost:8081/execute.
type ToolExecuteRequest struct {
	Tool        string          `json:"tool"`
	BackendType string          `json:"backendType"`
	Arguments   string          `json:"arguments"` // JSON-encoded
	RunName     string          `json:"runName"`
	Namespace   string          `json:"namespace"`
	SecretRefs  []ToolSecretRef `json:"secretRefs,omitempty"`
	Command     []string        `json:"command,omitempty"`
	Args        []string        `json:"args,omitempty"`
}

// ToolSecretRef describes a Secret to inject into a tool pod.
type ToolSecretRef struct {
	SecretName string `json:"secretName"`
	MountPath  string `json:"mountPath,omitempty"`
}

// ToolExecuteResponse is the JSON body returned by localhost:8081/execute.
type ToolExecuteResponse struct {
	Result        string `json:"result"`
	Error         string `json:"error,omitempty"`
	ChildSpendUSD string `json:"childSpendUSD,omitempty"`
}

// Executor handles tool dispatch requests from the model-router sidecar.
type Executor struct {
	k8s       kubernetes.Interface
	crdClient client.Client //nolint:unused

	namespace      string
	runName        string
	agentSA        string
	operatorAPIURL string
	saTokenFile    string
	store          state.Store // optional; enables child→parent token forwarding
	parentTokenKey string      // "tokens:{namespace}:{runName}"
}

// WithStore enables real-time token forwarding from child AgentRuns into the parent's token stream.
// When set, tokens produced by child agents are forwarded to the parent's Redis token stream so
// the UI sees streaming output during sub-agent execution rather than waiting for completion.
func (e *Executor) WithStore(s state.Store) {
	e.store = s
	e.parentTokenKey = "tokens:" + e.namespace + ":" + e.runName
}

// New creates an Executor using in-cluster Kubernetes credentials.
func New(namespace, runName, agentSA, operatorAPIURL, saTokenFile string) (*Executor, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("in-cluster config: %w", err)
	}
	k8s, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kubernetes client: %w", err)
	}
	return &Executor{k8s: k8s, namespace: namespace, runName: runName, agentSA: agentSA, operatorAPIURL: operatorAPIURL, saTokenFile: saTokenFile}, nil
}

// Dispatch executes a tool request in-process (used by the model-router sidecar).
// MCP tools must be invoked via the router's MCP client; this path returns an error for backendType=mcp.
func (e *Executor) Dispatch(ctx context.Context, req ToolExecuteRequest) (ToolExecuteResponse, error) {
	result, childSpend, execErr := e.dispatch(ctx, req)
	resp := ToolExecuteResponse{Result: result, ChildSpendUSD: childSpend}
	if execErr != nil {
		resp.Error = execErr.Error()
	}
	return resp, nil
}

// ServeHTTP implements http.Handler for the executor service.
func (e *Executor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/execute" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "reading body", http.StatusBadRequest)
		return
	}

	var req ToolExecuteRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	result, childSpend, execErr := e.dispatch(r.Context(), req)
	resp := ToolExecuteResponse{Result: result, ChildSpendUSD: childSpend}
	if execErr != nil {
		resp.Error = execErr.Error()
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// dispatch routes a tool call to the appropriate executor backend.
// Returns (result, childSpendUSD, error). childSpendUSD is only populated for agent-type tools.
func (e *Executor) dispatch(ctx context.Context, req ToolExecuteRequest) (string, string, error) {
	switch req.BackendType {
	case "agent":
		result, spend, err := e.executeAgentTool(ctx, req)
		return result, spend, err
	case "mcp":
		return "", "", fmt.Errorf("mcp tools are dispatched by model-router; backendType=mcp must not reach executor")
	case "regular", "":
		result, err := e.executePodTool(ctx, req)
		return result, "", err
	default:
		return "", "", fmt.Errorf("unknown backend type: %s", req.BackendType)
	}
}

// executePodTool spawns a fresh pod for the tool invocation and waits for it to complete.
// The pod runs the tool OCI image with the tool arguments injected as TOOL_INPUT env var.
// The pod's stdout is captured as the tool result.
func (e *Executor) executePodTool(ctx context.Context, req ToolExecuteRequest) (string, error) {
	podName := fmt.Sprintf("tool-%s-%d", sanitizeName(req.Tool), time.Now().UnixNano()%1000000)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: e.namespace,
			Labels: map[string]string{
				security.LabelAgentRunName: security.SafeLabelValue(e.runName),
				security.LabelManagedBy:    security.ManagedByValue,
				"agentorc.io/tool":         sanitizeName(req.Tool),
			},
		},
		Spec: corev1.PodSpec{
			ServiceAccountName: e.agentSA,
			RestartPolicy:      corev1.RestartPolicyNever,
			Containers: []corev1.Container{
				{
					Name:            "tool",
					Image:           req.Tool, // tool OCI image; resolved by controller before this point
					ImagePullPolicy: corev1.PullIfNotPresent,
					Env: []corev1.EnvVar{
						{Name: "TOOL_INPUT", Value: req.Arguments},
						{Name: "TOOL_NAME", Value: req.Tool},
					},
				},
			},
		},
	}
	if len(req.Command) > 0 {
		pod.Spec.Containers[0].Command = req.Command
	}
	if len(req.Args) > 0 {
		pod.Spec.Containers[0].Args = req.Args
	}

	// Mount tool-level secrets.
	for i, sr := range req.SecretRefs {
		volName := fmt.Sprintf("tool-secret-%d", i)
		if sr.MountPath != "" {
			pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
				Name: volName,
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{SecretName: sr.SecretName},
				},
			})
			pod.Spec.Containers[0].VolumeMounts = append(pod.Spec.Containers[0].VolumeMounts,
				corev1.VolumeMount{
					Name:      volName,
					MountPath: sr.MountPath,
					ReadOnly:  true,
				},
			)
		} else {
			pod.Spec.Containers[0].EnvFrom = append(pod.Spec.Containers[0].EnvFrom,
				corev1.EnvFromSource{
					SecretRef: &corev1.SecretEnvSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: sr.SecretName},
					},
				},
			)
		}
	}

	// Apply security hardening. Tool pods are standalone and never carry an agent
	// override — they always receive the restricted baseline.
	security.EnforcePodSecurity(pod, nil)

	_, err := e.k8s.CoreV1().Pods(e.namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("creating tool pod: %w", err)
	}
	defer func() {
		_ = e.k8s.CoreV1().Pods(e.namespace).Delete(context.Background(), podName, metav1.DeleteOptions{})
	}()

	// Wait for pod completion (max 5 minutes).
	if err := e.waitForPod(ctx, podName, 5*time.Minute); err != nil {
		return "", fmt.Errorf("waiting for tool pod: %w", err)
	}

	// Collect stdout.
	logs, err := e.k8s.CoreV1().Pods(e.namespace).GetLogs(podName, &corev1.PodLogOptions{}).Do(ctx).Raw()
	if err != nil {
		return "", fmt.Errorf("collecting tool logs: %w", err)
	}
	return string(logs), nil
}

// executeAgentTool creates a child AgentRun for the given agent and waits for completion.
// This implements the orchestrator pattern for agent-to-agent communication.
// Returns (output, childSpendUSD, error).
func (e *Executor) executeAgentTool(ctx context.Context, req ToolExecuteRequest) (string, string, error) {
	var args struct {
		Task string `json:"task"`
	}
	if err := json.Unmarshal([]byte(req.Arguments), &args); err != nil {
		return "", "", fmt.Errorf("parsing agent tool arguments: %w", err)
	}

	// Determine AgentRun name. Kubernetes label values are limited to 63 bytes,
	// and child run names are used as label values, so we must keep names short.
	// Truncate the parent name to 50 chars to leave room for the "-child-NNNNNN" suffix.
	parentPrefix := e.runName
	if len(parentPrefix) > 50 {
		parentPrefix = parentPrefix[:50]
	}
	runName := fmt.Sprintf("%s-child-%d", parentPrefix, time.Now().UnixNano()%1000000)

	timeout := metav1.Duration{Duration: 5 * time.Minute}
	childRun := &agentorcv1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      runName,
			Namespace: e.namespace,
			Labels: map[string]string{
				security.LabelManagedBy:  security.ManagedByValue,
				"agentorc.io/parent-run": e.runName,
			},
		},
		Spec: agentorcv1alpha1.AgentRunSpec{
			AgentRef:     req.Tool, // for agent-type tools, Tool = AgentRef
			Input:        args.Task,
			Timeout:      &timeout,
			ParentRunRef: e.runName,
		},
	}

	// Read the SA token to authenticate to the operator's internal API.
	saToken, err := e.readSAToken()
	if err != nil {
		return "", "", fmt.Errorf("reading SA token: %w", err)
	}

	body, err := json.Marshal(childRun)
	if err != nil {
		return "", "", err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/agentrun/%s", e.operatorAPIURL, e.namespace), bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+saToken)

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return "", "", fmt.Errorf("creating child AgentRun: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", "", fmt.Errorf("AgentRun creation failed (%d): %s", resp.StatusCode, b)
	}

	slog.Info("child AgentRun created", "run", runName, "agent", req.Tool)

	// Forward child tokens into the parent's token stream so the UI sees real-time output.
	// The goroutine exits when the child's stream closes or when executeAgentTool returns.
	if e.store != nil {
		childTokenKey := "tokens:" + e.namespace + ":" + runName
		forwardCtx, cancelForward := context.WithCancel(ctx)
		defer cancelForward()
		go func() {
			ch, err := e.store.TailTokens(forwardCtx, childTokenKey)
			if err != nil {
				slog.Warn("could not tail child token stream", "key", childTokenKey, "err", err)
				return
			}
			for token := range ch {
				if token == "" {
					// done sentinel from child — do not forward; parent has its own sentinel
					return
				}
				if err := e.store.SaveToken(forwardCtx, e.parentTokenKey, token); err != nil {
					slog.Warn("failed to forward child token to parent stream", "childKey", childTokenKey, "err", err)
					return
				}
			}
		}()
	}

	// Poll for completion (simple backoff; production would use watch).
	var output, childSpend string
	pollErr := wait.PollUntilContextTimeout(ctx, 2*time.Second, 10*time.Minute, true,
		func(ctx context.Context) (bool, error) {
			pollReq, err := http.NewRequestWithContext(ctx, http.MethodGet,
				fmt.Sprintf("%s/agentrun/%s/%s", e.operatorAPIURL, e.namespace, runName), nil)
			if err != nil {
				return false, nil
			}
			pollReq.Header.Set("Authorization", "Bearer "+saToken)
			statusResp, err := http.DefaultClient.Do(pollReq)
			if err != nil {
				return false, nil
			}
			defer func() { _ = statusResp.Body.Close() }()
			if statusResp.StatusCode != http.StatusOK {
				return false, nil
			}
			var run agentorcv1alpha1.AgentRun
			if err := json.NewDecoder(statusResp.Body).Decode(&run); err != nil {
				return false, nil
			}
			switch run.Status.Phase {
			case agentorcv1alpha1.AgentRunPhaseSucceeded:
				output = run.Status.Output
				childSpend = run.Status.SpendUSD
				return true, nil
			case agentorcv1alpha1.AgentRunPhaseFailed:
				childSpend = run.Status.SpendUSD
				reason := run.Status.FailureReason
				if reason == "" {
					reason = "no reason provided"
				}
				return true, fmt.Errorf("child AgentRun %s failed: %s", runName, reason)
			case agentorcv1alpha1.AgentRunPhaseWaitingForInput:
				// The child run needs human input. Surface the question back to the
				// parent orchestrator as a tool error so its LLM can use _clarify
				// to ask the human — the existing human-in-the-loop path.
				question := run.Status.ClarifyQuestion
				if question == "" {
					question = "child run requires human input but no question was recorded"
				}
				return true, fmt.Errorf("child run needs human input: %s", question)
			}
			return false, nil
		})
	if pollErr != nil {
		return "", childSpend, pollErr
	}
	return output, childSpend, nil
}

// waitForPod polls until the named pod reaches a terminal state.
func (e *Executor) waitForPod(ctx context.Context, podName string, timeout time.Duration) error {
	return wait.PollUntilContextTimeout(ctx, 1*time.Second, timeout, true,
		func(ctx context.Context) (bool, error) {
			pod, err := e.k8s.CoreV1().Pods(e.namespace).Get(ctx, podName, metav1.GetOptions{})
			if err != nil {
				return false, nil
			}
			switch pod.Status.Phase {
			case corev1.PodSucceeded:
				return true, nil
			case corev1.PodFailed:
				return true, fmt.Errorf("tool pod failed")
			}
			return false, nil
		})
}

// readSAToken reads the projected ServiceAccount token from disk.
func (e *Executor) readSAToken() (string, error) {
	tokenFile := e.saTokenFile
	if tokenFile == "" {
		tokenFile = "/var/run/secrets/agentorc/token"
	}
	data, err := os.ReadFile(tokenFile)
	if err != nil {
		return "", fmt.Errorf("reading SA token from %s: %w", tokenFile, err)
	}
	return strings.TrimSpace(string(data)), nil
}

// sanitizeName converts a tool name to a DNS-safe string for use in pod names.
func sanitizeName(name string) string {
	result := make([]byte, 0, len(name))
	for _, c := range []byte(name) {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			result = append(result, c)
		} else if c >= 'A' && c <= 'Z' {
			result = append(result, c+32) // lowercase
		} else {
			result = append(result, '-')
		}
	}
	if len(result) > 40 {
		result = result[:40]
	}
	return string(result)
}
