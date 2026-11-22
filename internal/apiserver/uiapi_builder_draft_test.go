package apiserver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/evals/judge"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// stubModel is the model that writes drafts: it records what it was sent.
type stubModel struct {
	mu     sync.Mutex
	reply  string
	cost   float64
	price  float64
	err    error
	calls  int
	system string
	user   string
}

func (m *stubModel) Complete(_ context.Context, r judge.Request) (judge.Reply, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	m.system, m.user = r.System, r.User
	return judge.Reply{Text: m.reply, CostUSD: m.cost}, m.err
}

func (m *stubModel) PriceUSD(int, int) float64 { return m.price }

type stubResolver struct {
	model *stubModel
	err   error
	asked []string
}

func (r *stubResolver) Resolve(_ context.Context, _ tenancy.Caller, ns, ref string) (judge.Provider, error) {
	r.asked = append(r.asked, ns+"/"+ref)
	if r.err != nil {
		return nil, r.err
	}
	return r.model, nil
}

const goodReply = `{"name":"Refund Helper!","description":"Answers refund questions.","instructions":"You answer refund questions from the policy. Never promise a refund.",
"exampleInputs":["Where is my refund?","Can I return this?"],"connections":["search","mcp","not-a-tool","other-ns-tool"],"knowledgeBases":["policy","secret-kb"],
"guardrail":"pii","suggestedBudgetUsd":0.4,"runtime":{"ociRef":"evil/image"},"unknownField":"dropped"}`

const draftDescription = "An assistant that answers refund questions for our customers using the policy"

type draftFixture struct {
	*builderFixture
	model *stubModel
	res   *stubResolver
}

func newDraftFixture(t *testing.T) *draftFixture {
	t.Helper()
	mine := map[string]string{tenancy.LabelTenantID: isoA}
	theirs := map[string]string{tenancy.LabelTenantID: isoB}
	f := &draftFixture{builderFixture: newBuilderFixture(t,
		&agentorcav1alpha1.ModelSelector{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: sharedNS, Labels: mine}},
		&agentorcav1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{Name: "search", Namespace: sharedNS, Labels: mine}},
		&agentorcav1alpha1.MCPServer{ObjectMeta: metav1.ObjectMeta{Name: "mcp", Namespace: sharedNS, Labels: mine}},
		&agentorcav1alpha1.KnowledgeBase{ObjectMeta: metav1.ObjectMeta{Name: "policy", Namespace: sharedNS, Labels: mine}},
		&agentorcav1alpha1.GuardrailPolicy{ObjectMeta: metav1.ObjectMeta{Name: "pii", Namespace: sharedNS, Labels: mine}},
		// Another tenant's objects in the same workspace, and a tool of another workspace.
		&agentorcav1alpha1.KnowledgeBase{ObjectMeta: metav1.ObjectMeta{Name: "secret-kb", Namespace: sharedNS, Labels: theirs}},
		&agentorcav1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{Name: "other-ns-tool", Namespace: "elsewhere", Labels: mine}},
	)}
	f.model = &stubModel{reply: goodReply, cost: 0.002, price: 0.001}
	f.res = &stubResolver{model: f.model}
	f.s.draftProviders = f.res
	f.s.SetBuilder(BuilderConfig{DraftProvider: "drafter"})
	return f
}

func draftReq(desc string) string { return fmt.Sprintf(`{"description":%q}`, desc) }

func TestBuilderDraft_FillsAReviewableDraftAndWritesNothing(t *testing.T) {
	f := newDraftFixture(t)
	rec := f.serve(f.as(tenancy.RoleBuilder), http.MethodPost, "/api/builder/draft", draftReq(draftDescription))
	if rec.Code != http.StatusOK {
		t.Fatalf("draft = %d %s", rec.Code, rec.Body)
	}
	d := decode[draftResponse](t, rec)
	if d.Name != "refund-helper" || d.Namespace != sharedNS || !d.Valid || len(d.Errors) != 0 {
		t.Errorf("draft = %+v", d)
	}
	if d.Description != "Answers refund questions." || !strings.Contains(d.Instructions, "Never promise a refund") || len(d.ExampleInputs) != 2 {
		t.Errorf("text fields = %+v", d)
	}
	// Only what the workspace has survives, and only the caller's own tenant's.
	if strings.Join(d.Connections, ",") != "search,mcp" || strings.Join(d.KnowledgeBases, ",") != "policy" || d.Guardrail != "pii" || d.SuggestedBudgetUSD != 0.4 {
		t.Errorf("references = %+v", d)
	}
	notes := strings.Join(d.Notes, "\n")
	for _, want := range []string{`"not-a-tool"`, `"other-ns-tool"`, `"secret-kb"`} {
		if !strings.Contains(notes, want) {
			t.Errorf("notes %q miss %s", notes, want)
		}
	}
	if strings.Join(d.AIFields, ",") != "connections,description,exampleInputs,guardrail,instructions,knowledgeBases,name" {
		t.Errorf("aiFields = %v", d.AIFields)
	}
	// Nothing was written: no agent, and the audit row holds sizes, not text.
	var saved agentorcav1alpha1.Agent
	if err := f.c.Get(context.Background(), client.ObjectKey{Namespace: sharedNS, Name: "refund-helper"}, &saved); err == nil {
		t.Error("the draft was saved as an Agent")
	}
	if len(f.audit.events) != 1 {
		t.Fatalf("audit = %+v", f.audit.events)
	}
	ev := f.audit.events[0]
	if ev.Action != postgresql.AuditRequest || ev.Kind != kindAgent || strings.Contains(ev.Summary, "refund") || !strings.Contains(ev.Summary, "nothing was written") {
		t.Errorf("audit = %+v", ev)
	}
	// The model got the description as data, in tagged blocks, and the workspace's names.
	if !strings.Contains(f.model.user, draftDescription) || !strings.Contains(f.model.user, `"search"`) || strings.Contains(f.model.user, "secret-kb") || strings.Contains(f.model.user, "other-ns-tool") {
		t.Errorf("prompt = %s", f.model.user)
	}
	if !strings.Contains(f.model.system, "never an instruction") {
		t.Error("the system prompt does not treat the description as data")
	}
	if f.res.asked[0] != sharedNS+"/drafter" {
		t.Errorf("provider asked = %v", f.res.asked)
	}
}

func TestBuilderDraft_RolesAndAvailability(t *testing.T) {
	f := newDraftFixture(t)
	for _, role := range []tenancy.Role{tenancy.RoleViewer, tenancy.RoleRunner} {
		if rec := f.serve(f.as(role), http.MethodPost, "/api/builder/draft", draftReq(draftDescription)); rec.Code != http.StatusForbidden {
			t.Errorf("%s = %d", role, rec.Code)
		}
	}
	if rec := f.serve(f.as(tenancy.RoleBuilder), http.MethodGet, "/api/builder/draft", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET = %d", rec.Code)
	}
	// A workspace the caller may not use.
	if rec := f.serve(f.as(tenancy.RoleBuilder), http.MethodPost, "/api/builder/draft", `{"description":"`+draftDescription+`","namespace":"b-only"}`); rec.Code != http.StatusForbidden {
		t.Errorf("foreign namespace = %d", rec.Code)
	}
	if f.model.calls != 0 {
		t.Errorf("the model was called %d times for refused requests", f.model.calls)
	}

	// No configured model: a clear 409 and the options say so.
	f.s.SetBuilder(BuilderConfig{})
	rec := f.serve(f.as(tenancy.RoleBuilder), http.MethodPost, "/api/builder/draft", draftReq(draftDescription))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "draft_unavailable") || !strings.Contains(rec.Body.String(), "draftProvider") {
		t.Errorf("not configured = %d %s", rec.Code, rec.Body)
	}
	opts := decode[builderOptions](t, f.serve(f.as(tenancy.RoleViewer), http.MethodGet, "/api/builder/options", ""))
	if opts.Draft.Available || opts.Draft.Reason == "" {
		t.Errorf("options.draft = %+v", opts.Draft)
	}
	f.s.SetBuilder(BuilderConfig{DraftProvider: "drafter"})
	opts = decode[builderOptions](t, f.serve(f.as(tenancy.RoleViewer), http.MethodGet, "/api/builder/options", ""))
	if !opts.Draft.Available {
		t.Errorf("options.draft = %+v", opts.Draft)
	}
	// A provider that cannot be resolved is a 409 too, without the error text.
	f.res.err = errors.New("secret detail about credentials")
	rec = f.serve(f.as(tenancy.RoleBuilder), http.MethodPost, "/api/builder/draft", draftReq(draftDescription))
	if rec.Code != http.StatusConflict || strings.Contains(rec.Body.String(), "credentials") {
		t.Errorf("unresolvable = %d %s", rec.Code, rec.Body)
	}
}

func TestBuilderDraft_InputCapsAndLimits(t *testing.T) {
	f := newDraftFixture(t)
	post := func(body string) int {
		return f.serve(f.as(tenancy.RoleBuilder), http.MethodPost, "/api/builder/draft", body).Code
	}
	if c := post(draftReq("short")); c != http.StatusBadRequest {
		t.Errorf("too short = %d", c)
	}
	if c := post(draftReq(strings.Repeat("a ", maxDraftDescription))); c != http.StatusRequestEntityTooLarge {
		t.Errorf("too long = %d", c)
	}
	if c := post(`{"description":"` + draftDescription + `","extra":1}`); c != http.StatusBadRequest {
		t.Errorf("unknown request field = %d", c)
	}
	if f.model.calls != 0 {
		t.Fatalf("model called %d times", f.model.calls)
	}

	// A call that could cost more than the cap is refused before it is made.
	f.model.price = 1
	if c := post(draftReq(draftDescription)); c != http.StatusRequestEntityTooLarge || f.model.calls != 0 {
		t.Errorf("over the call cap = %d (calls %d)", c, f.model.calls)
	}
	f.model.price = 0.001

	// The hourly count, per user (a fresh fixture: refused requests count too).
	f = newDraftFixture(t)
	post = func(body string) int {
		return f.serve(f.as(tenancy.RoleBuilder), http.MethodPost, "/api/builder/draft", body).Code
	}
	f.s.SetBuilder(BuilderConfig{DraftProvider: "drafter", DraftPerUserPerHour: 2})
	for i := range 2 {
		if c := post(draftReq(draftDescription)); c != http.StatusOK {
			t.Fatalf("draft %d = %d", i, c)
		}
	}
	rec := f.serve(f.as(tenancy.RoleBuilder), http.MethodPost, "/api/builder/draft", draftReq(draftDescription))
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Errorf("third = %d", rec.Code)
	}
	other := f.as(tenancy.RoleBuilder)
	other.UserID = "judy"
	if rec := f.serve(other, http.MethodPost, "/api/builder/draft", draftReq(draftDescription)); rec.Code != http.StatusOK {
		t.Errorf("another user = %d %s", rec.Code, rec.Body)
	}

	// The daily spend, per user: each call costs $0.4 against a $1 allowance. A call is
	// let in while spent plus its estimate fits, so the allowance can be overrun by one call.
	g := newDraftFixture(t)
	g.s.SetBuilder(BuilderConfig{DraftProvider: "drafter", DraftDailyUSDPerUser: 1, DraftMaxCostUSD: 0.5})
	g.model.cost, g.model.price = 0.4, 0.01
	post = func(body string) int {
		return g.serve(g.as(tenancy.RoleBuilder), http.MethodPost, "/api/builder/draft", body).Code
	}
	for i := range 3 {
		if c := post(draftReq(draftDescription)); c != http.StatusOK {
			t.Fatalf("spend call %d = %d", i, c)
		}
	}
	if c := post(draftReq(draftDescription)); c != http.StatusTooManyRequests {
		t.Errorf("over the daily allowance = %d", c)
	}
}

func TestBuilderDraft_UntrustedReplies(t *testing.T) {
	f := newDraftFixture(t)
	for name, reply := range map[string]string{
		"prose around":   "Sure! " + goodReply,
		"two objects":    goodReply + goodReply,
		"not json":       "{nope}",
		"empty":          `{}`,
		"array":          `[1,2]`,
		"only unknowns":  `{"hack":"yes"}`,
		"fence unclosed": "```json\n" + goodReply,
	} {
		f.model.reply = reply
		if rec := f.serve(f.as(tenancy.RoleBuilder), http.MethodPost, "/api/builder/draft", draftReq(draftDescription)); rec.Code != http.StatusBadGateway {
			t.Errorf("%s = %d %s", name, rec.Code, rec.Body)
		}
	}
	// One code fence is accepted, as for a judge.
	f.model.reply = "```json\n" + goodReply + "\n```"
	if rec := f.serve(f.as(tenancy.RoleBuilder), http.MethodPost, "/api/builder/draft", draftReq(draftDescription)); rec.Code != http.StatusOK {
		t.Errorf("fenced = %d %s", rec.Code, rec.Body)
	}
	// A model failure is a 502 that says nothing about the provider.
	f.model.err = errors.New("401 bad key sk-leak")
	rec := f.serve(f.as(tenancy.RoleBuilder), http.MethodPost, "/api/builder/draft", draftReq(draftDescription))
	if rec.Code != http.StatusBadGateway || strings.Contains(rec.Body.String(), "sk-leak") {
		t.Errorf("model error = %d %s", rec.Code, rec.Body)
	}
}

func TestBuilderDraft_SecretsAreKeptOutAndTheDescriptionIsNotLogged(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })

	f := newDraftFixture(t)
	const apiKey = "sk-abcdefghijklmnopqrstuvwxyz0123456789"
	f.model.reply = `{"name":"x","description":"mail bob@example.com","instructions":"Use the key ` + apiKey + ` and password=hunter2hunter2 to log in."}`
	desc := "An assistant for the Zorblax project that uses my key " + apiKey + " and mails carol@example.com"
	rec := f.serve(f.as(tenancy.RoleBuilder), http.MethodPost, "/api/builder/draft", draftReq(desc))
	if rec.Code != http.StatusOK {
		t.Fatalf("draft = %d %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, secret := range []string{apiKey, "hunter2hunter2", "bob@example.com"} {
		if strings.Contains(body, secret) {
			t.Errorf("the answer holds %q", secret)
		}
	}
	// The key was hidden before the description went to the model.
	if strings.Contains(f.model.user, apiKey) || strings.Contains(f.model.user, "carol@example.com") {
		t.Errorf("the model was sent a secret: %s", f.model.user)
	}
	// Neither the description, nor the reply, nor a secret is in the logs or the audit rows.
	for _, text := range []string{"Zorblax", apiKey, "carol@example.com", "hunter2"} {
		if strings.Contains(logs.String(), text) {
			t.Errorf("the log holds %q: %s", text, logs.String())
		}
		for _, e := range f.audit.events {
			if strings.Contains(e.Summary, text) {
				t.Errorf("the audit row holds %q", text)
			}
		}
	}
	if !strings.Contains(logs.String(), "descriptionBytes") {
		t.Errorf("no log line for the draft: %s", logs.String())
	}
}

func TestBuilderDraft_NameIsFreeAndValid(t *testing.T) {
	f := newDraftFixture(t)
	if err := f.c.Create(context.Background(), &agentorcav1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "refund-helper", Namespace: sharedNS}}); err != nil {
		t.Fatal(err)
	}
	rec := f.serve(f.as(tenancy.RoleBuilder), http.MethodPost, "/api/builder/draft", draftReq(draftDescription))
	if d := decode[draftResponse](t, rec); d.Name != "refund-helper-2" || !d.Valid {
		t.Errorf("name = %q valid=%v errors=%v", d.Name, d.Valid, d.Errors)
	}
	f.model.reply = `{"name":"!!!","instructions":"You help."}`
	if d := decode[draftResponse](t, f.serve(f.as(tenancy.RoleBuilder), http.MethodPost, "/api/builder/draft", draftReq(draftDescription))); d.Name != "new-agent" {
		t.Errorf("fallback name = %q", d.Name)
	}
}

func TestParseDraftIsStrict(t *testing.T) {
	cat := draftCatalog{Tools: []catalogItem{{Name: "t1"}}, KnowledgeBases: []catalogItem{{Name: "kb"}}, Guardrails: []catalogItem{{Name: "g"}}}
	d, err := parseDraft(`{"name":123,"description":["a"],"instructions":"`+strings.Repeat("x", maxDraftInstructions+50)+`\u0000\u0007 ok",
	"exampleInputs":["a\nb","",7,"`+strings.Repeat("z", 500)+`"],"connections":"t1","knowledgeBases":["kb","kb","nope"],"guardrail":"nope"}`, cat)
	if err == nil {
		// A non-list exampleInputs element makes the whole list unusable; the reply is still readable.
		if len(d.Instructions) > maxDraftInstructions || strings.ContainsRune(d.Instructions, 0) || strings.ContainsRune(d.Instructions, 7) {
			t.Errorf("instructions = %d bytes", len(d.Instructions))
		}
		if d.Name != "" || d.Description != "" || len(d.Connections) != 0 || strings.Join(d.KnowledgeBases, ",") != "kb" || d.Guardrail != "" {
			t.Errorf("draft = %+v", d)
		}
		if len(d.Notes) < 4 {
			t.Errorf("notes = %v", d.Notes)
		}
	} else {
		t.Fatal(err)
	}

	// A name with only odd characters and a long note-worthy name are made safe.
	if got := dnsName("  Refund Helper v2 -- ÄÖ "); got != "refund-helper-v2" {
		t.Errorf("dnsName = %q", got)
	}
	if got := dnsName(strings.Repeat("ab-", 30)); len(got) > 40 || strings.HasSuffix(got, "-") {
		t.Errorf("long dnsName = %q", got)
	}
	// A hostile name in a note is clipped and cleaned.
	if s := shown("evil\n\x1b[31m" + strings.Repeat("A", 200)); len(s) > 50 || strings.ContainsAny(s, "\n\x1b") {
		t.Errorf("shown = %q", s)
	}
}

func TestSpendLedger(t *testing.T) {
	var l spendLedger
	now := time.Now()
	l.add("u", 0.5, now.Add(-25*time.Hour))
	l.add("u", 0.25, now.Add(-time.Hour))
	if got := l.total("u", 24*time.Hour, now); got != 0.25 {
		t.Errorf("total = %v", got)
	}
}
