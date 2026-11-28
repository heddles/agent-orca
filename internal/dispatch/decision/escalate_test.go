package decision

import (
	"context"
	"errors"
	"testing"
)

type recorder struct {
	res   Response
	err   error
	asked []Request
}

func (r *recorder) Decide(_ context.Context, req Request) (Response, error) {
	r.asked = append(r.asked, req)
	return r.res, r.err
}

func choice(provider, key string, confidence, cost float64) *recorder {
	return &recorder{res: Response{Provider: provider, Usage: Usage{InputTokens: 10, CostUSD: cost},
		Answers: map[string]Answer{"agent": {Type: TypeChoice, Choice: key, Confidence: confidence}}}}
}

func TestASureFirstAnswerIsNotAskedAgain(t *testing.T) {
	first, second := choice("local", "a/triage", 0.8, 0), choice("judge", "a/summary", 1, 1)
	got, err := Escalating{First: first, Second: second, Threshold: 0.8}.Decide(context.Background(), request())
	if err != nil || got.Escalated || got.Provider != "local" || len(second.asked) != 0 {
		t.Fatalf("got %+v, %v, asked %d", got, err, len(second.asked))
	}
}

func TestAnUnsureAnswerIsAskedAgain(t *testing.T) {
	first, second := choice("local", "a/triage", 0.79, 0), choice("judge", "a/summary", 0.95, 0.5)
	got, err := Escalating{First: first, Second: second, Threshold: 0.8}.Decide(context.Background(), request())
	if err != nil || !got.Escalated || got.Provider != "judge" || got.Answers["agent"].Choice != "a/summary" {
		t.Fatalf("got %+v, %v", got, err)
	}
	if got.Usage != (Usage{InputTokens: 20, CostUSD: 0.5}) {
		t.Errorf("usage %+v", got.Usage)
	}
}

func TestTheFirstAnswerStandsWhenTheSecondGivesNone(t *testing.T) {
	for name, second := range map[string]*recorder{
		"error":       {err: errors.New("down"), res: Response{Usage: Usage{CostUSD: 0.5}}},
		"refusal":     {res: Response{Answers: map[string]Answer{"agent": Refusal("no")}, Usage: Usage{CostUSD: 0.5}}},
		"not offered": choice("judge", "b/admin", 1, 0.5),
	} {
		got, err := Escalating{First: choice("local", "a/triage", 0.2, 0), Second: second, Threshold: 0.8}.
			Decide(context.Background(), request())
		if err != nil || got.Escalated || got.Answers["agent"].Choice != "a/triage" || got.Answers["agent"].Confidence != 0.2 {
			t.Errorf("%s: got %+v, %v", name, got, err)
		}
		if got.Usage.CostUSD != 0.5 {
			t.Errorf("%s: a charged call was not counted: %+v", name, got.Usage)
		}
	}
}

func TestAFailedFirstDeciderIsCovered(t *testing.T) {
	broken := &recorder{err: errors.New("local is down")}
	got, err := Escalating{First: broken, Second: choice("judge", "a/summary", 0.9, 0), Threshold: 0.8}.
		Decide(context.Background(), request())
	if err != nil || !got.Escalated || got.Answers["agent"].Choice != "a/summary" {
		t.Fatalf("got %+v, %v", got, err)
	}
	_, err = Escalating{First: broken, Second: &recorder{err: errors.New("judge too")}, Threshold: 0.8}.
		Decide(context.Background(), request())
	if err == nil {
		t.Fatal("neither answered, and no error")
	}
}
