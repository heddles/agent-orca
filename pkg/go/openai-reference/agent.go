package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"

	openai "github.com/openai/openai-go/v3"
)

func main() {
	if _, ok := os.LookupEnv("AGENTORC_INPUT"); ok {
		envMode()
		return
	}
	httpMode()
}

func envMode() {
	inp := os.Getenv("AGENTORC_INPUT")
	if inp == "" {
		fmt.Println("No AGENTORC_INPUT set")
		os.Exit(1)
	}
	// Stream to stdout in OpenAI SSE format for CLI clients.
	w := &sseWriter{ResponseWriter: &streamWriter{w: os.Stdout}}
	if err := runOnce(w, inp); err != nil {
		fmt.Printf("\nError: %v\n", err)
		os.Exit(1)
	}
}

type streamWriter struct {
	w io.Writer
}

func (s *streamWriter) Header() http.Header         { return http.Header{} }
func (s *streamWriter) WriteHeader(int)             {}
func (s *streamWriter) Write(p []byte) (int, error) { return s.w.Write(p) }
func (s *streamWriter) Flush()                      {}

func httpMode() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8000"
	}
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("ok"))
	})
	http.HandleFunc("/invoke", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(404)
			return
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			w.WriteHeader(400)
			return
		}
		input := ""
		if v, ok := payload["input"].(string); ok {
			input = v
		}
		out := &sseWriter{ResponseWriter: w}
		if err := runOnce(out, input); err != nil {
			slog.Error("run failed", "err", err)
			// Once the SSE response has begun, runOnce already ended it with an
			// error event; a second WriteHeader would be superfluous.
			if !out.begun {
				writeErrorJSON(w, http.StatusBadGateway, err)
			}
		}
	})
	fmt.Printf("reference agent listening on :%s\n", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		slog.Error("reference agent server exited", "err", err)
		os.Exit(1)
	}
}

// sseWriter tracks whether the SSE response has begun, so an error is reported
// as an HTTP status before that and as an SSE error event after it.
type sseWriter struct {
	http.ResponseWriter
	begun bool
}

func (s *sseWriter) begin() {
	if s.begun {
		return
	}
	s.begun = true
	s.Header().Set("Content-Type", "text/event-stream")
	s.Header().Set("Cache-Control", "no-cache")
	s.Header().Set("Connection", "keep-alive")
	s.WriteHeader(http.StatusOK)
}

func (s *sseWriter) flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func writeErrorJSON(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"message": err.Error(), "type": "upstream_error"},
	})
}

// runOnce streams one completion to w. A failure before the first chunk is
// returned with nothing written, so the caller can answer with an error status;
// a failure after it ends the stream with an SSE error event and no [DONE].
func runOnce(out *sseWriter, input string) error {
	model, ok := os.LookupEnv("OPENAI_MODEL")
	if !ok || model == "" {
		model = "gpt-4o"
	}

	// Use agent-orca (OpenAI-compatible) via SDK; base URL comes from env
	// injected by the agent-orca framework (openai-compatible tier).
	client := openai.NewClient()
	stream := client.Chat.Completions.NewStreaming(context.TODO(), openai.ChatCompletionNewParams{
		Model: model,
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage(input),
		},
		StreamOptions: openai.ChatCompletionStreamOptionsParam{
			IncludeUsage: openai.Bool(true),
		},
	})
	defer func() { _ = stream.Close() }()

	for stream.Next() {
		chunk := stream.Current()
		out.begin()

		// Write the chunk back to the human client in exact OpenAI schema.
		data, _ := json.Marshal(chunk)
		_, _ = fmt.Fprintf(out, "data: %s\n\n", data)
		out.flush()

		// Also print to stdout for agent logs.
		if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" {
			fmt.Print(chunk.Choices[0].Delta.Content)
		}
	}

	if err := stream.Err(); err != nil {
		if out.begun {
			errData, _ := json.Marshal(map[string]any{
				"error": map[string]string{"message": err.Error(), "type": "upstream_error"},
			})
			_, _ = fmt.Fprintf(out, "data: %s\n\n", errData)
			out.flush()
		}
		return err
	}

	// Terminal [DONE] per OpenAI streaming spec.
	out.begin()
	_, _ = fmt.Fprintf(out, "data: [DONE]\n\n")
	out.flush()
	return nil
}
