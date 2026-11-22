package apiserver

import (
	"context"
	"net/http"
	"sort"

	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// The agent builder's API:
//
//	GET    /api/builder/options                   what the form can pick from
//	POST   /api/templates/{ns}/{name}/use         copy a template into the workspace
//	POST   /api/agents/{ns}/{name}/publish        create or update the agent's deployment
//	DELETE /api/agents/{ns}/{name}/publish        unpublish
//
// Dry run for writes lives in uiapi_dryrun.go.

// registerBuilderRoutes adds the agent builder endpoints to the UI API.
func (s *UIServer) registerBuilderRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/builder/options", s.handleBuilderOptions)
	mux.HandleFunc("/api/templates/{ns}/{name}/use", s.handleUseTemplate)
	mux.HandleFunc("/api/agents/{ns}/{name}/publish", s.handlePublish)
	mux.HandleFunc("/api/builder/check", s.handleBuilderCheck)
	mux.HandleFunc("/api/builder/draft", s.handleBuilderDraft)
}

// builderOption is one thing the builder form can offer.
type builderOption struct {
	Name        string `json:"name"`
	Namespace   string `json:"namespace"`
	Description string `json:"description,omitempty"`
}

// builderOptions is the body of GET /api/builder/options.
type builderOptions struct {
	ModelSelectors    []builderOption `json:"modelSelectors"`
	Tools             []builderOption `json:"tools"`
	MCPServers        []builderOption `json:"mcpServers"`
	KnowledgeBases    []builderOption `json:"knowledgeBases"`
	GuardrailPolicies []builderOption `json:"guardrailPolicies"`
	// Draft says whether "Draft from a description" can be offered.
	Draft draftAvailability `json:"draft"`
}

// handleBuilderOptions answers GET /api/builder/options with the objects an
// Agent can reference, from the caller's own namespaces plus platform-shared
// ones. Viewers may read it.
func (s *UIServer) handleBuilderOptions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !requireRole(w, r, tenancy.RoleViewer) {
		return
	}
	out, err := s.builderOptionsFor(r.Context(), r.URL.Query().Get("namespace"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out.Draft = s.draftAvailability(r.Context(), r.URL.Query().Get("namespace"))
	jsonResponse(w, out)
}

// builderOptionsFor lists what an Agent can reference, from the caller's own
// namespaces (or ns, when it is one of them) plus platform-scoped objects.
func (s *UIServer) builderOptionsFor(ctx context.Context, ns string) (builderOptions, error) {
	var (
		selectors  agentorcav1alpha1.ModelSelectorList
		tools      agentorcav1alpha1.ToolList
		mcps       agentorcav1alpha1.MCPServerList
		kbs        agentorcav1alpha1.KnowledgeBaseList
		guardrails agentorcav1alpha1.GuardrailPolicyList
	)
	for _, l := range []client.ObjectList{&selectors, &tools, &mcps, &kbs, &guardrails} {
		if err := listCRDsMultiNamespace(ctx, s.crdClient, l, ns); err != nil {
			return builderOptions{}, err
		}
	}
	out := builderOptions{
		ModelSelectors:    []builderOption{},
		Tools:             []builderOption{},
		MCPServers:        []builderOption{},
		KnowledgeBases:    []builderOption{},
		GuardrailPolicies: []builderOption{},
	}
	for i := range selectors.Items {
		o := &selectors.Items[i]
		out.ModelSelectors = append(out.ModelSelectors, builderOption{o.Name, o.Namespace, o.Annotations[AnnotationDescription]})
	}
	for i := range tools.Items {
		o := &tools.Items[i]
		desc := o.Annotations[AnnotationDescription]
		if desc == "" && o.Spec.Schema != nil {
			desc = o.Spec.Schema.Description
		}
		out.Tools = append(out.Tools, builderOption{o.Name, o.Namespace, desc})
	}
	for i := range mcps.Items {
		o := &mcps.Items[i]
		out.MCPServers = append(out.MCPServers, builderOption{o.Name, o.Namespace, o.Annotations[AnnotationDescription]})
	}
	for i := range kbs.Items {
		o := &kbs.Items[i]
		out.KnowledgeBases = append(out.KnowledgeBases, builderOption{o.Name, o.Namespace, o.Spec.Description})
	}
	for i := range guardrails.Items {
		o := &guardrails.Items[i]
		out.GuardrailPolicies = append(out.GuardrailPolicies, builderOption{o.Name, o.Namespace, o.Annotations[AnnotationDescription]})
	}
	for _, l := range []*[]builderOption{&out.ModelSelectors, &out.Tools, &out.MCPServers, &out.KnowledgeBases, &out.GuardrailPolicies} {
		sort.Slice(*l, func(i, j int) bool {
			a, b := (*l)[i], (*l)[j]
			if a.Namespace != b.Namespace {
				return a.Namespace < b.Namespace
			}
			return a.Name < b.Name
		})
	}
	return out, nil
}
