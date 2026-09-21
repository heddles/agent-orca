package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	usage, err := runOnce(inp)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Tokens: input=%d output=%d\n", usage.PromptTokens, usage.CompletionTokens)
}

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
		usage, err := runOnce(input)
		if err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": "upstream error"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"usage": map[string]interface{}{
				"input_tokens":  usage.PromptTokens,
				"output_tokens": usage.CompletionTokens,
			},
		})
	})
	fmt.Printf("reference agent listening on :%s\n", port)
	http.ListenAndServe(":"+port, nil)
}

func runOnce(input string) (openai.CompletionUsage, error) {
	model, ok := os.LookupEnv("OPENAI_MODEL")
	if !ok || model == "" {
		model = "gpt-4o"
	}
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

	var completeText string
	var output bytes.Buffer
	var usage openai.CompletionUsage
	for stream.Next() {
		chunk := stream.Current()
		if len(chunk.Choices) > 0 {
			if chunk.Choices[0].Delta.Content != "" {
				output.WriteString(chunk.Choices[0].Delta.Content)
				fmt.Print(chunk.Choices[0].Delta.Content)
			}
		}
		if chunk.Usage.PromptTokens > 0 || chunk.Usage.CompletionTokens > 0 || chunk.Usage.TotalTokens > 0 {
			usage = chunk.Usage
		}
	}
	_ = completeText
	return usage, stream.Err()
}
