/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/router"
)

// mergeGuardrailPolicyIntoRouterConfig loads the named GuardrailPolicy CR and populates cfg.Guardrails.
// If the policy cannot be fetched or has no filters, cfg.Guardrails is left unchanged.
func mergeGuardrailPolicyIntoRouterConfig(ctx context.Context, c client.Client, namespace, policyName string, cfg *router.Config) {
	if policyName == "" || cfg == nil {
		return
	}
	var gp agentorcav1alpha1.GuardrailPolicy
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: policyName}, &gp); err != nil {
		return
	}
	spec := gp.Spec
	if len(spec.InputFilters) == 0 && len(spec.OutputFilters) == 0 {
		return
	}
	gc := &router.GuardrailConfig{}
	for _, f := range spec.InputFilters {
		gc.InputFilters = append(gc.InputFilters, guardrailFilterToFilterConfig(f))
	}
	for _, f := range spec.OutputFilters {
		gc.OutputFilters = append(gc.OutputFilters, guardrailFilterToFilterConfig(f))
	}
	cfg.Guardrails = gc
}

func guardrailFilterToFilterConfig(f agentorcav1alpha1.GuardrailFilter) router.FilterConfig {
	fc := router.FilterConfig{
		Name:          f.Name,
		Type:          f.Type,
		Action:        f.Action,
		BlockMessage:  f.BlockMessage,
		AllowedTopics: append([]string(nil), f.AllowedTopics...),
	}
	for _, p := range f.Patterns {
		fc.Patterns = append(fc.Patterns, router.PatternConfig{
			Name:        p.Name,
			Pattern:     p.Pattern,
			Replacement: p.Replacement,
		})
	}
	if f.Keywords != nil {
		fc.Keywords = append([]string(nil), f.Keywords.Inline...)
	}
	return fc
}
