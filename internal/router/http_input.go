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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/floppyfish14/agent-orc/internal/state"
)

// RunHTTPInput implements http input mode: waits for the agent HTTP server to
// become available, POSTs the run input, reads the response, and saves it to
// the state store for the controller to pick up.
func RunHTTPInput(ctx context.Context, cfg HTTPInputConfig, store state.Store) error {
	addr := fmt.Sprintf("localhost:%d", cfg.Port)

	// Poll until the agent's TCP port is open.
	for i := 0; i < 120; i++ {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			conn.Close()
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
		if i == 119 {
			return fmt.Errorf("agent HTTP server at %s did not become available after 120s", addr)
		}
	}

	body, err := json.Marshal(map[string]string{
		"input":  cfg.Input,
		"run_id": cfg.RunName,
	})
	if err != nil {
		return fmt.Errorf("marshalling http input payload: %w", err)
	}

	url := fmt.Sprintf("http://%s%s", addr, cfg.Path)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("building http input request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("posting http input: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading http input response: %w", err)
	}

	// If the response is a JSON object with an "output" key, unwrap it.
	// This allows agents to return {"output": "..."} without the envelope
	// appearing as the run output in the UI.
	output := string(respBody)
	var envelope struct {
		Output string `json:"output"`
	}
	if json.Unmarshal(respBody, &envelope) == nil && envelope.Output != "" {
		output = envelope.Output
	}

	if err := store.SaveHTTPOutput(ctx, cfg.RunName, output); err != nil {
		return fmt.Errorf("saving http output: %w", err)
	}

	return nil
}
