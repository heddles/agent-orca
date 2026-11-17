package apiserver

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/tenancy"
)

const kindAgentDeployment = "AgentDeployment"

// publishRequest is the body of POST /api/agents/{ns}/{name}/publish. Both
// fields are optional.
type publishRequest struct {
	// Name of the AgentDeployment. Default: the agent's existing deployment, else
	// the agent's own name.
	Name string `json:"name,omitempty"`
	// Chat makes the deployment take chat input (the default).
	Chat *bool `json:"chat,omitempty"`
}

// handlePublish serves /api/agents/{ns}/{name}/publish.
//
// POST creates the AgentDeployment that runs the agent, or updates the one that
// does. The agent must be UI-owned in the caller's tenant (409 for a Git-owned
// one). It answers 201 (created) or 200 (updated) with {"deployment": {...}}.
// DELETE unpublishes: it deletes that deployment under the same ownership guard
// as any deployment delete.
func (s *UIServer) handlePublish(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	switch r.Method {
	case http.MethodPost:
		s.publishAgent(w, r, ns, name)
	case http.MethodDelete:
		s.unpublishAgent(w, r, ns, name)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// deploymentsOf lists the deployments in ns that run the agent.
func (s *UIServer) deploymentsOf(r *http.Request, ns, agent string) ([]agentorcav1alpha1.AgentDeployment, error) {
	var list agentorcav1alpha1.AgentDeploymentList
	if err := s.crdClient.List(r.Context(), &list, client.InNamespace(ns)); err != nil {
		return nil, err
	}
	var out []agentorcav1alpha1.AgentDeployment
	for _, d := range list.Items {
		if d.Spec.AgentRef == agent && canWrite(r.Context(), &d, kindAgentDeployment) {
			out = append(out, d)
		}
	}
	return out, nil
}

func (s *UIServer) publishAgent(w http.ResponseWriter, r *http.Request, ns, name string) {
	if !requireRole(w, r, tenancy.RoleBuilder) {
		return
	}
	var req publishRequest
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil || (len(body) > 0 && json.Unmarshal(body, &req) != nil) {
		jsonError(w, `the body must be {"name": "<optional>", "chat": true}`, http.StatusBadRequest)
		return
	}
	if req.Name != "" {
		if errs := validation.IsDNS1123Subdomain(req.Name); len(errs) > 0 {
			jsonError(w, fmt.Sprintf("name %q is not a valid object name", req.Name), http.StatusBadRequest)
			return
		}
	}
	var agent agentorcav1alpha1.Agent
	if err := s.crdClient.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &agent); err != nil {
		notFoundOr(w, err)
		return
	}
	if !s.authorizeWrite(w, r, &agent, kindAgent) {
		return
	}
	if o := tenancy.Detect(&agent, s.ownDetect); o.Owner != tenancy.OwnerUI {
		notEditable(w, kindAgent, ns, name, o, agent.ResourceVersion)
		return
	}
	existing, err := s.deploymentsOf(r, ns, name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	depName := req.Name
	var current *agentorcav1alpha1.AgentDeployment
	for i := range existing {
		if depName == "" || existing[i].Name == depName {
			current = &existing[i]
			break
		}
	}
	if depName == "" {
		depName = name
		if current != nil {
			depName = current.Name
		}
	}
	chat := req.Chat == nil || *req.Chat
	if current == nil {
		s.createDeployment(w, r, &agent, depName, chat)
		return
	}
	s.updateDeployment(w, r, current, &agent, chat)
}

// deploymentSpec is the minimal valid deployment of an agent: it runs the agent,
// takes chat input and keeps one replica. Everything else comes from the agent
// and its model selector.
func deploymentSpec(agent string, chat bool) agentorcav1alpha1.AgentDeploymentSpec {
	replicas := int32(1)
	spec := agentorcav1alpha1.AgentDeploymentSpec{AgentRef: agent, Replicas: &replicas}
	if chat {
		spec.InputSource = &agentorcav1alpha1.InputSourceConfig{Type: agentorcav1alpha1.InputSourceChat}
	}
	return spec
}

func (s *UIServer) createDeployment(w http.ResponseWriter, r *http.Request, agent *agentorcav1alpha1.Agent, depName string, chat bool) {
	dep := &agentorcav1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: depName, Namespace: agent.Namespace},
		Spec:       deploymentSpec(agent.Name, chat),
	}
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(dep)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	u := &unstructured.Unstructured{Object: m}
	u.SetAPIVersion(agentorcav1alpha1.GroupVersion.String())
	u.SetKind(kindAgentDeployment)
	delete(m, "status")
	unstructured.RemoveNestedField(m, "metadata", "creationTimestamp")
	// The deployment belongs to the agent's tenant, which is the caller's.
	if tid, _ := tenancy.TenantOf(agent); tid != "" {
		tenancy.Stamp(u, tid)
	} else if t, ok := TenantFromContext(r.Context()); ok && t != nil {
		tenancy.Stamp(u, t.TenantID)
	}
	stampUI(r, u)
	if err := s.persistNew(r, kindAgentDeployment, u); err != nil {
		if k8serrors.IsAlreadyExists(err) {
			// A deployment of that name exists but does not run this agent (or is not the caller's).
			writeConflict(w, fmt.Sprintf("AgentDeployment %s/%s already exists", agent.Namespace, depName), "")
			return
		}
		writeAPIError(w, "creating deployment", err)
		return
	}
	s.recordWrite(w, r, u, auditEventFor(postgresql.AuditCreate, kindAgentDeployment, u.GetNamespace(), u.GetName(), u.GetResourceVersion(),
		fmt.Sprintf("published Agent %s/%s as AgentDeployment %s", agent.Namespace, agent.Name, u.GetName())))
	w.WriteHeader(http.StatusCreated)
	s.respondDeployment(w, r, u)
}

func (s *UIServer) updateDeployment(w http.ResponseWriter, r *http.Request, dep *agentorcav1alpha1.AgentDeployment, agent *agentorcav1alpha1.Agent, chat bool) {
	if o := tenancy.Detect(dep, s.ownDetect); o.Owner != tenancy.OwnerUI {
		notEditable(w, kindAgentDeployment, dep.Namespace, dep.Name, o, dep.ResourceVersion)
		return
	}
	orig := dep.DeepCopy()
	want := deploymentSpec(agent.Name, chat)
	dep.Spec.AgentRef = want.AgentRef
	if chat {
		dep.Spec.InputSource = want.InputSource
	}
	if dep.Spec.Replicas == nil {
		dep.Spec.Replicas = want.Replicas
	}
	stampUI(r, dep)
	if err := s.crdClient.Patch(r.Context(), dep, client.MergeFrom(orig), client.FieldOwner(tenancy.FieldManagerUI)); err != nil {
		writeAPIError(w, "updating deployment", err)
		return
	}
	s.recordWrite(w, r, dep, auditEventFor(postgresql.AuditUpdate, kindAgentDeployment, dep.Namespace, dep.Name, dep.ResourceVersion,
		fmt.Sprintf("published Agent %s/%s through AgentDeployment %s", agent.Namespace, agent.Name, dep.Name)))
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(dep)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	u := &unstructured.Unstructured{Object: m}
	u.SetAPIVersion(agentorcav1alpha1.GroupVersion.String())
	u.SetKind(kindAgentDeployment)
	unstructured.RemoveNestedField(m, "metadata", "managedFields")
	s.respondDeployment(w, r, u)
}

func (s *UIServer) respondDeployment(w http.ResponseWriter, r *http.Request, u *unstructured.Unstructured) {
	u.Object["ownership"] = s.ownershipOf(r.Context(), u, kindAgentDeployment)
	jsonResponse(w, map[string]any{"deployment": u.Object})
}

// unpublishAgent deletes the deployment that runs the agent. ?deployment= picks one
// when several run it.
func (s *UIServer) unpublishAgent(w http.ResponseWriter, r *http.Request, ns, name string) {
	if !requireRole(w, r, tenancy.RoleBuilder) {
		return
	}
	if !namespaceAllowed(r.Context(), ns) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	deps, err := s.deploymentsOf(r, ns, name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	want := r.URL.Query().Get("deployment")
	if want == "" && len(deps) > 1 {
		names := make([]string, len(deps))
		for i := range deps {
			names[i] = deps[i].Name
		}
		jsonError(w, fmt.Sprintf("%d deployments run this agent (%s): say which with ?deployment=", len(deps), strings.Join(names, ", ")), http.StatusConflict)
		return
	}
	for i := range deps {
		if want == "" || deps[i].Name == want {
			s.handleDeleteDeployment(w, r, ns, deps[i].Name)
			return
		}
	}
	http.Error(w, "not found", http.StatusNotFound)
}
