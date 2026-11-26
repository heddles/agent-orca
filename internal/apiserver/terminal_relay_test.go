package apiserver

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/acpfs"
)

const terminalAgentName = "coder"

func terminalAgent(spec *agentorcav1alpha1.TerminalAccessSpec) *agentorcav1alpha1.Agent {
	return &agentorcav1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: terminalAgentName, Namespace: "tenant-acme"},
		Spec:       agentorcav1alpha1.AgentSpec{SystemPrompt: "x", Terminal: spec},
	}
}

// A command is relayed only for an agent with a local terminal, on a run whose
// caller offers one.
func TestInternalTerminalGating(t *testing.T) {
	for name, tc := range map[string]struct {
		spec     *agentorcav1alpha1.TerminalAccessSpec
		attached bool
		want     int
	}{
		"allowed":            {&agentorcav1alpha1.TerminalAccessSpec{}, true, http.StatusCreated},
		"no editor":          {&agentorcav1alpha1.TerminalAccessSpec{}, false, http.StatusConflict},
		"agent has none":     {nil, true, http.StatusForbidden},
		"agent has none, ui": {nil, false, http.StatusForbidden},
	} {
		t.Run(name, func(t *testing.T) {
			run := fsTestRun("run-1")
			run.Spec.AgentRef = terminalAgentName
			if tc.attached {
				run.Annotations = map[string]string{AnnotationLocalTerminal: claimTrue}
			}
			relay := acpfs.NewMemRelay()
			s := newACPServer(t, run, terminalAgent(tc.spec))
			srv := &Server{crdClient: s.crdClient, fsRelay: relay}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/agentrun/tenant-acme/run-1/fs",
				bytes.NewBufferString(`{"method":"terminal/run","params":{"command":"ls"}}`))
			srv.enqueueFSRequest(rec, req, fsCaller, "tenant-acme", "run-1")
			if rec.Code != tc.want {
				t.Fatalf("status = %d (%s), want %d", rec.Code, rec.Body.String(), tc.want)
			}
			pending, _ := relay.Pending(t.Context(), "tenant-acme:run-1")
			if got := len(pending) == 1; got != (tc.want == http.StatusCreated) {
				t.Fatalf("pending = %v", pending)
			}
		})
	}
}

// createdRun posts a run and returns the AgentRun that was made.
func createdRun(t *testing.T, s *ACPServer, path, body string) agentorcav1alpha1.AgentRun {
	t.Helper()
	rec := httptest.NewRecorder()
	s.withTenant(testTenant()).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var runs agentorcav1alpha1.AgentRunList
	if err := s.crdClient.List(t.Context(), &runs, client.InNamespace("tenant-acme")); err != nil || len(runs.Items) != 1 {
		t.Fatalf("runs = %v, %v", runs.Items, err)
	}
	return runs.Items[0]
}

func TestACPCreateRunMarksALocalTerminal(t *testing.T) {
	const input = `"agent_name":"coder","input":[{"role":"user","parts":[{"content":"hi"}]}]`
	for name, tc := range map[string]struct {
		spec *agentorcav1alpha1.TerminalAccessSpec
		body string
		want bool
	}{
		"editor offers one": {&agentorcav1alpha1.TerminalAccessSpec{}, `{` + input + `,"client_capabilities":{"terminal":true}}`, true},
		"editor does not":   {&agentorcav1alpha1.TerminalAccessSpec{}, `{` + input + `,"client_capabilities":{"terminal":false}}`, false},
		"older bridge":      {&agentorcav1alpha1.TerminalAccessSpec{}, `{` + input + `}`, false},
		"agent without one": {nil, `{` + input + `,"client_capabilities":{"terminal":true}}`, false},
	} {
		// `aoctl acp serve` uses /agents/{name}/run; /runs is the ACP route.
		for _, path := range []string{"/agents/coder/run", "/runs"} {
			t.Run(name+" "+path, func(t *testing.T) {
				run := createdRun(t, newACPServer(t, terminalAgent(tc.spec)), path, tc.body)
				if got := run.Annotations[AnnotationLocalTerminal] == claimTrue; got != tc.want {
					t.Fatalf("annotations = %v, want marked %v", run.Annotations, tc.want)
				}
			})
		}
	}
}

// A resumed run is a new run: it is marked by what the resuming caller offers,
// not by what the first one did.
func TestACPResumeMarksALocalTerminal(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want bool
	}{
		"resumed from the editor": {`{"await_resume":{"answer":"y"},"client_capabilities":{"terminal":true}}`, true},
		"resumed elsewhere":       {`{"await_resume":{"answer":"y"}}`, false},
	} {
		t.Run(name, func(t *testing.T) {
			run := fsTestRun("run-1")
			run.Spec.AgentRef = terminalAgentName
			run.Annotations = map[string]string{AnnotationLocalTerminal: claimTrue}
			run.Status.Phase = agentorcav1alpha1.AgentRunPhaseWaitingForInput
			s := newACPServer(t, run, terminalAgent(&agentorcav1alpha1.TerminalAccessSpec{}))
			rec := httptest.NewRecorder()
			req := asTenant(httptest.NewRequest(http.MethodPost, "/runs/run-1", strings.NewReader(tc.body)), testTenant())
			s.resumeACPRun(rec, req, run.DeepCopy())
			if rec.Code != http.StatusOK {
				t.Fatalf("resume: %d %s", rec.Code, rec.Body.String())
			}
			var resp ACPRun
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			var runs agentorcav1alpha1.AgentRunList
			if err := s.crdClient.List(t.Context(), &runs, client.InNamespace("tenant-acme")); err != nil {
				t.Fatal(err)
			}
			var next *agentorcav1alpha1.AgentRun
			for i := range runs.Items {
				if runs.Items[i].Spec.PriorRunRef == "run-1" {
					next = &runs.Items[i]
				}
			}
			if next == nil {
				t.Fatalf("no continuation among %v", runs.Items)
			}
			if got := next.Annotations[AnnotationLocalTerminal] == claimTrue; got != tc.want {
				t.Fatalf("annotations = %v, want marked %v", next.Annotations, tc.want)
			}
		})
	}
}

func TestHandleAgentManifestReportsTheTerminal(t *testing.T) {
	for _, spec := range []*agentorcav1alpha1.TerminalAccessSpec{nil, {MaxTimeoutSeconds: 90}, {}} {
		s := newACPServer(t, terminalAgent(spec))
		rec := httptest.NewRecorder()
		s.withTenant(testTenant()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/agents/coder", nil))
		var m ACPAgentManifest
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		switch {
		case spec == nil && m.Terminal != nil:
			t.Fatalf("no terminal expected, got %+v", m.Terminal)
		case spec != nil && m.Terminal == nil:
			t.Fatal("terminal policy missing")
		case spec != nil && spec.MaxTimeoutSeconds == 90 && m.Terminal.MaxTimeoutSeconds != 90,
			spec != nil && spec.MaxTimeoutSeconds == 0 && m.Terminal.MaxTimeoutSeconds != 600:
			t.Fatalf("limit = %d", m.Terminal.MaxTimeoutSeconds)
		}
	}
}
