package tenancy

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

// Ref is one reference in an object's spec to another object.
type Ref struct {
	// Kind of the target: an agent-orca kind, "Secret", "ConfigMap" or "ServiceAccount".
	Kind string
	// Namespace of the target; empty means the referencing object's namespace.
	Namespace string
	Name      string
	// Field is the spec path, for messages.
	Field string
}

func (r Ref) String() string { return fmt.Sprintf("%s %q (%s)", r.Kind, r.Name, r.Field) }

// RefsOf lists every spec reference obj makes to another object, for the kinds
// that carry tenant ownership. A reference to a Secret is found by type, so a
// new SecretKeyRef or SecretMount field is covered without editing this list.
func RefsOf(obj client.Object) []Ref {
	var refs []Ref
	add := func(kind, field string, names ...string) {
		for _, n := range names {
			if n != "" {
				refs = append(refs, Ref{Kind: kind, Name: n, Field: field})
			}
		}
	}
	switch o := obj.(type) {
	case *agentorcav1alpha1.Agent:
		s := &o.Spec
		add("ModelSelector", "spec.modelSelectorRef", s.ModelSelectorRef)
		add("Tool", "spec.tools", s.Tools...)
		add("KnowledgeBase", "spec.knowledgeBases", s.KnowledgeBases...)
		for _, k := range s.KnowledgeBaseRefs {
			add("KnowledgeBase", "spec.knowledgeBaseRefs", k.Name)
		}
		add("GuardrailPolicy", "spec.guardrailPolicyRef", s.GuardrailPolicyRef)
		if s.ServiceAccountRef != nil {
			add("ServiceAccount", "spec.serviceAccountRef", s.ServiceAccountRef.Name)
		}
		if s.Memory != nil {
			add("ModelSelector", "spec.memory.summaryModelSelectorRef", s.Memory.SummaryModelSelectorRef)
			add("KnowledgeBase", "spec.memory.longTermMemoryRef", s.Memory.LongTermMemoryRef)
		}
	case *agentorcav1alpha1.Tool:
		add("Agent", "spec.agentRef", o.Spec.AgentRef)
	case *agentorcav1alpha1.KnowledgeBase:
		add("ModelSelector", "spec.embedding.modelSelectorRef", o.Spec.Embedding.ModelSelectorRef)
		if in := o.Spec.Ingestion; in != nil {
			for _, m := range in.MCP {
				add("MCPServer", "spec.ingestion.mcp.mcpServerRef", m.MCPServerRef)
			}
			for _, c := range in.ConfigMapRefs {
				add("ConfigMap", "spec.ingestion.configMapRefs", c.Name)
			}
		}
	case *agentorcav1alpha1.ModelSelector:
		for _, p := range o.Spec.Providers {
			add("ModelProvider", "spec.providers", p.Name)
		}
		add("ModelProvider", "spec.fallbackChain", o.Spec.FallbackChain...)
		if o.Spec.MetaRouter != nil {
			add("ModelProvider", "spec.metaRouter.providerRef", o.Spec.MetaRouter.ProviderRef)
		}
	case *agentorcav1alpha1.AgentDeployment:
		add("Agent", "spec.agentRef", o.Spec.AgentRef)
	case *agentorcav1alpha1.AgentWorkflow:
		for _, st := range o.Spec.Steps {
			add("Agent", "spec.steps["+st.Name+"].agentRef", st.AgentRef)
		}
		if p := o.Spec.AdaptivePolicy; p != nil {
			add("Agent", "spec.adaptivePolicy.allowedAgentRefs", p.AllowedAgentRefs...)
		}
	case *agentorcav1alpha1.AgentRun:
		add("Agent", "spec.agentRef", o.Spec.AgentRef)
	}
	if spec := specValue(obj); spec.IsValid() {
		collectSecretRefs(spec, "spec", &refs)
	}
	return refs
}

func specValue(obj client.Object) reflect.Value {
	v := reflect.ValueOf(obj)
	for v.Kind() == reflect.Pointer && !v.IsNil() {
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return reflect.Value{}
	}
	return v.FieldByName("Spec")
}

var (
	secretKeyRefType      = reflect.TypeFor[agentorcav1alpha1.SecretKeyRef]()
	secretMountType       = reflect.TypeFor[agentorcav1alpha1.SecretMount]()
	secretKeySelectorType = reflect.TypeFor[corev1.SecretKeySelector]()
)

// collectSecretRefs walks v and records every Secret reference it holds.
func collectSecretRefs(v reflect.Value, path string, out *[]Ref) {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			collectSecretRefs(v.Elem(), path, out)
		}
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			collectSecretRefs(v.Index(i), fmt.Sprintf("%s[%d]", path, i), out)
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			collectSecretRefs(v.MapIndex(k), fmt.Sprintf("%s[%v]", path, k), out)
		}
	case reflect.Struct:
		switch v.Type() {
		case secretKeyRefType:
			r := v.Interface().(agentorcav1alpha1.SecretKeyRef)
			if r.Name != "" {
				*out = append(*out, Ref{Kind: "Secret", Namespace: r.Namespace, Name: r.Name, Field: path})
			}
			return
		case secretMountType:
			if n := v.Interface().(agentorcav1alpha1.SecretMount).Name; n != "" {
				*out = append(*out, Ref{Kind: "Secret", Name: n, Field: path})
			}
			return
		case secretKeySelectorType:
			if n := v.Interface().(corev1.SecretKeySelector).Name; n != "" {
				*out = append(*out, Ref{Kind: "Secret", Name: n, Field: path})
			}
			return
		}
		t := v.Type()
		for i := range v.NumField() {
			if t.Field(i).IsExported() {
				collectSecretRefs(v.Field(i), path+"."+jsonName(t.Field(i)), out)
			}
		}
	}
}

func jsonName(f reflect.StructField) string {
	tag := f.Tag.Get("json")
	for i := range len(tag) {
		if tag[i] == ',' {
			tag = tag[:i]
			break
		}
	}
	if tag == "" || tag == "-" {
		return f.Name
	}
	return tag
}

// NewObject returns an empty object of an agent-orca, Secret, ConfigMap or
// ServiceAccount kind, or nil for any other kind.
func NewObject(kind string) client.Object {
	switch kind {
	case "Agent":
		return &agentorcav1alpha1.Agent{}
	case "Tool":
		return &agentorcav1alpha1.Tool{}
	case "MCPServer":
		return &agentorcav1alpha1.MCPServer{}
	case "KnowledgeBase":
		return &agentorcav1alpha1.KnowledgeBase{}
	case "ModelSelector":
		return &agentorcav1alpha1.ModelSelector{}
	case "ModelProvider":
		return &agentorcav1alpha1.ModelProvider{}
	case "GuardrailPolicy":
		return &agentorcav1alpha1.GuardrailPolicy{}
	case "Secret":
		return &corev1.Secret{}
	case "ConfigMap":
		return &corev1.ConfigMap{}
	case "ServiceAccount":
		return &corev1.ServiceAccount{}
	}
	return nil
}

// RefViolation is a reference to another tenant's object.
type RefViolation struct {
	Ref    Ref
	Reason string
}

func (v RefViolation) Error() string { return v.Ref.String() + ": " + v.Reason }

// CheckRefs returns the references of owner that point at another tenant's
// objects, or at objects whose tenant cannot be established. A target is
// acceptable when it carries the owner's tenant ID or is platform-shared. A
// target with no tenant-id label belongs to whoever owns its namespace, so it is
// acceptable only in a namespace that the owner's tenant claims alone; in a
// shared namespace it may be any tenant's (or a plain Secret a person made), and
// in a namespace no tenant claims (agent-orca-system, kube-system) the operator
// would read it with its own permissions. There it must be labelled with the
// owner's tenant ID or scope=platform. A platform-shared owner may reference
// only platform-shared targets, so shared configuration never reads from one
// tenant's objects.
//
// A reference into another namespace is also checked against the TenantConfigs:
// the namespace must not be claimed by tenants that all differ from the owner's.
// tcs is the list of TenantConfigs.
//
// Targets that do not exist are skipped: a missing reference is a readiness
// problem, not an isolation one. reader must see the targets' kinds; owner must
// already carry its ownership label.
func CheckRefs(ctx context.Context, reader client.Reader, owner client.Object, tcs []agentorcav1alpha1.TenantConfig) ([]RefViolation, error) {
	ownerID, ownerPlatform := TenantOf(owner)
	if ownerID == "" && !ownerPlatform {
		return nil, nil
	}
	var out []RefViolation
	for _, ref := range RefsOf(owner) {
		target := NewObject(ref.Kind)
		if target == nil {
			continue
		}
		ns := ref.Namespace
		if ns == "" {
			ns = owner.GetNamespace()
		}
		if ns != owner.GetNamespace() && !ownerPlatform {
			if ids, claims := NamespaceClaimants(tcs, ns); claims > 0 && !slices.Contains(ids, ownerID) {
				out = append(out, RefViolation{ref, fmt.Sprintf("namespace %q belongs to another tenant", ns)})
				continue
			}
		}
		if err := reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, target); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("reading %s %s/%s: %w", ref.Kind, ns, ref.Name, err)
		}
		targetID, targetPlatform := TenantOf(target)
		switch {
		case targetPlatform:
		case ownerPlatform && targetID != "":
			out = append(out, RefViolation{ref, "a platform-shared object cannot reference a tenant's object"})
		case targetID != "" && targetID != ownerID:
			out = append(out, RefViolation{ref, fmt.Sprintf("belongs to tenant %s, not %s", targetID, ownerID)})
		case targetID == "" && !unlabelledTargetOK(tcs, ns, ownerID, ownerPlatform):
			out = append(out, RefViolation{ref, fmt.Sprintf("has no %s label and namespace %q is not claimed by this tenant alone: label it with the owner's tenant ID or %s=%s", LabelTenantID, ns, LabelScope, ScopePlatform)})
		}
	}
	return out, nil
}

// unlabelledTargetOK reports whether an unlabelled target in namespace ns may
// be referenced by an owner of the given tenant: only when the owner's tenant is
// the sole claimant of the namespace.
func unlabelledTargetOK(tcs []agentorcav1alpha1.TenantConfig, ns, ownerID string, ownerPlatform bool) bool {
	if ownerPlatform {
		return false
	}
	ids, claims := NamespaceClaimants(tcs, ns)
	return claims == 1 && len(ids) == 1 && ids[0] == ownerID
}

// EnforceRefs is the controllers' side of the reference rule. It checks the
// references of each owner in turn, counts every violation, and returns a
// message for the first set to deny in enforce mode (empty in audit mode, where
// the violations are only logged and counted). Controllers run it at the point
// they resolve references, because a webhook can be bypassed (failurePolicy
// Ignore, objects that predate it) and a target can change tenant after the
// owner was admitted.
func EnforceRefs(ctx context.Context, reader client.Reader, owners ...client.Object) (string, error) {
	var tcs agentorcav1alpha1.TenantConfigList
	if err := reader.List(ctx, &tcs); err != nil {
		return "", fmt.Errorf("listing TenantConfigs: %w", err)
	}
	for _, owner := range owners {
		violations, err := CheckRefs(ctx, reader, owner, tcs.Items)
		if err != nil {
			return "", err
		}
		var msgs []string
		for _, v := range violations {
			if Violation(RuleCrossTenantRef, fmt.Sprintf("%T", owner), owner.GetNamespace(), owner.GetName(), v.Error()) {
				msgs = append(msgs, v.Error())
			}
		}
		if len(msgs) > 0 {
			return fmt.Sprintf("%T %s/%s references another tenant's objects: %s", owner, owner.GetNamespace(), owner.GetName(), strings.Join(msgs, "; ")), nil
		}
	}
	return "", nil
}
