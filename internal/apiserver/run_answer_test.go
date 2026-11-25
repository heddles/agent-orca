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
	"errors"
	"testing"
	"time"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/runoutput"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type fakeAnswers struct {
	row   *postgresql.RunAnswer
	err   error
	asked int
}

func (f *fakeAnswers) GetRunAnswer(context.Context, string, string) (*postgresql.RunAnswer, error) {
	f.asked++
	return f.row, f.err
}

func TestAnswerComesFromTheArchiveWhenNothingElseHasIt(t *testing.T) {
	done := time.Date(2026, 11, 25, 16, 4, 20, 0, time.UTC)
	later := done.Add(time.Hour)
	archived := func(output string, at *time.Time) *postgresql.RunAnswer {
		return &postgresql.RunAnswer{Output: output, Phase: "Succeeded", CompletionTime: at}
	}
	run := func(phase agentorcav1alpha1.AgentRunPhase, status string) *agentorcav1alpha1.AgentRun {
		r := &agentorcav1alpha1.AgentRun{ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "ns"}}
		r.Status.Phase = phase
		r.Status.Output = status
		at := metav1.NewTime(done)
		r.Status.CompletionTime = &at
		return r
	}
	for _, tc := range []struct {
		name    string
		run     *agentorcav1alpha1.AgentRun
		archive *fakeAnswers
		want    string
		asked   int
	}{
		{"archive has it", run("Succeeded", ""), &fakeAnswers{row: archived("kept", &done)}, "kept", 1},
		{"status wins, archive not asked", run("Succeeded", "in status"), &fakeAnswers{row: archived("kept", &done)}, "in status", 0},
		{"a failed run has no answer to look up", run("Failed", ""), &fakeAnswers{row: archived("kept", &done)}, "", 0},
		{"not archived", run("Succeeded", ""), &fakeAnswers{}, "", 1},
		{"archive error", run("Succeeded", ""), &fakeAnswers{err: errors.New("down")}, "", 1},
		{"the note that nothing was stored is not an answer", run("Succeeded", ""), &fakeAnswers{row: archived(runoutput.Gone, &done)}, "", 1},
		{"an earlier run of the same name", run("Succeeded", ""), &fakeAnswers{row: archived("old", &later)}, "", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := answerOf(context.Background(), nil, tc.archive, tc.run); got != tc.want {
				t.Errorf("answer = %q, want %q", got, tc.want)
			}
			if tc.archive.asked != tc.asked {
				t.Errorf("archive asked %d times, want %d", tc.archive.asked, tc.asked)
			}
		})
	}
}

func TestAnswerWithoutAnArchive(t *testing.T) {
	s := &UIServer{}
	r := &agentorcav1alpha1.AgentRun{}
	r.Status.Phase = agentorcav1alpha1.AgentRunPhaseSucceeded
	if got := s.runAnswer(context.Background(), r); got != "" {
		t.Errorf("answer = %q, want none", got)
	}
}
