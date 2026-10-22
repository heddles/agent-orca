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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/heddles/agent-orca/internal/security"
)

const (
	// hindsightKeySecretName is the per-namespace copy of the hindsight API key.
	// Router pods run in tenant namespaces and cannot reference the Secret the
	// operator reads the key from, so the operator mirrors it here. The agent
	// container never mounts it: only the model-router container does, and the
	// per-run token Secret that is envFrom-ed into agents does not carry it.
	hindsightKeySecretName = "agentorca-hindsight-api-key"
	// hindsightKeySecretKey is both the Secret key and the router env var name.
	hindsightKeySecretKey = "HINDSIGHT_API_KEY"
)

// hindsightKeyRef returns the secretKeyRef the model-router container uses to
// read the hindsight API key, or nil when no key is configured (hindsight runs
// without authentication).
func hindsightKeyRef(apiKey string) *corev1.SecretKeySelector {
	if apiKey == "" {
		return nil
	}
	return &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: hindsightKeySecretName},
		Key:                  hindsightKeySecretKey,
	}
}

// ensureHindsightKeySecret mirrors the operator's hindsight API key into the
// namespace the router pods run in. It is a no-op without a key, creates the
// Secret when missing and rewrites it when the key was rotated. Pods read the
// key at start, so a rotation reaches running warm pods when they are recycled.
func ensureHindsightKeySecret(ctx context.Context, c client.Client, namespace, apiKey string) error {
	if apiKey == "" {
		return nil
	}
	var existing corev1.Secret
	key := types.NamespacedName{Name: hindsightKeySecretName, Namespace: namespace}
	err := c.Get(ctx, key, &existing)
	switch {
	case errors.IsNotFound(err):
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      hindsightKeySecretName,
				Namespace: namespace,
				Labels:    map[string]string{security.LabelManagedBy: security.ManagedByValue},
			},
			Data: map[string][]byte{hindsightKeySecretKey: []byte(apiKey)},
		}
		if err := c.Create(ctx, secret); err != nil && !errors.IsAlreadyExists(err) {
			return fmt.Errorf("creating hindsight key secret in %s: %w", namespace, err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("getting hindsight key secret in %s: %w", namespace, err)
	}
	if string(existing.Data[hindsightKeySecretKey]) == apiKey {
		return nil
	}
	if existing.Data == nil {
		existing.Data = map[string][]byte{}
	}
	existing.Data[hindsightKeySecretKey] = []byte(apiKey)
	if err := c.Update(ctx, &existing); err != nil {
		return fmt.Errorf("updating hindsight key secret in %s: %w", namespace, err)
	}
	return nil
}
