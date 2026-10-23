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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/podbuilder"
)

func TestRedisSecretName(t *testing.T) {
	if got := redisSecretName("agentorca-run-r", ""); got != "" {
		t.Errorf("no password: got %q, want empty", got)
	}
	if got := redisSecretName("agentorca-run-r", "pw"); got != "agentorca-run-r-redis" {
		t.Errorf("got %q, want agentorca-run-r-redis", got)
	}
}

func TestEnsureRedisPasswordSecret(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := agentorcav1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	run := &agentorcav1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "agents", UID: "uid-1"},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(run).Build()
	ctx := context.Background()
	key := types.NamespacedName{Name: "agentorca-run-r-redis", Namespace: "agents"}

	if err := ensureRedisPasswordSecret(ctx, cl, scheme, run, key.Name, "first"); err != nil {
		t.Fatal(err)
	}
	var secret corev1.Secret
	if err := cl.Get(ctx, key, &secret); err != nil {
		t.Fatalf("secret not created: %v", err)
	}
	if string(secret.Data[podbuilder.RedisSecretKey]) != "first" {
		t.Errorf("password: got %q, want first", secret.Data[podbuilder.RedisSecretKey])
	}
	if len(secret.OwnerReferences) != 1 || secret.OwnerReferences[0].UID != run.UID {
		t.Errorf("expected the AgentRun to own the secret, got %+v", secret.OwnerReferences)
	}

	// A rotated password is written through on the next reconcile.
	if err := ensureRedisPasswordSecret(ctx, cl, scheme, run, key.Name, "rotated"); err != nil {
		t.Fatal(err)
	}
	if err := cl.Get(ctx, key, &secret); err != nil {
		t.Fatal(err)
	}
	if string(secret.Data[podbuilder.RedisSecretKey]) != "rotated" {
		t.Errorf("password after rotation: got %q, want rotated", secret.Data[podbuilder.RedisSecretKey])
	}

	// No password configured: nothing is created.
	if err := ensureRedisPasswordSecret(ctx, cl, scheme, run, redisSecretName("agentorca-run-x", ""), ""); err != nil {
		t.Fatal(err)
	}
	var secrets corev1.SecretList
	if err := cl.List(ctx, &secrets); err != nil {
		t.Fatal(err)
	}
	if len(secrets.Items) != 1 {
		t.Errorf("expected exactly one secret, got %d", len(secrets.Items))
	}
}
