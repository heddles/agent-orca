package router

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func TestNormalizeSystemMessages(t *testing.T) {
	parts := func(texts ...string) []any {
		out := make([]any, 0, len(texts))
		for _, s := range texts {
			out = append(out, map[string]any{"type": "text", "text": s})
		}
		return out
	}
	tests := []struct {
		name string
		in   []Message
		want []Message
	}{
		{
			name: "multiple leading systems",
			in:   []Message{{Role: "system", Content: "a"}, {Role: "system", Content: "b"}, {Role: "user", Content: "hi"}},
			want: []Message{{Role: "system", Content: "a\n\nb"}, {Role: "user", Content: "hi"}},
		},
		{
			name: "mid-conversation system folds into head",
			in: []Message{{Role: "system", Content: "a"}, {Role: "user", Content: "u1"},
				{Role: "assistant", Content: "r1"}, {Role: "system", Content: "summary"}, {Role: "user", Content: "u2"}},
			want: []Message{{Role: "system", Content: "a\n\nsummary"}, {Role: "user", Content: "u1"},
				{Role: "assistant", Content: "r1"}, {Role: "user", Content: "u2"}},
		},
		{
			name: "system only after user",
			in:   []Message{{Role: "user", Content: "u"}, {Role: "system", Content: "late"}},
			want: []Message{{Role: "system", Content: "late"}, {Role: "user", Content: "u"}},
		},
		{
			name: "developer role",
			in:   []Message{{Role: "developer", Content: "d"}, {Role: "system", Content: "s"}, {Role: "user", Content: "u"}},
			want: []Message{{Role: "system", Content: "d\n\ns"}, {Role: "user", Content: "u"}},
		},
		{
			name: "content part arrays keep text only",
			in: []Message{
				{Role: "system", Content: parts("p1", "p2")},
				{Role: "system", Content: []any{map[string]any{"type": "image_url", "image_url": "x"}, map[string]any{"type": "text", "text": "p3"}}},
				{Role: "user", Content: "u"}},
			want: []Message{{Role: "system", Content: "p1\n\np2\n\np3"}, {Role: "user", Content: "u"}},
		},
		{
			name: "no systems unchanged",
			in:   []Message{{Role: "user", Content: "u"}, {Role: "assistant", Content: "a"}},
			want: []Message{{Role: "user", Content: "u"}, {Role: "assistant", Content: "a"}},
		},
		{
			name: "empty systems skipped",
			in:   []Message{{Role: "system", Content: ""}, {Role: "system", Content: "a"}, {Role: "system", Content: "  "}, {Role: "user", Content: "u"}},
			want: []Message{{Role: "system", Content: "a"}, {Role: "user", Content: "u"}},
		},
		{
			name: "only empty systems removed",
			in:   []Message{{Role: "system", Content: ""}, {Role: "user", Content: "u"}},
			want: []Message{{Role: "user", Content: "u"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before, _ := json.Marshal(tc.in)
			got := normalizeSystemMessages(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %+v\nwant %+v", got, tc.want)
			}
			after, _ := json.Marshal(tc.in)
			if string(before) != string(after) {
				t.Errorf("input mutated:\n%s\n%s", before, after)
			}
		})
	}
}

// capturingUpstream records the last chat-completions body and answers with a
// minimal valid reply for both streaming and non-streaming requests.
func capturingUpstream(t *testing.T) (*httptest.Server, *atomic.Value) {
	t.Helper()
	var last atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		last.Store(b)
		var body struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(b, &body)
		if body.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
			return
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	t.Cleanup(srv.Close)
	return srv, &last
}

type sentMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

// send forwards req through the streaming or non-streaming path and returns
// the messages the upstream received.
func send(t *testing.T, stream bool, req ChatCompletionRequest) []sentMessage {
	t.Helper()
	srv, last := capturingUpstream(t)
	r, p := openAITestRouter(t, srv.URL)
	r.cfg.ToolDefinitions = nil
	return sendVia(t, r, p, stream, req, last)
}

func sendVia(t *testing.T, r *Router, p *ProviderConfig, stream bool, req ChatCompletionRequest, last *atomic.Value) []sentMessage {
	t.Helper()
	if stream {
		resp, err := r.forwardToProviderStream(context.Background(), p, req)
		if err != nil {
			t.Fatal(err)
		}
		_ = readBody(t, resp)
	} else if _, err := r.forwardToProvider(context.Background(), p, req); err != nil {
		t.Fatal(err)
	}
	var body struct {
		Messages []sentMessage `json:"messages"`
	}
	if err := json.Unmarshal(last.Load().([]byte), &body); err != nil {
		t.Fatal(err)
	}
	return body.Messages
}

func assertSingleLeadingSystem(t *testing.T, msgs []sentMessage, wantInOrder ...string) {
	t.Helper()
	if len(msgs) == 0 || msgs[0].Role != "system" {
		t.Fatalf("first message is not system: %+v", msgs)
	}
	for i, m := range msgs[1:] {
		if m.Role == "system" || m.Role == "developer" {
			t.Errorf("extra %s message at index %d", m.Role, i+1)
		}
	}
	text, _ := msgs[0].Content.(string)
	pos := 0
	for _, w := range wantInOrder {
		idx := strings.Index(text[pos:], w)
		if idx < 0 {
			t.Fatalf("system message missing %q (in order) after offset %d:\n%s", w, pos, text)
		}
		pos += idx + len(w)
	}
}

func TestOutboundBody_SingleSystemMessage(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "non-streaming"
		if stream {
			name = "streaming"
		}
		t.Run(name, func(t *testing.T) {
			srv, last := capturingUpstream(t)
			r, p := openAITestRouter(t, srv.URL)
			r.cfg.SystemPrompt = "You are the test agent."
			r.cfg.ToolDefinitions = []ToolDefinition{{Name: "_done", BackendType: "builtin"}, {Name: "_clarify", BackendType: "builtin"}}

			// A resumed prior conversation holding compaction output, plus a turn in progress.
			r.priorMessages = []Message{
				{Role: "system", Content: "PINNED TASK: do the thing"},
				{Role: "system", Content: "SUMMARY: earlier work"},
				{Role: "user", Content: "first question"},
				{Role: "assistant", Content: "first answer"},
				{Role: "system", Content: "CLIENT LATE SYSTEM"},
			}
			r.messages = []Message{
				{Role: "user", Content: "second question"},
				{Role: "system", Content: "<rag-context trust=\"low\">stale</rag-context>"},
			}
			priorBefore, _ := json.Marshal(r.priorMessages)
			curBefore, _ := json.Marshal(r.messages)

			preamble := r.buildPreamble(context.Background(), "")
			if len(preamble) < 2 {
				t.Fatalf("preamble should hold prompt and hint, got %d", len(preamble))
			}
			conv := make([]Message, 0, len(preamble)+len(r.priorMessages)+len(r.messages)+1)
			conv = append(conv, preamble...)
			conv = append(conv, r.priorMessages...)
			conv = append(conv, r.messages...)
			conv = append(conv, Message{Role: "system", Content: hindsightRecallOpen + "recalled\n</hindsight-recall>"})

			got := sendVia(t, r, p, stream, ChatCompletionRequest{Messages: conv}, last)
			assertSingleLeadingSystem(t, got,
				"You are the test agent.", "Platform tools", "PINNED TASK", "SUMMARY", "CLIENT LATE SYSTEM",
				"<rag-context", "<hindsight-recall>", ragTrustBoundaryInstruction)
			roles := make([]string, 0, len(got))
			for _, m := range got[1:] {
				roles = append(roles, m.Role)
			}
			if want := []string{"user", "assistant", "user"}; !reflect.DeepEqual(roles, want) {
				t.Errorf("non-system roles = %v, want %v", roles, want)
			}
			if strings.Count(got[0].Content.(string), "SECURITY: Some content below") != 1 {
				t.Errorf("trust-boundary instruction should appear exactly once")
			}

			priorAfter, _ := json.Marshal(r.priorMessages)
			curAfter, _ := json.Marshal(r.messages)
			if string(priorBefore) != string(priorAfter) || string(curBefore) != string(curAfter) {
				t.Errorf("stored conversation changed")
			}
		})
	}
}

func TestOutboundBody_RAGTrustBoundaryOnStreaming(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{"with rag context", "<rag-context trust=\"low\">doc</rag-context>", true},
		{"without rag context", "plain", false},
	}
	for _, stream := range []bool{false, true} {
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				got := send(t, stream, ChatCompletionRequest{Messages: []Message{
					{Role: "system", Content: "base"},
					{Role: "system", Content: tc.content},
					{Role: "user", Content: "hi"},
				}})
				text, _ := got[0].Content.(string)
				if has := strings.Contains(text, ragTrustBoundaryInstruction); has != tc.want {
					t.Errorf("stream=%v hint present = %v, want %v", stream, has, tc.want)
				}
				assertSingleLeadingSystem(t, got, "base", tc.content)
			})
		}
	}
}
