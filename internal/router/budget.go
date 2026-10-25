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

package router

// Run budget (AgentRun.spec.budget). The router warns once when spend passes
// the warning threshold and checks the budget before every LLM call. At the cap
// it either halts the run through the safeguard path ("fail") or holds the
// request ("pause"): the SSE stream stays open with keepalives, so the agent
// process, its sandbox and its workspace stay alive while the operator asks
// approvers to raise the cap. The router never sets its own cap: it reports
// state and spend to the operator and reads the cap back, so only an approved
// raise (applied by the operator) lets the run continue.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// traceEventTypeBudget is the trace event for budget state changes.
const traceEventTypeBudget = "budget"

// Budget states reported to the operator and in trace events.
const (
	budgetStateWarning  = "warning"
	budgetStateExceeded = "exceeded"
	budgetStateResumed  = "resumed"
	budgetStateRejected = "rejected"
)

// defaultBudgetPoll is how often a paused router asks the operator for the cap.
const defaultBudgetPoll = 5 * time.Second

// budgetKeepalive is how often a paused router writes an SSE comment so the
// agent's HTTP client does not time out waiting for bytes.
const budgetKeepalive = 15 * time.Second

// BudgetConfig is the run's spend cap, from AgentRun.spec.budget.
type BudgetConfig struct {
	// MaxUSD is the initial cap.
	MaxUSD float64 `json:"maxUSD"`
	// WarnAtPercent is the share of the cap that triggers the warning (default 80).
	WarnAtPercent int `json:"warnAtPercent,omitempty"`
	// OnExceeded is "fail" (default) or "pause".
	OnExceeded string `json:"onExceeded,omitempty"`
}

func (b *BudgetConfig) pauses() bool { return b != nil && b.OnExceeded == "pause" }

func (b *BudgetConfig) warnAt() float64 {
	if b.WarnAtPercent <= 0 || b.WarnAtPercent >= 100 {
		return 0.8
	}
	return float64(b.WarnAtPercent) / 100
}

// budgetOn reports whether the run has a spend cap to enforce.
func (r *Router) budgetOn() bool { return r.cfg.Budget != nil && r.cfg.Budget.MaxUSD > 0 }

// budgetCapLocked is the current cap. The caller holds r.mu.
func (r *Router) budgetCapLocked() float64 {
	if r.budgetCap > 0 {
		return r.budgetCap
	}
	return r.cfg.Budget.MaxUSD
}

// noteBudgetSpendLocked warns once when spend passes the warning threshold. It
// is called from recordSpend with r.mu held and does its I/O in a goroutine.
func (r *Router) noteBudgetSpendLocked() {
	if !r.budgetOn() || r.budgetWarned {
		return
	}
	capUSD := r.budgetCapLocked()
	if r.spendUSD < capUSD*r.cfg.Budget.warnAt() || r.spendUSD >= capUSD {
		return
	}
	r.budgetWarned = true
	spent := r.spendUSD
	go r.announceBudget(budgetStateWarning, spent, capUSD)
}

// announceBudget records a budget state change in the run's trace and reports
// it to the operator. Failures are logged: the budget is still enforced here.
func (r *Router) announceBudget(state string, spent, capUSD float64) {
	ev, _ := json.Marshal(map[string]string{
		"type": traceEventTypeBudget, "state": state,
		"spentUSD": formatUSD(spent), "capUSD": formatUSD(capUSD),
	})
	r.emitTraceEventBoth(string(ev))
	if state == budgetStateResumed || state == budgetStateRejected {
		return // the operator drives those
	}
	body, _ := json.Marshal(map[string]string{"state": state, "spentUSD": formatUSD(spent)})
	if _, err := r.operatorCall(context.Background(), http.MethodPost, "budget", body); err != nil {
		slog.Warn("reporting the budget state to the operator", "run", r.cfg.RunName, "state", state, "err", err)
	}
}

// operatorBudget is the operator's answer to GET .../budget.
type operatorBudget struct {
	State    string `json:"state"`
	CapUSD   string `json:"capUSD"`
	Terminal bool   `json:"terminal"`
}

func (r *Router) fetchOperatorBudget(ctx context.Context) (operatorBudget, error) {
	var ob operatorBudget
	body, err := r.operatorCall(ctx, http.MethodGet, "budget", nil)
	if err != nil {
		return ob, err
	}
	return ob, json.Unmarshal(body, &ob)
}

// operatorCall calls the operator's per-run internal API as this run's router.
func (r *Router) operatorCall(ctx context.Context, method, endpoint string, body []byte) ([]byte, error) {
	if r.cfg.OperatorAPIURL == "" {
		return nil, fmt.Errorf("no operator API URL configured")
	}
	token, err := os.ReadFile(r.cfg.SATokenFile)
	if err != nil {
		return nil, fmt.Errorf("reading SA token: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	url := fmt.Sprintf("%s/agentrun/%s/%s/%s", r.cfg.OperatorAPIURL, r.cfg.RunNamespace, r.cfg.RunName, endpoint)
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("operator returned %d", resp.StatusCode)
	}
	return data, nil
}

// syncBudgetCap adopts the operator's cap when it differs (a restarted router
// learns raises approved earlier). It reports whether the operator says the run
// is over.
func (r *Router) syncBudgetCap(ctx context.Context) (terminal bool) {
	ob, err := r.fetchOperatorBudget(ctx)
	if err != nil {
		return false
	}
	if capUSD, err := strconv.ParseFloat(ob.CapUSD, 64); err == nil && capUSD > 0 {
		r.mu.Lock()
		if capUSD != r.budgetCap {
			if capUSD > r.budgetCap {
				r.budgetWarned = false // re-arm the warning for the new cap
			}
			r.budgetCap = capUSD
		}
		r.mu.Unlock()
	}
	return ob.Terminal || ob.State == budgetStateRejected
}

// checkBudget runs before every LLM call. It returns false when the request
// must not proceed, having already answered it. started tells whether the SSE
// response was already begun for this request (a tool-loop continuation).
func (r *Router) checkBudget(w http.ResponseWriter, req *http.Request, started bool) bool {
	if !r.budgetOn() {
		return true
	}
	r.mu.Lock()
	synced := r.budgetSynced
	r.budgetSynced = true
	r.mu.Unlock()
	if !synced {
		r.syncBudgetCap(req.Context())
	}
	for {
		r.mu.Lock()
		spent, capUSD := r.spendUSD, r.budgetCapLocked()
		r.mu.Unlock()
		if spent < capUSD {
			return true
		}
		if !r.cfg.Budget.pauses() {
			r.haltForBudget(w, spent, capUSD)
			return false
		}
		if !r.pauseForBudget(w, req, started, spent, capUSD) {
			return false
		}
		started = true // the pause began the response
	}
}

// haltForBudget stops the run at its cap, like a safeguard trip: the operator
// fails the run and later requests get 410.
func (r *Router) haltForBudget(w http.ResponseWriter, spent, capUSD float64) {
	reason := fmt.Sprintf("budget of $%s reached ($%s spent)", formatUSD(capUSD), formatUSD(spent))
	ev, _ := json.Marshal(map[string]string{
		"type": traceEventTypeBudget, "state": budgetStateExceeded,
		"spentUSD": formatUSD(spent), "capUSD": formatUSD(capUSD),
	})
	r.emitTraceEventBoth(string(ev))
	r.tripSafeguard(safeguardTrip{Reason: reason})
	http.Error(w, "run halted by safeguard", http.StatusGone)
}

// pauseForBudget holds the request while approvers decide. It returns true once
// the cap is above spend again, and false when the run was rejected, timed out
// or cancelled (the operator fails it; this router stops answering).
func (r *Router) pauseForBudget(w http.ResponseWriter, req *http.Request, started bool, spent, capUSD float64) bool {
	flusher, ok := w.(http.Flusher)
	if !ok {
		r.haltForBudget(w, spent, capUSD)
		return false
	}
	slog.Warn("budget reached; pausing the run for approval", "run", r.cfg.RunName,
		"spentUSD", spent, "capUSD", capUSD)
	r.announceBudget(budgetStateExceeded, spent, capUSD)

	if !started {
		// Begin the response so the agent's HTTP client sees bytes (keepalives)
		// instead of timing out; handleStreamingResponse then skips its headers.
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		r.sseStarted.Store(true)
	}

	poll := time.NewTicker(r.budgetPollInterval())
	defer poll.Stop()
	keepalive := time.NewTicker(budgetKeepalive)
	defer keepalive.Stop()
	for {
		select {
		case <-req.Context().Done():
			endPausedStream(w, flusher)
			return false
		case <-r.cancelCtx.Done():
			endPausedStream(w, flusher)
			return false
		case <-keepalive.C:
			_, _ = fmt.Fprint(w, ": budget-paused\n\n")
			flusher.Flush()
		case <-poll.C:
			if r.syncBudgetCap(req.Context()) {
				r.rejectedForBudget(spent, capUSD)
				endPausedStream(w, flusher)
				return false
			}
			r.mu.Lock()
			newCap := r.budgetCapLocked()
			r.mu.Unlock()
			if newCap > spent {
				r.announceBudget(budgetStateResumed, spent, newCap)
				slog.Info("budget raised; resuming the run", "run", r.cfg.RunName, "capUSD", newCap)
				return true
			}
		}
	}
}

// endPausedStream closes the held SSE response cleanly.
func endPausedStream(w http.ResponseWriter, flusher http.Flusher) {
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// rejectedForBudget ends a paused run whose raise was refused or timed out: the
// operator is already failing it, so stop answering this agent.
func (r *Router) rejectedForBudget(spent, capUSD float64) {
	r.announceBudget(budgetStateRejected, spent, capUSD)
	r.mu.Lock()
	r.loopDetected = true
	r.mu.Unlock()
}

func (r *Router) budgetPollInterval() time.Duration {
	if r.budgetPoll > 0 {
		return r.budgetPoll
	}
	return defaultBudgetPoll
}

// formatUSD renders an amount with the precision model costs need.
func formatUSD(v float64) string { return strconv.FormatFloat(v, 'f', 4, 64) }
