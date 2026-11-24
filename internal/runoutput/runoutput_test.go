package runoutput

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

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

const (
	reasoning   = `{"id":"gen-1","choices":[{"index":0,"delta":{"reasoning":"thinking hard"}}]}`
	otherChoice = `{"id":"gen-1","choices":[{"index":1,"delta":{"content":"alternate"}}]}`
	emptyFrame  = `{"id":"g","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":""}]}`
)

// The same agent output must give the same result whether it arrived as an HTTP
// response (200) or as stdout (0).
func TestInterpret(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		wantText    string
		wantFailure string
	}{
		{"json envelope", `{"output":"the answer"}`, "the answer", ""},
		{"plain text", "just text", "just text", ""},
		{"streamed completion", sseBody(contentFrame("I'm"), contentFrame(" online"), contentFrame("."), "[DONE]"), "I'm online.", ""},
		{"empty chunks then text", sseBody(emptyFrame, emptyFrame, contentFrame("All "), contentFrame("good."), "[DONE]"), "All good.", ""},
		{"reasoning and other choices are not the answer", sseBody(reasoning, contentFrame("answer"), otherChoice, "[DONE]"), "answer", ""},
		{"content after a premature [DONE]", sseBody(reasoning, "[DONE]", contentFrame("real"), contentFrame(" content"), "[DONE]"), "real content", ""},
		{"non-streamed completion", `{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"all good"}}]}`, "all good", ""},
		{"stream with no text", sseBody(emptyFrame, emptyFrame, "[DONE]"), "", "no answer"},
		{"reasoning-only stream", sseBody(reasoning, "[DONE]"), "", "no answer"},
		{"sse error event", sseBody(`{"error":{"message":"upstream error"}}`), "", "upstream error"},
		{"legacy error body", "data: [DONE]\n{\"error\":\"upstream error\"}\n", "", "upstream error"},
		{"plain JSON that mentions an error is an answer", `{"error":"none","ok":true}`, `{"error":"none","ok":true}`, ""},
	}
	for _, status := range []int{200, 0} {
		for _, tt := range tests {
			t.Run(fmt.Sprintf("%d/%s", status, tt.name), func(t *testing.T) {
				got := Interpret(status, []byte(tt.body))
				if tt.wantFailure != "" {
					if !strings.Contains(got.Failure, tt.wantFailure) || strings.Contains(got.Failure, "data:") || got.Text != "" {
						t.Errorf("got %+v, want failure containing %q", got, tt.wantFailure)
					}
					return
				}
				if got.Failure != "" || got.Text != tt.wantText {
					t.Errorf("got %+v, want text %q", got, tt.wantText)
				}
			})
		}
	}
}

func TestInterpretErrorStatus(t *testing.T) {
	got := Interpret(502, []byte(`{"error":{"message":"upstream error"}}`))
	if got.Failure != "agent returned HTTP 502: upstream error" {
		t.Errorf("failure = %q", got.Failure)
	}
	if got := Interpret(500, nil); got.Failure != "agent returned HTTP 500" {
		t.Errorf("failure = %q", got.Failure)
	}
}

func TestStdoutIsInterpretedLikeAResponse(t *testing.T) {
	if got := Stdout(sseBody(emptyFrame, contentFrame("hi"), "[DONE]")); got.Text != "hi" {
		t.Errorf("got %+v", got)
	}
	if got := Stdout("plain\n"); got.Text != "plain\n" || got.Failure != "" {
		t.Errorf("got %+v", got)
	}
}

func TestLooksLikeStream(t *testing.T) {
	for in, want := range map[string]bool{
		"data: {\"id\":\"gen-1\",\"choices\":[]}\n\ndata: {}": true,
		"  data:{\"a\":1}":                   true,
		"The cluster is healthy.":            false,
		"The log says data: {x} was dropped": false,
		"":                                   false,
	} {
		if got := LooksLikeStream(in); got != want {
			t.Errorf("LooksLikeStream(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestClip(t *testing.T) {
	if got := Clip("short"); got != "short" {
		t.Fatalf("short output changed: %q", got)
	}
	exact := strings.Repeat("a", MaxBytes)
	if got := Clip(exact); got != exact {
		t.Fatal("output at exactly the cap must not be clipped")
	}
	got := Clip(strings.Repeat("a", MaxBytes+500))
	if len(got) > MaxBytes || !strings.HasSuffix(got, truncationMarker) {
		t.Fatalf("len=%d tail=%q", len(got), got[len(got)-30:])
	}
	got = Clip(strings.Repeat("é", MaxBytes))
	if !utf8.ValidString(got) || len(got) > MaxBytes {
		t.Fatal("clip split a multi-byte rune or exceeded the cap")
	}
}
