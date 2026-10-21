package router

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// roundTripFunc lets a test intercept http.DefaultClient, which the provider
// forwarders use.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// capturedRequest is what an intercepted provider call looked like on the wire.
type capturedRequest struct {
	URL    *url.URL
	Header http.Header
	Body   []byte
}

// captureHTTP swaps http.DefaultTransport for the test, records the request
// sent to a provider, and answers 200 with respBody.
func captureHTTP(t *testing.T, respBody string) *capturedRequest {
	t.Helper()
	captured := &capturedRequest{}
	orig := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		captured.URL = r.URL
		captured.Header = r.Header.Clone()
		captured.Body, _ = io.ReadAll(r.Body)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(respBody)),
			Request:    r,
		}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = orig })
	return captured
}

func anthropicTestRouter(t *testing.T, workspaceID string) (*Router, *ProviderConfig) {
	t.Helper()
	keyFile := filepath.Join(t.TempDir(), "api-key")
	if err := os.WriteFile(keyFile, []byte("sk-ant-test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p := ProviderConfig{
		Name:         "anthropic-claude-sonnet-5-5",
		LiteLLMModel: "anthropic/claude-sonnet-5-5",
		APIKeyFile:   keyFile,
		WorkspaceID:  workspaceID,
	}
	r := &Router{
		cfg:       &Config{Providers: []ProviderConfig{p}, LLMRequestTimeout: time.Minute},
		cancelCtx: ctx,
	}
	return r, &r.cfg.Providers[0]
}

var anthropicOKResponse = `{"id":"msg_1","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`

func chatReqUser() ChatCompletionRequest {
	return ChatCompletionRequest{Messages: []Message{{Role: "user", Content: "hello"}}}
}

// Regression: runs chat-senior-programmer-chat-4nfkg / acp-senior-programmer-p7tp2
// failed over from Anthropic on 400 "must include the anthropic-workspace-id header".
func TestAnthropic_WorkspaceHeader(t *testing.T) {
	tests := []struct {
		name        string
		workspaceID string
		stream      bool
		wantHeader  string
	}{
		{"non-stream with workspace", "wrkspc_123", false, "wrkspc_123"},
		{"stream with workspace", "wrkspc_123", true, "wrkspc_123"},
		{"non-stream without workspace", "", false, ""},
		{"stream without workspace", "", true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := captureHTTP(t, anthropicOKResponse)
			r, p := anthropicTestRouter(t, tt.workspaceID)

			if tt.stream {
				resp, err := r.forwardToProviderStream(context.Background(), p, chatReqUser())
				if err != nil {
					t.Fatal(err)
				}
				_ = resp.Body.Close()
			} else if _, err := r.forwardToProvider(context.Background(), p, chatReqUser()); err != nil {
				t.Fatal(err)
			}

			if got := req.Header.Get(anthropicWorkspaceHeader); got != tt.wantHeader {
				t.Errorf("%s = %q, want %q", anthropicWorkspaceHeader, got, tt.wantHeader)
			}
			if _, present := req.Header[http.CanonicalHeaderKey(anthropicWorkspaceHeader)]; present != (tt.wantHeader != "") {
				t.Errorf("header presence = %v, want %v", present, tt.wantHeader != "")
			}
			if req.Header.Get("x-api-key") != "sk-ant-test" {
				t.Errorf("x-api-key = %q", req.Header.Get("x-api-key"))
			}
			if req.Header.Get("anthropic-version") != anthropicAPIVersion {
				t.Errorf("anthropic-version = %q", req.Header.Get("anthropic-version"))
			}
			if req.URL.String() != anthropicMessagesURL {
				t.Errorf("url = %s", req.URL)
			}
		})
	}
}

// A custom BaseURL means an OpenAI-compatible proxy; Anthropic headers must not leak to it.
func TestAnthropic_WorkspaceHeaderNotSentToBaseURLProxy(t *testing.T) {
	req := captureHTTP(t, `{"choices":[{"message":{"role":"assistant","content":"hi"}}]}`)
	r, p := anthropicTestRouter(t, "wrkspc_123")
	p.BaseURL = "http://proxy.local"
	if _, err := r.forwardToProvider(context.Background(), p, chatReqUser()); err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get(anthropicWorkspaceHeader); got != "" {
		t.Errorf("workspace header leaked to proxy: %q", got)
	}
}

func TestToAnthropicMessages(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: "sys one"},
		{Role: "system", Content: "sys two"},
		{Role: "user", Content: "do it"},
		{Role: "assistant", Content: "ok", ToolCalls: []ToolCall{
			{ID: "t1", Type: "function", Function: FunctionCall{Name: "a", Arguments: `{"x":1}`}},
			{ID: "t2", Type: "function", Function: FunctionCall{Name: "b"}},
		}},
		{Role: "tool", ToolCallID: "t1", Content: "result"},
	}
	system, out := toAnthropicMessages(msgs)

	if strings.Join(system, "|") != "sys one|sys two" {
		t.Errorf("system = %v", system)
	}
	b, _ := json.Marshal(out)
	want := `[{"content":"do it","role":"user"},` +
		`{"content":[{"text":"ok","type":"text"},{"id":"t1","input":{"x":1},"name":"a","type":"tool_use"},` +
		`{"id":"t2","input":{},"name":"b","type":"tool_use"}],"role":"assistant"},` +
		`{"content":[{"content":"result","tool_use_id":"t1","type":"tool_result"}],"role":"user"}]`
	if string(b) != want {
		t.Errorf("messages =\n%s\nwant\n%s", b, want)
	}
}

func TestToAnthropicTools(t *testing.T) {
	got := toAnthropicTools([]map[string]any{
		{"function": map[string]any{"name": "full", "description": "d", "parameters": map[string]any{"type": "object"}}},
		{"function": map[string]any{"name": "bare"}},
		{"type": "not-a-function"},
	})
	b, _ := json.Marshal(got)
	want := `[{"description":"d","input_schema":{"type":"object"},"name":"full"},` +
		`{"input_schema":{"properties":{},"type":"object"},"name":"bare"}]`
	if string(b) != want {
		t.Errorf("tools =\n%s\nwant\n%s", b, want)
	}
}
