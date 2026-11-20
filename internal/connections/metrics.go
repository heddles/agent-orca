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
package connections

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/heddles/agent-orca/internal/postgresql"
)

// Metrics of the connection lifecycle. No label carries a user, a server or a
// tenant: the counts say how many, and the audit log says whose.
const (
	reasonInvalidGrant  = "invalid_grant"
	reasonProviderError = "provider_error"
	reasonUnavailable   = "unavailable"
)

var (
	refreshFailures = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "agentorca_mcp_token_refresh_failures_total",
		Help: "Failed refreshes of a per-user MCP access token, by reason (invalid_grant makes the connection need renewing).",
	}, []string{"reason"})

	rotationRows = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "agentorca_mcp_key_rotation_rows_total",
		Help: "Connection rows handled by the key-rotation job, by result (rewrapped, failed).",
	}, []string{"result"})

	rotationPending = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "agentorca_mcp_key_rotation_pending_rows",
		Help: "Connection rows still under a key other than the active one, as of the last rotation pass.",
	})

	idleExpired = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "agentorca_mcp_connections_expired_total",
		Help: "Connections expired for idleness, by whether the provider revocation succeeded (revoked, not_revoked, failed).",
	}, []string{"revocation"})
)

func init() {
	ctrlmetrics.Registry.MustRegister(refreshFailures, rotationRows, rotationPending, idleExpired)
}

// statusCounter is what the connection gauge reads.
type statusCounter interface {
	CountConnectionsByStatus(ctx context.Context) (map[string]int, error)
}

// statusStatuses are the statuses the gauge reports, always, so a status with no
// rows reads 0 rather than disappearing.
var statusStatuses = []string{postgresql.ConnectionActive, postgresql.ConnectionNeedsReauth, postgresql.ConnectionExpired}

// StatusCollector exposes agentorca_mcp_connections{status}. It reads the
// database on scrape (cached for a short while), so every replica answers the
// same, whichever one is the leader.
type StatusCollector struct {
	store statusCounter
	ttl   time.Duration
	now   func() time.Time
	desc  *prometheus.Desc

	mu     sync.Mutex
	at     time.Time
	counts map[string]int
}

// NewStatusCollector returns the connection gauge collector.
func NewStatusCollector(store statusCounter) *StatusCollector {
	return &StatusCollector{store: store, ttl: 30 * time.Second, now: time.Now,
		desc: prometheus.NewDesc("agentorca_mcp_connections", "Per-user MCP connections by status.", []string{"status"}, nil)}
}

// Describe implements prometheus.Collector.
func (c *StatusCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

// Collect implements prometheus.Collector. When the database cannot be read it
// reports nothing rather than stale or zero values.
func (c *StatusCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.counts == nil || c.now().Sub(c.at) >= c.ttl {
		ctx, cancel := context.WithTimeout(postgresql.WithPlatform(context.Background()), 5*time.Second)
		defer cancel()
		counts, err := c.store.CountConnectionsByStatus(ctx)
		if err != nil {
			slog.Warn("could not count MCP connections for the metric", "err", err)
			return
		}
		c.counts, c.at = counts, c.now()
	}
	for _, st := range statusStatuses {
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(c.counts[st]), st)
	}
}

// RegisterStatusGauge registers the connection gauge on the operator's metrics registry.
func RegisterStatusGauge(store statusCounter) error {
	return ctrlmetrics.Registry.Register(NewStatusCollector(store))
}
