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
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/heddles/agent-orca/internal/state"
)

const eventStream = "text/event-stream"

// sseBody builds a streamed chat completion body from raw data payloads.
func sseBody(frames ...string) string {
	var sb strings.Builder
	for _, f := range frames {
		sb.WriteString("data: " + f + "\n\n")
	}
	return sb.String()
}

func contentFrame(s string) string {
	return fmt.Sprintf(`{"id":"gen-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":%q}}]}`, s)
}

func TestHTTPRunOutput(t *testing.T) {
	reasoning := `{"id":"gen-1","choices":[{"index":0,"delta":{"reasoning":"thinking hard"}}]}`
	otherChoice := `{"id":"gen-1","choices":[{"index":1,"delta":{"content":"alternate"}}]}`
	tests := []struct {
		name        string
		contentType string
		body        string
		want        string
	}{
		{"json envelope", "application/json", `{"output":"the answer"}`, "the answer"},
		{"plain text", "text/plain", "just text", "just text"},
		{
			"streamed completion (#112)", eventStream,
			sseBody(contentFrame("I'm"), contentFrame(" online"), contentFrame("."), "[DONE]"),
			"I'm online.",
		},
		{
			"reasoning and other choices are not the answer", eventStream,
			sseBody(reasoning, contentFrame("answer"), otherChoice, "[DONE]"),
			"answer",
		},
		{
			"content after a premature [DONE] is kept", eventStream,
			sseBody(reasoning, "[DONE]", contentFrame("real"), contentFrame(" content"), "[DONE]"),
			"real content",
		},
		{
			"stream detected without a content type", "",
			sseBody(contentFrame("hello"), "[DONE]"),
			"hello",
		},
		{
			"stream with no content keeps the raw body", eventStream,
			sseBody(reasoning, "[DONE]"),
			sseBody(reasoning, "[DONE]"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := httpRunOutput(tt.contentType, []byte(tt.body)); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// outputStore records the http output RunHTTPInput saves.
type outputStore struct {
	state.Store
	output string
}

func (s *outputStore) SaveHTTPOutput(_ context.Context, _ string, output string) error {
	s.output = output
	return nil
}

// TestRunHTTPInputSavesStreamedAnswer drives RunHTTPInput against an agent
// that answers with a streamed chat completion, as senior-programmer does.
func TestRunHTTPInputSavesStreamedAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", eventStream)
		_, _ = fmt.Fprint(w, sseBody(contentFrame("I'm"), contentFrame(" online."), "[DONE]"))
	}))
	t.Cleanup(srv.Close)
	port := srv.Listener.Addr().(*net.TCPAddr).Port

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store := &outputStore{Store: newTestStore(t)}
	if err := RunHTTPInput(ctx, HTTPInputConfig{Port: port, Path: "/invoke", RunName: "r", Input: "hi"}, store); err != nil {
		t.Fatal(err)
	}
	if store.output != "I'm online." {
		t.Fatalf("saved output %q, want %q", store.output, "I'm online.")
	}
}
