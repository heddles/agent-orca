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

package decision

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Path is where a decision service answers.
const Path = "/v1/decisions"

const (
	// maxBodyBytes bounds a request or response body. It leaves room for the
	// largest valid request, JSON escaping included.
	maxBodyBytes = 6 << 20

	// DefaultTimeout bounds a call to a decision service. A request waits on
	// this before it can start, so it is short.
	DefaultTimeout = 3 * time.Second
)

// Client calls a decision service such as orca-decide.
type Client struct {
	// BaseURL is the service root, without Path.
	BaseURL string
	// HTTP is the client to use; nil means http.DefaultClient.
	HTTP *http.Client
	// Timeout bounds one call; zero means DefaultTimeout.
	Timeout time.Duration
}

var _ Decider = Client{}

// Decide posts req to the service.
func (c Client) Decide(ctx context.Context, req Request) (Response, error) {
	if err := req.Validate(); err != nil {
		return Response{}, err
	}
	body, err := json.Marshal(req)
	if err != nil {
		return Response{}, err
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+Path, bytes.NewReader(body))
	if err != nil {
		return Response{}, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	res, err := hc.Do(hreq)
	if err != nil {
		return Response{}, fmt.Errorf("calling the decision service: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		// The body is not read into the error: it may quote the request.
		return Response{}, fmt.Errorf("the decision service answered %d", res.StatusCode)
	}
	var out Response
	if err := json.NewDecoder(io.LimitReader(res.Body, maxBodyBytes)).Decode(&out); err != nil {
		return Response{}, fmt.Errorf("reading the decision service's answer: %w", err)
	}
	return out, nil
}

// Handler serves d on Path.
func Handler(d Decider) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+Path, func(w http.ResponseWriter, r *http.Request) {
		var req Request
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
		if err := dec.Decode(&req); err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				http.Error(w, "the request is too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "the request is not valid JSON", http.StatusBadRequest)
			return
		}
		if err := req.Validate(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		res, err := d.Decide(r.Context(), req)
		if err != nil {
			slog.Error("deciding", "err", err)
			http.Error(w, "the decision could not be made", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(res); err != nil {
			slog.Warn("writing a decision", "err", err)
		}
	})
	return mux
}
