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
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/postgresql"
)

const (
	// routerSpendMetric is the model-router counter that carries a run's spend by
	// call outcome (internal/router/metrics.go).
	routerSpendMetric = "agentorca_modelrouter_spend_usd_total"
	// routerMetricsPort is the model-router's Prometheus port.
	routerMetricsPort = 9091
	// maxInFlightScrapes bounds how many router pods one /api/costs request reads.
	maxInFlightScrapes = 50
	// inFlightScrapeConcurrency bounds parallel scrapes.
	inFlightScrapeConcurrency = 8
	// inFlightScrapeTimeout bounds the whole in-flight scrape so a stuck pod
	// cannot stall the dashboard.
	inFlightScrapeTimeout = 3 * time.Second
)

// promSample is one parsed Prometheus text-format sample.
type promSample struct {
	labels map[string]string
	value  float64
}

// parsePromSamples returns the samples of the metric called name from a
// Prometheus text-format exposition, with their labels. Lines for other
// metrics, comments and malformed lines are skipped.
func parsePromSamples(r io.Reader, name string) []promSample {
	var out []promSample
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' || !strings.HasPrefix(line, name) {
			continue
		}
		rest := line[len(name):]
		labels := map[string]string{}
		switch {
		case strings.HasPrefix(rest, "{"):
			var ok bool
			labels, rest, ok = parsePromLabels(rest)
			if !ok {
				continue
			}
		case strings.HasPrefix(rest, " "):
		default:
			continue // a longer metric name that merely starts with ours
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		v, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			continue
		}
		out = append(out, promSample{labels: labels, value: v})
	}
	return out
}

// parsePromLabels parses a leading {k="v",...} block, honoring the \\, \" and
// \n escapes the text format defines, and returns the text after the closing brace.
func parsePromLabels(s string) (map[string]string, string, bool) {
	labels := map[string]string{}
	i := 1 // after '{'
	for {
		for i < len(s) && (s[i] == ' ' || s[i] == ',') {
			i++
		}
		if i >= len(s) {
			return nil, "", false
		}
		if s[i] == '}' {
			return labels, s[i+1:], true
		}
		eq := strings.IndexByte(s[i:], '=')
		if eq < 0 || i+eq+1 >= len(s) || s[i+eq+1] != '"' {
			return nil, "", false
		}
		key := strings.TrimSpace(s[i : i+eq])
		i += eq + 2 // past ="
		var val strings.Builder
		for {
			if i >= len(s) {
				return nil, "", false
			}
			c := s[i]
			if c == '\\' && i+1 < len(s) {
				switch s[i+1] {
				case 'n':
					val.WriteByte('\n')
				default:
					val.WriteByte(s[i+1])
				}
				i += 2
				continue
			}
			if c == '"' {
				i++
				break
			}
			val.WriteByte(c)
			i++
		}
		labels[key] = val.String()
	}
}

// routerSpend is the live spend of one run, read from its model-router.
type routerSpend struct {
	usd float64
	// model is the model the run has spent the most on so far.
	model string
}

// foldRouterSpend sums the spend samples that belong to run (in namespace) and
// finds its dominant model. A warm pod serves many runs one after another and
// keeps every run's series, so samples are matched on the run label.
func foldRouterSpend(samples []promSample, namespace, run string) (routerSpend, bool) {
	perModel := map[string]float64{}
	var total float64
	found := false
	for _, smp := range samples {
		if smp.labels["run"] != run {
			continue
		}
		if ns := smp.labels["namespace"]; ns != "" && ns != namespace {
			continue
		}
		found = true
		total += smp.value
		perModel[smp.labels["model"]] += smp.value
	}
	if !found {
		return routerSpend{}, false
	}
	models := make([]string, 0, len(perModel))
	for m := range perModel {
		models = append(models, m)
	}
	sort.Slice(models, func(i, j int) bool {
		if perModel[models[i]] != perModel[models[j]] {
			return perModel[models[i]] > perModel[models[j]]
		}
		return models[i] < models[j]
	})
	out := routerSpend{usd: total}
	if len(models) > 0 && perModel[models[0]] > 0 {
		out.model = models[0]
	}
	return out, true
}

// fetchRouterSamples reads the spend samples from one router's metrics endpoint.
func (s *UIServer) fetchRouterSamples(ctx context.Context, url string) ([]promSample, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("metrics endpoint returned %d", resp.StatusCode)
	}
	return parsePromSamples(resp.Body, routerSpendMetric), nil
}

func (s *UIServer) routerMetricsURL(podIP string) string {
	if s.routerMetricsURLFor != nil {
		return s.routerMetricsURLFor(podIP)
	}
	return fmt.Sprintf("http://%s:%d/metrics", podIP, routerMetricsPort)
}

// scrapeInFlightSpend reads the live spend of runs that have not finished from
// their model-router pods. A running run's Status.SpendUSD is empty until the
// controller stamps it at a terminal state or retry, so without this its spend
// is invisible. Only unfinished runs are read: a finished run's spend is already
// in its status, and its router (a warm pod keeps every run's series) would be
// counted twice. unscraped counts unfinished runs whose router could not be read.
func (s *UIServer) scrapeInFlightSpend(ctx context.Context, runs []*agentorcav1alpha1.AgentRun) (map[string]routerSpend, int) {
	out := map[string]routerSpend{}
	if s.k8s == nil || len(runs) == 0 {
		return out, 0
	}
	ctx, cancel := context.WithTimeout(ctx, inFlightScrapeTimeout)
	defer cancel()

	var (
		mu        sync.Mutex
		wg        sync.WaitGroup
		unscraped int
		overCap   int // runs beyond maxInFlightScrapes; only touched by this goroutine
		sem       = make(chan struct{}, inFlightScrapeConcurrency)
	)
	for i, run := range runs {
		if i >= maxInFlightScrapes {
			overCap = len(runs) - i
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(run *agentorcav1alpha1.AgentRun) {
			defer wg.Done()
			defer func() { <-sem }()
			spend, ok := s.scrapeRunSpend(ctx, run)
			mu.Lock()
			defer mu.Unlock()
			if ok {
				out[run.Namespace+"/"+run.Name] = spend
			} else {
				unscraped++
			}
		}(run)
	}
	wg.Wait()
	return out, unscraped + overCap
}

func (s *UIServer) scrapeRunSpend(ctx context.Context, run *agentorcav1alpha1.AgentRun) (routerSpend, bool) {
	podName := run.Status.RouterPodName
	if podName == "" {
		podName = run.Status.PodName
	}
	if podName == "" {
		return routerSpend{}, false
	}
	pod, err := s.k8s.CoreV1().Pods(run.Namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil || pod.Status.PodIP == "" {
		return routerSpend{}, false
	}
	samples, err := s.fetchRouterSamples(ctx, s.routerMetricsURL(pod.Status.PodIP))
	if err != nil {
		slog.Debug("scraping router spend", "run", run.Name, "pod", podName, "err", err)
		return routerSpend{}, false
	}
	return foldRouterSpend(samples, run.Namespace, run.Name)
}

// applyInFlightSpend raises the spend of unfinished live entries to what their
// router reports, and returns how many could not be read. Entries are matched by
// key; runs holds the unfinished AgentRuns the entries came from.
func (s *UIServer) applyInFlightSpend(ctx context.Context, entries []costEntry, runs []*agentorcav1alpha1.AgentRun) int {
	scraped, unscraped := s.scrapeInFlightSpend(ctx, runs)
	for i := range entries {
		e := &entries[i]
		if e.terminal {
			continue
		}
		rs, ok := scraped[e.key]
		if !ok {
			continue
		}
		// The CRD may already carry a stamped value (e.g. after a restart); the
		// router's counter is the more current of the two while the run lives.
		if rs.usd > e.spend {
			e.spend = rs.usd
		}
		if e.model == "" {
			e.model = rs.model
		}
	}
	return unscraped
}

// isUnfinished reports whether the run can still be spending.
func isUnfinished(run *agentorcav1alpha1.AgentRun) bool {
	return !postgresql.IsTerminalPhase(run.Status.Phase)
}
