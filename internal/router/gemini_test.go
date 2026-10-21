package router

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const realSig = `{"google":{"thought_signature":"REAL-SIG"}}`

func geminiProvider() *ProviderConfig {
	return &ProviderConfig{Name: "google-gemini-3-8-flash", LiteLLMModel: "gemini/gemini-3.8-flash"}
}

func toolMsg(calls ...ToolCall) Message {
	return Message{Role: "assistant", ToolCalls: calls}
}

func TestAdaptToolCallsForProvider_Gemini(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: "go"},
		toolMsg(
			ToolCall{ID: "signed", Function: FunctionCall{Name: "a"}, ExtraContent: json.RawMessage(realSig)},
			ToolCall{ID: "unsigned", Function: FunctionCall{Name: "b"}},
		),
		{Role: "tool", ToolCallID: "signed", Content: "r"},
	}
	orig, _ := json.Marshal(msgs)

	got := adaptToolCallsForProvider(geminiProvider(), msgs)

	if string(got[1].ToolCalls[0].ExtraContent) != realSig {
		t.Errorf("real signature not preserved: %s", got[1].ToolCalls[0].ExtraContent)
	}
	if !strings.Contains(string(got[1].ToolCalls[1].ExtraContent), geminiSkipThoughtSignature) {
		t.Errorf("unsigned call not given placeholder: %s", got[1].ToolCalls[1].ExtraContent)
	}
	if after, _ := json.Marshal(msgs); string(after) != string(orig) {
		t.Errorf("input conversation was mutated:\n%s\n%s", orig, after)
	}
}

func TestAdaptToolCallsForProvider_NonGeminiStripsExtraContent(t *testing.T) {
	msgs := []Message{toolMsg(ToolCall{ID: "x", ExtraContent: json.RawMessage(realSig)})}
	got := adaptToolCallsForProvider(&ProviderConfig{LiteLLMModel: "openai/gpt-6-luna"}, msgs)
	if len(got[0].ToolCalls[0].ExtraContent) != 0 {
		t.Errorf("extra_content leaked to non-Gemini provider: %s", got[0].ToolCalls[0].ExtraContent)
	}
	if len(msgs[0].ToolCalls[0].ExtraContent) == 0 {
		t.Error("stored conversation lost its signature")
	}
}

func TestAdaptToolCallsForProvider_NoOpReturnsSameSlice(t *testing.T) {
	msgs := []Message{{Role: "user", Content: "hi"}, toolMsg(ToolCall{ID: "x", ExtraContent: json.RawMessage(realSig)})}
	got := adaptToolCallsForProvider(geminiProvider(), msgs)
	if &got[0] != &msgs[0] {
		t.Error("expected no copy when nothing needs changing")
	}
}

func TestToolCall_ExtraContentJSONRoundTrip(t *testing.T) {
	// Shape of a Gemini OpenAI-compat non-streaming response.
	in := `{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"},"extra_content":` + realSig + `}`
	var tc ToolCall
	if err := json.Unmarshal([]byte(in), &tc); err != nil {
		t.Fatal(err)
	}
	if string(tc.ExtraContent) != realSig {
		t.Fatalf("ExtraContent = %s", tc.ExtraContent)
	}
	out, _ := json.Marshal(tc)
	if !strings.Contains(string(out), `"extra_content":`+realSig) {
		t.Errorf("not re-serialized: %s", out)
	}
	// Absent -> omitted, so other providers see an unchanged payload.
	out, _ = json.Marshal(ToolCall{ID: "c2"})
	if strings.Contains(string(out), "extra_content") {
		t.Errorf("empty extra_content serialized: %s", out)
	}
}

func TestMergeToolCallDeltas_PreservesExtraContent(t *testing.T) {
	merged := mergeToolCallDeltas(nil, []ToolCall{{Index: 0, ID: "c1", Type: "function",
		Function: FunctionCall{Name: "f", Arguments: `{"a":`}, ExtraContent: json.RawMessage(realSig)}})
	// Later argument-only chunks carry no extra_content and must not erase it.
	merged = mergeToolCallDeltas(merged, []ToolCall{{Index: 0, Function: FunctionCall{Arguments: `1}`}}})
	if string(merged[0].ExtraContent) != realSig {
		t.Errorf("signature lost across chunks: %s", merged[0].ExtraContent)
	}
	if merged[0].Function.Arguments != `{"a":1}` {
		t.Errorf("arguments = %s", merged[0].Function.Arguments)
	}

	// Signature arriving on a later chunk for the same call is also kept.
	late := mergeToolCallDeltas(nil, []ToolCall{{Index: 0, ID: "c1", Function: FunctionCall{Name: "f"}}})
	late = mergeToolCallDeltas(late, []ToolCall{{Index: 0, ExtraContent: json.RawMessage(realSig)}})
	if string(late[0].ExtraContent) != realSig {
		t.Errorf("late signature dropped: %s", late[0].ExtraContent)
	}
}

// Regression: run chat-senior-programmer-chat-4nfkg got HTTP 400 from Gemini
// "Function call is missing a thought_signature in functionCall parts"
// (`_search_history`, position 2) when the history held tool calls without one.
func TestForwardToProvider_GeminiRequestCarriesThoughtSignatures(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "api-key")
	if err := os.WriteFile(keyFile, []byte("g-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := geminiProvider()
	p.APIKeyFile = keyFile
	ctx := t.Context()
	cfg := &Config{Providers: []ProviderConfig{*p}, LLMRequestTimeout: time.Minute}
	r := &Router{cfg: cfg, cancelCtx: ctx, ruleRouter: NewRuleRouter(cfg)}

	history := []Message{
		{Role: "user", Content: "go"},
		toolMsg(ToolCall{ID: "from-glm", Type: "function", Function: FunctionCall{Name: "_search_history", Arguments: "{}"}}),
		{Role: "tool", ToolCallID: "from-glm", Content: "r"},
		toolMsg(ToolCall{ID: "from-gemini", Type: "function", Function: FunctionCall{Name: "f", Arguments: "{}"}, ExtraContent: json.RawMessage(realSig)}),
		{Role: "tool", ToolCallID: "from-gemini", Content: "r"},
	}

	for _, stream := range []bool{false, true} {
		req := captureHTTP(t, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
		if stream {
			resp, err := r.forwardToProviderStream(context.Background(), p, ChatCompletionRequest{Messages: history})
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
		} else if _, err := r.forwardToProvider(context.Background(), p, ChatCompletionRequest{Messages: history}); err != nil {
			t.Fatal(err)
		}

		var body struct {
			Messages []Message `json:"messages"`
		}
		if err := json.Unmarshal(req.Body, &body); err != nil {
			t.Fatal(err)
		}
		if got := string(body.Messages[1].ToolCalls[0].ExtraContent); !strings.Contains(got, geminiSkipThoughtSignature) {
			t.Errorf("stream=%v: unsigned call sent as %q", stream, got)
		}
		if got := string(body.Messages[3].ToolCalls[0].ExtraContent); got != realSig {
			t.Errorf("stream=%v: real signature sent as %q", stream, got)
		}
	}
}

// Regression: Gemini streams parallel tool calls without an "index" (all 0) but
// with distinct IDs. They used to merge into one call named after the last and
// carrying both calls' JSON arguments concatenated, which Gemini then rejected
// with "Request contains an invalid argument" on the next turn.
func TestMergeToolCallDeltas_GeminiParallelCallsWithoutIndex(t *testing.T) {
	var merged []ToolCall
	merged = mergeToolCallDeltas(merged, []ToolCall{{ID: "call_1", Type: "function",
		Function: FunctionCall{Name: "get_weather", Arguments: `{"city":"Paris"}`}, ExtraContent: json.RawMessage(realSig)}})
	merged = mergeToolCallDeltas(merged, []ToolCall{{ID: "call_2", Type: "function",
		Function: FunctionCall{Name: "get_time", Arguments: `{"city":"Tokyo"}`}}})

	if len(merged) != 2 {
		t.Fatalf("got %d calls, want 2: %+v", len(merged), merged)
	}
	if merged[0].Function.Name != "get_weather" || merged[0].Function.Arguments != `{"city":"Paris"}` || string(merged[0].ExtraContent) != realSig {
		t.Errorf("first call corrupted: %+v", merged[0])
	}
	if merged[1].ID != "call_2" || merged[1].Function.Name != "get_time" || merged[1].Function.Arguments != `{"city":"Tokyo"}` {
		t.Errorf("second call wrong: %+v", merged[1])
	}
	if merged[0].Index == merged[1].Index {
		t.Errorf("calls share index %d", merged[0].Index)
	}
}

// OpenAI-style streams repeat the same ID (or none) on continuation chunks; those must still merge.
func TestMergeToolCallDeltas_SameIDContinuationStillMerges(t *testing.T) {
	merged := mergeToolCallDeltas(nil, []ToolCall{{Index: 0, ID: "c", Function: FunctionCall{Name: "f", Arguments: `{"a"`}}})
	merged = mergeToolCallDeltas(merged, []ToolCall{{Index: 0, ID: "c", Function: FunctionCall{Arguments: `:1}`}}})
	merged = mergeToolCallDeltas(merged, []ToolCall{{Index: 1, ID: "d", Function: FunctionCall{Name: "g", Arguments: `{}`}}})
	if len(merged) != 2 || merged[0].Function.Arguments != `{"a":1}` || merged[1].Function.Name != "g" {
		t.Errorf("unexpected merge: %+v", merged)
	}
}
