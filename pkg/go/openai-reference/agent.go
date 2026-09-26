package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"

	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/shared"
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
	w := &streamWriter{w: os.Stdout}
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
		w.Write([]byte("ok"))
	})
	http.HandleFunc("/invoke", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(404)
			return
		}
		var payload map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			w.WriteHeader(400)
			return
		}
		input := ""
		if v, ok := payload["input"].(string); ok {
			input = v
		}
		if err := runOnce(w, input); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": "upstream error"})
			return
		}
	})
	fmt.Printf("reference agent listening on :%s\n", port)
	http.ListenAndServe(":"+port, nil)
}

func runOnce(w http.ResponseWriter, input string) error {
	model, ok := os.LookupEnv("OPENAI_MODEL")
	if !ok || model == "" {
		model = "gpt-4o"
	}

	// Use agent-orca (OpenAI-compatible) via SDK; base URL comes from env
	// injected by the agent-orca framework (openai-compatible tier).
	client := openai.NewClient()
	stream := client.Chat.Completions.NewStreaming(context.TODO(), openai.ChatCompletionNewParams{
		Model: shared.ChatModel(model),
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage(input),
		},
		StreamOptions: openai.ChatCompletionStreamOptionsParam{
			IncludeUsage: openai.Bool(true),
		},
	})
	defer func() { _ = stream.Close() }()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher, ok := w.(http.Flusher)

	for stream.Next() {
		chunk := stream.Current()

		// Write the chunk back to the human client in exact OpenAI schema.
		data, _ := json.Marshal(chunk)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
		if ok {
			flusher.Flush()
		}

		// Also print to stdout for agent logs.
		if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" {
			fmt.Print(chunk.Choices[0].Delta.Content)
		}
	}

	// Terminal [DONE] per OpenAI streaming spec.
	_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
	if ok {
		flusher.Flush()
	}

	return stream.Err()
}
