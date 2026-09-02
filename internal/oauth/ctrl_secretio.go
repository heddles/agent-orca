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
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CtrlSecretIO implements SecretIO against a controller-runtime client (the one the
// operator's controllers already hold). The MCPServer reconciler uses it to satisfy
// OAuth-managed MCPServers without a separate sidecar proxy.
type CtrlSecretIO struct {
	Client client.Client
}

// NewCtrlSecretIO returns a SecretIO backed by a controller-runtime client.
func NewCtrlSecretIO(c client.Client) *CtrlSecretIO { return &CtrlSecretIO{Client: c} }

func (c *CtrlSecretIO) ReadKey(ctx context.Context, namespace, name, key string) ([]byte, bool, error) {
	var s corev1.Secret
	if err := c.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &s); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("getting secret %s/%s: %w", namespace, name, err)
	}
	v, ok := s.Data[key]
	return v, ok, nil
}

func (c *CtrlSecretIO) WriteKey(ctx context.Context, namespace, name, key string, value []byte) error {
	for {
		var s corev1.Secret
		if err := c.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &s); err != nil {
			if !apierrors.IsNotFound(err) {
				return fmt.Errorf("getting secret %s/%s: %w", namespace, name, err)
			}
			// Secret doesn't exist yet — create it.
			s = corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
				Type:       corev1.SecretTypeOpaque,
				Data:       map[string][]byte{key: value},
			}
			if err := c.Client.Create(ctx, &s); err != nil {
				if !apierrors.IsAlreadyExists(err) {
					return fmt.Errorf("creating secret %s/%s: %w", namespace, name, err)
				}
			}
			continue
		}
		if s.Data == nil {
			s.Data = map[string][]byte{}
		}
		s.Data[key] = value
		if err := c.Client.Update(ctx, &s); err != nil {
			if apierrors.IsConflict(err) {
				continue // another writer beat us; retry from Get
			}
			return fmt.Errorf("updating secret %s/%s: %w", namespace, name, err)
		}
		return nil
	}
}
