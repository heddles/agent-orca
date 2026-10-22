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
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientgofake "k8s.io/client-go/kubernetes/fake"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

const spendExposition = `# HELP agentorca_modelrouter_spend_usd_total USD spent.
# TYPE agentorca_modelrouter_spend_usd_total counter
agentorca_modelrouter_spend_usd_total{model="gpt-4o",namespace="ns",outcome="success",provider="openai",run="run-a"} 0.25
agentorca_modelrouter_spend_usd_total{model="gpt-4o",namespace="ns",outcome="cancelled",provider="openai",run="run-a"} 0.05
agentorcav1_other_total{run="run-a"} 99
agentorca_modelrouter_spend_usd_total_extra{run="run-a"} 77
agentorca_modelrouter_spend_usd_total{model="claude",namespace="ns",outcome="success",provider="anthropic",run="run-a"} 0.5
agentorca_modelrouter_spend_usd_total{model="gpt-4o",namespace="ns",outcome="success",provider="openai",run="old-run"} 9
agentorca_modelrouter_spend_usd_total{model="gpt-4o",namespace="other-ns",outcome="success",provider="openai",run="run-a"} 4
`

func TestParsePromSamples_OnlyTheNamedMetric(t *testing.T) {
	got := parsePromSamples(strings.NewReader(spendExposition), routerSpendMetric)
	if len(got) != 5 {
		t.Fatalf("got %d samples, want the 5 spend samples (not the comment, the other metric or the _extra look-alike): %+v", len(got), got)
	}
	if got[0].labels["outcome"] != "success" || got[0].value != 0.25 {
		t.Fatalf("first sample = %+v", got[0])
	}
}

func TestParsePromSamples_EscapesAndMalformedLines(t *testing.T) {
	in := strings.Join([]string{
		`m{a="x\"y",b="line1\nline2",c="back\\slash"} 1`,
		`m{a="unterminated} 2`,
		`m{a="ok"} not-a-number`,
		`m 3`,
		`m{} 4`,
		`m{a="v"} 5 1700000000000`,
	}, "\n")
	got := parsePromSamples(strings.NewReader(in), "m")
	if len(got) != 4 {
		t.Fatalf("got %d samples, want 4 well-formed ones: %+v", len(got), got)
	}
	if got[0].labels["a"] != `x"y` || got[0].labels["b"] != "line1\nline2" || got[0].labels["c"] != `back\slash` {
		t.Fatalf("escapes not decoded: %+v", got[0].labels)
	}
	if got[1].value != 3 || got[2].value != 4 || got[3].value != 5 {
		t.Fatalf("values = %v %v %v, want 3 4 5 (timestamp ignored)", got[1].value, got[2].value, got[3].value)
	}
}

// A warm pod keeps every run's series; only the asked-for run counts.
func TestFoldRouterSpend_MatchesRunAndNamespace(t *testing.T) {
	samples := parsePromSamples(strings.NewReader(spendExposition), routerSpendMetric)

	got, ok := foldRouterSpend(samples, "ns", "run-a")
	if !ok {
		t.Fatal("run-a not found")
	}
	if got.usd != 0.8 {
		t.Fatalf("usd = %v, want 0.25+0.05+0.5 (every outcome counts, other runs and namespaces do not)", got.usd)
	}
	if got.model != "claude" {
		t.Fatalf("dominant model = %q, want claude (0.5 beats gpt-4o 0.30)", got.model)
	}

	if _, ok := foldRouterSpend(samples, "ns", "missing"); ok {
		t.Fatal("a run with no series must report not found")
	}
}

func TestFoldRouterSpend_ZeroSpendIsFoundWithNoModel(t *testing.T) {
	samples := []promSample{{labels: map[string]string{"run": "r", "model": "m", "outcome": "success"}, value: 0}}
	got, ok := foldRouterSpend(samples, "ns", "r")
	if !ok || got.usd != 0 || got.model != "" {
		t.Fatalf("got %+v ok=%v, want found, $0, no dominant model", got, ok)
	}
}

// routerFixture serves spendExposition for every pod and counts requests.
func routerFixture(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(spendExposition))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// podWithIP is a pod in the test namespace "ns".
func podWithIP(name, ip string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"}, Status: corev1.PodStatus{PodIP: ip}}
}

// runWithPod is a run in the test namespace "ns" whose router runs in pod.
func runWithPod(name string, phase agentorcav1alpha1.AgentRunPhase, pod string) *agentorcav1alpha1.AgentRun {
	r := liveRun(name, "ns", "agent-1", phase, "", costDay2)
	r.Status.PodName = pod
	return r
}

func inFlightServer(t *testing.T, router *httptest.Server, pods []*corev1.Pod, runs ...*agentorcav1alpha1.AgentRun) *UIServer {
	t.Helper()
	s := costsServer(t, nil, runs...)
	cs := clientgofake.NewClientset()
	for _, p := range pods {
		if _, err := cs.CoreV1().Pods(p.Namespace).Create(context.Background(), p, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	s.k8s = cs
	s.routerMetricsURLFor = func(string) string { return router.URL }
	return s
}

// Regression for #79: a running run's Status.SpendUSD is empty until the
// controller stamps it, so its live spend never reached the dashboard.
func TestHandleCosts_InFlightSpendComesFromTheRouter(t *testing.T) {
	router, _ := routerFixture(t)
	run := runWithPod("run-a", agentorcav1alpha1.AgentRunPhaseRunning, "pod-a")
	run.Status.RoutingDecisions = nil // none recorded yet: the router's dominant model fills in
	s := inFlightServer(t, router, []*corev1.Pod{podWithIP("pod-a", "10.0.0.1")}, run)

	got := getCosts(t, s, "?namespace=ns", context.Background())

	if got.InFlightUSD != usd(0.8) {
		t.Fatalf("inFlightUSD = %s, want 0.8000 from the router's counters", got.InFlightUSD)
	}
	if got.TotalUSD != usd(0.8) {
		t.Fatalf("total = %s, want live spend included", got.TotalUSD)
	}
	if got.ByModel["claude"] != usd(0.8) {
		t.Fatalf("byModel = %v, want the run attributed to its dominant model", got.ByModel)
	}
	if got.InFlightUnscraped != 0 {
		t.Fatalf("inFlightUnscraped = %d, want 0", got.InFlightUnscraped)
	}
}

// A finished run's spend is already in its status and a warm pod still holds
// its series: reading the router for it would count it twice.
func TestHandleCosts_FinishedRunsAreNotScraped(t *testing.T) {
	router, hits := routerFixture(t)
	done := runWithPod("run-a", agentorcav1alpha1.AgentRunPhaseSucceeded, "pod-a")
	done.Status.SpendUSD = "0.8"
	s := inFlightServer(t, router, []*corev1.Pod{podWithIP("pod-a", "10.0.0.1")}, done)

	got := getCosts(t, s, "?namespace=ns", context.Background())

	if hits.Load() != 0 {
		t.Fatalf("router scraped %d times for a finished run", hits.Load())
	}
	if got.TotalUSD != usd(0.8) || got.InFlightUSD != usd(0) {
		t.Fatalf("total=%s inFlight=%s, want 0.8000 / 0.0000", got.TotalUSD, got.InFlightUSD)
	}
}

// If the CRD already has a higher stamped value (e.g. after a restart the new
// pod's counter starts from 0), the larger figure wins.
func TestHandleCosts_InFlightKeepsHigherStampedSpend(t *testing.T) {
	router, _ := routerFixture(t)
	run := runWithPod("run-a", agentorcav1alpha1.AgentRunPhaseRunning, "pod-a")
	run.Status.SpendUSD = "2.0"
	s := inFlightServer(t, router, []*corev1.Pod{podWithIP("pod-a", "10.0.0.1")}, run)

	got := getCosts(t, s, "?namespace=ns", context.Background())

	if got.InFlightUSD != usd(2) {
		t.Fatalf("inFlightUSD = %s, want the stamped 2.0000", got.InFlightUSD)
	}
}

// A router that cannot be read must be reported, not silently treated as $0.
func TestHandleCosts_UnreadableRoutersAreCounted(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusServiceUnavailable)
	}))
	t.Cleanup(dead.Close)
	s := inFlightServer(t, dead,
		[]*corev1.Pod{podWithIP("pod-a", "10.0.0.1")},
		runWithPod("run-a", agentorcav1alpha1.AgentRunPhaseRunning, "pod-a"),
		runWithPod("run-b", agentorcav1alpha1.AgentRunPhaseRunning, "missing-pod"),
		runWithPod("run-c", agentorcav1alpha1.AgentRunPhasePending, ""),
	)

	got := getCosts(t, s, "?namespace=ns", context.Background())

	if got.InFlightUnscraped != 3 {
		t.Fatalf("inFlightUnscraped = %d, want 3 (bad status, missing pod, no pod yet)", got.InFlightUnscraped)
	}
	if got.InFlightUSD != usd(0) {
		t.Fatalf("inFlightUSD = %s", got.InFlightUSD)
	}
}

func TestHandleCosts_RouterSplitPodIsPreferred(t *testing.T) {
	router, _ := routerFixture(t)
	run := runWithPod("run-a", agentorcav1alpha1.AgentRunPhaseRunning, "agent-pod-has-no-router-ip")
	run.Status.RouterPodName = "router-pod"
	s := inFlightServer(t, router, []*corev1.Pod{podWithIP("router-pod", "10.0.0.9")}, run)

	got := getCosts(t, s, "?namespace=ns", context.Background())

	if got.InFlightUSD != usd(0.8) {
		t.Fatalf("inFlightUSD = %s, want the split-pod router's spend", got.InFlightUSD)
	}
}

func TestScrapeInFlightSpend_IsBounded(t *testing.T) {
	router, hits := routerFixture(t)
	runs := make([]*agentorcav1alpha1.AgentRun, 0, maxInFlightScrapes+7)
	pods := make([]*corev1.Pod, 0, maxInFlightScrapes+7)
	for i := range maxInFlightScrapes + 7 {
		name := "run-" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		runs = append(runs, runWithPod(name, agentorcav1alpha1.AgentRunPhaseRunning, "pod-"+name))
		pods = append(pods, podWithIP("pod-"+name, "10.0.0.1"))
	}
	s := inFlightServer(t, router, pods)

	_, unscraped := s.scrapeInFlightSpend(context.Background(), runs)

	if int(hits.Load()) > maxInFlightScrapes {
		t.Fatalf("scraped %d routers, want at most %d", hits.Load(), maxInFlightScrapes)
	}
	if unscraped < 7 {
		t.Fatalf("unscraped = %d, want the 7 runs over the cap reported", unscraped)
	}
}

func TestScrapeInFlightSpend_NoClusterClientIsANoop(t *testing.T) {
	s := &UIServer{}
	got, unscraped := s.scrapeInFlightSpend(context.Background(), []*agentorcav1alpha1.AgentRun{
		runWithPod("r", agentorcav1alpha1.AgentRunPhaseRunning, "p"),
	})
	if len(got) != 0 || unscraped != 0 {
		t.Fatalf("got %v / %d, want nothing without a cluster client", got, unscraped)
	}
}

func TestRouterMetricsURLDefault(t *testing.T) {
	s := &UIServer{}
	if got := s.routerMetricsURL("10.1.2.3"); got != "http://10.1.2.3:9091/metrics" {
		t.Fatalf("url = %q", got)
	}
}

// A recorded routing decision wins over the router's dominant model.
func TestHandleCosts_InFlightKeepsRecordedModel(t *testing.T) {
	router, _ := routerFixture(t)
	s := inFlightServer(t, router,
		[]*corev1.Pod{podWithIP("pod-a", "10.0.0.1")},
		runWithPod("run-a", agentorcav1alpha1.AgentRunPhaseRunning, "pod-a"), // liveRun records gpt-4o
	)

	got := getCosts(t, s, "?namespace=ns", context.Background())

	if got.ByModel["gpt-4o"] != usd(0.8) {
		t.Fatalf("byModel = %v, want the recorded gpt-4o", got.ByModel)
	}
}
