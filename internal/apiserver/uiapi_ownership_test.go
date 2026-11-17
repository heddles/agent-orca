package apiserver

import (
	"context"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/tenancy"
)

const argoTracking = "argocd.argoproj.io/tracking-id"

func testAgent(name string, labels, annotations map[string]string) *agentorcav1alpha1.Agent {
	return &agentorcav1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: sharedNS, Labels: labels, Annotations: annotations,
			ResourceVersion: "5", UID: types.UID("uid-" + name),
			Finalizers: []string{"agentorca.io/finalizer"},
		},
		Spec: agentorcav1alpha1.AgentSpec{
			ModelSelectorRef: "sel", SystemPrompt: "be helpful",
			Runtime: agentorcav1alpha1.AgentRuntime{OCIRef: "registry.example/a:1"},
		},
		Status: agentorcav1alpha1.AgentStatus{ServiceAccountName: "sa-" + name},
	}
}

func mine(extra map[string]string) map[string]string {
	l := map[string]string{tenancy.LabelTenantID: isoA}
	maps.Copy(l, extra)
	return l
}

type ownershipFixture struct{ *writeFixture }

func newOwnershipFixture(t *testing.T) *ownershipFixture {
	t.Helper()
	f := newWriteFixture(t,
		testAgent("ui-agent", mine(map[string]string{"team": "support"}), map[string]string{
			tenancy.AnnotationOwner: tenancy.OwnerUI, tenancy.AnnotationLastModifiedBy: "ada", tenancy.AnnotationLastModifiedAt: "2026-11-01T00:00:00Z",
			tenancy.AnnotationMaintainer: "support-team", "note": "keep me",
		}),
		testAgent("argo-agent", mine(nil), map[string]string{argoTracking: "shop:agentorca.agentorca.io/Agent:shared/argo-agent"}),
		testAgent("flux-agent", mine(map[string]string{"kustomize.toolkit.fluxcd.io/name": "apps", "kustomize.toolkit.fluxcd.io/namespace": "flux-system"}), nil),
		testAgent("explicit-git", mine(nil), map[string]string{tenancy.AnnotationOwner: tenancy.OwnerGit}),
		testAgent("by-hand", mine(nil), nil),
		testAgent("theirs", map[string]string{tenancy.LabelTenantID: isoB}, map[string]string{tenancy.AnnotationOwner: tenancy.OwnerUI}),
		testAgent("template", map[string]string{tenancy.LabelScope: tenancy.ScopePlatform}, map[string]string{argoTracking: "platform:g/Agent:shared/template", "agentorca.io/template": "true"}),
		isoTool(sharedNS, "ui-tool", tenancy.LabelTenantID, isoA),
	)
	return &ownershipFixture{f}
}

func (f *ownershipFixture) agent(name string) *agentorcav1alpha1.Agent {
	f.t.Helper()
	var a agentorcav1alpha1.Agent
	if err := f.c.Get(context.Background(), client.ObjectKey{Namespace: sharedNS, Name: name}, &a); err != nil {
		f.t.Fatalf("get agent %s: %v", name, err)
	}
	return &a
}

func ownershipOfBody(t *testing.T, body []byte) ownershipView {
	t.Helper()
	var v struct {
		Ownership ownershipView `json:"ownership"`
	}
	if err := yaml.Unmarshal(body, &v); err != nil {
		t.Fatalf("decoding %q: %v", body, err)
	}
	return v.Ownership
}

func TestOwnershipInResourceResponses(t *testing.T) {
	f := newOwnershipFixture(t)
	tests := []struct {
		name  string
		role  tenancy.Role
		path  string
		owner string
		tool  string
		app   string
		edit  bool
	}{
		{"ui, builder", tenancy.RoleBuilder, "agents/shared/ui-agent", tenancy.OwnerUI, "", "", true},
		{"ui, viewer", tenancy.RoleViewer, "agents/shared/ui-agent", tenancy.OwnerUI, "", "", false},
		{"ui, runner", tenancy.RoleRunner, "agents/shared/ui-agent", tenancy.OwnerUI, "", "", false},
		{"argo", tenancy.RoleBuilder, "agents/shared/argo-agent", tenancy.OwnerGit, tenancy.ToolArgoCD, "shop", false},
		{"flux", tenancy.RoleAdmin, "agents/shared/flux-agent", tenancy.OwnerGit, tenancy.ToolFluxKustomize, "flux-system/apps", false},
		{"explicit git", tenancy.RoleBuilder, "agents/shared/explicit-git", tenancy.OwnerGit, tenancy.ToolGit, "", false},
		{"no owner", tenancy.RoleBuilder, "agents/shared/by-hand", tenancy.OwnerUnknown, "", "", false},
		{"platform template is read-only", tenancy.RoleAdmin, "agents/shared/template", tenancy.OwnerGit, tenancy.ToolArgoCD, "platform", false},
		{"admin kind needs admin: builder", tenancy.RoleBuilder, "tools/shared/ui-tool", tenancy.OwnerUI, "", "", false},
		{"admin kind needs admin: admin", tenancy.RoleAdmin, "tools/shared/ui-tool", tenancy.OwnerUI, "", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := f.do(f.as(tc.role), http.MethodGet, "/api/resources/"+tc.path, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("get = %d %s", rec.Code, rec.Body.String())
			}
			got := ownershipOfBody(t, rec.Body.Bytes())
			if got.Owner != tc.owner || got.Tool != tc.tool || got.App != tc.app || got.Editable != tc.edit || got.Handoff.Pending {
				t.Errorf("ownership = %+v", got)
			}
		})
	}

	t.Run("a pending handoff is shown", func(t *testing.T) {
		a := f.agent("ui-agent")
		exp := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
		tenancy.SetHandoff(a, exp)
		if err := f.c.Update(context.Background(), a); err != nil {
			t.Fatal(err)
		}
		got := ownershipOfBody(t, f.do(f.as(tenancy.RoleBuilder), http.MethodGet, "/api/resources/agents/shared/ui-agent", "").Body.Bytes())
		if !got.Handoff.Pending || got.Handoff.Expires != exp.Format(time.RFC3339) || !got.Editable {
			t.Errorf("ownership = %+v", got)
		}
	})

	t.Run("every item of a list carries it", func(t *testing.T) {
		rec := f.do(f.as(tenancy.RoleBuilder), http.MethodGet, "/api/resources/agents", "")
		var list struct {
			Items []struct {
				Metadata  metav1.ObjectMeta `json:"metadata"`
				Ownership *ownershipView    `json:"ownership"`
			} `json:"items"`
		}
		decodeJSON(t, rec, &list)
		seen := map[string]string{}
		for _, it := range list.Items {
			if it.Ownership == nil {
				t.Fatalf("%s has no ownership", it.Metadata.Name)
			}
			seen[it.Metadata.Name] = it.Ownership.Owner
		}
		if seen["ui-agent"] != tenancy.OwnerUI || seen["argo-agent"] != tenancy.OwnerGit || seen["by-hand"] != tenancy.OwnerUnknown {
			t.Errorf("owners = %v", seen)
		}
		if _, leaked := seen["theirs"]; leaked {
			t.Error("another tenant's agent is listed")
		}
	})

	t.Run("the catalog summaries carry it", func(t *testing.T) {
		var out []struct {
			Name      string        `json:"name"`
			Ownership ownershipView `json:"ownership"`
		}
		rec := recordList(f.s.handleListAgents, f.as(tenancy.RoleBuilder), "/api/agents")
		decodeJSON(t, rec, &out)
		owners := map[string]ownershipView{}
		for _, o := range out {
			owners[o.Name] = o.Ownership
		}
		if !owners["ui-agent"].Editable || owners["argo-agent"].Owner != tenancy.OwnerGit || owners["argo-agent"].Editable {
			t.Errorf("summaries = %+v", owners)
		}
		rec = recordList(f.s.handleListTools, f.as(tenancy.RoleAdmin), "/api/tools")
		var tools []struct {
			Ownership ownershipView `json:"ownership"`
		}
		decodeJSON(t, rec, &tools)
		if len(tools) != 1 || tools[0].Ownership.Owner != tenancy.OwnerUI {
			t.Errorf("tools = %+v", tools)
		}
	})
}

func TestUpdateKeepsPendingHandoff(t *testing.T) {
	f := newOwnershipFixture(t)
	a := f.agent("ui-agent")
	tenancy.SetHandoff(a, time.Now().Add(time.Hour))
	if err := f.c.Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	rv := f.agent("ui-agent").ResourceVersion
	rec := f.do(f.as(tenancy.RoleBuilder), http.MethodPut, "/api/resources/agents/shared/ui-agent",
		`{"metadata":{"resourceVersion":"`+rv+`","annotations":{"agentorca.io/handoff":"pending","agentorca.io/handoff-expires":"2099-01-01T00:00:00Z"}},"spec":{"modelSelectorRef":"sel","runtime":{"ociRef":"registry.example/a:2"}}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("update = %d %s", rec.Code, rec.Body.String())
	}
	got := f.agent("ui-agent")
	if !tenancy.HandoffOf(got).Pending || got.Annotations[tenancy.AnnotationHandoffExpires] == "2099-01-01T00:00:00Z" {
		t.Errorf("an edit changed the handoff: %v", got.Annotations)
	}
}

func TestExportResource(t *testing.T) { //nolint:gocyclo // one fixture, many cases
	f := newOwnershipFixture(t)
	f.s.SetOwnership(tenancy.DetectOptions{}, 48*time.Hour)
	builder := f.as(tenancy.RoleBuilder)

	t.Run("a clean manifest, and a handoff on the live object", func(t *testing.T) {
		before := time.Now()
		rec := f.do(builder, http.MethodPost, "/api/resources/agents/shared/ui-agent/export", "")
		if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/yaml") {
			t.Fatalf("export = %d %s %s", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
		}
		var m map[string]any
		if err := yaml.Unmarshal(rec.Body.Bytes(), &m); err != nil {
			t.Fatalf("not YAML: %v\n%s", err, rec.Body.String())
		}
		if m["apiVersion"] != agentorcav1alpha1.GroupVersion.String() || m["kind"] != kindAgent {
			t.Errorf("type = %v %v", m["apiVersion"], m["kind"])
		}
		if _, has := m["status"]; has {
			t.Error("status exported")
		}
		md, _ := m["metadata"].(map[string]any)
		for _, k := range []string{"managedFields", "resourceVersion", "uid", "creationTimestamp", "generation", "finalizers", "ownerReferences"} {
			if _, has := md[k]; has {
				t.Errorf("metadata.%s exported", k)
			}
		}
		if md["name"] != "ui-agent" || md["namespace"] != sharedNS {
			t.Errorf("metadata = %v", md)
		}
		ann, _ := md["annotations"].(map[string]any)
		want := map[string]any{tenancy.AnnotationOwner: "git", tenancy.AnnotationMaintainer: "support-team", "note": "keep me"}
		if len(ann) != len(want) {
			t.Errorf("annotations = %v, want %v", ann, want)
		}
		for k, v := range want {
			if ann[k] != v {
				t.Errorf("annotation %s = %v, want %v", k, ann[k], v)
			}
		}
		labels, _ := md["labels"].(map[string]any)
		if labels[tenancy.LabelTenantID] != isoA || labels["team"] != "support" {
			t.Errorf("labels = %v", labels)
		}
		spec, _ := m["spec"].(map[string]any)
		if spec["modelSelectorRef"] != "sel" {
			t.Errorf("spec = %v", spec)
		}

		live := f.agent("ui-agent")
		h := tenancy.HandoffOf(live)
		if !h.Pending || h.Expires.Before(before.Add(47*time.Hour)) || h.Expires.After(time.Now().Add(49*time.Hour)) {
			t.Errorf("handoff on the live object = %+v (annotations %v)", h, live.Annotations)
		}
		if live.Annotations[tenancy.AnnotationOwner] != tenancy.OwnerUI {
			t.Errorf("the live object's owner changed: %v", live.Annotations)
		}
		last := f.audit.events[len(f.audit.events)-1]
		if last.Name != "ui-agent" || !strings.Contains(last.Summary, "exported") || last.UserID != builderUser {
			t.Errorf("audit = %+v", last)
		}
	})

	t.Run("handoff=false leaves the object alone", func(t *testing.T) {
		rec := f.do(builder, http.MethodPost, "/api/resources/agents/shared/ui-agent/export?handoff=false", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("export = %d", rec.Code)
		}
		// The earlier test set one: cancel it, then export again without.
		if rec = f.do(builder, http.MethodDelete, "/api/resources/agents/shared/ui-agent/handoff", ""); rec.Code != http.StatusOK {
			t.Fatalf("cancel = %d %s", rec.Code, rec.Body.String())
		}
		rec = f.do(builder, http.MethodPost, "/api/resources/agents/shared/ui-agent/export?handoff=false", "")
		if rec.Code != http.StatusOK || tenancy.HandoffOf(f.agent("ui-agent")).Requested {
			t.Errorf("export without handoff = %d, annotations %v", rec.Code, f.agent("ui-agent").Annotations)
		}
	})

	t.Run("only a UI-owned object can be exported for handoff", func(t *testing.T) {
		for _, name := range []string{"argo-agent", "flux-agent", "explicit-git", "by-hand"} {
			rec := f.do(builder, http.MethodPost, "/api/resources/agents/shared/"+name+"/export", "")
			if rec.Code != http.StatusConflict {
				t.Errorf("%s: export = %d %s", name, rec.Code, rec.Body.String())
			}
			if tenancy.HandoffOf(f.agent(name)).Requested {
				t.Errorf("%s was marked for handoff", name)
			}
		}
	})

	t.Run("roles", func(t *testing.T) {
		for _, role := range []tenancy.Role{tenancy.RoleViewer, tenancy.RoleRunner} {
			if rec := f.do(f.as(role), http.MethodPost, "/api/resources/agents/shared/ui-agent/export", ""); rec.Code != http.StatusForbidden {
				t.Errorf("%s export = %d", role, rec.Code)
			}
		}
		if rec := f.do(builder, http.MethodPost, "/api/resources/tools/shared/ui-tool/export", ""); rec.Code != http.StatusForbidden {
			t.Errorf("builder export of an admin kind = %d", rec.Code)
		}
		if rec := f.do(f.as(tenancy.RoleAdmin), http.MethodPost, "/api/resources/tools/shared/ui-tool/export", ""); rec.Code != http.StatusOK {
			t.Errorf("admin export of an admin kind = %d", rec.Code)
		}
	})

	t.Run("tenant scoping", func(t *testing.T) {
		if rec := f.do(builder, http.MethodPost, "/api/resources/agents/shared/theirs/export", ""); rec.Code != http.StatusNotFound {
			t.Errorf("another tenant's object = %d", rec.Code)
		}
		if rec := f.do(builder, http.MethodPost, "/api/resources/agents/shared/template/export", ""); rec.Code != http.StatusForbidden {
			t.Errorf("a platform-shared object = %d", rec.Code)
		}
		if rec := f.do(builder, http.MethodPost, "/api/resources/agents/shared/missing/export", ""); rec.Code != http.StatusNotFound {
			t.Errorf("a missing object = %d", rec.Code)
		}
		if tenancy.HandoffOf(f.agent("theirs")).Requested {
			t.Error("the other tenant's object was marked")
		}
	})

	t.Run("the method matters", func(t *testing.T) {
		if rec := f.do(builder, http.MethodGet, "/api/resources/agents/shared/ui-agent/export", ""); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("GET export = %d", rec.Code)
		}
		if rec := f.do(builder, http.MethodPost, "/api/resources/agents/shared/ui-agent/bogus", ""); rec.Code != http.StatusNotFound {
			t.Errorf("unknown action = %d", rec.Code)
		}
	})
}

func TestCancelHandoff(t *testing.T) {
	f := newOwnershipFixture(t)
	builder := f.as(tenancy.RoleBuilder)
	if rec := f.do(builder, http.MethodDelete, "/api/resources/agents/shared/ui-agent/handoff", ""); rec.Code != http.StatusConflict {
		t.Errorf("cancel with nothing pending = %d %s", rec.Code, rec.Body.String())
	}
	if rec := f.do(builder, http.MethodPost, "/api/resources/agents/shared/ui-agent/export", ""); rec.Code != http.StatusOK {
		t.Fatalf("export = %d", rec.Code)
	}
	if rec := f.do(f.as(tenancy.RoleViewer), http.MethodDelete, "/api/resources/agents/shared/ui-agent/handoff", ""); rec.Code != http.StatusForbidden {
		t.Errorf("viewer cancel = %d", rec.Code)
	}
	if rec := f.do(builder, http.MethodDelete, "/api/resources/agents/shared/theirs/handoff", ""); rec.Code != http.StatusNotFound {
		t.Errorf("another tenant's object = %d", rec.Code)
	}
	if !tenancy.HandoffOf(f.agent("ui-agent")).Pending {
		t.Fatal("the failed cancels removed the handoff")
	}
	rec := f.do(builder, http.MethodDelete, "/api/resources/agents/shared/ui-agent/handoff", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel = %d %s", rec.Code, rec.Body.String())
	}
	got := f.agent("ui-agent")
	if tenancy.HandoffOf(got).Requested || got.Annotations[tenancy.AnnotationOwner] != tenancy.OwnerUI {
		t.Errorf("annotations after cancel = %v", got.Annotations)
	}
	last := f.audit.events[len(f.audit.events)-1]
	if !strings.Contains(last.Summary, "cancelled the handoff") {
		t.Errorf("audit = %+v", last)
	}
}

func TestDuplicateResource(t *testing.T) { //nolint:gocyclo // one fixture, many cases
	f := newOwnershipFixture(t)
	builder := f.as(tenancy.RoleBuilder)
	dup := func(id *TenantIdentity, src, body string) *struct {
		code int
		body string
	} {
		rec := f.do(id, http.MethodPost, "/api/resources/agents/shared/"+src+"/duplicate", body)
		return &struct {
			code int
			body string
		}{rec.Code, rec.Body.String()}
	}

	t.Run("a Git-owned object becomes a UI-owned copy without its tracking", func(t *testing.T) {
		before := len(f.audit.events)
		res := dup(builder, "argo-agent", `{"name":"argo-copy"}`)
		if res.code != http.StatusCreated {
			t.Fatalf("duplicate = %d %s", res.code, res.body)
		}
		got := f.agent("argo-copy")
		if got.Annotations[tenancy.AnnotationOwner] != tenancy.OwnerUI || got.Annotations[argoTracking] != "" ||
			got.Annotations[tenancy.AnnotationLastModifiedBy] != "ada" {
			t.Errorf("annotations = %v", got.Annotations)
		}
		if got.Labels[tenancy.LabelTenantID] != isoA || got.Spec.SystemPrompt != "be helpful" || got.Namespace != sharedNS {
			t.Errorf("copy = %+v %+v", got.ObjectMeta, got.Spec)
		}
		if got.Status.ServiceAccountName != "" || len(got.Finalizers) != 0 || got.UID == f.agent("argo-agent").UID {
			t.Errorf("server-owned fields were copied: %+v", got.ObjectMeta)
		}
		if o := ownershipOfBody(t, []byte(res.body)); o.Owner != tenancy.OwnerUI || !o.Editable {
			t.Errorf("response ownership = %+v", o)
		}
		if len(f.audit.events) != before+1 || !strings.Contains(f.audit.events[len(f.audit.events)-1].Summary, "duplicated Agent shared/argo-agent") {
			t.Errorf("audit = %+v", f.audit.events[before:])
		}
		// The source is untouched.
		if f.agent("argo-agent").Annotations[argoTracking] == "" {
			t.Error("the source lost its tracking")
		}
	})

	t.Run("a Flux label and a handoff do not carry over", func(t *testing.T) {
		src := f.agent("flux-agent")
		tenancy.SetHandoff(src, time.Now().Add(time.Hour))
		if err := f.c.Update(context.Background(), src); err != nil {
			t.Fatal(err)
		}
		if res := dup(builder, "flux-agent", `{"name":"flux-copy"}`); res.code != http.StatusCreated {
			t.Fatalf("duplicate = %d %s", res.code, res.body)
		}
		got := f.agent("flux-copy")
		if got.Labels["kustomize.toolkit.fluxcd.io/name"] != "" || tenancy.HandoffOf(got).Requested {
			t.Errorf("copy = %v %v", got.Labels, got.Annotations)
		}
		if o := tenancy.Detect(got, tenancy.DetectOptions{}); o.Owner != tenancy.OwnerUI {
			t.Errorf("owner = %+v", o)
		}
	})

	t.Run("a UI-owned, an unowned and a platform template can be copied", func(t *testing.T) {
		for _, src := range []string{"ui-agent", "by-hand", "template"} {
			if res := dup(builder, src, `{"name":"copy-of-`+src+`"}`); res.code != http.StatusCreated {
				t.Errorf("duplicate of %s = %d %s", src, res.code, res.body)
				continue
			}
			got := f.agent("copy-of-" + src)
			if tenancy.Detect(got, tenancy.DetectOptions{}).Owner != tenancy.OwnerUI || got.Labels[tenancy.LabelTenantID] != isoA || got.Labels[tenancy.LabelScope] != "" {
				t.Errorf("copy of %s = %v %v", src, got.Labels, got.Annotations)
			}
		}
		if got := f.agent("copy-of-ui-agent"); got.Annotations[tenancy.AnnotationMaintainer] != "support-team" || got.Annotations["note"] != "keep me" {
			t.Errorf("ordinary annotations lost: %v", got.Annotations)
		}
	})

	t.Run("it lands in the caller's workspace namespace", func(t *testing.T) {
		id := f.as(tenancy.RoleBuilder)
		id.Namespace = "a-only"
		rec := f.do(id, http.MethodPost, "/api/resources/agents/shared/argo-agent/duplicate", `{"name":"elsewhere"}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("duplicate = %d %s", rec.Code, rec.Body.String())
		}
		var got agentorcav1alpha1.Agent
		if err := f.c.Get(context.Background(), client.ObjectKey{Namespace: "a-only", Name: "elsewhere"}, &got); err != nil {
			t.Errorf("not in the workspace namespace: %v", err)
		}
	})

	t.Run("refusals", func(t *testing.T) {
		if res := dup(builder, "argo-agent", `{"name":"argo-copy"}`); res.code != http.StatusConflict {
			t.Errorf("a name that exists = %d %s", res.code, res.body)
		}
		if res := dup(builder, "argo-agent", `{}`); res.code != http.StatusBadRequest {
			t.Errorf("no name = %d", res.code)
		}
		if res := dup(builder, "argo-agent", `nope`); res.code != http.StatusBadRequest {
			t.Errorf("a bad body = %d", res.code)
		}
		for _, role := range []tenancy.Role{tenancy.RoleViewer, tenancy.RoleRunner} {
			if res := dup(f.as(role), "argo-agent", `{"name":"x-`+string(role)+`"}`); res.code != http.StatusForbidden {
				t.Errorf("%s duplicate = %d", role, res.code)
			}
		}
		if res := dup(builder, "theirs", `{"name":"stolen"}`); res.code != http.StatusNotFound {
			t.Errorf("another tenant's object = %d", res.code)
		}
		if res := dup(builder, "missing", `{"name":"x"}`); res.code != http.StatusNotFound {
			t.Errorf("a missing object = %d", res.code)
		}
		rec := f.do(builder, http.MethodPost, "/api/resources/tools/shared/ui-tool/duplicate", `{"name":"tool-copy"}`)
		if rec.Code != http.StatusForbidden {
			t.Errorf("builder duplicate of an admin kind = %d", rec.Code)
		}
	})
}

// recordList calls a catalog list handler as id.
func recordList(h http.HandlerFunc, id *TenantIdentity, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h(rec, asTenant(httptest.NewRequest(http.MethodGet, path, nil), id))
	return rec
}
