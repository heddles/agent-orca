package apiserver

import (
	"net/http"
	"strings"
	"testing"

	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// withJudge adds an llm grader (and, when human, a human grader) to the gate's dataset.
func (f *gateFixture) withJudge(human bool) {
	f.gates.graders[1] = &postgresql.EvalGrader{ID: 1, TenantID: isoA, DatasetID: 1, Name: "judge", Kind: "llm"}
	f.gates.graders[2] = &postgresql.EvalGrader{ID: 2, TenantID: isoA, DatasetID: 1, Name: "exact", Kind: "contains"}
	if human {
		f.gates.graders[3] = &postgresql.EvalGrader{ID: 3, TenantID: isoA, DatasetID: 1, Name: "people", Kind: "human"}
	}
}

func TestPublishGate_UntrustedJudgeBlocksUnlessCalibrated(t *testing.T) {
	// No human labels at all: the judge is untrusted.
	f := newGateFixture(t)
	f.withJudge(false)
	f.evaluated(gateRows(false), f.hash, 7)
	rec := f.publish(tenancy.RoleBuilder, ``)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "untrusted_judge") {
		t.Fatalf("uncalibrated judge = %d %s", rec.Code, rec.Body)
	}

	// Labels, but a low kappa (half the verdicts flipped).
	f = newGateFixture(t)
	f.withJudge(true)
	f.gates.pairs = calibrationPairs(24, 12)
	f.evaluated(gateRows(false), f.hash, 7)
	if rec := f.publish(tenancy.RoleBuilder, ``); rec.Code != http.StatusConflict {
		t.Fatalf("low kappa = %d %s", rec.Code, rec.Body)
	}
	// An admin may accept it explicitly, audited as an override.
	if rec := f.publish(tenancy.RoleAdmin, overrideBody); rec.Code != http.StatusCreated || len(f.overrides()) != 1 {
		t.Fatalf("admin accept = %d %s, overrides %d", rec.Code, rec.Body, len(f.overrides()))
	}

	// Too few pairs, even if they all agree.
	f = newGateFixture(t)
	f.withJudge(true)
	f.gates.pairs = calibrationPairs(10, 0)
	f.evaluated(gateRows(false), f.hash, 7)
	if rec := f.publish(tenancy.RoleBuilder, ``); rec.Code != http.StatusConflict {
		t.Fatalf("too few pairs = %d %s", rec.Code, rec.Body)
	}

	// Calibrated: two disagreements over 24.
	f = newGateFixture(t)
	f.withJudge(true)
	f.gates.pairs = calibrationPairs(24, 2)
	f.evaluated(gateRows(false), f.hash, 7)
	if rec := f.publish(tenancy.RoleBuilder, ``); rec.Code != http.StatusCreated {
		t.Fatalf("calibrated = %d %s", rec.Code, rec.Body)
	}
}
