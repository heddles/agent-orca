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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func hindsightKeyTestClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func getHindsightKeySecret(t *testing.T, c client.Client, ns string) (*corev1.Secret, bool) {
	t.Helper()
	var s corev1.Secret
	err := c.Get(context.Background(), types.NamespacedName{Name: hindsightKeySecretName, Namespace: ns}, &s)
	if err != nil {
		return nil, false
	}
	return &s, true
}

func TestHindsightKeyRef(t *testing.T) {
	if hindsightKeyRef("") != nil {
		t.Fatal("no key configured must yield no ref")
	}
	ref := hindsightKeyRef("k")
	if ref == nil || ref.Name != hindsightKeySecretName || ref.Key != hindsightKeySecretKey {
		t.Fatalf("unexpected ref %+v", ref)
	}
}

func TestEnsureHindsightKeySecret_NoKeyIsNoop(t *testing.T) {
	c := hindsightKeyTestClient(t)
	if err := ensureHindsightKeySecret(context.Background(), c, "tenant-a", ""); err != nil {
		t.Fatal(err)
	}
	if _, found := getHindsightKeySecret(t, c, "tenant-a"); found {
		t.Fatal("a Secret was created although no key is configured")
	}
}

func TestEnsureHindsightKeySecret_CreatesInRunNamespace(t *testing.T) {
	c := hindsightKeyTestClient(t)
	if err := ensureHindsightKeySecret(context.Background(), c, "tenant-a", "s3cret"); err != nil {
		t.Fatal(err)
	}
	s, found := getHindsightKeySecret(t, c, "tenant-a")
	if !found {
		t.Fatal("Secret not created")
	}
	if got := string(s.Data[hindsightKeySecretKey]); got != "s3cret" {
		t.Fatalf("stored key = %q, want %q", got, "s3cret")
	}
	if _, other := getHindsightKeySecret(t, c, "tenant-b"); other {
		t.Fatal("Secret leaked into a namespace that never ran anything")
	}
}

func TestEnsureHindsightKeySecret_Idempotent(t *testing.T) {
	c := hindsightKeyTestClient(t)
	for range 3 {
		if err := ensureHindsightKeySecret(context.Background(), c, "tenant-a", "s3cret"); err != nil {
			t.Fatal(err)
		}
	}
	if s, found := getHindsightKeySecret(t, c, "tenant-a"); !found || string(s.Data[hindsightKeySecretKey]) != "s3cret" {
		t.Fatalf("unexpected secret after repeated ensure: %+v", s)
	}
}

// A rotated key must replace the mirrored copy, otherwise new pods keep the old
// (now rejected) key.
func TestEnsureHindsightKeySecret_RotationUpdates(t *testing.T) {
	old := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: hindsightKeySecretName, Namespace: "tenant-a"},
		Data:       map[string][]byte{hindsightKeySecretKey: []byte("old")},
	}
	c := hindsightKeyTestClient(t, old)
	if err := ensureHindsightKeySecret(context.Background(), c, "tenant-a", "new"); err != nil {
		t.Fatal(err)
	}
	s, _ := getHindsightKeySecret(t, c, "tenant-a")
	if got := string(s.Data[hindsightKeySecretKey]); got != "new" {
		t.Fatalf("stored key = %q after rotation, want %q", got, "new")
	}
}
