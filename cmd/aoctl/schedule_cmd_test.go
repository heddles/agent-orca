package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestScheduleCommands(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.RequestURI()+" auth="+r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == schedulesPath:
			_, _ = w.Write([]byte(`[{"name":"digest","namespace":"team-a","target":"helper","targetKind":"agent",` +
				`"schedule":"0 8 * * 1-5","timeZone":"Europe/Berlin","state":"active","nextRun":"2026-11-16T07:00:00Z",` +
				`"lastResult":{"status":"succeeded"},"ownership":{"owner":"ui"}}]`))
		case r.URL.Path == "/api/schedules/team-a/digest/run":
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"kind":"AgentRun","name":"digest-m-x1","namespace":"team-a"}`))
		case r.URL.Path == "/api/schedules/team-a/digest/suspend", r.URL.Path == "/api/schedules/team-a/digest/resume":
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	base := []string{"--token", "tok", "--ui-endpoint", srv.URL}
	run := func(args ...string) string {
		t.Helper()
		out, _, err := runCLI(t, nil, append(append([]string{"schedule"}, args...), base...)...)
		if err != nil {
			t.Fatalf("schedule %v: %v", args, err)
		}
		return out
	}
	if out := run("list"); !strings.Contains(out, "digest") || !strings.Contains(out, "0 8 * * 1-5 Europe/Berlin") {
		t.Errorf("list = %q", out)
	}
	if out := run("get", "digest"); !strings.Contains(out, "team-a/digest") || !strings.Contains(out, "owner: ui") {
		t.Errorf("get = %q", out)
	}
	if out := run("suspend", "digest"); !strings.Contains(out, "suspended team-a/digest") {
		t.Errorf("suspend = %q", out)
	}
	if out := run("resume", "digest"); !strings.Contains(out, "resumed") {
		t.Errorf("resume = %q", out)
	}
	if out := run("run", "digest"); !strings.Contains(out, "digest-m-x1") {
		t.Errorf("run = %q", out)
	}
	if _, _, err := runCLI(t, nil, append([]string{"schedule", "get", "nope"}, base...)...); err == nil {
		t.Error("an unknown schedule must fail")
	}
	for _, c := range calls {
		if !strings.Contains(c, "auth=Bearer tok") {
			t.Errorf("call without the token: %s", c)
		}
	}
}
