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

// Package openshell runs AgentRun workloads in NVIDIA OpenShell sandboxes.
//
// The operator never creates OpenShell's Kubernetes objects itself: it talks to
// the OpenShell gateway over its Go SDK, and the gateway's Kubernetes compute
// driver creates the agent-sandbox Sandbox CR, the supervisor and workload pods,
// and their network fence in the gateway's sandbox namespace. This package owns
// the translation from agent-orca CRDs to a sandbox spec and policy, so upstream
// schema changes stay contained here.
package openshell

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	osv1 "github.com/NVIDIA/OpenShell/sdk/go/openshell/v1"
)

// Gateway is the slice of the OpenShell gateway API the AgentRun controller uses,
// bound to one workspace.
type Gateway interface {
	// Create creates a sandbox. It returns the existing sandbox when one with the
	// same name already exists, so a reconcile retry is idempotent.
	Create(ctx context.Context, name string, spec *osv1.SandboxSpec, labels map[string]string) (*osv1.Sandbox, error)
	// Get returns the sandbox, or an error satisfying osv1.IsNotFound.
	Get(ctx context.Context, name string) (*osv1.Sandbox, error)
	// Delete deletes the sandbox. A missing sandbox is not an error.
	Delete(ctx context.Context, name string) error
	// MainProcessLogs returns the tail of the workload's main-process output.
	MainProcessLogs(ctx context.Context, sb *osv1.Sandbox) (string, error)
	// SandboxNamespace is the Kubernetes namespace the gateway creates sandbox pods in.
	SandboxNamespace() string
	// EnsureProfile creates the provider profile, or updates it when its
	// declared credentials changed.
	EnsureProfile(ctx context.Context, profile *osv1.ProviderProfile) error
	// EnsureProvider creates the provider, or updates its credentials.
	EnsureProvider(ctx context.Context, provider *osv1.Provider) error
	// DeleteProviders deletes every provider carrying all of labels.
	DeleteProviders(ctx context.Context, labels map[string]string) error
}

// Config holds the operator's connection settings for the OpenShell gateway.
type Config struct {
	// Address is the gateway gRPC address, e.g. "openshell.openshell.svc:8080".
	Address string
	// Workspace is the gateway workspace sandboxes are created in.
	Workspace string
	// SandboxNamespace is the namespace the gateway creates sandbox pods in. The
	// router NetworkPolicy admits the run's supervisor pod from this namespace.
	SandboxNamespace string
	// CertFile, KeyFile and CAFile configure mTLS to the gateway.
	CertFile string
	KeyFile  string
	CAFile   string
	// TokenFile optionally holds a bearer token (OIDC gateways).
	TokenFile string
	// Insecure disables TLS verification. Development only.
	Insecure bool
}

// ConfigFromEnv reads the gateway settings from OPENSHELL_* environment
// variables. ok is false when OPENSHELL_GATEWAY_ADDRESS is unset, meaning
// sandbox support is disabled.
func ConfigFromEnv() (cfg Config, ok bool) {
	cfg = Config{
		Address:          strings.TrimSpace(os.Getenv("OPENSHELL_GATEWAY_ADDRESS")),
		Workspace:        envOr("OPENSHELL_WORKSPACE", "default"),
		SandboxNamespace: envOr("OPENSHELL_SANDBOX_NAMESPACE", "openshell"),
		CertFile:         os.Getenv("OPENSHELL_TLS_CERT_FILE"),
		KeyFile:          os.Getenv("OPENSHELL_TLS_KEY_FILE"),
		CAFile:           os.Getenv("OPENSHELL_TLS_CA_FILE"),
		TokenFile:        os.Getenv("OPENSHELL_TOKEN_FILE"),
		Insecure:         os.Getenv("OPENSHELL_TLS_INSECURE") == "true",
	}
	return cfg, cfg.Address != ""
}

func envOr(name, def string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return def
}

// Connect dials the gateway described by cfg. podLogs reads main-process
// output from the workload pod.
func Connect(cfg Config, podLogs PodLogFunc) (Gateway, error) {
	sdkCfg := osv1.Config{
		Address: cfg.Address,
		Timeout: 30 * time.Second,
	}
	if cfg.CertFile != "" || cfg.CAFile != "" || cfg.Insecure {
		sdkCfg.TLS = &osv1.TLSConfig{
			CertFile: cfg.CertFile,
			KeyFile:  cfg.KeyFile,
			CAFile:   cfg.CAFile,
			Insecure: cfg.Insecure,
		}
	}
	if cfg.TokenFile != "" {
		token, err := os.ReadFile(cfg.TokenFile)
		if err != nil {
			return nil, fmt.Errorf("reading OpenShell gateway token: %w", err)
		}
		sdkCfg.Auth = osv1.StaticToken(strings.TrimSpace(string(token)))
	}
	client, err := osv1.NewClient(sdkCfg)
	if err != nil {
		return nil, fmt.Errorf("connecting to OpenShell gateway %s: %w", cfg.Address, err)
	}
	return NewGateway(client.Sandboxes(), client.Providers(), cfg.Workspace, cfg.SandboxNamespace, podLogs), nil
}

// PodLogFunc reads a container's logs from a pod. The gateway does not relay
// main-process output, so the operator reads it from the workload pod directly.
type PodLogFunc func(ctx context.Context, namespace, pod, container string) (string, error)

// NewGateway binds a sandbox client to a workspace. With a nil podLogs,
// MainProcessLogs returns an error.
func NewGateway(sandboxes osv1.SandboxInterface, providers osv1.ProviderInterface, workspace, sandboxNamespace string, podLogs PodLogFunc) *SDKGateway {
	return &SDKGateway{sandboxes: sandboxes, providers: providers, workspace: workspace, namespace: sandboxNamespace, podLogs: podLogs}
}

// SDKGateway implements Gateway over the OpenShell Go SDK.
type SDKGateway struct {
	sandboxes osv1.SandboxInterface
	providers osv1.ProviderInterface
	workspace string
	namespace string
	podLogs   PodLogFunc
}

// SandboxNamespace implements Gateway.
func (g *SDKGateway) SandboxNamespace() string { return g.namespace }

// Create implements Gateway.
func (g *SDKGateway) Create(ctx context.Context, name string, spec *osv1.SandboxSpec, labels map[string]string) (*osv1.Sandbox, error) {
	sb, err := g.sandboxes.Create(ctx, g.workspace, name, spec, osv1.WithLabels(labels))
	if osv1.IsAlreadyExists(err) {
		return g.sandboxes.Get(ctx, g.workspace, name)
	}
	return sb, err
}

// Get implements Gateway.
func (g *SDKGateway) Get(ctx context.Context, name string) (*osv1.Sandbox, error) {
	return g.sandboxes.Get(ctx, g.workspace, name)
}

// Delete implements Gateway.
func (g *SDKGateway) Delete(ctx context.Context, name string) error {
	_, err := g.sandboxes.Delete(ctx, g.workspace, name, osv1.DeleteOptions{AllowMissing: true})
	if osv1.IsNotFound(err) {
		return nil
	}
	return err
}

// workloadContainer is the container the Kubernetes driver runs the main process in.
const workloadContainer = "agent"

// MainProcessLogs implements Gateway.
func (g *SDKGateway) MainProcessLogs(ctx context.Context, sb *osv1.Sandbox) (string, error) {
	if g.podLogs == nil {
		return "", fmt.Errorf("pod log reader not configured")
	}
	pod := sb.Status.AgentPod
	if pod == "" {
		// The gateway clears AgentPod once the main process fails. The Kubernetes
		// driver names the workload pod after the Sandbox resource,
		// "<workspace>--<sandbox>", so read that pod's logs instead.
		pod = g.workspace + "--" + sb.Name
	}
	return g.podLogs(ctx, g.namespace, pod, workloadContainer)
}

// profileSource tags profiles the operator imports.
const profileSource = "agent-orca"

// EnsureProfile implements Gateway.
func (g *SDKGateway) EnsureProfile(ctx context.Context, profile *osv1.ProviderProfile) error {
	profiles := g.providers.Profiles()
	existing, err := profiles.Get(ctx, g.workspace, profile.ID)
	item := osv1.ProfileImportItem{Profile: *profile, Source: profileSource}
	switch {
	case osv1.IsNotFound(err):
		res, err := profiles.Import(ctx, g.workspace, []osv1.ProfileImportItem{item})
		if err != nil {
			if osv1.IsAlreadyExists(err) {
				return nil // a concurrent reconcile imported it
			}
			return fmt.Errorf("importing provider profile %s: %w", profile.ID, err)
		}
		if !res.Imported {
			return fmt.Errorf("provider profile %s rejected: %s", profile.ID, diagnostics(res.Diagnostics))
		}
		return nil
	case err != nil:
		return fmt.Errorf("reading provider profile %s: %w", profile.ID, err)
	}
	if sameCredentials(existing.Credentials, profile.Credentials) {
		return nil
	}
	res, err := profiles.Update(ctx, g.workspace, profile.ID, existing.ResourceVersion, item)
	if err != nil {
		return fmt.Errorf("updating provider profile %s: %w", profile.ID, err)
	}
	if !res.Updated {
		return fmt.Errorf("provider profile %s update rejected: %s", profile.ID, diagnostics(res.Diagnostics))
	}
	return nil
}

func sameCredentials(a, b []osv1.ProfileCredential) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name || !slices.Equal(a[i].EnvVars, b[i].EnvVars) ||
			a[i].AuthStyle != b[i].AuthStyle || !strings.EqualFold(a[i].HeaderName, b[i].HeaderName) {
			return false
		}
	}
	return true
}

func diagnostics(ds []osv1.ProfileDiagnostic) string {
	msgs := make([]string, 0, len(ds))
	for _, d := range ds {
		msgs = append(msgs, d.Field+": "+d.Message)
	}
	if len(msgs) == 0 {
		return "no diagnostics"
	}
	return strings.Join(msgs, "; ")
}

// EnsureProvider implements Gateway. The provider's profile is looked up in
// the gateway workspace EnsureProfile imports into; an empty ProfileWorkspace
// would mean the global scope.
func (g *SDKGateway) EnsureProvider(ctx context.Context, provider *osv1.Provider) error {
	p := *provider
	p.Spec.ProfileWorkspace = g.workspace
	if _, err := g.providers.Ensure(ctx, g.workspace, &p); err != nil {
		return fmt.Errorf("ensuring provider %s: %w", provider.Name, err)
	}
	return nil
}

// DeleteProviders implements Gateway.
func (g *SDKGateway) DeleteProviders(ctx context.Context, labels map[string]string) error {
	selector := make([]string, 0, len(labels))
	for k, v := range labels {
		selector = append(selector, k+"="+v)
	}
	slices.Sort(selector)
	providers, err := g.providers.ListAll(ctx, g.workspace, osv1.ListOptions{LabelSelector: strings.Join(selector, ",")})
	if err != nil {
		return fmt.Errorf("listing providers: %w", err)
	}
	var errs []error
	for _, p := range providers {
		if _, err := g.providers.Delete(ctx, g.workspace, p.Name, osv1.DeleteOptions{AllowMissing: true}); err != nil && !osv1.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("deleting provider %s: %w", p.Name, err))
		}
	}
	return errors.Join(errs...)
}
