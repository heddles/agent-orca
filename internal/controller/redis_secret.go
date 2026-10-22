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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/heddles/agent-orca/internal/podbuilder"
	"github.com/heddles/agent-orca/internal/security"
)

// redisSecretName returns the Redis password Secret name for a resource prefix
// such as "agentorca-run-<run>", or "" when no password is configured.
func redisSecretName(prefix, password string) string {
	if password == "" {
		return ""
	}
	return prefix + podbuilder.RedisSecretSuffix
}

// ensureRedisPasswordSecret writes the Redis password to a Secret named
// name in owner's namespace, owned by owner, so the model-router mounts it
// instead of reading it from its ConfigMap (issue #107). An empty name (no
// password configured, see redisSecretName) is a no-op.
func ensureRedisPasswordSecret(
	ctx context.Context,
	c client.Client,
	scheme *runtime.Scheme,
	owner client.Object,
	name string,
	password string,
) error {
	if name == "" {
		return nil
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: owner.GetNamespace()},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, c, secret, func() error {
		if secret.Labels == nil {
			secret.Labels = map[string]string{}
		}
		secret.Labels[security.LabelManagedBy] = security.ManagedByValue
		secret.Type = corev1.SecretTypeOpaque
		secret.Data = map[string][]byte{podbuilder.RedisSecretKey: []byte(password)}
		return ctrl.SetControllerReference(owner, secret, scheme)
	}); err != nil {
		return fmt.Errorf("ensuring redis password secret: %w", err)
	}
	return nil
}
