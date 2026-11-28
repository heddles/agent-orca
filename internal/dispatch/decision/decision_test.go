package decision

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func route() Question {
	return Question{Type: TypeChoice, Criteria: map[string]string{"a/triage": "incidents", "a/summary": "summaries"}}
}

func request() Request {
	return Request{State: "the pager went off", Questions: map[string]Question{"agent": route()}}
}

func TestValidate(t *testing.T) {
	if err := request().Validate(); err != nil {
		t.Fatal(err)
	}
	many := map[string]string{}
	for i := range MaxOptions + 1 {
		many[strings.Repeat("k", i+1)] = ""
	}
	for name, mutate := range map[string]func(*Request){
		"empty state":  func(r *Request) { r.State = "" },
		"huge state":   func(r *Request) { r.State = strings.Repeat("a", MaxStateBytes+1) },
		"bad utf8":     func(r *Request) { r.State = "\xff" },
		"no questions": func(r *Request) { r.Questions = nil },
		"untyped":      func(r *Request) { r.Questions = map[string]Question{"q": {}} },
		"no options":   func(r *Request) { r.Questions = map[string]Question{"q": {Type: TypeChoice}} },
		"too many":     func(r *Request) { r.Questions = map[string]Question{"q": {Type: TypeChoice, Criteria: many}} },
		"empty key": func(r *Request) {
			r.Questions = map[string]Question{"q": {Type: TypeChoice, Criteria: map[string]string{"": "x"}}}
		},
		"huge criterion": func(r *Request) { r.Questions["agent"].Criteria["a/triage"] = strings.Repeat("a", MaxCriterionBytes+1) },
	} {
		r := request()
		mutate(&r)
		if err := r.Validate(); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: got %v", name, err)
		}
	}
}

func TestChoiceAnswerAcceptsOnlyWhatWasOffered(t *testing.T) {
	good := Answer{Type: TypeChoice, Choice: "a/triage", Confidence: 0.8, Probabilities: map[string]float64{"a/triage": 0.8, "a/summary": 0.2}}
	if _, ok := (Response{Answers: map[string]Answer{"agent": good}}).ChoiceAnswer("agent", route()); !ok {
		t.Fatal("a well-formed answer was not read")
	}
	bad := 1.5
	for name, mutate := range map[string]func(*Answer){
		"refusal":             func(a *Answer) { *a = Refusal("no") },
		"other type":          func(a *Answer) { a.Type = "noul" },
		"not offered":         func(a *Answer) { a.Choice = "b/admin" },
		"confidence over one": func(a *Answer) { a.Confidence = 1.2 },
		"nan":                 func(a *Answer) { a.Confidence = math.NaN() },
		"bad fit":             func(a *Answer) { a.Fit = &bad },
		"unoffered prob":      func(a *Answer) { a.Probabilities = map[string]float64{"b/admin": 1} },
		"negative prob":       func(a *Answer) { a.Probabilities = map[string]float64{"a/triage": -1} },
	} {
		a := good
		mutate(&a)
		if _, ok := (Response{Answers: map[string]Answer{"agent": a}}).ChoiceAnswer("agent", route()); ok {
			t.Errorf("%s: was read as an answer", name)
		}
	}
	if _, ok := (Response{}).ChoiceAnswer("agent", route()); ok {
		t.Error("a missing answer was read")
	}
}

type stub struct {
	res   Response
	err   error
	delay time.Duration
}

func (s stub) Decide(ctx context.Context, _ Request) (Response, error) {
	select {
	case <-time.After(s.delay):
		return s.res, s.err
	case <-ctx.Done():
		return Response{}, ctx.Err()
	}
}

func TestClientAndHandler(t *testing.T) {
	want := Response{Model: "m", Answers: map[string]Answer{"agent": {Type: TypeChoice, Choice: "a/triage", Confidence: 1}}}
	srv := httptest.NewServer(Handler(stub{res: want}))
	defer srv.Close()
	got, err := Client{BaseURL: srv.URL + "/"}.Decide(context.Background(), request())
	if err != nil {
		t.Fatal(err)
	}
	if a, ok := got.ChoiceAnswer("agent", route()); !ok || a.Choice != "a/triage" || got.Model != "m" {
		t.Fatalf("got %+v", got)
	}
}

func TestClientFailures(t *testing.T) {
	failing := httptest.NewServer(Handler(stub{err: errors.New("secret detail: the pager went off")}))
	defer failing.Close()
	_, err := Client{BaseURL: failing.URL}.Decide(context.Background(), request())
	if err == nil || strings.Contains(err.Error(), "pager") {
		t.Fatalf("want an error that does not quote the service, got %v", err)
	}

	slow := httptest.NewServer(Handler(stub{delay: time.Second}))
	defer slow.Close()
	_, err = Client{BaseURL: slow.URL, Timeout: 20 * time.Millisecond}.Decide(context.Background(), request())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want a deadline error, got %v", err)
	}
}

func TestHandlerRefusesBadRequests(t *testing.T) {
	srv := httptest.NewServer(Handler(stub{}))
	defer srv.Close()
	for body, want := range map[string]int{
		"{":            http.StatusBadRequest,
		`{"state":""}`: http.StatusBadRequest,
		`{"state":"` + strings.Repeat("a", maxBodyBytes) + `"}`: http.StatusRequestEntityTooLarge,
	} {
		res, err := http.Post(srv.URL+Path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != want {
			t.Errorf("%.20q: got %d, want %d", body, res.StatusCode, want)
		}
	}
}
