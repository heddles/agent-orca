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
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

// errSandboxClassUnusable fails workloads whose SandboxClass is missing or not
// available to their namespace.
var errSandboxClassUnusable = errors.New("sandbox class unusable")

// loadSandboxClass resolves a class shared by AgentRuns and AgentWorkflows.
func loadSandboxClass(ctx context.Context, cl client.Client, namespace, name string) (*agentorcav1alpha1.SandboxClass, error) {
	if name == "" {
		return nil, nil
	}
	var class agentorcav1alpha1.SandboxClass
	if err := cl.Get(ctx, types.NamespacedName{Name: name}, &class); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("%w: SandboxClass %q not found", errSandboxClassUnusable, name)
		}
		return nil, fmt.Errorf("reading SandboxClass %q: %w", name, err)
	}
	if sel := class.Spec.NamespaceSelector; sel != nil {
		var ns corev1.Namespace
		if err := cl.Get(ctx, types.NamespacedName{Name: namespace}, &ns); err != nil {
			return nil, fmt.Errorf("reading namespace %q: %w", namespace, err)
		}
		selector, err := metav1.LabelSelectorAsSelector(sel)
		if err != nil {
			return nil, fmt.Errorf("%w: SandboxClass %q has an invalid namespaceSelector: %v", errSandboxClassUnusable, name, err)
		}
		if !selector.Matches(labels.Set(ns.Labels)) {
			return nil, fmt.Errorf("%w: SandboxClass %q is not available to namespace %q", errSandboxClassUnusable, name, namespace)
		}
	}
	return &class, nil
}
