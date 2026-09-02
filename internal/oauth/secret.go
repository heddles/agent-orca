/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package oauth

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// K8sSecrets implements SecretIO over a Kubernetes cluster using client-go. It is
// what cmd/oauth-proxy uses at runtime; tests use the in-package fakeSecretIO.
type K8sSecrets struct {
	Client kubernetes.Interface
}

// NewK8sSecrets returns a SecretIO backed by a kubernetes clientset.
func NewK8sSecrets(client kubernetes.Interface) *K8sSecrets {
	return &K8sSecrets{Client: client}
}

// ReadKey returns the secret bytes for key, or (nil,false,nil) if absent.
func (k *K8sSecrets) ReadKey(ctx context.Context, namespace, name, key string) ([]byte, bool, error) {
	secret, err := k.Client.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("getting secret %s/%s: %w", namespace, name, err)
	}
	val, ok := secret.Data[key]
	return val, ok, nil
}

// WriteKey creates-or-updates a single key in the Secret. Uses optimistic
// retries on conflicts so two writers (e.g. concurrent refresh) don't clobber.
func (k *K8sSecrets) WriteKey(ctx context.Context, namespace, name, key string, value []byte) error {
	secrets := k.Client.CoreV1().Secrets(namespace)
	for {
		secret, err := secrets.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			created := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
				Type:       corev1.SecretTypeOpaque,
				Data:       map[string][]byte{key: value},
			}
			if _, err := secrets.Create(ctx, created, metav1.CreateOptions{}); err != nil {
				if !apierrors.IsAlreadyExists(err) {
					return fmt.Errorf("creating secret %s/%s: %w", namespace, name, err)
				}
			}
			// Lost the race to create; fall through and retry the update path.
			continue
		}
		if err != nil {
			return fmt.Errorf("getting secret %s/%s: %w", namespace, name, err)
		}
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		// Clear any stale StringData entry for this key so Data wins.
		if secret.StringData != nil {
			delete(secret.StringData, key)
		}
		secret.Data[key] = value
		if _, err := secrets.Update(ctx, secret, metav1.UpdateOptions{}); err != nil {
			if apierrors.IsConflict(err) {
				continue // another writer beat us; retry from Get
			}
			return fmt.Errorf("updating secret %s/%s: %w", namespace, name, err)
		}
		return nil
	}
}
