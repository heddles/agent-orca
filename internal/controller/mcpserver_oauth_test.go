/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentorcav1alpha1 "github.com/floppyfish14/agent-orca/api/v1alpha1"
)

func ignoreNotFoundDelete(obj client.Object) {
	err := k8sClient.Delete(context.Background(), obj)
	if err != nil && !apierrors.IsNotFound(err) {
		Fail(err.Error())
	}
}

var _ = Describe("MCPServer OAuth (forward-only; client does the exchange)", func() {
	const ns = "default"
	ctx := context.Background()

	AfterEach(func() {
		ignoreNotFoundDelete(&agentorcav1alpha1.MCPServer{ObjectMeta: metav1.ObjectMeta{Name: "slack-mcp", Namespace: ns}})
		ignoreNotFoundDelete(&agentorcav1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{Name: "slack-mcp", Namespace: ns}})
		ignoreNotFoundDelete(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "slack-mcp-oauth", Namespace: ns}})
	})

	It("forwards auth.oauth to the connector Tool and creates no bearerSecret", func() {
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "slack-mcp-oauth", Namespace: ns},
			Type:       corev1.SecretTypeOpaque,
			StringData: map[string]string{
				"client_id":     "cid",
				"client_secret": "csec",
				"refresh_token": "rt",
			},
		})).To(Succeed())
		Expect(k8sClient.Create(ctx, &agentorcav1alpha1.MCPServer{
			ObjectMeta: metav1.ObjectMeta{Name: "slack-mcp", Namespace: ns},
			Spec: agentorcav1alpha1.MCPServerSpec{
				Transport: "http",
				URL:       "https://mcp.slack.com/mcp",
				Auth: &agentorcav1alpha1.MCPAuthConfig{
					OAuth: &agentorcav1alpha1.MCPOAuthConfig{
						Credentials: agentorcav1alpha1.LocalObjectRef{Name: "slack-mcp-oauth"},
						Scopes:      []string{"search:read.public"},
					},
				},
			},
		})).To(Succeed())

		_, err := (&MCPServerReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}).Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: "slack-mcp", Namespace: ns},
		})
		Expect(err).NotTo(HaveOccurred())

		var tool agentorcav1alpha1.Tool
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "slack-mcp", Namespace: ns}, &tool)).To(Succeed())
		Expect(tool.Spec.MCPConfig.Auth).NotTo(BeNil())
		// OAuth is forwarded to the child Tool (the model-router performs the exchange
		// in-memory); NO bearerToken is synthesized, so no token Secret is mounted.
		Expect(tool.Spec.MCPConfig.Auth.OAuth).NotTo(BeNil())
		Expect(tool.Spec.MCPConfig.Auth.OAuth.Credentials.Name).To(Equal("slack-mcp-oauth"))
		Expect(tool.Spec.MCPConfig.Auth.BearerToken).To(BeNil())

		var updated agentorcav1alpha1.MCPServer
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "slack-mcp", Namespace: ns}, &updated)).To(Succeed())
		Expect(updated.Status.Ready).To(BeTrue())
		// No token Secret is created by the controller (the model-router does OAuth in-memory).
		var tokenSec corev1.Secret
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: "slack-mcp-token", Namespace: ns}, &tokenSec))).To(BeTrue())
	})
})
