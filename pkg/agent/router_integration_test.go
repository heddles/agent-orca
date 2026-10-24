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

package agent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/heddles/agent-orca/internal/router"
	"github.com/heddles/agent-orca/internal/state"
	"github.com/heddles/agent-orca/pkg/agent"
	"github.com/heddles/agent-orca/pkg/agent/devtools"
)

// TestSDKAgainstRouter runs the SDK with devtools against the real
// model-router, whose LLM is a canned provider: the model writes a file,
// runs a shell command that reads it back, then answers. It checks the whole
// agent-run tool protocol end to end.
func TestSDKAgainstRouter(t *testing.T) {
	turns := []string{
		toolTurn("call_w", "write_file", `{"path":"hello.txt","content":"hi from the sandbox\n"}`),
		toolTurn("call_s", "shell", `{"command":"cat hello.txt && echo done"}`),
		textTurn("The file says hi."),
	}
	var calls atomic.Int32
	var lastBody atomic.Value
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		lastBody.Store(string(body))
		i := int(calls.Add(1)) - 1
		if i >= len(turns) {
			http.Error(w, "no more turns", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, turns[i])
	}))
	t.Cleanup(llm.Close)

	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, []byte("test-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := state.NewStoreFromConfig(state.Config{})
	if err != nil {
		t.Fatal(err)
	}
	r, err := router.New(&router.Config{
		RunName:           "sdk-it",
		RunNamespace:      "default",
		LocalTools:        true,
		LLMRequestTimeout: time.Minute,
		SATokenFile:       keyFile,
		Providers: []router.ProviderConfig{{
			Name: "test", LiteLLMModel: "openai/gpt-4o", BaseURL: llm.URL,
			APIKeyFile: keyFile, ContextWindow: 100000, Weight: 1,
		}},
		Safeguards: router.RouterSafeguards{ToolExecutionTimeoutSec: 60},
	}, store, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", r.HandleChatCompletions)
	routerSrv := httptest.NewServer(mux)
	t.Cleanup(routerSrv.Close)

	workspace := t.TempDir()
	tools, err := devtools.New(workspace, devtools.Options{})
	if err != nil {
		t.Fatal(err)
	}
	a := agent.New(agent.WithBaseURL(routerSrv.URL + "/v1"))
	tools.Register(a)

	res, err := a.Run(context.Background(), "write hello.txt and read it back")
	if err != nil {
		t.Fatal(err)
	}
	if res.Phase != agent.PhaseSucceeded || res.Output != "The file says hi." {
		t.Errorf("result = %+v", res)
	}
	if got, _ := os.ReadFile(filepath.Join(workspace, "hello.txt")); string(got) != "hi from the sandbox\n" {
		t.Errorf("hello.txt = %q", got)
	}
	if n := calls.Load(); n != 3 {
		t.Errorf("LLM called %d times, want 3", n)
	}
	// The last LLM request carries the shell result the SDK produced.
	body, _ := lastBody.Load().(string)
	if !strings.Contains(body, "hi from the sandbox") || !strings.Contains(body, "exit code: 0") {
		t.Errorf("final LLM request lacks the shell result: %s", body)
	}
}

// turn is one canned LLM response: a delta, then the finish reason, then [DONE].
func turn(delta map[string]any, finishReason string) string {
	frame := func(delta map[string]any, finish any) string {
		data, _ := json.Marshal(map[string]any{
			"id": "c", "object": "chat.completion.chunk",
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
		})
		return fmt.Sprintf("data: %s\n\n", data)
	}
	return frame(delta, nil) + frame(map[string]any{}, finishReason) + "data: [DONE]\n\n"
}

func toolTurn(id, name, args string) string {
	return turn(map[string]any{"tool_calls": []any{map[string]any{
		"index": 0, "id": id, "type": "function",
		"function": map[string]any{"name": name, "arguments": args},
	}}}, "tool_calls")
}

func textTurn(text string) string {
	return turn(map[string]any{"content": text}, "stop")
}
