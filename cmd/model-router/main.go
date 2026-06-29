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

// Command model-router is the sidecar that runs alongside every agent pod.
// It exposes an OpenAI-compatible API on :8080 and a Gemini-compatible API on :8082.
// runtime decision logging Prometheus metrics (trace-event client counters) are on :9091/metrics.
// All LLM calls from the agent are proxied through here, enabling:
//   - Multi-model routing (rule-based + LLM meta-router)
//   - Kubernetes JWT authentication (TokenReview)
//   - Conversation state checkpointing
//   - Unified tool dispatch (Tool CRDs, MCP servers, agent-as-tool; in-process executor, no :8081)
//   - Cost tracking and budget enforcement
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/floppyfish14/agent-orc/internal/executor"
	"github.com/floppyfish14/agent-orc/internal/router"
	"github.com/floppyfish14/agent-orc/internal/state"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := router.ConfigFromEnv()
	if err != nil {
		slog.Error("failed to load config", "err", err)
		os.Exit(1)
	}

	// Initialize conversation state store.
	store, err := state.NewStoreFromConfig(cfg.StateConfig)
	if err != nil {
		slog.Error("failed to connect to state store", "err", err)
		os.Exit(1)
	}
	defer store.Close()

	// Load checkpoint if resuming a failed run.
	var initialMessages []json.RawMessage
	if cfg.ResumeCheckpointKey != "" {
		slog.Info("resuming from checkpoint", "key", cfg.ResumeCheckpointKey)
		initialMessages, err = store.LoadMessages(context.Background(), cfg.ResumeCheckpointKey)
		if err != nil {
			slog.Warn("could not load checkpoint, starting fresh", "err", err)
		}
	}

	r, err := router.New(cfg, store, initialMessages, nil)
	if err != nil {
		slog.Error("failed to create router", "err", err)
		os.Exit(1)
	}

	bindExecutor := func(runName string) {
		exec, execErr := executor.New(cfg.RunNamespace, runName, cfg.AgentSAName, cfg.OperatorAPIURL, cfg.SATokenFile)
		if execErr != nil {
			slog.Warn("executor unavailable, tool dispatch disabled", "err", execErr)
			return
		}
		exec.WithStore(store)
		r.SetExecutor(exec)
	}
	if !cfg.WarmMode {
		bindExecutor(cfg.RunName)
	}

	metricsReg := prometheus.NewRegistry()
	metricsSrv := &http.Server{
		Addr:              ":9091",
		Handler:           promhttp.HandlerFor(metricsReg, promhttp.HandlerOpts{EnableOpenMetrics: true}),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		slog.Info("model-router Prometheus metrics listening", "addr", ":9091")
		if err := metricsSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("metrics server error", "err", err)
		}
	}()

	// In warm mode, expose a dedicated management server on :9090 for the claim-run handshake.
	// This server is separate from the agent-facing API so it is never accidentally exposed
	// to the agent workload and can be shut down after the run is claimed.
	var warmServer *http.Server
	var warmOnce sync.Once
	warmReadyCh := make(chan struct{}) // closed when /v1/claim-run is received
	if cfg.WarmMode {
		warmMux := http.NewServeMux()
		warmMux.HandleFunc("/v1/claim-run", func(w http.ResponseWriter, req *http.Request) {
			if req.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			var input router.WarmRunInput
			if err := json.NewDecoder(req.Body).Decode(&input); err != nil {
				http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
				return
			}
			if input.RunName == "" {
				http.Error(w, "runName is required", http.StatusBadRequest)
				return
			}
			r.ClaimRun(input)
			bindExecutor(input.RunName)
			w.WriteHeader(http.StatusOK)
			slog.Info("warm pod claimed", "run", input.RunName, "priorRun", input.PriorRunRef)
			// Signal the http-input goroutine to start, but only once.
			warmOnce.Do(func() { close(warmReadyCh) })
		})
		warmMux.HandleFunc("/healthz", handleHealthz)
		warmServer = &http.Server{Addr: ":9090", Handler: warmMux, ReadHeaderTimeout: 10 * time.Second}
	}

	// OpenAI-compatible API (used by most frameworks).
	openAIMux := http.NewServeMux()
	openAIMux.HandleFunc("/v1/chat/completions", r.HandleChatCompletions)
	openAIMux.HandleFunc("/v1/models", r.HandleModels)
	openAIMux.HandleFunc("/v1/state/", r.HandleState)
	openAIMux.HandleFunc("/healthz", handleHealthz)
	// Internal endpoint for the operator to subscribe to real-time token streaming.
	// Bypasses pod log buffering by reading directly from the model-router.
	openAIMux.HandleFunc("/internal/stream", r.HandleInternalStream)

	// Gemini-compatible API (used by native Google ADK agents).
	geminiMux := http.NewServeMux()
	geminiMux.HandleFunc("/v1beta/models/", r.HandleGemini)
	geminiMux.HandleFunc("/healthz", handleHealthz)

	openAIServer := &http.Server{Addr: ":8080", Handler: openAIMux, ReadHeaderTimeout: 10 * time.Second}
	geminiServer := &http.Server{Addr: ":8082", Handler: geminiMux, ReadHeaderTimeout: 10 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	go func() {
		slog.Info("model-router OpenAI endpoint listening", "addr", ":8080")
		if err := openAIServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("OpenAI server error", "err", err)
		}
	}()
	go func() {
		slog.Info("model-router Gemini endpoint listening", "addr", ":8082")
		if err := geminiServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("Gemini server error", "err", err)
		}
	}()
	if warmServer != nil {
		go func() {
			slog.Info("model-router warm-mode management endpoint listening", "addr", ":9090")
			if err := warmServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				slog.Error("warm server error", "err", err)
			}
		}()
	}

	// Connect to MCP servers in the background so subprocess startup does not
	// delay the /healthz startup probe. Tool schemas are available once connected.
	go r.InitMCPServers()

	if cfg.HTTPInput.Enabled {
		if cfg.WarmMode {
			// In warm mode, defer http-input processing and executor startup until the pod is claimed.
			go func() {
				select {
				case <-warmReadyCh:
					// Run config was injected via /v1/claim-run; executor was bound in claim handler.
					if err := router.RunHTTPInput(ctx, cfg.HTTPInput, store); err != nil {
						slog.Error("http input failed", "err", err)
					}
				case <-ctx.Done():
				}
			}()
		} else {
			go func() {
				if err := router.RunHTTPInput(ctx, cfg.HTTPInput, store); err != nil {
					slog.Error("http input failed", "err", err)
				}
			}()
		}
	}

	<-ctx.Done()
	slog.Info("model-router shutting down", "spend_usd", fmt.Sprintf("%.6f", r.SpendUSD()))

	// Emit a terminal trace-event release decision if the run hasn't already reached one.
	// Must happen before HTTP server shutdown so the operator POST can still be delivered.
	r.Finalize()

	// Close the token broadcaster so /internal/stream subscribers get notified.
	r.CloseTokenBroadcaster()
	r.CloseMCPClient()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = openAIServer.Shutdown(shutdownCtx)
	_ = geminiServer.Shutdown(shutdownCtx)
	if warmServer != nil {
		_ = warmServer.Shutdown(shutdownCtx)
	}
	_ = metricsSrv.Shutdown(shutdownCtx)
}

func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	fmt.Fprint(w, "ok")
}
