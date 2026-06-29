package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
)

var (
	port        = flag.String("port", "8080", "Listening port")
	staticFiles = flag.String("static", "/srv", "Path to static files")
)

func main() {
	flag.Parse()

	// Get MCP server URL from environment
	mcpServer := os.Getenv("MCP_SERVER")
	if mcpServer == "" {
		mcpServer = "http://localhost:8080" // default for local development
	}

	// Parse MCP server URL for proxying
	mcpURL, err := url.Parse(mcpServer)
	if err != nil {
		log.Fatalf("invalid MCP server URL: %v", err)
	}

	mcpProxyURL := *mcpURL

	mux := http.NewServeMux()

	// Serve API routes
	mux.HandleFunc("/api/causal/", func(w http.ResponseWriter, r *http.Request) {
		proxyCausalAPI(w, r, mcpProxyURL)
	})

	// Health check
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	// Serve static files with SPA fallback
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		p := path.Clean(r.URL.Path)
		// If the path has a file extension and exists, serve it directly
		if p != "/" && strings.Contains(path.Base(p), ".") {
			http.FileServer(http.Dir(*staticFiles)).ServeHTTP(w, r)
			return
		}
		// For all other paths, serve index.html (SPA routing)
		http.ServeFile(w, r, *staticFiles+"/index.html")
	})

	addr := ":" + *port
	log.Printf("Starting server on %s, proxying MCP to %s", addr, mcpServer)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("server exited: %v", err)
	}
}

func proxyCausalAPI(w http.ResponseWriter, r *http.Request, mcpURL url.URL) {
	// Parse path: /api/causal/{sessionId}/analyze
	pathParts := strings.TrimPrefix(r.URL.Path, "/api/causal/")
	parts := strings.SplitN(pathParts, "/", 3)

	if len(parts) < 2 {
		http.Error(w, "invalid path: expected /api/causal/{sessionId}/analyze", http.StatusBadRequest)
		return
	}

	_ = parts[0] // sessionId - could be used for session tracking
	action := parts[1]

	switch action {
	case "analyze":
		// Translate REST call to JSON-RPC
		var req struct {
			Pattern string `json:"pattern"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}

		jsonRPCRequest := map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"method":  "tools/call",
			"params": map[string]interface{}{
				"name":      "propose-hypothesis",
				"arguments": map[string]interface{}{
					"observational_pattern": req.Pattern,
					"domain":              "general",
				},
			},
		}

		body, err := json.Marshal(jsonRPCRequest)
		if err != nil {
			http.Error(w, "failed to marshal request", http.StatusInternalServerError)
			return
		}

		// Create new request to MCP server
		targetURL := mcpURL
		targetURL.Path = "/"
		proxyReq, err := http.NewRequest("POST", targetURL.String(), strings.NewReader(string(body)))
		if err != nil {
			http.Error(w, "failed to create request", http.StatusInternalServerError)
			return
		}
		proxyReq.Header.Set("Content-Type", "application/json")

		resp, err := http.DefaultClient.Do(proxyReq)
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to call MCP server: %v", err), http.StatusInternalServerError)
			return
		}
		defer resp.Body.Close()

		respBody, _ := io.ReadAll(resp.Body)
		var jsonRPCResponse map[string]interface{}
		if err := json.Unmarshal(respBody, &jsonRPCResponse); err != nil {
			http.Error(w, "invalid MCP response", http.StatusInternalServerError)
			return
		}

		// Extract result from JSON-RPC response
		if result, ok := jsonRPCResponse["result"].(map[string]interface{}); ok {
			if content, ok := result["content"].([]interface{}); ok && len(content) > 0 {
				if first, ok := content[0].(map[string]interface{}); ok {
					if text, ok := first["text"].(string); ok {
						var hypothesis map[string]interface{}
						if err := json.Unmarshal([]byte(text), &hypothesis); err == nil {
							w.Header().Set("Content-Type", "application/json")
							w.Header().Set("Access-Control-Allow-Origin", "*")
							json.NewEncoder(w).Encode(hypothesis)
							return
						}
					}
				}
			}
		}

		// If we couldn't parse, just return the raw response
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Write(respBody)

	default:
		http.Error(w, fmt.Sprintf("unknown action: %s", action), http.StatusNotFound)
	}
}