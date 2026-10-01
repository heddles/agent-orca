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

package apiserver

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/heddles/agent-orca/internal/state"
)

// AlertState describes whether an alert is actively firing or has been resolved.
type AlertState string

const (
	AlertFiring   AlertState = "firing"
	AlertResolved AlertState = "resolved"
)

// Alert represents the current state of a subsystem alert.
type Alert struct {
	// ID is a stable identifier for the alert (e.g. "redis-down").
	ID string `json:"id"`
	// SubSystem is the subsystem the alert pertains to (e.g. "redis", "kubernetes-api").
	SubSystem string `json:"subsystem"`
	// State is "firing" or "resolved".
	State AlertState `json:"state"`
	// Message is a human-readable description of the current condition.
	Message string `json:"message"`
	// FirstSeen is when the alert first entered the firing state.
	FirstSeen time.Time `json:"firstSeen"`
	// LastSeen is when the alert was last evaluated.
	LastSeen time.Time `json:"lastSeen"`
	// ResolvedAt is set when the alert transitions to resolved.
	ResolvedAt *time.Time `json:"resolvedAt,omitempty"`
}

// AlertWebhook configures webhook delivery for alert state transitions.
// The webhook receives a JSON payload signed with HMAC-SHA256 using HMACKey.
type AlertWebhook struct {
	URL     string `json:"url"`
	HMACKey []byte `json:"-"` // raw HMAC key (resolved from K8s Secret by caller)
}

// AlertManager evaluates subsystem health checks and fires webhooks on state
// transitions (up→down or down→up). Alert state is persisted in Redis so it
// survives operator restarts.
type AlertManager struct {
	store    state.Store
	webhooks []AlertWebhook
}

// NewAlertManager creates an AlertManager backed by the given Redis store.
// webhooks configures webhook delivery on alert transitions. May be empty
// to disable webhook delivery (state is still tracked for the status endpoint).
func NewAlertManager(store state.Store, webhooks []AlertWebhook) *AlertManager {
	return &AlertManager{store: store, webhooks: webhooks}
}

// Evaluate checks a subsystem's health and records any state transition.
// It returns the current Alert (always non-nil) so the caller can include it
// in the system status response. If the subsystem transitioned state,
// appropriate webhooks are fired.
func (am *AlertManager) Evaluate(ctx context.Context, name, detail string, up bool) *Alert {
	if am == nil {
		return &Alert{
			ID:        name + "-down",
			SubSystem: name,
			State:     AlertFiring,
			Message:   detail,
			LastSeen:  time.Now().UTC(),
		}
	}

	now := time.Now().UTC()
	alertID := name + "-down"
	currentState := AlertFiring
	if up {
		currentState = AlertResolved
	}

	// Load previous state from Redis.
	var prevAlert *Alert
	if am.store != nil {
		if data, err := am.store.LoadKV(ctx, "agentorca:alerts", alertID); err == nil && data != nil {
			_ = json.Unmarshal(data, &prevAlert)
		}
	}

	var alert *Alert
	if prevAlert != nil {
		alert = prevAlert
		alert.LastSeen = now
		if up {
			if alert.State == AlertFiring {
				// Transition: firing → resolved.
				resolvedAt := now
				alert.ResolvedAt = &resolvedAt
				alert.State = AlertResolved
				alert.Message = detail
			} else {
				// Already resolved; update message + clear ResolvedAt.
				alert.ResolvedAt = nil
				alert.Message = detail
			}
		} else {
			// Still firing; update message.
			alert.ResolvedAt = nil
			alert.State = AlertFiring
			alert.Message = detail
		}
	} else {
		// First evaluation for this alert.
		alert = &Alert{
			ID:        alertID,
			SubSystem: name,
			State:     currentState,
			Message:   detail,
			FirstSeen: now,
			LastSeen:  now,
		}
		if up {
			resolvedAt := now
			alert.ResolvedAt = &resolvedAt
		}
	}

	// Persist current state.
	if am.store != nil {
		data, _ := json.Marshal(alert)
		_ = am.store.SaveKV(ctx, "agentorca:alerts", alertID, data, 7*24*time.Hour)
	}

	// Fire webhook on transition (newly firing, or newly resolved).
	if prevAlert != nil && prevAlert.State != alert.State {
		am.fireWebhook(ctx, alert)
	} else if prevAlert == nil && alert.State == AlertFiring {
		// First time seeing this alert as firing.
		am.fireWebhook(ctx, alert)
	}

	return alert
}

// fireWebhook sends an alert payload to all configured webhooks, signed with
// HMAC-SHA256. Uses a short timeout so webhook failures never block the
// system status response.
func (am *AlertManager) fireWebhook(ctx context.Context, alert *Alert) {
	if len(am.webhooks) == 0 {
		return
	}

	payload, _ := json.Marshal(map[string]any{
		"subsystem":  alert.SubSystem,
		"state":      alert.State,
		"message":    alert.Message,
		"firstSeen":  alert.FirstSeen.Format(time.RFC3339),
		"lastSeen":   alert.LastSeen.Format(time.RFC3339),
		"resolvedAt": alert.ResolvedAt,
		"timestamp":  time.Now().UTC().Format(time.RFC3339),
	})

	for _, wh := range am.webhooks {
		go func(url string, hmacKey []byte) {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
			if err != nil {
				return
			}
			req.Header.Set("Content-Type", "application/json")
			// HMAC-SHA256 signature (mirrors TaskCallback pattern in external_api.go).
			mac := hmac.New(sha256.New, hmacKey)
			mac.Write(payload)
			req.Header.Set("X-Agentorc-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
			req.Header.Set("X-Agentorc-Timestamp", nowRFC3339())
			client := &http.Client{Timeout: 10 * time.Second}
			resp, err := client.Do(req)
			if err != nil {
				slog.Warn("alert webhook delivery failed", "url", url, "err", err)
				return
			}
			_ = resp.Body.Close()
		}(wh.URL, wh.HMACKey)
	}

	slog.Info("alert state changed, webhook fired",
		"subsystem", alert.SubSystem, "state", alert.State)
}

func nowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// ListAlerts returns all active (non-resolved) alerts from the store.
// Used by GET /api/system/alerts.
func (am *AlertManager) ListAlerts(ctx context.Context) []*Alert {
	if am == nil || am.store == nil {
		return nil
	}
	keys, err := am.store.ListKV(ctx, "agentorca:alerts")
	if err != nil || len(keys) == 0 {
		return nil
	}
	// ListKV strips the scope prefix, so keys are bare alert IDs (e.g. "redis-down").
	var alerts []*Alert
	for _, id := range keys {
		data, err := am.store.LoadKV(ctx, "agentorca:alerts", id)
		if err != nil || data == nil {
			continue
		}
		var a Alert
		if err := json.Unmarshal(data, &a); err != nil {
			continue
		}
		alerts = append(alerts, &a)
	}
	return alerts
}
