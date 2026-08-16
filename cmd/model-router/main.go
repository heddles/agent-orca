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
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/floppyfish14/agent-orc/internal/executor"
	"github.com/floppyfish14/agent-orc/internal/router"
	"github.com/floppyfish14/agent-orc/internal/state"
)

func main() { //nolint:gocyclo
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
	defer func() { _ = store.Close() }()

	// Warm pods: layer a local-disk L1 cache (the disk-backed emptyDir mounted by
	// the operator) on top of the durable Redis store so checkpoint reads are
	// local-first (faster warm resume, no Redis round-trip) and the pod can still
	// resume a conversation if Redis is transiently unavailable. The local cache
	// is only meaningful alongside a durable backend; skip it for the no-op store.
	cacheDir := cfg.WarmLocalCacheDir
	if cacheDir == "" {
		cacheDir = os.Getenv(state.LocalCacheDirEnv) // fallback for manually-wired envs
	}
	if cacheDir != "" {
		if cfg.StateConfig.Backend == "redis" {
			store = state.NewLocalCacheStore(store, cacheDir)
			slog.Info("warm local cache activated", "dir", cacheDir, "sizeMi", cfg.WarmLocalCacheSizeMi)
		} else {
			slog.Warn("warm local cache requested but state backend is not redis; local cache disabled",
				"backend", cfg.StateConfig.Backend, "dir", cacheDir)
		}
	}

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
	// to the agent workload.
	//
	// warmClaimCh signals the http-input goroutine to process a claimed run. It is
	// BUFFERED (size 1) and SEND-based rather than close-Once-based: a warm pod is
	// reused across many runs, so the signal must re-arm on every claim. The old
	// sync.Once + close(warmReadyCh) fired exactly once, after which the goroutine
	// exited and subsequent claims never triggered RunHTTPInput — the warm pod
	// accepted claims but stopped being driven, so the agent never received new run
	// input after the first run's done sentinel. In the normal sequential model
	// (the warm-pool controller claims only idle pods and returns them to idle on
	// completion) the goroutine is always back in the select between claims, so the
	// buffer is drained before the next send and no signal is ever dropped.
	var warmServer *http.Server
	warmClaimCh := make(chan struct{}, 1)
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
			// Signal the http-input goroutine to process this run. Non-blocking: if the
			// goroutine is still driving a previous claim (only possible if the
			// warm-pool controller mis-labels a busy pod as idle), drop the signal and
			// let the controller fall back to claiming another pod afterwards.
			select {
			case warmClaimCh <- struct{}{}:
			default:
				slog.Warn("warm pod still busy; claim signal dropped", "run", input.RunName)
			}
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
			// In warm mode, defer http-input processing until the pod is claimed,
			// and keep going for every subsequent claim so a warm pod is REUSED
			// across chat turns / long-trajectory runs. Each claim calls
			// ClaimRun (which resets per-run router state and repoints
			// cfg.HTTPInput at the new run) and then sends on warmClaimCh; this
			// goroutine wakes and runs RunHTTPInput for that run. The loop +
			// buffered signal replaces the old once-close gate, which terminated
			// the goroutine after the first run and left later claims un-driven.
			go func() {
				for {
					select {
					case <-warmClaimCh:
						// Run config was injected via /v1/claim-run; executor was bound in claim handler.
						if err := router.RunHTTPInput(ctx, cfg.HTTPInput, store); err != nil {
							slog.Error("http input failed", "err", err)
						}
					case <-ctx.Done():
						return
					}
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
	_, _ = fmt.Fprint(w, "ok")
}
