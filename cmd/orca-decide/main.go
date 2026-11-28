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

// Command orca-decide serves the local decision model that agent dispatch asks
// which agent a request is for. It runs on CPU in front of a llama.cpp server
// holding a small embedding model, and sends nothing outside the pod.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/heddles/agent-orca/internal/dispatch/decision"
	"github.com/heddles/agent-orca/internal/dispatch/localdecider"
)

func main() {
	listen := flag.String("listen", ":8080", "address to serve on")
	llama := flag.String("llama-url", "http://127.0.0.1:8081", "the llama.cpp server embedding for this service")
	model := flag.String("model", "gte-small-q8", "name of the embedding model, reported with each decision")
	tokens := flag.Int("request-tokens", localdecider.DefaultRequestTokens,
		"how many tokens of a request are read; more is slower and slightly more accurate")
	flag.Parse()

	embedder := localdecider.LlamaCPP{URL: *llama}
	srv := &http.Server{
		Addr:              *listen,
		Handler:           handler(localdecider.New(embedder, *model, *tokens), embedder.Healthy),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdown); err != nil {
			slog.Error("shutting down", "err", err)
		}
	}()

	slog.Info("orca-decide listening", "addr", *listen, "model", *model, "requestTokens", *tokens)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("orca-decide stopped", "err", err)
		os.Exit(1)
	}
}

// handler serves decisions and the probes. The service is alive while it
// answers, and ready only while its embedding server is.
func handler(d decision.Decider, ready func(context.Context) error) http.Handler {
	mux := http.NewServeMux()
	mux.Handle(decision.Path, decision.Handler(d))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := ready(ctx); err != nil {
			http.Error(w, "the embedding server is not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	return mux
}
