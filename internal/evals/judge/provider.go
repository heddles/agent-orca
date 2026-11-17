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

package judge

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/router"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// RouterProvider calls a ModelProvider down the model-router's provider path:
// the wire code the shadow judge and every routed call use, with the provider's
// own prices.
type RouterProvider struct {
	Config router.ProviderConfig
}

// Complete makes one non-streaming call with no tools and temperature 0.
func (p RouterProvider) Complete(ctx context.Context, req Request) (Reply, error) {
	temp := 0.0
	out, err := router.CompleteOnce(ctx, p.Config, router.ChatCompletionRequest{
		Messages: []router.Message{
			{Role: "system", Content: req.System},
			{Role: "user", Content: req.User},
		},
		Temperature: &temp,
		// max_completion_tokens is the field the newer OpenAI models accept; the
		// shadow judge sends it the same way.
		MaxCompletionTokens: req.MaxTokens,
	}, callTimeout)
	return Reply{Text: out.Text, PromptTokens: out.Usage.PromptTokens, CompletionTokens: out.Usage.CompletionTokens, CostUSD: out.CostUSD}, err
}

// PriceUSD prices a token count on the provider.
func (p RouterProvider) PriceUSD(promptTokens, completionTokens int) float64 {
	return router.PriceUSD(p.Config, promptTokens, completionTokens)
}

// ErrProvider is wrapped by every error about naming or reading a judge's provider.
var ErrProvider = errors.New("the judge's provider cannot be used")

// KubeResolver finds a ModelProvider and its API key in the cluster.
type KubeResolver struct {
	Reader client.Reader
}

// Resolve returns the provider named ref for a tenant: the ModelProvider of that
// name in namespace or, failing that, in a platform catalog namespace, if the
// tenant may use it (tenancy.CheckAccess, the check an arm's references get). The
// API key is read from the Secret the provider names, in the provider's own
// namespace: a provider cannot point the operator at another namespace's Secret.
func (r KubeResolver) Resolve(ctx context.Context, caller tenancy.Caller, namespace, ref string) (Provider, error) {
	mp, err := r.find(ctx, caller, namespace, ref)
	if err != nil {
		return nil, err
	}
	var secret corev1.Secret
	if err := r.Reader.Get(ctx, client.ObjectKey{Namespace: mp.Namespace, Name: mp.Spec.CredentialsRef.Name}, &secret); err != nil {
		if k8serrors.IsNotFound(err) {
			return nil, fmt.Errorf("%w: the credentials Secret %q of provider %q does not exist", ErrProvider, mp.Spec.CredentialsRef.Name, ref)
		}
		return nil, err
	}
	key := string(secret.Data[mp.Spec.CredentialsRef.Key])
	if key == "" {
		return nil, fmt.Errorf("%w: the credentials Secret %q has no key %q", ErrProvider, mp.Spec.CredentialsRef.Name, mp.Spec.CredentialsRef.Key)
	}
	return RouterProvider{Config: ProviderConfigFrom(mp, key)}, nil
}

func (r KubeResolver) find(ctx context.Context, caller tenancy.Caller, namespace, ref string) (*agentorcav1alpha1.ModelProvider, error) {
	namespaces := append([]string{namespace}, tenancy.PlatformNamespaces()...)
	for _, ns := range namespaces {
		var mp agentorcav1alpha1.ModelProvider
		err := r.Reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref}, &mp)
		if k8serrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if tenancy.CheckAccess(caller, &mp, "ModelProvider") == tenancy.AccessNone {
			continue
		}
		return &mp, nil
	}
	return nil, fmt.Errorf("%w: ModelProvider %q does not exist in namespace %s or the platform catalog", ErrProvider, ref, namespace)
}

// ProviderConfigFrom maps a ModelProvider to the router's provider config, with
// the key held in memory. It reads the same spec fields the controllers'
// providerConfigFromSpec does.
func ProviderConfigFrom(mp *agentorcav1alpha1.ModelProvider, apiKey string) router.ProviderConfig {
	perMillion := func(s string) float64 {
		v, err := strconv.ParseFloat(s, 64)
		if err != nil || v < 0 {
			return 0
		}
		return v / 1e6
	}
	return router.ProviderConfig{
		Name:               mp.Name,
		LiteLLMModel:       mp.Spec.LiteLLMModel,
		APIKey:             apiKey,
		ContextWindow:      mp.Spec.Constraints.ContextWindow,
		MaxOutputTokens:    mp.Spec.Constraints.MaxOutputTokens,
		BaseURL:            mp.Spec.BaseURL,
		WorkspaceID:        mp.Spec.WorkspaceID,
		CostPerInputToken:  perMillion(mp.Spec.Constraints.CostPerMillionInputTokens),
		CostPerOutputToken: perMillion(mp.Spec.Constraints.CostPerMillionOutputTokens),
	}
}
