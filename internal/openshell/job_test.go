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

package openshell

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// localExec runs "sandbox" commands in a local shell, so the job scripts are
// tested as shell, not as strings. The sandbox is a temporary directory.
type localExec struct {
	calls int
	err   error
}

func (l *localExec) Exec(ctx context.Context, _ string, command []string, opts ExecOptions) (*ExecResult, error) {
	l.calls++
	if l.err != nil {
		return nil, l.err
	}
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	for k, v := range opts.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Env = append(cmd.Env, "PATH=/usr/bin:/bin:/usr/sbin:/sbin")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if ee := (&exec.ExitError{}); errors.As(err, &ee) {
		code, err = ee.ExitCode(), nil
	}
	return &ExecResult{ExitCode: code, Stdout: out.Bytes(), Stderr: errb.Bytes()}, err
}

func useTempJobRoot(t *testing.T) {
	t.Helper()
	old := jobRoot
	jobRoot = t.TempDir()
	t.Cleanup(func() { jobRoot = old })
}

func waitDone(t *testing.T, ex Executor, id string) JobStatus {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	st, err := WaitJob(ctx, ex, "sb", id, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestJobRunsToCompletionAndReportsOutputAndExitCode(t *testing.T) {
	useTempJobRoot(t)
	ex := &localExec{}
	if err := StartJob(context.Background(), ex, "sb", "ok-1", "echo hello; echo oops >&2; exit 0", nil, ""); err != nil {
		t.Fatal(err)
	}
	st := waitDone(t, ex, "ok-1")
	if st.Phase != JobExited || st.ExitCode != 0 || !strings.Contains(st.Output, "hello") || !strings.Contains(st.Output, "oops") {
		t.Errorf("status = %+v, want exited 0 with stdout and stderr", st)
	}

	if err := StartJob(context.Background(), ex, "sb", "fail-1", "echo about to fail; exit 7", nil, ""); err != nil {
		t.Fatal(err)
	}
	st = waitDone(t, ex, "fail-1")
	if st.Phase != JobExited || st.ExitCode != 7 || !strings.Contains(st.Output, "about to fail") {
		t.Errorf("status = %+v, want exit 7", st)
	}
}

func TestJobIsDetachedFromTheStartingCommand(t *testing.T) {
	useTempJobRoot(t)
	ex := &localExec{}
	start := time.Now()
	if err := StartJob(context.Background(), ex, "sb", "slow-1", "sleep 3; echo done", nil, ""); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("StartJob took %v: it waited for the job instead of returning", took)
	}
	st, err := PollJob(context.Background(), ex, "sb", "slow-1")
	if err != nil || st.Phase != JobRunning {
		t.Fatalf("poll = %+v, %v; want running", st, err)
	}
	if st := waitDone(t, ex, "slow-1"); st.Phase != JobExited || !strings.Contains(st.Output, "done") {
		t.Errorf("after waiting: %+v", st)
	}
}

func TestStartingAJobTwiceRunsItOnce(t *testing.T) {
	useTempJobRoot(t)
	ex := &localExec{}
	script := "echo ran >> " + jobRoot + "/counter"
	for range 3 {
		if err := StartJob(context.Background(), ex, "sb", "once-1", script, nil, ""); err != nil {
			t.Fatal(err)
		}
	}
	waitDone(t, ex, "once-1")
	out, err := (&localExec{}).Exec(context.Background(), "sb", []string{"sh", "-c", "wc -l < " + jobRoot + "/counter"}, ExecOptions{})
	if err != nil || strings.TrimSpace(string(out.Stdout)) != "1" {
		t.Errorf("the script ran %q times, want 1 (a retried start after a crash must not rerun the step)", strings.TrimSpace(string(out.Stdout)))
	}
}

func TestJobEnvironmentAndWorkDir(t *testing.T) {
	useTempJobRoot(t)
	ex := &localExec{}
	dir := t.TempDir()
	env := map[string]string{
		"PLAIN":    "value",
		"QUOTES":   `it's "quoted" $HOME \ back`,
		"MULTI":    "line one\nline two",
		"SUBSHELL": "$(echo pwned)`echo pwned`",
	}
	script := `echo "plain=$PLAIN"; echo "quotes=$QUOTES"; printf 'multi=%s\n' "$MULTI"; echo "sub=$SUBSHELL"; pwd`
	if err := StartJob(context.Background(), ex, "sb", "env-1", script, env, dir); err != nil {
		t.Fatal(err)
	}
	st := waitDone(t, ex, "env-1")
	for _, want := range []string{"plain=value", `quotes=it's "quoted" $HOME \ back`, "multi=line one\nline two", "sub=$(echo pwned)`echo pwned`"} {
		if !strings.Contains(st.Output, want) {
			t.Errorf("output lacks %q:\n%s", want, st.Output)
		}
	}
	if strings.Contains(st.Output, "sub=pwned") {
		t.Error("an env value was evaluated by the shell")
	}
	if !strings.Contains(st.Output, strings.TrimPrefix(dir, "/private")) && !strings.Contains(st.Output, dir) {
		t.Errorf("not run in %s:\n%s", dir, st.Output)
	}
}

func TestJobThatCannotEnterItsWorkDirFails(t *testing.T) {
	useTempJobRoot(t)
	ex := &localExec{}
	if err := StartJob(context.Background(), ex, "sb", "nodir-1", "echo should not run", nil, "/no/such/dir"); err != nil {
		t.Fatal(err)
	}
	st := waitDone(t, ex, "nodir-1")
	if st.Phase != JobExited || st.ExitCode == 0 || strings.Contains(st.Output, "should not run") {
		t.Errorf("status = %+v, want a non-zero exit without running the script", st)
	}
}

func TestPollOfAnUnknownJobIsMissing(t *testing.T) {
	useTempJobRoot(t)
	st, err := PollJob(context.Background(), &localExec{}, "sb", "never-1")
	if err != nil || st.Phase != JobMissing {
		t.Errorf("status = %+v, %v; want missing", st, err)
	}
}

func TestAJobWhoseProcessDiedWithoutAnExitCodeIsLost(t *testing.T) {
	useTempJobRoot(t)
	ex := &localExec{}
	if err := StartJob(context.Background(), ex, "sb", "lost-1", "sleep 30", nil, ""); err != nil {
		t.Fatal(err)
	}
	// Kill it the way an OOM killer would: the wrapper dies before it can write the exit code.
	if _, err := ex.Exec(context.Background(), "sb", []string{"sh", "-c", `pid=$(cat ` + jobRoot + `/lost-1/pid); kill -KILL -- -$pid 2>/dev/null; kill -KILL $pid 2>/dev/null; pkill -KILL -P $pid 2>/dev/null; sleep 0.3; true`}, ExecOptions{}); err != nil {
		t.Fatal(err)
	}
	st, err := PollJob(context.Background(), ex, "sb", "lost-1")
	if err != nil {
		t.Fatal(err)
	}
	if st.Phase != JobLost && st.Phase != JobExited {
		t.Errorf("status = %+v, want lost (or exited with a signal code)", st)
	}
	if st.Phase == JobExited && st.ExitCode == 0 {
		t.Errorf("a killed job reported success: %+v", st)
	}
}

func TestStopJob(t *testing.T) {
	useTempJobRoot(t)
	ex := &localExec{}
	if err := StartJob(context.Background(), ex, "sb", "stop-1", "sleep 60", nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := StopJob(context.Background(), ex, "sb", "stop-1"); err != nil {
		t.Fatal(err)
	}
	st := waitDone(t, ex, "stop-1")
	if st.Phase == JobRunning {
		t.Errorf("status = %+v, want the job stopped", st)
	}
	// Stopping a job that never ran, or already ended, is harmless.
	if err := StopJob(context.Background(), ex, "sb", "never-1"); err != nil {
		t.Errorf("stopping an unknown job: %v", err)
	}
}

func TestJobIDsAreValidated(t *testing.T) {
	ex := &localExec{}
	for _, id := range []string{"", "../etc", "a/b", "UPPER", "-lead", "with space", "x; rm -rf /", strings.Repeat("a", 64)} {
		if err := StartJob(context.Background(), ex, "sb", id, "true", nil, ""); err == nil {
			t.Errorf("StartJob accepted id %q", id)
		}
		if _, err := PollJob(context.Background(), ex, "sb", id); err == nil {
			t.Errorf("PollJob accepted id %q", id)
		}
		if err := StopJob(context.Background(), ex, "sb", id); err == nil {
			t.Errorf("StopJob accepted id %q", id)
		}
	}
	if ex.calls != 0 {
		t.Errorf("%d commands were run for invalid ids", ex.calls)
	}
}

func TestExecErrorsAndNonZeroLaunchersSurface(t *testing.T) {
	boom := errors.New("gateway unreachable")
	if err := StartJob(context.Background(), &localExec{err: boom}, "sb", "x-1", "true", nil, ""); !errors.Is(err, boom) {
		t.Errorf("StartJob error = %v", err)
	}
	if _, err := PollJob(context.Background(), &localExec{err: boom}, "sb", "x-1"); !errors.Is(err, boom) {
		t.Errorf("PollJob error = %v", err)
	}
}

func TestParseJobPollRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"", "no separator", "STATE\n---\nout", "STATE nonsense\n---\n", "STATE exited\n---\n", "STATE exited abc\n---\n"} {
		if _, err := parseJobPoll(bad); err == nil {
			t.Errorf("parseJobPoll(%q) accepted", bad)
		}
	}
	st, err := parseJobPoll("STATE exited 3\n---\nline1\nline2\n")
	if err != nil || st.ExitCode != 3 || st.Output != "line1\nline2" {
		t.Errorf("parse = %+v, %v", st, err)
	}
}

func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{"": `''`, "plain": `'plain'`, "it's": `'it'\''s'`, "a b": `'a b'`} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %s, want %s", in, got, want)
		}
	}
	if !ValidEnvName("GIT_TOKEN") || ValidEnvName("1BAD") || ValidEnvName("a-b") || ValidEnvName("A=B") || ValidEnvName("") {
		t.Error("ValidEnvName wrong")
	}
}
