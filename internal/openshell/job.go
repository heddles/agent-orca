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

// Jobs: commands that run detached inside a sandbox.
//
// A workflow step can run for an hour. Holding a streaming Exec call open for
// that long ties the step to one connection to the gateway and to the operator
// process holding it: a gateway restart, a network blip or an operator restart
// would lose the step, or worse, lose track of one that is still running. So a
// job is started with a short Exec that returns at once, and its state lives in
// files inside the sandbox (its script, its pid, its output, and its exit code
// when it ends). Anyone, including a restarted operator, can read that state
// back with another short Exec. Starting a job twice with the same id runs it
// once, so a retried start after a crash is safe.
//
// Only the sandbox itself ending takes a job with it; the caller then sees
// JobMissing or JobLost and fails the step so it can be rerun.

import (
	"context"
	"encoding/base64"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Executor is the part of the gateway jobs need.
type Executor interface {
	Exec(ctx context.Context, sandbox string, command []string, opts ExecOptions) (*ExecResult, error)
}

// JobPhase is where a job stands.
type JobPhase string

// Job phases.
const (
	// JobRunning means the job's process is alive.
	JobRunning JobPhase = "running"
	// JobExited means the job finished; ExitCode says how.
	JobExited JobPhase = "exited"
	// JobLost means the job started but its process is gone without an exit code:
	// it was killed from outside (an out-of-memory kill, a sandbox restart).
	JobLost JobPhase = "lost"
	// JobMissing means no such job was ever started in this sandbox.
	JobMissing JobPhase = "missing"
)

// JobStatus is a snapshot of a job.
type JobStatus struct {
	Phase    JobPhase
	ExitCode int
	// Output is the tail of what the job printed, stdout and stderr together.
	Output string
}

// jobRoot is where jobs keep their files in the sandbox (/tmp is writable in
// every sandbox policy). Tests point it at a temporary directory.
var jobRoot = "/tmp/.agentorca-jobs"

// jobOutputBytes is how much of the output tail a poll returns.
const jobOutputBytes = 12000

var jobIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// startScript launches the job. It is idempotent: a job that was already
// started is left alone. setsid puts the job in its own session so it outlives
// the exec session that started it; nohup is the fallback where setsid is absent.
const startScript = `set -eu
id="$1"; b64="$2"; root="$3"
dir="$root/$id"
mkdir -p "$dir"
if [ -e "$dir/pid" ]; then echo already-started; exit 0; fi
printf '%s' "$b64" | base64 -d > "$dir/script.sh"
run='sh "$1"; code=$?; echo "$code" > "$2.tmp"; mv "$2.tmp" "$2"'
if command -v setsid >/dev/null 2>&1; then
  setsid sh -c "$run" sh "$dir/script.sh" "$dir/exit" > "$dir/out" 2>&1 < /dev/null &
else
  nohup sh -c "$run" sh "$dir/script.sh" "$dir/exit" > "$dir/out" 2>&1 < /dev/null &
fi
echo $! > "$dir/pid"
echo started
`

// pollScript prints "STATE <phase> [code]", a separator line and the output tail.
// A process that has just exited may not have written its exit code yet, so a
// dead process with no exit file is checked once more after a moment.
const pollScript = `id="$1"; root="$2"; max="$3"
dir="$root/$id"
state=missing
if [ -f "$dir/exit" ]; then
  state="exited $(cat "$dir/exit")"
elif [ -f "$dir/pid" ]; then
  if kill -0 "$(cat "$dir/pid")" 2>/dev/null; then
    state=running
  else
    sleep 1
    if [ -f "$dir/exit" ]; then state="exited $(cat "$dir/exit")"; else state=lost; fi
  fi
fi
echo "STATE $state"
echo "---"
tail -c "$max" "$dir/out" 2>/dev/null || true
`

// stopScript ends a job's process group, if it is still running.
const stopScript = `id="$1"; root="$2"
pid="$(cat "$root/$id/pid" 2>/dev/null || true)"
[ -n "$pid" ] || exit 0
kill -TERM -- "-$pid" 2>/dev/null || kill -TERM "$pid" 2>/dev/null || true
`

// StartJob starts script (run by sh) as a detached job named id in the sandbox.
// env is exported to the script; workDir, if set, is its working directory.
func StartJob(ctx context.Context, ex Executor, sandbox, id, script string, env map[string]string, workDir string) error {
	if !jobIDPattern.MatchString(id) {
		return fmt.Errorf("invalid job id %q", id)
	}
	full := jobPrologue(env, workDir) + script
	b64 := base64.StdEncoding.EncodeToString([]byte(full))
	res, err := ex.Exec(ctx, sandbox, []string{"sh", "-c", startScript, "sh", id, b64, jobRoot}, ExecOptions{NoLoginShell: true})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("starting job %s in sandbox %s failed (exit %d): %s", id, sandbox, res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}
	return nil
}

// jobPrologue exports env and enters workDir ahead of the script. Values are
// single-quoted, so they may hold anything, newlines and quotes included.
func jobPrologue(env map[string]string, workDir string) string {
	var b strings.Builder
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "export %s=%s\n", k, shellQuote(env[k]))
	}
	if workDir != "" {
		fmt.Fprintf(&b, "cd %s || exit 1\n", shellQuote(workDir))
	}
	return b.String()
}

// shellQuote quotes s as one word for sh.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidEnvName reports whether name is usable as an environment variable name
// in a job (it is written into a shell script, so it must be a plain name).
func ValidEnvName(name string) bool { return envNamePattern.MatchString(name) }

// PollJob reads a job's state and the tail of its output.
func PollJob(ctx context.Context, ex Executor, sandbox, id string) (JobStatus, error) {
	if !jobIDPattern.MatchString(id) {
		return JobStatus{}, fmt.Errorf("invalid job id %q", id)
	}
	res, err := ex.Exec(ctx, sandbox, []string{"sh", "-c", pollScript, "sh", id, jobRoot, strconv.Itoa(jobOutputBytes)}, ExecOptions{NoLoginShell: true})
	if err != nil {
		return JobStatus{}, err
	}
	if res.ExitCode != 0 {
		return JobStatus{}, fmt.Errorf("polling job %s in sandbox %s failed (exit %d): %s", id, sandbox, res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}
	return parseJobPoll(string(res.Stdout))
}

func parseJobPoll(out string) (JobStatus, error) {
	head, tail, found := strings.Cut(out, "\n---\n")
	if !found {
		return JobStatus{}, fmt.Errorf("unexpected job poll output %q", truncateRunes(out, 120))
	}
	fields := strings.Fields(strings.TrimPrefix(strings.TrimSpace(head), "STATE"))
	if len(fields) == 0 {
		return JobStatus{}, fmt.Errorf("unexpected job state in %q", truncateRunes(out, 120))
	}
	st := JobStatus{Phase: JobPhase(fields[0]), Output: strings.TrimRight(tail, "\n")}
	switch st.Phase {
	case JobRunning, JobLost, JobMissing:
	case JobExited:
		if len(fields) < 2 {
			return JobStatus{}, fmt.Errorf("job exited without a code in %q", truncateRunes(out, 120))
		}
		code, err := strconv.Atoi(fields[1])
		if err != nil {
			return JobStatus{}, fmt.Errorf("bad job exit code %q", fields[1])
		}
		st.ExitCode = code
	default:
		return JobStatus{}, fmt.Errorf("unknown job phase %q", fields[0])
	}
	return st, nil
}

func truncateRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// StopJob asks a running job to stop. A job that is not running is left alone.
func StopJob(ctx context.Context, ex Executor, sandbox, id string) error {
	if !jobIDPattern.MatchString(id) {
		return fmt.Errorf("invalid job id %q", id)
	}
	_, err := ex.Exec(ctx, sandbox, []string{"sh", "-c", stopScript, "sh", id, jobRoot}, ExecOptions{NoLoginShell: true})
	return err
}

// WaitJob polls a job every interval until it is no longer running, or ctx ends.
// The controller does not use it (it polls once per reconcile); it is for
// callers that want to block, such as the sandbox checkout.
func WaitJob(ctx context.Context, ex Executor, sandbox, id string, interval time.Duration) (JobStatus, error) {
	for {
		st, err := PollJob(ctx, ex, sandbox, id)
		if err != nil || st.Phase != JobRunning {
			return st, err
		}
		select {
		case <-ctx.Done():
			return st, ctx.Err()
		case <-time.After(interval):
		}
	}
}
