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

package localdecider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"unicode/utf8"
)

// bytesPerToken bounds how much text is tokenized for a limit of tokens. It is
// far above what the model's tokenizer produces for ordinary text, and keeps a
// huge input from being tokenized only to be thrown away.
const bytesPerToken = 20

const maxReplyBytes = 8 << 20

// LlamaCPP embeds text with a llama.cpp server running an embedding model with
// mean pooling.
type LlamaCPP struct {
	URL  string
	HTTP *http.Client
}

// Embed returns the unit vector of text cut to at most maxTokens, and how many
// tokens that was. The text is tokenized first and sent as token ids, because
// the server rejects an input that is too long instead of cutting it.
func (l LlamaCPP) Embed(ctx context.Context, text string, maxTokens int) ([]float32, int, error) {
	var tokens struct {
		Tokens []int `json:"tokens"`
	}
	if err := l.post(ctx, "/tokenize", map[string]any{
		"content": clipBytes(text, maxTokens*bytesPerToken), "add_special": true,
	}, &tokens); err != nil {
		return nil, 0, err
	}
	ids := tokens.Tokens
	if len(ids) == 0 {
		return nil, 0, errors.New("the embedding server returned no tokens")
	}
	if len(ids) > maxTokens {
		ids = append(ids[:maxTokens-1:maxTokens-1], ids[len(ids)-1]) // keep the closing [SEP]
	}
	var reply struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := l.post(ctx, "/v1/embeddings", map[string]any{"input": ids}, &reply); err != nil {
		return nil, 0, err
	}
	if len(reply.Data) != 1 || len(reply.Data[0].Embedding) == 0 {
		return nil, 0, errors.New("the embedding server returned no embedding")
	}
	v := reply.Data[0].Embedding
	var norm float64
	for _, x := range v {
		norm += float64(x) * float64(x)
	}
	if norm == 0 || math.IsNaN(norm) || math.IsInf(norm, 0) {
		return nil, 0, errors.New("the embedding server returned an unusable embedding")
	}
	scale := float32(1 / math.Sqrt(norm))
	for i := range v {
		v[i] *= scale
	}
	return v, len(ids), nil
}

// Healthy reports whether the server is ready to embed.
func (l LlamaCPP) Healthy(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(l.URL, "/")+"/health", nil)
	if err != nil {
		return err
	}
	res, err := l.client().Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("the embedding server answered %d", res.StatusCode)
	}
	return nil
}

func (l LlamaCPP) client() *http.Client {
	if l.HTTP != nil {
		return l.HTTP
	}
	return http.DefaultClient
}

func (l LlamaCPP) post(ctx context.Context, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(l.URL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := l.client().Do(req)
	if err != nil {
		return fmt.Errorf("calling the embedding server: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		// The body is not read into the error: it may quote the input.
		return fmt.Errorf("the embedding server answered %d to %s", res.StatusCode, path)
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, maxReplyBytes)).Decode(out); err != nil {
		return fmt.Errorf("reading the embedding server's reply to %s: %w", path, err)
	}
	return nil
}

func clipBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
