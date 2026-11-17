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

package router

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// OneShotReply is the answer to CompleteOnce.
type OneShotReply struct {
	// Text is the reply's text, "" when the model answered with nothing.
	Text string
	// Usage is what the provider reported.
	Usage TokenUsage
	// CostUSD is Usage priced on the provider's per-token costs.
	CostUSD float64
}

// ErrNoChoices is returned when a provider's reply holds no choice.
var ErrNoChoices = errors.New("the provider returned no choices")

// CompleteOnce sends one chat completion to a provider with no tools, no
// streaming and no routing, and returns its text, usage and cost. It is the same
// wire path the shadow judge uses (forwardToProviderWithin), for callers that
// have no run and so no router, such as the operator's evaluation judge. The
// provider's key is read from provider.APIKey, else provider.APIKeyFile.
func CompleteOnce(ctx context.Context, provider ProviderConfig, req ChatCompletionRequest, timeout time.Duration) (OneShotReply, error) {
	r := &Router{cfg: &Config{}}
	req.Stream, req.StreamOptions, req.Tools, req.ToolChoice = false, nil, nil, nil
	body, err := r.forwardToProviderWithin(ctx, &provider, req, timeout)
	if err != nil {
		return OneShotReply{}, err
	}
	var resp ChatCompletionResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return OneShotReply{}, err
	}
	out := OneShotReply{Usage: resp.Usage, CostUSD: usageCost(resp.Usage, &provider)}
	if len(resp.Choices) == 0 {
		return out, ErrNoChoices
	}
	out.Text = messageText(resp.Choices[0].Message)
	return out, nil
}

// PriceUSD prices a token count on a provider, as the router does for its calls.
func PriceUSD(provider ProviderConfig, promptTokens, completionTokens int) float64 {
	return usageCost(TokenUsage{PromptTokens: promptTokens, CompletionTokens: completionTokens}, &provider)
}
