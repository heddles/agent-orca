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
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/floppyfish14/agent-orca/api/v1alpha1"
)

// ModelProviderReconciler reconciles a ModelProvider object.
type ModelProviderReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=agentorca.agentorca.io,resources=modelproviders,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agentorca.agentorca.io,resources=modelproviders/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentorca.agentorca.io,resources=modelproviders/finalizers,verbs=update

func (r *ModelProviderReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var mp agentorcav1alpha1.ModelProvider
	if err := r.Get(ctx, req.NamespacedName, &mp); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	ready, message := r.validate(&mp)

	patch := client.MergeFrom(mp.DeepCopy())
	mp.Status.Ready = ready
	mp.Status.Message = message
	now := metav1.Now()
	mp.Status.LastChecked = &now

	condStatus := metav1.ConditionTrue
	if !ready {
		condStatus = metav1.ConditionFalse
	}
	setCondition(&mp.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             condStatus,
		Reason:             "Validated",
		Message:            message,
		LastTransitionTime: now,
	})

	if err := r.Status().Patch(ctx, &mp, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("patching ModelProvider status: %w", err)
	}
	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

// validate checks the ModelProvider spec for obvious configuration errors.
// Full health-checking (calling the provider API) is deferred to the model-router sidecar.
func (r *ModelProviderReconciler) validate(mp *agentorcav1alpha1.ModelProvider) (bool, string) {
	if mp.Spec.LiteLLMModel == "" {
		return false, "spec.litellmModel is required"
	}
	if !strings.Contains(mp.Spec.LiteLLMModel, "/") {
		return false, fmt.Sprintf("spec.litellmModel %q must be in <provider>/<model> format (e.g. anthropic/claude-sonnet-4-6)", mp.Spec.LiteLLMModel)
	}
	if mp.Spec.CredentialsRef.Name == "" {
		return false, "spec.credentialsRef.name is required"
	}
	if mp.Spec.CredentialsRef.Key == "" {
		return false, "spec.credentialsRef.key is required"
	}
	return true, fmt.Sprintf("provider %s validated", mp.Spec.LiteLLMModel)
}

// SetupWithManager sets up the controller with the Manager.
func (r *ModelProviderReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentorcav1alpha1.ModelProvider{}).
		Named("modelprovider").
		Complete(r)
}
