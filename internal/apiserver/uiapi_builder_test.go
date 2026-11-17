package apiserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/tenancy"
)

const (
	platformNS = "platform-ns"
	tplName    = "tpl"
	tplGroup   = "tpl-group"

	builderUser     = "ada"
	builderSelector = "default"
)

// builderFixture is a writeFixture with a platform namespace holding one template
// (an agent, a tool, a guardrail policy and a deployment) and some other objects.
type builderFixture struct {
	*writeFixture
}

func platformLabels(extra ...string) map[string]string {
	l := map[string]string{tenancy.LabelScope: tenancy.ScopePlatform}
	for i := 0; i < len(extra); i += 2 {
		l[extra[i]] = extra[i+1]
	}
	return l
}

func newBuilderFixture(t *testing.T, objs ...client.Object) *builderFixture {
	t.Helper()
	tenancy.SetPlatformNamespaces([]string{platformNS})
	t.Cleanup(func() { tenancy.SetPlatformNamespaces(nil) })
	group := LabelTemplateGroup
	base := []client.Object{
		&agentorcav1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{
				Name: tplName, Namespace: platformNS,
				Labels: platformLabels(LabelTemplate, "true", group, tplGroup),
				Annotations: map[string]string{
					tenancy.AnnotationOwner: "git", tenancy.AnnotationLastModifiedBy: "someone", tenancy.AnnotationHandoff: "pending",
					tenancy.AnnotationHandoffExpires: "2099-01-01T00:00:00Z",
					AnnotationMaintainer:             "Platform team", AnnotationDescription: "does things", AnnotationExampleInputs: `["a"]`,
				},
			},
			Spec: agentorcav1alpha1.AgentSpec{
				ModelSelectorRef: builderSelector, Tools: []string{"tpl-tool", "shared-tool"}, GuardrailPolicyRef: "tpl-guard",
				SystemPrompt: "be helpful",
			},
		},
		&agentorcav1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{Name: "tpl-tool", Namespace: platformNS, Labels: platformLabels(group, tplGroup)}},
		&agentorcav1alpha1.GuardrailPolicy{ObjectMeta: metav1.ObjectMeta{Name: "tpl-guard", Namespace: platformNS, Labels: platformLabels(group, tplGroup)}},
		&agentorcav1alpha1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: "tpl-chat", Namespace: platformNS, Labels: platformLabels(group, tplGroup)},
			Spec:       agentorcav1alpha1.AgentDeploymentSpec{AgentRef: tplName},
		},
		// Platform-scoped but not a template, and a template-looking agent that is not platform-scoped.
		&agentorcav1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "plain", Namespace: platformNS, Labels: platformLabels()}},
		&agentorcav1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "private-tpl", Namespace: platformNS, Labels: map[string]string{LabelTemplate: "true"}}},
		// A template label outside a platform namespace is just a label.
		&agentorcav1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "fake-tpl", Namespace: sharedNS, Labels: platformLabels(LabelTemplate, "true")}},
	}
	return &builderFixture{newWriteFixture(t, append(base, objs...)...)}
}

// refTargets are what the template's references need in a workspace namespace:
// the tenant's own ModelSelector "default" and Tool "shared-tool".
func refTargets(namespaces ...string) []client.Object {
	out := make([]client.Object, 0, 2*len(namespaces))
	for _, ns := range namespaces {
		l := map[string]string{tenancy.LabelTenantID: isoA}
		out = append(out,
			&agentorcav1alpha1.ModelSelector{ObjectMeta: metav1.ObjectMeta{Name: builderSelector, Namespace: ns, Labels: l}},
			&agentorcav1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{Name: "shared-tool", Namespace: ns, Labels: l}})
	}
	return out
}

// serve sends a request through the builder routes.
func (f *builderFixture) serve(id *TenantIdentity, method, path, body string) *httptest.ResponseRecorder {
	f.t.Helper()
	mux := http.NewServeMux()
	f.s.registerBuilderRoutes(mux)
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if id != nil {
		req = asTenant(req, id)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func (f *builderFixture) exists(obj client.Object, name string) bool {
	f.t.Helper()
	return f.c.Get(context.Background(), client.ObjectKey{Namespace: sharedNS, Name: name}, obj) == nil
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decoding %q: %v", rec.Body.String(), err)
	}
	return v
}

// ── use a template ──────────────────────────────────────────────────────────

func TestUseTemplateCopiesTheGroup(t *testing.T) {
	f := newBuilderFixture(t, refTargets(sharedNS)...)
	rec := f.serve(f.as(tenancy.RoleBuilder), http.MethodPost, "/api/templates/"+platformNS+"/"+tplName+"/use", `{"name":"my-bot"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("use = %d %s", rec.Code, rec.Body.String())
	}
	resp := decode[struct {
		Agent   map[string]any `json:"agent"`
		Created []createdRef   `json:"created"`
	}](t, rec)
	want := []createdRef{
		{"GuardrailPolicy", sharedNS, "my-bot-guardrail"}, {"Tool", sharedNS, "my-bot-tool"},
		{"Agent", sharedNS, "my-bot"}, {"AgentDeployment", sharedNS, "my-bot-deploy"},
	}
	if !slices.Equal(resp.Created, want) {
		t.Errorf("created = %+v, want %+v", resp.Created, want)
	}
	if md, _ := resp.Agent["metadata"].(map[string]any); md["name"] != "my-bot" || resp.Agent["ownership"] == nil {
		t.Errorf("agent = %v", resp.Agent)
	}

	var agent agentorcav1alpha1.Agent
	if !f.exists(&agent, "my-bot") {
		t.Fatal("the agent was not created in the caller's namespace")
	}
	if agent.Spec.ModelSelectorRef != builderSelector || agent.Spec.GuardrailPolicyRef != "my-bot-guardrail" ||
		!slices.Equal(agent.Spec.Tools, []string{"my-bot-tool", "shared-tool"}) {
		t.Errorf("references = %+v (own objects follow the copies, others stay)", agent.Spec)
	}
	var dep agentorcav1alpha1.AgentDeployment
	if !f.exists(&dep, "my-bot-deploy") || dep.Spec.AgentRef != "my-bot" {
		t.Errorf("deployment = %+v", dep.Spec)
	}
	for _, o := range []client.Object{&agent, &dep} {
		l, a := o.GetLabels(), o.GetAnnotations()
		if l[tenancy.LabelTenantID] != isoA || l[tenancy.LabelScope] != "" || l[LabelTemplate] != "" || l[LabelTemplateGroup] != "" {
			t.Errorf("%T labels = %v", o, l)
		}
		if a[tenancy.AnnotationOwner] != tenancy.OwnerUI || a[tenancy.AnnotationLastModifiedBy] != builderUser ||
			a[tenancy.AnnotationHandoff] != "" || a[tenancy.AnnotationHandoffExpires] != "" {
			t.Errorf("%T annotations = %v", o, a)
		}
	}
	a := agent.Annotations
	if a[AnnotationMaintainer] != "Platform team" || a[AnnotationDescription] != "does things" || a[AnnotationExampleInputs] != `["a"]` {
		t.Errorf("the catalog annotations are kept: %v", a)
	}
	if !f.exists(&agentorcav1alpha1.Tool{}, "my-bot-tool") || !f.exists(&agentorcav1alpha1.GuardrailPolicy{}, "my-bot-guardrail") {
		t.Error("tool or guardrail missing")
	}
	if len(f.audit.events) != 4 {
		t.Errorf("audit = %+v", f.audit.events)
	}
}

func TestUseTemplateRulesAndFailures(t *testing.T) {
	use := func(src string) string { return "/api/templates/" + platformNS + "/" + src + "/use" }
	tests := []struct {
		name   string
		role   tenancy.Role
		noID   bool
		path   string
		body   string
		before []client.Object
		want   int
	}{
		{name: "viewer", role: tenancy.RoleViewer, path: use(tplName), body: `{"name":"x"}`, want: http.StatusForbidden},
		{name: "runner", role: tenancy.RoleRunner, path: use(tplName), body: `{"name":"x"}`, want: http.StatusForbidden},
		{name: "builder", role: tenancy.RoleBuilder, path: use(tplName), body: `{"name":"x"}`, want: http.StatusCreated},
		{name: "unknown source", role: tenancy.RoleBuilder, path: use("nope"), body: `{"name":"x"}`, want: http.StatusNotFound},
		{name: "platform object that is not a template", role: tenancy.RoleBuilder, path: use("plain"), body: `{"name":"x"}`, want: http.StatusNotFound},
		{name: "not platform scoped", role: tenancy.RoleBuilder, path: use("private-tpl"), body: `{"name":"x"}`, want: http.StatusNotFound},
		{name: "outside a platform namespace", role: tenancy.RoleBuilder, path: "/api/templates/" + sharedNS + "/fake-tpl/use", body: `{"name":"x"}`, want: http.StatusNotFound},
		{name: "no name", role: tenancy.RoleBuilder, path: use(tplName), body: `{}`, want: http.StatusBadRequest},
		{name: "bad name", role: tenancy.RoleBuilder, path: use(tplName), body: `{"name":"Not_OK"}`, want: http.StatusBadRequest},
		{name: "wrong method", role: tenancy.RoleBuilder, path: use(tplName), body: ``, want: http.StatusMethodNotAllowed},
		{
			name: "name taken", role: tenancy.RoleBuilder, path: use(tplName), body: `{"name":"taken"}`, want: http.StatusConflict,
			before: []client.Object{&agentorcav1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "taken", Namespace: sharedNS, Labels: map[string]string{tenancy.LabelTenantID: isoB}}}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newBuilderFixture(t, append(refTargets(sharedNS), tc.before...)...)
			method := http.MethodPost
			if tc.name == "wrong method" {
				method = http.MethodGet
			}
			rec := f.serve(f.as(tc.role), method, tc.path, tc.body)
			if rec.Code != tc.want {
				t.Fatalf("= %d %s, want %d", rec.Code, rec.Body.String(), tc.want)
			}
			if tc.want != http.StatusCreated && len(f.audit.events) != 0 {
				t.Errorf("a failed call left audit events: %+v", f.audit.events)
			}
			if tc.want == http.StatusConflict {
				// The guardrail and tool were made before the agent clashed: they are gone.
				if f.exists(&agentorcav1alpha1.Tool{}, "taken-tool") || f.exists(&agentorcav1alpha1.GuardrailPolicy{}, "taken-guardrail") {
					t.Error("a partial copy was left behind")
				}
			}
		})
	}
}

func TestUseTemplateNamespaceChoice(t *testing.T) {
	f := newBuilderFixture(t, refTargets(sharedNS, "a-only")...)
	id := f.as(tenancy.RoleBuilder)
	id.Namespace, id.Namespaces = "a-only", []string{"a-only", sharedNS}
	// A namespace the tenant may use is honoured; one it may not use is replaced by its first.
	if rec := f.serve(id, http.MethodPost, "/api/templates/"+platformNS+"/"+tplName+"/use", `{"name":"one","namespace":"shared"}`); rec.Code != http.StatusCreated {
		t.Fatalf("own namespace = %d %s", rec.Code, rec.Body.String())
	}
	if !f.exists(&agentorcav1alpha1.Agent{}, "one") {
		t.Error("not copied into the requested namespace")
	}
	for i, ns := range []string{"b-only", platformNS} {
		name := []string{"two", "three"}[i]
		rec := f.serve(id, http.MethodPost, "/api/templates/"+platformNS+"/"+tplName+"/use", `{"name":"`+name+`","namespace":"`+ns+`"}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("namespace %s = %d %s", ns, rec.Code, rec.Body.String())
		}
		var a agentorcav1alpha1.Agent
		if err := f.c.Get(context.Background(), client.ObjectKey{Namespace: "a-only", Name: name}, &a); err != nil {
			t.Fatalf("namespace %s: the copy should land in the tenant's first namespace: %v", ns, err)
		}
	}
}

// ── publish ─────────────────────────────────────────────────────────────────

func uiAgent(name string, labels map[string]string, owner string) *agentorcav1alpha1.Agent {
	return &agentorcav1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: sharedNS, Labels: labels, Annotations: map[string]string{tenancy.AnnotationOwner: owner}},
		Spec:       agentorcav1alpha1.AgentSpec{ModelSelectorRef: builderSelector},
	}
}

func TestPublishAndUnpublish(t *testing.T) {
	mine := map[string]string{tenancy.LabelTenantID: isoA}
	theirs := map[string]string{tenancy.LabelTenantID: isoB}
	f := newBuilderFixture(t,
		uiAgent("pub-agent", mine, tenancy.OwnerUI), uiAgent("gitty", mine, "git"), uiAgent("theirs", theirs, tenancy.OwnerUI),
		&agentorcav1alpha1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: "gitty-deploy", Namespace: sharedNS, Labels: mine, Annotations: map[string]string{tenancy.AnnotationOwner: "git"}},
			Spec:       agentorcav1alpha1.AgentDeploymentSpec{AgentRef: "ui-agent-with-git-deploy"},
		},
		uiAgent("ui-agent-with-git-deploy", mine, tenancy.OwnerUI),
	)
	pub := func(role tenancy.Role, method, agent, body string) *httptest.ResponseRecorder {
		return f.serve(f.as(role), method, "/api/agents/"+sharedNS+"/"+agent+"/publish", body)
	}
	for _, role := range []tenancy.Role{tenancy.RoleViewer, tenancy.RoleRunner} {
		if rec := pub(role, http.MethodPost, "pub-agent", ``); rec.Code != http.StatusForbidden {
			t.Errorf("publish as %s = %d", role, rec.Code)
		}
		if rec := pub(role, http.MethodDelete, "pub-agent", ``); rec.Code != http.StatusForbidden {
			t.Errorf("unpublish as %s = %d", role, rec.Code)
		}
	}
	for _, c := range []struct {
		agent string
		want  int
	}{{"theirs", http.StatusNotFound}, {"missing", http.StatusNotFound}, {"gitty", http.StatusConflict}, {"ui-agent-with-git-deploy", http.StatusConflict}} {
		if rec := pub(tenancy.RoleBuilder, http.MethodPost, c.agent, ``); rec.Code != c.want {
			t.Errorf("publish %s = %d %s, want %d", c.agent, rec.Code, rec.Body.String(), c.want)
		}
	}
	if rec := pub(tenancy.RoleBuilder, http.MethodPut, "pub-agent", ``); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("PUT = %d", rec.Code)
	}
	if rec := pub(tenancy.RoleBuilder, http.MethodPost, "pub-agent", `{"name":"Bad_Name"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("bad name = %d", rec.Code)
	}
	if len(f.audit.events) != 0 {
		t.Fatalf("refused calls left audit events: %+v", f.audit.events)
	}

	// Create.
	rec := pub(tenancy.RoleBuilder, http.MethodPost, "pub-agent", `{"chat":true}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("publish = %d %s", rec.Code, rec.Body.String())
	}
	if d := decode[struct {
		Deployment map[string]any `json:"deployment"`
	}](t, rec).Deployment; d["ownership"] == nil {
		t.Errorf("deployment = %v", d)
	}
	var dep agentorcav1alpha1.AgentDeployment
	if !f.exists(&dep, "pub-agent") {
		t.Fatal("no deployment")
	}
	if dep.Spec.AgentRef != "pub-agent" || dep.Spec.InputSource == nil || dep.Spec.InputSource.Type != agentorcav1alpha1.InputSourceChat ||
		dep.Labels[tenancy.LabelTenantID] != isoA || dep.Annotations[tenancy.AnnotationOwner] != tenancy.OwnerUI ||
		dep.Annotations[tenancy.AnnotationLastModifiedBy] != builderUser {
		t.Errorf("deployment = %+v %v %v", dep.Spec, dep.Labels, dep.Annotations)
	}
	// Publishing again updates the same deployment.
	if rec = pub(tenancy.RoleBuilder, http.MethodPost, "pub-agent", ``); rec.Code != http.StatusOK {
		t.Fatalf("republish = %d %s", rec.Code, rec.Body.String())
	}
	var deps agentorcav1alpha1.AgentDeploymentList
	if err := f.c.List(context.Background(), &deps, client.InNamespace(sharedNS)); err != nil {
		t.Fatal(err)
	}
	if n := countRunning(deps.Items, "pub-agent"); n != 1 {
		t.Errorf("deployments of mine = %d", n)
	}
	if len(f.audit.events) != 2 || f.audit.events[0].Action != "create" || f.audit.events[1].Action != "update" {
		t.Errorf("audit = %+v", f.audit.events)
	}

	// Unpublish.
	if rec = pub(tenancy.RoleBuilder, http.MethodDelete, "pub-agent", ``); rec.Code != http.StatusOK {
		t.Fatalf("unpublish = %d %s", rec.Code, rec.Body.String())
	}
	if f.exists(&agentorcav1alpha1.AgentDeployment{}, "pub-agent") {
		t.Error("the deployment is still there")
	}
	if rec = pub(tenancy.RoleBuilder, http.MethodDelete, "pub-agent", ``); rec.Code != http.StatusNotFound {
		t.Errorf("unpublish twice = %d", rec.Code)
	}
	// The Git-owned deployment is guarded like any deployment delete.
	if rec = pub(tenancy.RoleBuilder, http.MethodDelete, "ui-agent-with-git-deploy", ``); rec.Code != http.StatusForbidden {
		t.Errorf("unpublish a Git-owned deployment = %d", rec.Code)
	}
	if rec = pub(tenancy.RoleBuilder, http.MethodDelete, "theirs", ``); rec.Code != http.StatusNotFound {
		t.Errorf("unpublish another tenant's = %d", rec.Code)
	}
}

func countRunning(items []agentorcav1alpha1.AgentDeployment, agent string) int {
	n := 0
	for _, d := range items {
		if d.Spec.AgentRef == agent {
			n++
		}
	}
	return n
}

// ── options ─────────────────────────────────────────────────────────────────

func TestBuilderOptions(t *testing.T) {
	mine := map[string]string{tenancy.LabelTenantID: isoA}
	f := newBuilderFixture(t,
		&agentorcav1alpha1.ModelSelector{ObjectMeta: metav1.ObjectMeta{Name: "mine-sel", Namespace: sharedNS, Labels: mine, Annotations: map[string]string{AnnotationDescription: "fast"}}},
		&agentorcav1alpha1.ModelSelector{ObjectMeta: metav1.ObjectMeta{Name: "theirs-sel", Namespace: sharedNS, Labels: map[string]string{tenancy.LabelTenantID: isoB}}},
		&agentorcav1alpha1.ModelSelector{ObjectMeta: metav1.ObjectMeta{Name: "shared-sel", Namespace: platformNS, Labels: platformLabels()}},
		&agentorcav1alpha1.ModelSelector{ObjectMeta: metav1.ObjectMeta{Name: "hidden-sel", Namespace: platformNS}},
		&agentorcav1alpha1.KnowledgeBase{ObjectMeta: metav1.ObjectMeta{Name: "kb", Namespace: sharedNS, Labels: mine}, Spec: agentorcav1alpha1.KnowledgeBaseSpec{Description: "docs"}},
		&agentorcav1alpha1.MCPServer{ObjectMeta: metav1.ObjectMeta{Name: "mcp", Namespace: sharedNS, Labels: mine}},
	)
	rec := f.serve(f.as(tenancy.RoleViewer), http.MethodGet, "/api/builder/options", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("options as viewer = %d %s", rec.Code, rec.Body.String())
	}
	got := decode[builderOptions](t, rec)
	if want := []builderOption{{"shared-sel", platformNS, ""}, {"mine-sel", sharedNS, "fast"}}; !slices.Equal(got.ModelSelectors, want) {
		t.Errorf("modelSelectors = %+v, want %+v", got.ModelSelectors, want)
	}
	if want := []builderOption{{"kb", sharedNS, "docs"}}; !slices.Equal(got.KnowledgeBases, want) {
		t.Errorf("knowledgeBases = %+v", got.KnowledgeBases)
	}
	if len(got.MCPServers) != 1 || len(got.Tools) != 1 || got.Tools[0].Name != "tpl-tool" || len(got.GuardrailPolicies) != 1 {
		t.Errorf("options = %+v", got)
	}
	// Lists are present even when empty.
	empty := newBuilderFixture(t).serve(f.as(tenancy.RoleViewer), http.MethodGet, "/api/builder/options", "")
	if !strings.Contains(empty.Body.String(), `"knowledgeBases":[]`) {
		t.Errorf("empty list = %s", empty.Body.String())
	}
	noRole := f.as(tenancy.RoleViewer)
	noRole.Roles = nil
	if rec = f.serve(noRole, http.MethodGet, "/api/builder/options", ""); rec.Code != http.StatusForbidden {
		t.Errorf("no role = %d", rec.Code)
	}
	if rec = f.serve(f.as(tenancy.RoleBuilder), http.MethodPost, "/api/builder/options", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST = %d", rec.Code)
	}
}

// ── dry run ─────────────────────────────────────────────────────────────────

func TestDryRunWrites(t *testing.T) {
	const body = `{"metadata":{"name":"bot"},"spec":{"modelSelectorRef":"sel","runtime":{"ociRef":"registry.example/a:1"}}}`
	f := newBuilderFixture(t, &agentorcav1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{
		Name: "owned", Namespace: sharedNS, Labels: map[string]string{tenancy.LabelTenantID: isoA}, Annotations: map[string]string{tenancy.AnnotationOwner: "git"},
	}})
	type result struct {
		Valid  *bool          `json:"valid"`
		Errors []string       `json:"errors"`
		Object map[string]any `json:"object"`
	}

	rec := f.do(f.as(tenancy.RoleBuilder), http.MethodPost, "/api/resources/agents?dryRun=true", body)
	if res := decode[result](t, rec); rec.Code != http.StatusOK || res.Valid == nil || !*res.Valid || res.Object == nil {
		t.Fatalf("dry run create = %d %s", rec.Code, rec.Body.String())
	}
	if f.exists(&agentorcav1alpha1.Agent{}, "bot") || len(f.audit.events) != 0 {
		t.Error("a dry run stored the object or wrote an audit row")
	}

	for _, tc := range []struct {
		name         string
		role         tenancy.Role
		method, path string
		body         string
		want         int
	}{
		{"create as runner", tenancy.RoleRunner, http.MethodPost, "/api/resources/agents?dryRun=true", body, http.StatusForbidden},
		{"tools need admin", tenancy.RoleBuilder, http.MethodPost, "/api/resources/tools?dryRun=true", `{"metadata":{"name":"t"}}`, http.StatusForbidden},
		{"bad body", tenancy.RoleBuilder, http.MethodPost, "/api/resources/agents?dryRun=true", `{"metadata":{"name":"x"},"spec":{"tools":"no"}}`, http.StatusBadRequest},
		{"missing name", tenancy.RoleBuilder, http.MethodPost, "/api/resources/agents?dryRun=true", `{"spec":{}}`, http.StatusBadRequest},
		{"update of a Git-owned object", tenancy.RoleAdmin, http.MethodPut, "/api/resources/tools/shared/owned?dryRun=true", `{"metadata":{"resourceVersion":"1"}}`, http.StatusConflict},
		{"update of another tenant's object", tenancy.RoleAdmin, http.MethodPut, "/api/resources/tools/shared/none?dryRun=true", `{"metadata":{"resourceVersion":"1"}}`, http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := f.do(f.as(tc.role), tc.method, tc.path, tc.body)
			res := decode[result](t, rec)
			if rec.Code != tc.want || res.Valid == nil || *res.Valid || len(res.Errors) == 0 {
				t.Errorf("= %d %s, want %d with valid:false and errors", rec.Code, rec.Body.String(), tc.want)
			}
		})
	}
	if len(f.audit.events) != 0 {
		t.Errorf("audit = %+v", f.audit.events)
	}
}

func TestDryRunWriterReshapesDenials(t *testing.T) {
	d := &dryRunWriter{header: http.Header{}, status: http.StatusForbidden}
	d.body.WriteString(`creating resource: admission webhook "vagent.example" denied the request: tool "x" belongs to tenant y`)
	rec := httptest.NewRecorder()
	d.finish(rec)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), `"errors":["tool \"x\" belongs to tenant y"]`) {
		t.Errorf("= %d %s", rec.Code, rec.Body.String())
	}
}

// ── references of a template ────────────────────────────────────────────────

func TestUseTemplateChecksReferences(t *testing.T) {
	use := func(f *builderFixture, body string) *httptest.ResponseRecorder {
		return f.serve(f.as(tenancy.RoleBuilder), http.MethodPost, "/api/templates/"+platformNS+"/"+tplName+"/use", body)
	}
	nothingCreated := func(t *testing.T, f *builderFixture) {
		t.Helper()
		if f.exists(&agentorcav1alpha1.Agent{}, "my-bot") || f.exists(&agentorcav1alpha1.Tool{}, "my-bot-tool") || len(f.audit.events) != 0 {
			t.Errorf("something was created or audited: %+v", f.audit.events)
		}
	}
	errorsOf := func(t *testing.T, rec *httptest.ResponseRecorder) []string {
		t.Helper()
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("= %d %s, want 422", rec.Code, rec.Body.String())
		}
		return decode[struct {
			Errors []string `json:"errors"`
		}](t, rec).Errors
	}

	t.Run("missing selector", func(t *testing.T) {
		f := newBuilderFixture(t, &agentorcav1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{Name: "shared-tool", Namespace: sharedNS, Labels: map[string]string{tenancy.LabelTenantID: isoA}}})
		errs := errorsOf(t, use(f, `{"name":"my-bot"}`))
		if len(errs) != 1 || !strings.Contains(errs[0], `ModelSelector "default" does not exist in namespace shared`) || strings.Contains(errs[0], "one of its own") {
			t.Errorf("errors = %q", errs)
		}
		nothingCreated(t, f)
		body := decode[struct {
			Message string       `json:"message"`
			Missing []missingRef `json:"missing"`
		}](t, use(f, `{"name":"my-bot"}`))
		want := missingRef{Kind: "ModelSelector", Name: builderSelector, Namespace: sharedNS, Alternatives: []string{}, ReplaceKey: "ModelSelector/default"}
		if body.Message != "This template needs 1 thing that is not set up in workspace shared." ||
			len(body.Missing) != 1 || body.Missing[0].Alternatives == nil || !slices.Equal(body.Missing[0].Alternatives, want.Alternatives) || body.Missing[0].ReplaceKey != want.ReplaceKey {
			t.Errorf("body = %+v", body)
		}
		if raw := use(f, `{"name":"my-bot"}`).Body.String(); !strings.Contains(raw, `"alternatives":[]`) {
			t.Errorf("alternatives must be [] not null: %s", raw)
		}
	})
	t.Run("alternatives are named and replace satisfies", func(t *testing.T) {
		other := &agentorcav1alpha1.ModelSelector{ObjectMeta: metav1.ObjectMeta{Name: "team-sel", Namespace: sharedNS, Labels: map[string]string{tenancy.LabelTenantID: isoA}}}
		f := newBuilderFixture(t, other, &agentorcav1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{Name: "shared-tool", Namespace: sharedNS, Labels: map[string]string{tenancy.LabelTenantID: isoA}}})
		errs := errorsOf(t, use(f, `{"name":"my-bot"}`))
		if len(errs) != 1 || !strings.Contains(errs[0], "team-sel") {
			t.Errorf("errors = %q", errs)
		}
		if raw := use(f, `{"name":"my-bot"}`).Body.String(); !strings.Contains(raw, `"alternatives":["team-sel"]`) || !strings.Contains(raw, `"replaceKey":"ModelSelector/default"`) {
			t.Errorf("missing = %s", raw)
		}
		nothingCreated(t, f)
		if rec := use(f, `{"name":"my-bot","replace":{"ModelSelector/default":"team-sel"}}`); rec.Code != http.StatusCreated {
			t.Fatalf("with replace = %d %s", rec.Code, rec.Body.String())
		}
		var a agentorcav1alpha1.Agent
		if !f.exists(&a, "my-bot") || a.Spec.ModelSelectorRef != "team-sel" {
			t.Errorf("selector = %q", a.Spec.ModelSelectorRef)
		}
		// A replacement that does not exist is still caught.
		if rec := use(f, `{"name":"other","replace":{"ModelSelector/default":"ghost"}}`); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("replace with a missing object = %d", rec.Code)
		}
		if rec := use(f, `{"name":"other","replace":{"bad":"x"}}`); rec.Code != http.StatusBadRequest {
			t.Errorf("bad replace key = %d", rec.Code)
		}
	})
	t.Run("missing tool outside the group, several problems", func(t *testing.T) {
		f := newBuilderFixture(t)
		errs := errorsOf(t, use(f, `{"name":"my-bot"}`))
		if len(errs) != 2 || !strings.Contains(strings.Join(errs, "|"), `Tool "shared-tool"`) {
			t.Errorf("errors = %q", errs)
		}
		if raw := use(f, `{"name":"my-bot"}`).Body.String(); !strings.Contains(raw, "This template needs 2 things that are not set up in workspace shared.") {
			t.Errorf("message = %s", raw)
		}
		nothingCreated(t, f)
	})
	t.Run("an object of another tenant counts as missing", func(t *testing.T) {
		theirs := map[string]string{tenancy.LabelTenantID: isoB}
		f := newBuilderFixture(t,
			&agentorcav1alpha1.ModelSelector{ObjectMeta: metav1.ObjectMeta{Name: builderSelector, Namespace: sharedNS, Labels: theirs}},
			&agentorcav1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{Name: "shared-tool", Namespace: sharedNS, Labels: map[string]string{tenancy.LabelTenantID: isoA}}})
		errs := errorsOf(t, use(f, `{"name":"my-bot"}`))
		if len(errs) != 1 || strings.Contains(errs[0], "one of its own") {
			t.Errorf("errors = %q (the other tenant's selector must not be offered)", errs)
		}
	})
}

func TestUnpublishNeedsAChoiceOfSeveral(t *testing.T) {
	mine := map[string]string{tenancy.LabelTenantID: isoA}
	dep := func(name string) *agentorcav1alpha1.AgentDeployment {
		return &agentorcav1alpha1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: sharedNS, Labels: mine, Annotations: map[string]string{tenancy.AnnotationOwner: tenancy.OwnerUI}},
			Spec:       agentorcav1alpha1.AgentDeploymentSpec{AgentRef: "pub-agent"},
		}
	}
	f := newBuilderFixture(t, dep("d1"), dep("d2"))
	path := "/api/agents/" + sharedNS + "/pub-agent/publish"
	if rec := f.serve(f.as(tenancy.RoleBuilder), http.MethodDelete, path, ""); rec.Code != http.StatusConflict {
		t.Fatalf("several, no choice = %d %s", rec.Code, rec.Body.String())
	}
	if rec := f.serve(f.as(tenancy.RoleBuilder), http.MethodDelete, path+"?deployment=d2", ""); rec.Code != http.StatusOK {
		t.Fatalf("chosen = %d %s", rec.Code, rec.Body.String())
	}
	if rec := f.serve(f.as(tenancy.RoleBuilder), http.MethodDelete, path, ""); rec.Code != http.StatusOK {
		t.Fatalf("the one left needs no parameter: %d %s", rec.Code, rec.Body.String())
	}
}
