package localdecider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/heddles/agent-orca/internal/dispatch/decision"
)

// words embeds a text as the counts of a few words, so a test can say which
// texts are close.
type words struct {
	mu     sync.Mutex
	calls  []string
	limits map[string]int
	err    error
	gate   chan struct{} // when set, texts other than "input" wait on it
}

var vocabulary = []string{"pod", "invoice", "poem"}

func (w *words) Embed(ctx context.Context, text string, maxTokens int) ([]float32, int, error) {
	w.mu.Lock()
	w.calls = append(w.calls, text)
	if w.limits == nil {
		w.limits = map[string]int{}
	}
	w.limits[text] = maxTokens
	err, gate := w.err, w.gate
	w.mu.Unlock()
	if gate != nil && !strings.HasPrefix(text, "input") {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		}
	}
	v := make([]float64, len(vocabulary)+1)
	v[len(vocabulary)] = 0.1
	for i, word := range vocabulary {
		v[i] = float64(strings.Count(text, word))
	}
	return unitVector(v...), 3, err
}

func (w *words) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.calls)
}

func request(state string, textsByKey map[string]decision.OptionTexts) decision.Request {
	q := decision.Question{Type: decision.TypeChoice, Criteria: map[string]string{}, Texts: textsByKey}
	for k := range textsByKey {
		q.Criteria[k] = k
	}
	return decision.Request{Tenant: "t", State: state, Questions: map[string]decision.Question{"agent": q}}
}

func decide(t *testing.T, d *Decider, req decision.Request) (decision.Response, decision.Answer) {
	t.Helper()
	res, err := d.Decide(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	a, ok := res.ChoiceAnswer("agent", req.Questions["agent"])
	if !ok {
		t.Fatalf("no answer: %+v", res)
	}
	return res, a
}

func many(text string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s %d", text, i)
	}
	return out
}

func TestTheClosestDescriptionDecides(t *testing.T) {
	e := &words{}
	d := New(e, "m", 0)
	res, a := decide(t, d, request("input: my pod", map[string]decision.OptionTexts{
		"ops":     {Descriptions: []string{"writes a poem", "a pod"}},
		"finance": {Descriptions: []string{"an invoice"}},
	}))
	if a.Choice != "ops" || a.Confidence < 0.9 || res.Model != "m/descriptions" || res.Provider != Provider {
		t.Fatalf("got %+v from %s", a, res.Model)
	}
	if got := a.Probabilities["ops"] + a.Probabilities["finance"]; got < 0.999 || got > 1.001 {
		t.Errorf("probabilities sum to %v", got)
	}
	if e.limits["input: my pod"] != DefaultRequestTokens || e.limits["a pod"] != textTokens {
		t.Errorf("limits %v", e.limits)
	}
	if res.Usage.InputTokens != 3 {
		t.Errorf("usage %+v", res.Usage)
	}
}

func TestExamplesAreUsedOnlyWhenEveryOptionHasEnough(t *testing.T) {
	// Described identically, so only the examples can tell them apart.
	options := func(opsExamples, financeExamples int) map[string]decision.OptionTexts {
		return map[string]decision.OptionTexts{
			"ops":     {Descriptions: []string{"helps"}, Examples: many("pod", opsExamples)},
			"finance": {Descriptions: []string{"helps"}, Examples: many("invoice", financeExamples)},
		}
	}
	e := &words{}
	res, a := decide(t, New(e, "m", 0), request("input: invoice", options(20, MinExamples)))
	if res.Model != "m/examples" || a.Choice != "finance" || a.Confidence < 0.8 {
		t.Errorf("with enough on both: %+v from %s", a, res.Model)
	}
	e = &words{}
	res, a = decide(t, New(e, "m", 0), request("input: invoice", options(20, MinExamples-1)))
	if res.Model != "m/descriptions" || a.Confidence > 0.51 {
		t.Errorf("with too few on one: %+v from %s", a, res.Model)
	}
	if i := slices.IndexFunc(e.calls, func(s string) bool { return strings.HasPrefix(s, "pod") }); i >= 0 {
		t.Error("examples that are not used were embedded")
	}
}

func TestPreparationIsCachedPerTenant(t *testing.T) {
	e := &words{}
	d := New(e, "m", 0)
	req := request("input", map[string]decision.OptionTexts{
		"a": {Descriptions: []string{"pod"}}, "b": {Descriptions: []string{"poem"}},
	})
	decide(t, d, req)
	decide(t, d, req)
	if n := e.count(); n != 4 { // two descriptions once, the input twice
		t.Errorf("embedded %d times: %q", n, e.calls)
	}
	req.Tenant = "other"
	decide(t, d, req)
	if n := e.count(); n != 7 {
		t.Errorf("another tenant reused the cache: %d", n)
	}
}

func TestPreparationOutlivesTheCaller(t *testing.T) {
	e := &words{gate: make(chan struct{})}
	d := New(e, "m", 0)
	req := request("input", map[string]decision.OptionTexts{
		"a": {Descriptions: []string{"pod"}}, "b": {Descriptions: []string{"poem"}},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := d.Decide(ctx, req); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	close(e.gate)
	decide(t, d, req)
	if n := e.count(); n != 3 {
		t.Errorf("the work was repeated: %q", e.calls)
	}
}

func TestAFailureIsNotCached(t *testing.T) {
	e := &words{err: errors.New("down")}
	d := New(e, "m", 0)
	req := request("input pod", map[string]decision.OptionTexts{
		"a": {Descriptions: []string{"pod"}}, "b": {Descriptions: []string{"poem"}},
	})
	if _, err := d.Decide(context.Background(), req); err == nil {
		t.Fatal("no error")
	}
	e.mu.Lock()
	e.err = nil
	e.mu.Unlock()
	if _, a := decide(t, d, req); a.Choice != "a" {
		t.Errorf("got %+v", a)
	}
}

func TestOtherQuestionsAndTrivialChoices(t *testing.T) {
	e := &words{}
	res, err := New(e, "m", 0).Decide(context.Background(), decision.Request{State: "x", Questions: map[string]decision.Question{
		"agent": {Type: decision.TypeChoice, Criteria: map[string]string{"only": ""}},
		"can":   {Type: "noul"},
	}})
	if err != nil || res.Answers["agent"].Choice != "only" || res.Answers["can"].Type != decision.TypeRefusal {
		t.Fatalf("got %+v, %v", res, err)
	}
	if e.count() != 0 {
		t.Error("nothing needed embedding")
	}
}

func TestAnOptionWithoutTextsUsesItsCriterion(t *testing.T) {
	_, a := decide(t, New(&words{}, "m", 0), decision.Request{State: "input: poem", Questions: map[string]decision.Question{
		"agent": {Type: decision.TypeChoice, Criteria: map[string]string{"a": "pod", "b": "poem"}},
	}})
	if a.Choice != "b" {
		t.Errorf("got %+v", a)
	}
}

func TestLRUEvictsTheOldest(t *testing.T) {
	c := newLRU[int](2)
	c.put([32]byte{1}, 1)
	c.put([32]byte{2}, 2)
	c.get([32]byte{1})
	c.put([32]byte{3}, 3)
	if _, ok := c.get([32]byte{2}); ok {
		t.Error("the entry used longest ago stayed")
	}
	if v, ok := c.get([32]byte{1}); !ok || v != 1 {
		t.Error("a recently used entry was evicted")
	}
}

func TestLlamaCPPCutsAndNormalises(t *testing.T) {
	var sent []int
	var tokenized string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Content string `json:"content"`
			Input   []int  `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch r.URL.Path {
		case "/tokenize":
			tokenized = body.Content
			_, _ = w.Write([]byte(`{"tokens":[101,1,2,3,4,102]}`))
		case "/v1/embeddings":
			sent = body.Input
			_, _ = w.Write([]byte(`{"data":[{"embedding":[3,4]}]}`))
		}
	}))
	defer srv.Close()
	v, n, err := LlamaCPP{URL: srv.URL + "/"}.Embed(context.Background(), strings.Repeat("é", 1000), 4)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sent, []int{101, 1, 2, 102}) || n != 4 {
		t.Errorf("sent %v (%d): the closing token must stay", sent, n)
	}
	if v[0] != 0.6 || v[1] != 0.8 {
		t.Errorf("got %v", v)
	}
	if len(tokenized) != 4*bytesPerToken {
		t.Errorf("tokenized %d bytes", len(tokenized))
	}
}

func TestLlamaCPPFailuresDoNotQuoteTheInput(t *testing.T) {
	for _, h := range []http.HandlerFunc{
		func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "secret input", http.StatusBadRequest) },
		func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"tokens":[]}`)) },
		func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"tokens":[101,102],"data":[{"embedding":[0,0]}]}`))
		},
	} {
		srv := httptest.NewServer(h)
		_, _, err := LlamaCPP{URL: srv.URL}.Embed(context.Background(), "secret input", 8)
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Errorf("got %v", err)
		}
		srv.Close()
	}
}
