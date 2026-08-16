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
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentorcv1alpha1 "github.com/floppyfish14/agent-orc/api/v1alpha1"
)

func newQuotaScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := agentorcv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding agentorc scheme: %v", err)
	}
	return s
}

func TestRateLimiter_NoLimitsAlwaysAllowed(t *testing.T) {
	rl := NewRateLimiter()
	for i := range 1000 {
		if _, ok := rl.Allow("tenant", 0); !ok {
			t.Fatalf("request %d rejected despite rpm=0", i)
		}
	}
	if _, ok := rl.Allow("", 1); !ok {
		t.Fatal("empty tenant should never be rate-limited")
	}
}

func TestRateLimiterBurstThenRejects(t *testing.T) {
	rl := NewRateLimiter()
	// rpm=2 -> burst=2; two requests pass, the third is rejected.
	for i := range 2 {
		if _, ok := rl.Allow("acme", 2); !ok {
			t.Fatalf("request %d unexpectedly rejected", i)
		}
	}
	wait, ok := rl.Allow("acme", 2)
	if ok {
		t.Fatal("third request should be rate-limited")
	}
	if wait <= 0 {
		t.Fatalf("expected positive retry-after, got %v", wait)
	}
}

func TestRateLimiter_RefillOverTime(t *testing.T) {
	rl := NewRateLimiter()
	// rpm=60 -> 1 token/sec. Drain the burst, then wait to earn one token back.
	for i := range 60 {
		if _, ok := rl.Allow("slow", 60); !ok {
			t.Fatalf("drain request %d unexpectedly rejected", i)
		}
	}
	if _, ok := rl.Allow("slow", 60); ok {
		t.Fatal("expected bucket drained")
	}
	time.Sleep(1200 * time.Millisecond)
	if _, ok := rl.Allow("slow", 60); !ok {
		t.Fatal("expected a token to refill after waiting")
	}
}

func TestAttachQuotaFields(t *testing.T) {
	t.Run("nil config leaves quotas unset", func(t *testing.T) {
		out := attachQuotaFields(&TenantIdentity{TenantName: "x"}, nil)
		if out.RateLimitRPM != 0 || out.ConcurrentRuns != 0 || out.BudgetPerDayUSD != "" {
			t.Fatalf("quota fields not zeroed: %+v", out)
		}
	})
	t.Run("nil id is safe", func(t *testing.T) {
		if got := attachQuotaFields(nil, &agentorcv1alpha1.TenantConfig{}); got != nil {
			t.Fatalf("expected nil, got %+v", got)
		}
	})
	t.Run("populates from TenantConfig", func(t *testing.T) {
		tc := &agentorcv1alpha1.TenantConfig{
			Spec: agentorcv1alpha1.TenantConfigSpec{
				RateLimit:       &agentorcv1alpha1.TenantRateLimit{RequestsPerMinute: 60, ConcurrentRuns: 3},
				BudgetPerDayUSD: "100.00",
			},
		}
		out := attachQuotaFields(&TenantIdentity{}, tc)
		if out.RateLimitRPM != 60 || out.ConcurrentRuns != 3 || out.BudgetPerDayUSD != "100.00" {
			t.Fatalf("quota fields wrong: %+v", out)
		}
	})
	t.Run("nil rateLimit block zeroes fields", func(t *testing.T) {
		tc := &agentorcv1alpha1.TenantConfig{
			Spec: agentorcv1alpha1.TenantConfigSpec{BudgetPerDayUSD: "5.00"},
		}
		out := attachQuotaFields(&TenantIdentity{RateLimitRPM: 9, ConcurrentRuns: 9}, tc)
		if out.RateLimitRPM != 0 || out.ConcurrentRuns != 0 || out.BudgetPerDayUSD != "5.00" {
			t.Fatalf("fields not reset from nil rateLimit: %+v", out)
		}
	})
}

// runWith builds an AgentRun labelled as tenant acme's external task.
func runWith(ns, name, phase, spend string) *agentorcv1alpha1.AgentRun { //nolint:unparam

	return &agentorcv1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels: map[string]string{
				"agentorc.io/tenant":        "acme",
				"agentorc.io/external-task": "true",
			},
		},
		Status: agentorcv1alpha1.AgentRunStatus{
			Phase:    agentorcv1alpha1.AgentRunPhase(phase),
			SpendUSD: spend,
		},
	}
}

func fakeClientWith(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(newQuotaScheme(t)).
		WithObjects(objs...).
		Build()
}

func TestEnforceQuotas_NoLimitsPasses(t *testing.T) {
	kube := fakeClientWith(t, runWith("tenant-acme", "r1", "Running", "0.01"))
	tenant := &TenantIdentity{TenantName: "acme", Namespace: "tenant-acme"}
	if err := enforceQuotas(context.Background(), kube, NewRateLimiter(), tenant); err != nil {
		t.Fatalf("expected no quota error, got %v", err)
	}
}

func TestEnforceQuotas_NilTenantPasses(t *testing.T) {
	if err := enforceQuotas(context.Background(), fakeClientWith(t), NewRateLimiter(), nil); err != nil {
		t.Fatalf("nil tenant should not be limited: %v", err)
	}
}

func TestEnforceQuotas_RateLimited(t *testing.T) {
	kube := fakeClientWith(t)
	rl := NewRateLimiter()
	tenant := &TenantIdentity{TenantName: "acme", Namespace: "tenant-acme", RateLimitRPM: 1}
	if err := enforceQuotas(context.Background(), kube, rl, tenant); err != nil {
		t.Fatalf("first submission should pass: %v", err)
	}
	err := enforceQuotas(context.Background(), kube, rl, tenant)
	qe, ok := err.(*QuotaError)
	if !ok {
		t.Fatalf("expected *QuotaError, got %T: %v", err, err)
	}
	if qe.Code != 429 {
		t.Fatalf("expected 429, got %d", qe.Code)
	}
	if qe.RetryAfter <= 0 {
		t.Fatal("expected positive retry-after on rate limit")
	}
}

func TestEnforceQuotas_ConcurrentRunCap(t *testing.T) {
	// Two active external tasks; cap allows 2 -> next rejected with 429.
	kube := fakeClientWith(t,
		runWith("tenant-acme", "r1", "Running", "0.01"),
		runWith("tenant-acme", "r2", "WaitingForInput", "0.00"),
	)
	tenant := &TenantIdentity{TenantName: "acme", Namespace: "tenant-acme", ConcurrentRuns: 2}
	err := enforceQuotas(context.Background(), kube, NewRateLimiter(), tenant)
	qe, ok := err.(*QuotaError)
	if !ok {
		t.Fatalf("expected *QuotaError, got %T: %v", err, err)
	}
	if qe.Code != 429 {
		t.Fatalf("expected 429 for concurrency, got %d", qe.Code)
	}
	if !strings.Contains(qe.Message, "concurrent") {
		t.Fatalf("expected concurrent-run message, got %q", qe.Message)
	}
}

func TestEnforceQuotas_ConcurrentCapNotReached(t *testing.T) {
	// One active run, cap 3 -> allowed.
	kube := fakeClientWith(t, runWith("tenant-acme", "r1", "Running", "0.01"))
	tenant := &TenantIdentity{TenantName: "acme", Namespace: "tenant-acme", ConcurrentRuns: 3}
	if err := enforceQuotas(context.Background(), kube, NewRateLimiter(), tenant); err != nil {
		t.Fatalf("under cap should pass: %v", err)
	}
}

func TestEnforceQuotas_BudgetExceeded(t *testing.T) {
	// Two runs completed today summing to $0.05 == $0.05 cap -> 402.
	yesterday := metav1.Time{Time: time.Now().Add(-24 * time.Hour)}
	kube := fakeClientWith(t,
		&agentorcv1alpha1.AgentRun{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "r-today-1",
				Namespace: "tenant-acme",
				Labels:    map[string]string{"agentorc.io/tenant": "acme"},
			},
			Status: agentorcv1alpha1.AgentRunStatus{
				Phase:    agentorcv1alpha1.AgentRunPhaseSucceeded,
				SpendUSD: "0.03",
			},
		},
		runWith("tenant-acme", "r-today-2", "Succeeded", "0.02"),
		// A yesterday run that must NOT be counted toward today's budget.
		&agentorcv1alpha1.AgentRun{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "r-yesterday",
				Namespace:         "tenant-acme",
				CreationTimestamp: yesterday,
				Labels:            map[string]string{"agentorc.io/tenant": "acme"},
			},
			Status: agentorcv1alpha1.AgentRunStatus{
				Phase:    agentorcv1alpha1.AgentRunPhaseSucceeded,
				SpendUSD: "50.00",
			},
		},
	)
	tenant := &TenantIdentity{TenantName: "acme", Namespace: "tenant-acme", BudgetPerDayUSD: "0.05"}
	err := enforceQuotas(context.Background(), kube, NewRateLimiter(), tenant)
	qe, ok := err.(*QuotaError)
	if !ok {
		t.Fatalf("expected *QuotaError, got %T: %v", err, err)
	}
	if qe.Code != 402 {
		t.Fatalf("expected 402 for budget, got %d", qe.Code)
	}
}

func TestEnforceQuotas_BudgetWithinLimit(t *testing.T) {
	kube := fakeClientWith(t, runWith("tenant-acme", "r1", "Succeeded", "0.01"))
	tenant := &TenantIdentity{TenantName: "acme", Namespace: "tenant-acme", BudgetPerDayUSD: "1.00"}
	if err := enforceQuotas(context.Background(), kube, NewRateLimiter(), tenant); err != nil {
		t.Fatalf("expected no error under budget, got %v", err)
	}
}
