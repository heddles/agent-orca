package apiserver

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/state"
)

// resumeWithAnswer answers the question of a run that is WaitingForInput.
//
// A warm model-router only looks for a clarify answer at startup, so the run is
// not changed in place: a continuation run starts from the prior run's
// checkpoint (PriorRunRef) and its router injects the answer. The original run
// is marked Succeeded with a pointer to the continuation. Every way a person
// can answer (the UI, ACP, a chat channel) goes through here, so they all
// resume a run the same way.
func resumeWithAnswer(ctx context.Context, c client.Client, store state.Store, run *agentorcav1alpha1.AgentRun, answer string) (*agentorcav1alpha1.AgentRun, error) {
	input := run.Spec.Input
	hasCheckpoint := false
	if store != nil {
		key := fmt.Sprintf("agentorca/runs/%s/state", run.Name)
		if msgs, err := runState(store, run).LoadMessages(ctx, key); err == nil && len(msgs) > 0 {
			hasCheckpoint = true
		}
	}
	if !hasCheckpoint {
		// No checkpoint (the controller's safety-net path): carry the exchange in
		// the input so the continuation has the whole context.
		input = fmt.Sprintf(
			"%s\n\n---\nPrevious attempt asked: %s\nHuman answered: %s\n---\nPlease proceed with the above information.",
			run.Spec.Input, run.Status.ClarifyQuestion, answer)
	}

	continuation := &agentorcav1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: run.Name + "-cont-",
			Namespace:    run.Namespace,
			Labels:       run.Labels, // deployment, session and source, so a warm pod is claimed
		},
		Spec: agentorcav1alpha1.AgentRunSpec{
			AgentRef:        run.Spec.AgentRef,
			Input:           input,
			Timeout:         run.Spec.Timeout,
			RestartPolicy:   run.Spec.RestartPolicy,
			PriorRunRef:     run.Name,
			WorkflowSandbox: run.Spec.WorkflowSandbox,
		},
	}
	// The same person's run: it keeps the invoker, so a "connect your account"
	// pause resumes with the connection just made.
	if by := run.Annotations[AnnotationCreatedBy]; by != "" {
		continuation.Annotations = map[string]string{AnnotationCreatedBy: by}
	}
	if err := c.Create(ctx, continuation); err != nil {
		return nil, fmt.Errorf("creating continuation run: %w", err)
	}
	if store != nil {
		// Under the continuation's name: its router starts fresh and looks there.
		key := fmt.Sprintf("clarify-answer:%s:%s", run.Namespace, continuation.Name)
		if err := runState(store, continuation).SaveAnswer(ctx, key, answer, time.Hour); err != nil {
			slog.Warn("saving the clarify answer", "run", continuation.Name, "err", err)
		}
	}
	patch := client.MergeFrom(run.DeepCopy())
	run.Status.Phase = agentorcav1alpha1.AgentRunPhaseSucceeded
	run.Status.ClarifyAnswer = answer
	run.Status.ContinuationRunRef = continuation.Name
	if err := c.Status().Patch(ctx, run, patch); err != nil {
		slog.Warn("pointing the original run at its continuation", "run", run.Name, "err", err)
	}
	return continuation, nil
}
