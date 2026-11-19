package apiserver

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/schedule"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// Annotations and labels the agent catalog reads. All are optional: an Agent
// without them still shows up, with its system prompt as the description.
const (
	// AnnotationDescription is a short, plain-language description of what the
	// agent does. Falls back to the start of the system prompt.
	AnnotationDescription = "agentorca.io/description"
	// AnnotationExampleInputs is a JSON array of example inputs (strings).
	AnnotationExampleInputs = "agentorca.io/example-inputs"
	// AnnotationInputSchema is a JSON Schema for the run form. Without it the
	// form is a single free-text field.
	AnnotationInputSchema = "agentorca.io/input-schema"
	// AnnotationMaintainer names the team that maintains the agent, for the catalog.
	AnnotationMaintainer = "agentorca.io/maintainer"
	// AnnotationCreatedBy records the user who started a run through the UI.
	AnnotationCreatedBy = "agentorca.io/created-by"

	// LabelTemplate marks an Agent as a template: "true".
	LabelTemplate = "agentorca.io/template"
	// templateMarker is the LabelTemplate value of a template.
	templateMarker = "true"

	actionManifest = "manifest"
	actionRun      = "run"

	// LabelTemplateGroup ties a template's Agent, AgentDeployment and Tools together.
	LabelTemplateGroup = "agentorca.io/template-group"
)

// agentDescription is the agent's plain-language description: the annotation,
// else the first paragraph of the system prompt.
func agentDescription(agent *agentorcav1alpha1.Agent) string {
	if d := strings.TrimSpace(agent.Annotations[AnnotationDescription]); d != "" {
		return d
	}
	return agent.Spec.SystemPrompt
}

// agentExampleInputs parses the example-inputs annotation. Malformed values
// are ignored: examples are a convenience.
func agentExampleInputs(agent *agentorcav1alpha1.Agent) []string {
	raw := agent.Annotations[AnnotationExampleInputs]
	if raw == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

// agentInputSchemaOverride parses the input-schema annotation, or returns nil
// when it is missing or not a JSON object.
func agentInputSchemaOverride(agent *agentorcav1alpha1.Agent) map[string]any {
	raw := agent.Annotations[AnnotationInputSchema]
	if raw == "" {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil || out == nil {
		return nil
	}
	return out
}

// registerAgentRoutes adds the agent catalog and the browser-facing manifest
// and run endpoints to the UI API.
func (s *UIServer) registerAgentRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/catalog", s.handleCatalog)
	mux.HandleFunc("/api/my/runs", s.handleMyRuns)
	s.registerFleetRoutes(mux)
	mux.HandleFunc("/api/agents/", s.handleAgentAction)
}

// acpHelper is the ACP server's logic over the UI server's clients, so the
// browser path shares the manifest and run code instead of copying it.
func (s *UIServer) acpHelper() *ACPServer {
	return &ACPServer{k8s: s.k8s, crdClient: s.crdClient, store: s.store, checkpoint: s.checkpoint}
}

// handleAgentAction serves GET /api/agents/{ns}/{name}/manifest and
// POST /api/agents/{ns}/{name}/run.
func (s *UIServer) handleAgentAction(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/agents/"), "/"), "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" {
		http.NotFound(w, r)
		return
	}
	ns, name, action := parts[0], parts[1], parts[2]
	var wantMethod string
	switch action {
	case actionManifest:
		wantMethod = http.MethodGet
	case actionRun:
		wantMethod = http.MethodPost
	default:
		http.NotFound(w, r)
		return
	}
	if r.Method != wantMethod {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()
	var agent agentorcav1alpha1.Agent
	if err := s.crdClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &agent); err != nil {
		if apierrors.IsNotFound(err) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// An agent outside the caller's tenant is reported as not found.
	if !canRead(ctx, &agent, "Agent") {
		http.NotFound(w, r)
		return
	}

	if action == actionRun && agent.Labels[LabelTemplate] == templateMarker {
		// A template is copied into a workspace and the copy is run. Running it
		// in place would start runs inside the platform's own namespace.
		writeACPError(w, "conflict",
			"this agent is a template: use the template to make your own copy, then run the copy",
			http.StatusConflict)
		return
	}

	acp := s.acpHelper()
	if action == actionManifest {
		acp.writeAgentManifest(w, r, &agent, agent.Namespace)
		return
	}

	tenant, _ := TenantFromContext(ctx)
	if tenant != nil {
		// A platform-shared agent is readable, but a run lands in the agent's
		// namespace, which must be one the caller may use.
		if !namespaceAllowed(ctx, agent.Namespace) {
			http.NotFound(w, r)
			return
		}
		if !tenantCanAccessAgent(tenant, agent.Name) {
			writeACPError(w, "unauthorized", "agent not allowed for tenant", http.StatusForbidden)
			return
		}
	}
	acp.handleAgentRun(w, r, tenant, &agent)
}

// catalogEntry is one agent in GET /api/catalog.
type catalogEntry struct {
	Name          string   `json:"name"`
	Namespace     string   `json:"namespace"`
	Description   string   `json:"description"`
	ExampleInputs []string `json:"exampleInputs"`
	// Maintainer is the team named by the maintainer annotation, else the tenant name,
	// else "Platform" for a platform-shared agent.
	Maintainer string `json:"maintainer"`
	// Published is true when an AgentDeployment runs the agent.
	Published  bool   `json:"published"`
	Deployment string `json:"deployment,omitempty"`
	// Conversational is true for a published agent, which can be chatted with.
	Conversational bool     `json:"conversational"`
	Template       bool     `json:"template"`
	TemplateGroup  string   `json:"templateGroup,omitempty"`
	Tools          []string `json:"tools"`
	ModelSelector  string   `json:"modelSelector"`
	// RunCount is how many finished runs the cost figure is based on.
	RunCount int `json:"runCount"`
	// TypicalCostUSD is the median spend of the agent's finished runs. Null
	// when it has none with a recorded cost.
	TypicalCostUSD *float64 `json:"typicalCostUSD"`
	// Members lists the AgentDeployments and Tools that share the template group.
	Members []catalogMember `json:"members,omitempty"`
}

type catalogMember struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// handleCatalog answers GET /api/catalog with every agent the caller may read,
// its published state, and the typical cost of one run.
func (s *UIServer) handleCatalog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()
	ns := r.URL.Query().Get("namespace")

	var agents agentorcav1alpha1.AgentList
	if err := listCRDsMultiNamespace(ctx, s.crdClient, &agents, ns); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var deployments agentorcav1alpha1.AgentDeploymentList
	if err := listCRDsMultiNamespace(ctx, s.crdClient, &deployments, ns); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var runs agentorcav1alpha1.AgentRunList
	if err := listCRDsMultiNamespace(ctx, s.crdClient, &runs, ns); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var tools agentorcav1alpha1.ToolList
	if err := listCRDsMultiNamespace(ctx, s.crdClient, &tools, ns); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	depByAgent := map[string]*agentorcav1alpha1.AgentDeployment{}
	for i := range deployments.Items {
		d := &deployments.Items[i]
		key := d.Namespace + "/" + d.Spec.AgentRef
		if _, dup := depByAgent[key]; !dup {
			depByAgent[key] = d
		}
	}
	spend := map[string][]float64{}
	for i := range runs.Items {
		run := &runs.Items[i]
		switch run.Status.Phase {
		case agentorcav1alpha1.AgentRunPhaseSucceeded, agentorcav1alpha1.AgentRunPhaseFailed:
		default:
			continue
		}
		v, err := strconv.ParseFloat(run.Status.SpendUSD, 64)
		if err != nil || v < 0 {
			continue
		}
		key := run.Namespace + "/" + run.Spec.AgentRef
		spend[key] = append(spend[key], v)
	}

	out := make([]catalogEntry, 0, len(agents.Items))
	for i := range agents.Items {
		a := &agents.Items[i]
		key := a.Namespace + "/" + a.Name
		e := catalogEntry{
			Name:          a.Name,
			Namespace:     a.Namespace,
			Description:   agentDescription(a),
			ExampleInputs: agentExampleInputs(a),
			Maintainer:    agentMaintainer(a),
			Template:      a.Labels[LabelTemplate] == templateMarker,
			TemplateGroup: a.Labels[LabelTemplateGroup],
			Tools:         append([]string{}, a.Spec.Tools...),
			ModelSelector: a.Spec.ModelSelectorRef,
		}
		if e.ExampleInputs == nil {
			e.ExampleInputs = []string{}
		}
		if d := depByAgent[key]; d != nil {
			e.Published = true
			e.Deployment = d.Name
			e.Conversational = true
		}
		if vals := spend[key]; len(vals) > 0 {
			sort.Float64s(vals)
			med := vals[len(vals)/2]
			if len(vals)%2 == 0 {
				med = (vals[len(vals)/2-1] + vals[len(vals)/2]) / 2
			}
			e.RunCount = len(vals)
			e.TypicalCostUSD = &med
		}
		if e.TemplateGroup != "" {
			for j := range deployments.Items {
				if d := &deployments.Items[j]; d.Namespace == a.Namespace && d.Labels[LabelTemplateGroup] == e.TemplateGroup {
					e.Members = append(e.Members, catalogMember{Kind: "AgentDeployment", Name: d.Name})
				}
			}
			for j := range tools.Items {
				if t := &tools.Items[j]; t.Namespace == a.Namespace && t.Labels[LabelTemplateGroup] == e.TemplateGroup {
					e.Members = append(e.Members, catalogMember{Kind: "Tool", Name: t.Name})
				}
			}
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	jsonResponse(w, out)
}

// agentMaintainer is the display maintainer of an agent.
func agentMaintainer(a *agentorcav1alpha1.Agent) string {
	if o := strings.TrimSpace(a.Annotations[AnnotationMaintainer]); o != "" {
		return o
	}
	if a.Labels[tenancy.LabelScope] == tenancy.ScopePlatform {
		return "Platform"
	}
	return a.Labels["agentorca.io/tenant"]
}

// myRun is one run in GET /api/my/runs.
type myRun struct {
	Name            string `json:"name"`
	Namespace       string `json:"namespace"`
	AgentRef        string `json:"agentRef"`
	Phase           string `json:"phase"`
	SpendUSD        string `json:"spendUSD"`
	Input           string `json:"input"`
	Output          string `json:"output,omitempty"`
	ClarifyQuestion string `json:"clarifyQuestion,omitempty"`
	CreatedBy       string `json:"createdBy,omitempty"`
	CreatedAt       string `json:"createdAt"`
	CompletionTime  string `json:"completionTime,omitempty"`
}

// myRunsResponse is the body of GET /api/my/runs. Scope says whose runs they
// are: "user" when they were started by the signed-in user, "tenant" when the
// user has none that record who started them and the tenant's recent runs are
// shown instead.
type myRunsResponse struct {
	Scope  string  `json:"scope"`
	UserID string  `json:"userId"`
	Runs   []myRun `json:"runs"`
}

const myRunsLimit = 20

// handleMyRuns answers GET /api/my/runs with the signed-in user's recent runs.
// Runs record their starter in the agentorca.io/created-by annotation when they
// are started through the UI or ACP API. Runs started any other way (chat
// deployments, workflows, kubectl) do not, so a user with none of their own
// sees the tenant's recent runs, labelled as such.
func (s *UIServer) handleMyRuns(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()
	var runs agentorcav1alpha1.AgentRunList
	if err := listCRDsMultiNamespace(ctx, s.crdClient, &runs, r.URL.Query().Get("namespace")); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sort.Slice(runs.Items, func(i, j int) bool {
		return runs.Items[j].CreationTimestamp.Time.Before(runs.Items[i].CreationTimestamp.Time)
	})

	userID := ""
	if t, ok := TenantFromContext(ctx); ok && t != nil {
		userID = t.UserID
	}
	pick := func(mine bool) []myRun {
		out := []myRun{}
		for i := range runs.Items {
			run := &runs.Items[i]
			// A scheduled run has no invoker: it shows under Scheduled, not here.
			if strings.HasPrefix(run.Annotations[AnnotationCreatedBy], schedule.ActorPrefix) {
				continue
			}
			if mine && run.Annotations[AnnotationCreatedBy] != userID {
				continue
			}
			mr := myRun{
				Name: run.Name, Namespace: run.Namespace, AgentRef: run.Spec.AgentRef,
				Phase: string(run.Status.Phase), SpendUSD: run.Status.SpendUSD,
				Input: truncateRunes(run.Spec.Input, 400), Output: run.Status.Output,
				ClarifyQuestion: run.Status.ClarifyQuestion,
				CreatedBy:       run.Annotations[AnnotationCreatedBy],
				CreatedAt:       run.CreationTimestamp.UTC().Format(time.RFC3339),
			}
			if run.Status.CompletionTime != nil {
				mr.CompletionTime = run.Status.CompletionTime.UTC().Format(time.RFC3339)
			}
			out = append(out, mr)
			if len(out) == myRunsLimit {
				break
			}
		}
		return out
	}
	resp := myRunsResponse{Scope: "user", UserID: userID}
	if userID != "" {
		resp.Runs = pick(true)
	}
	if len(resp.Runs) == 0 {
		resp.Scope = "tenant"
		resp.Runs = pick(false)
	}
	jsonResponse(w, resp)
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
