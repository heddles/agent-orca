package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/heddles/agent-orca/internal/dispatch/decision"
)

type fixed struct{}

func (fixed) Decide(context.Context, decision.Request) (decision.Response, error) {
	answer := decision.Answer{Type: decision.TypeChoice, Choice: "a", Confidence: 1}
	return decision.Response{Answers: map[string]decision.Answer{"agent": answer}}, nil
}

func TestHandlerServesDecisionsAndProbes(t *testing.T) {
	var notReady error
	srv := httptest.NewServer(handler(fixed{}, func(context.Context) error { return notReady }))
	defer srv.Close()

	question := decision.Question{Type: decision.TypeChoice, Criteria: map[string]string{"a": ""}}
	res, err := decision.Client{BaseURL: srv.URL}.Decide(context.Background(), decision.Request{
		State: "x", Questions: map[string]decision.Question{"agent": question},
	})
	if err != nil || res.Answers["agent"].Choice != "a" {
		t.Fatalf("got %+v, %v", res, err)
	}
	status := func(path string) int {
		r, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = r.Body.Close() }()
		return r.StatusCode
	}
	if status("/healthz") != 200 || status("/readyz") != 200 {
		t.Error("a healthy service did not say so")
	}
	notReady = errors.New("loading")
	if status("/healthz") != 200 || status("/readyz") != 503 {
		t.Error("an embedding server that is loading must only fail readiness")
	}
}
