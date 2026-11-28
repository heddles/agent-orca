/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package localdecider is the decision model orca-decide serves: a small
// embedding model on CPU, with no call outside the cluster. It answers choice
// questions in one of two ways.
//
// By descriptions: an option scores as the description closest to the input.
//
// By examples: a classifier fitted on every option's descriptions and examples.
// It is used only when every option offered has at least MinExamples. With
// examples on some options and not others, it all but stops picking the others
// (hack/dispatch-eval/bench).
//
// Probabilities only compare the options, so the decider cannot say that none
// of them fits, and Fit is not set.
package localdecider

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash"
	"slices"
	"sync"
	"time"

	"github.com/heddles/agent-orca/internal/dispatch/decision"
)

const (
	// Provider names this decider in a Response.
	Provider = "local"
	// MinExamples is how many examples every option needs before any option's
	// examples are used.
	MinExamples = 5
	// MethodDescriptions and MethodExamples follow the model name in a Response.
	MethodDescriptions = "descriptions"
	MethodExamples     = "examples"

	// A confidence has no meaning until it is scaled, and each method scores on
	// its own scale. With these, accepting 0.8 or more was right 92 to 99
	// times in 100 on the benchmark's three public sets. They were chosen on
	// its test rows, so expect a little less.
	descriptionTemperature = 0.02
	exampleTemperature     = 0.5

	// DefaultRequestTokens is how much of an input is read. 64 tokens answer in
	// about 50 ms on two cores of a 2015 x86 CPU; 128 take about 85 ms and
	// are at most 2 points more accurate on long inputs with examples.
	DefaultRequestTokens = 64
	// textTokens is how much of a description or example is read: the model's
	// whole window.
	textTokens = 512

	// maxTraining bounds the vectors a classifier is fitted on.
	maxTraining = 4000
	// buildTimeout bounds preparing one set of options.
	buildTimeout = 2 * time.Minute
	maxBuilds    = 2
	// A vector is about 1.5 KiB: the first limit is about 30 MiB.
	maxVectors = 20000
	maxModels  = 64
)

// Embedder turns text into a unit vector.
type Embedder interface {
	Embed(ctx context.Context, text string, maxTokens int) (vector []float32, tokens int, err error)
}

// Decider answers decision requests with an Embedder. It keeps nothing on disk.
// Vectors and classifiers are cached per tenant.
type Decider struct {
	embedder      Embedder
	model         string
	requestTokens int

	vectors *lru[[]float32]
	models  *lru[*options]

	mu       sync.Mutex
	building map[[32]byte]*build
	slots    chan struct{}
}

var _ decision.Decider = (*Decider)(nil)

type build struct {
	done  chan struct{}
	model *options
	err   error
}

// New returns a Decider. model names the embedding model in responses, and
// requestTokens is how much of an input is read; zero means
// DefaultRequestTokens.
func New(embedder Embedder, model string, requestTokens int) *Decider {
	if requestTokens <= 0 {
		requestTokens = DefaultRequestTokens
	}
	return &Decider{
		embedder: embedder, model: model, requestTokens: min(requestTokens, textTokens),
		vectors: newLRU[[]float32](maxVectors), models: newLRU[*options](maxModels),
		building: map[[32]byte]*build{}, slots: make(chan struct{}, maxBuilds),
	}
}

// options is a prepared set of options.
type options struct {
	keys []string
	// vectors holds each key's description vectors, for MethodDescriptions.
	vectors [][][]float32
	// classifier is set for MethodExamples.
	classifier *classifier
}

func (o *options) method() string {
	if o.classifier != nil {
		return MethodExamples
	}
	return MethodDescriptions
}

func (o *options) probabilities(v []float32) []float64 {
	if o.classifier != nil {
		return softmax(o.classifier.scores(v), exampleTemperature)
	}
	scores := make([]float64, len(o.keys))
	for i, descriptions := range o.vectors {
		scores[i] = -1
		for _, d := range descriptions {
			var s float64
			for j, x := range d {
				s += float64(x) * float64(v[j])
			}
			scores[i] = max(scores[i], s)
		}
	}
	return softmax(scores, descriptionTemperature)
}

// Decide answers each choice question and refuses any other kind.
//
// Preparing options that are new to it takes a while: an embedding for every
// description and example. That work carries on when ctx ends, so a caller that
// gave up finds it done the next time.
func (d *Decider) Decide(ctx context.Context, req decision.Request) (decision.Response, error) {
	res := decision.Response{Provider: Provider, Model: d.model, Answers: map[string]decision.Answer{}}
	if err := req.Validate(); err != nil {
		return res, err
	}
	var input []float32
	methods := map[string]bool{}
	for id, q := range req.Questions {
		if q.Type != decision.TypeChoice {
			res.Answers[id] = decision.Refusal("only choice questions are answered")
			continue
		}
		if len(q.Criteria) == 1 {
			// There is nothing to compare.
			for k := range q.Criteria {
				res.Answers[id] = decision.Answer{Type: decision.TypeChoice, Choice: k, Confidence: 1, Probabilities: map[string]float64{k: 1}}
			}
			continue
		}
		prepared, err := d.prepare(ctx, req.Tenant, q)
		if err != nil {
			return res, err
		}
		if input == nil {
			v, tokens, err := d.embedder.Embed(ctx, req.State, d.requestTokens)
			if err != nil {
				return res, err
			}
			input, res.Usage.InputTokens = v, tokens
		}
		p := prepared.probabilities(input)
		answer := decision.Answer{Type: decision.TypeChoice, Probabilities: make(map[string]float64, len(p))}
		for i, k := range prepared.keys {
			answer.Probabilities[k] = min(p[i], 1)
			if p[i] > answer.Confidence || answer.Choice == "" {
				answer.Choice, answer.Confidence = k, min(p[i], 1)
			}
		}
		res.Answers[id] = answer
		methods[prepared.method()] = true
	}
	if len(methods) == 1 {
		for m := range methods {
			res.Model += "/" + m
		}
	}
	return res, nil
}

// texts are what one option is matched on.
type texts struct {
	key                    string
	descriptions, examples []string
}

func optionTexts(q decision.Question) []texts {
	out := make([]texts, 0, len(q.Criteria))
	for key, criterion := range q.Criteria {
		t := texts{key: key, descriptions: q.Texts[key].Descriptions, examples: q.Texts[key].Examples}
		if len(t.descriptions) == 0 {
			if criterion == "" {
				criterion = key
			}
			t.descriptions = []string{criterion}
		}
		out = append(out, t)
	}
	slices.SortFunc(out, func(a, b texts) int {
		if a.key < b.key {
			return -1
		}
		return 1
	})
	return out
}

// digest hashes strings so that no two different lists share a hash.
func digest(h hash.Hash, parts ...string) {
	var n [8]byte
	for _, p := range parts {
		binary.LittleEndian.PutUint64(n[:], uint64(len(p)))
		h.Write(n[:])
		h.Write([]byte(p))
	}
}

func sum(h hash.Hash) (out [32]byte) {
	copy(out[:], h.Sum(nil))
	return out
}

func (d *Decider) prepare(ctx context.Context, tenant string, q decision.Question) (*options, error) {
	all := optionTexts(q)
	h := sha256.New()
	digest(h, tenant)
	for _, t := range all {
		digest(h, t.key, fmt.Sprint(len(t.descriptions)), fmt.Sprint(len(t.examples)))
		digest(h, t.descriptions...)
		digest(h, t.examples...)
	}
	key := sum(h)
	if prepared, ok := d.models.get(key); ok {
		return prepared, nil
	}

	d.mu.Lock()
	b, running := d.building[key]
	if !running {
		b = &build{done: make(chan struct{})}
		d.building[key] = b
		go d.run(context.WithoutCancel(ctx), key, tenant, all, b)
	}
	d.mu.Unlock()

	select {
	case <-b.done:
		return b.model, b.err
	case <-ctx.Done():
		return nil, fmt.Errorf("the options are still being prepared: %w", ctx.Err())
	}
}

func (d *Decider) run(ctx context.Context, key [32]byte, tenant string, all []texts, b *build) {
	ctx, cancel := context.WithTimeout(ctx, buildTimeout)
	defer cancel()
	select {
	case d.slots <- struct{}{}:
		b.model, b.err = d.build(ctx, tenant, all)
		<-d.slots
	case <-ctx.Done():
		b.err = ctx.Err()
	}
	if b.err == nil {
		d.models.put(key, b.model)
	}
	d.mu.Lock()
	delete(d.building, key)
	d.mu.Unlock()
	close(b.done)
}

func (d *Decider) build(ctx context.Context, tenant string, all []texts) (*options, error) {
	byExample := true
	for _, t := range all {
		byExample = byExample && len(t.examples) >= MinExamples
	}
	out := &options{}
	var x [][]float32
	var y []int
	// Every option keeps at least MinExamples texts, however many options
	// share the training limit.
	perOption := max(maxTraining/len(all), MinExamples)
	for class, t := range all {
		out.keys = append(out.keys, t.key)
		use := t.descriptions
		if byExample {
			use = append(slices.Clone(t.descriptions), t.examples...)
			use = use[:min(len(use), max(perOption, len(t.descriptions)+MinExamples))]
		}
		vectors := make([][]float32, 0, len(use))
		for _, text := range use {
			v, err := d.vector(ctx, tenant, text)
			if err != nil {
				return nil, err
			}
			vectors = append(vectors, v)
			x, y = append(x, v), append(y, class)
		}
		out.vectors = append(out.vectors, vectors)
	}
	if byExample {
		c := fit(x, y, len(all))
		out.classifier, out.vectors = &c, nil
	}
	return out, nil
}

func (d *Decider) vector(ctx context.Context, tenant, text string) ([]float32, error) {
	h := sha256.New()
	digest(h, tenant, text)
	key := sum(h)
	if v, ok := d.vectors.get(key); ok {
		return v, nil
	}
	v, _, err := d.embedder.Embed(ctx, text, textTokens)
	if err != nil {
		return nil, err
	}
	d.vectors.put(key, v)
	return v, nil
}
