package runoutput

import (
	"context"
	"log/slog"
	"time"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/state"
)

// A run's answer is not kept in the AgentRun status: every status write is an
// etcd write, and an answer can be large. It lives in the state store, keyed by
// run, and is copied into the archived run (PostgreSQL) when the run is
// archived. Save and Load are the only code that knows the key.

// Scope is the state-store KV scope holding run outputs.
const Scope = "run-output"

// TTL is how long an answer stays in the state store. The archived run keeps it
// for as long as archived-run retention allows.
const TTL = 30 * 24 * time.Hour

// Gone is archived in place of the answer of a succeeded run whose stored answer
// could not be found when it was archived. It is a note, not an answer.
const Gone = "[No answer is stored for this run: it was not in the state store when the run was archived.]"

// StatusMaxBytes caps the answer copied into AgentRun.status.output when no
// state store can take it (the fallback), so status stays small.
const StatusMaxBytes = 10240

func key(namespace, name string) string { return namespace + "/" + name }

// Save stores a run's answer. st must be the run's tenant view of the state
// store. An error means the answer was not stored.
func Save(ctx context.Context, st state.Store, namespace, name, text string) error {
	return st.SaveKV(ctx, Scope, key(namespace, name), []byte(Clip(text)), TTL)
}

// Load returns a run's stored answer; ok is false when none is stored.
func Load(ctx context.Context, st state.Store, namespace, name string) (string, bool) {
	if st == nil {
		return "", false
	}
	b, err := st.LoadKV(ctx, Scope, key(namespace, name))
	if err != nil {
		slog.Warn("loading a run output", "run", key(namespace, name), "err", err)
		return "", false
	}
	if b == nil {
		return "", false
	}
	return string(b), true
}

// Of is a run's answer: the stored one, else what an older run (or one that
// could not be stored) kept in status.output.
func Of(ctx context.Context, st state.Store, run *agentorcav1alpha1.AgentRun) string {
	// Only a run that succeeded has a stored answer; skip the store for the rest
	// (a stream handler asks while the run is still going).
	if run.Status.Phase != agentorcav1alpha1.AgentRunPhaseSucceeded {
		return run.Status.Output
	}
	if out, ok := Load(ctx, st, run.Namespace, run.Name); ok {
		return out
	}
	return run.Status.Output
}

// Finish stores out for a run that just completed and returns what belongs in
// status.output: nothing when the answer was stored, a clipped copy when it
// could not be, so an answer is never lost.
func Finish(ctx context.Context, st state.Store, namespace, name, out string) string {
	if st == nil {
		return ClipTo(out, StatusMaxBytes)
	}
	err := Save(ctx, st, namespace, name, out)
	if err == nil {
		return ""
	}
	slog.Warn("storing a run output, keeping it in status", "run", key(namespace, name), "err", err)
	return ClipTo(out, StatusMaxBytes)
}
