package apiserver

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// Dry run for writes: POST /api/resources/{kind}?dryRun=true and
// PUT /api/resources/{kind}/{ns}/{name}?dryRun=true run the real write handler
// (tenant checks, roles, ownership rules, label stamping, server-side apply and
// both kinds of webhook) with the API server's dryRun=All, so nothing is stored,
// no audit row is written and no last-modified annotation lands anywhere. The
// answer is reshaped:
//
//	200 {"valid":true,"object":{...as it would be stored...}}
//	4xx {"valid":false,"errors":["message", ...]}   (the status a real write gives)

type dryRunKey struct{}

// isDryRun reports whether r is being served as a dry run.
func isDryRun(r *http.Request) bool {
	v, _ := r.Context().Value(dryRunKey{}).(bool)
	return v
}

// withDryRun marks ctx as a dry run: the write below it is made with dryRun=All.
func withDryRun(ctx context.Context) context.Context {
	return context.WithValue(ctx, dryRunKey{}, true)
}

// mustURL parses a URL that is built in code.
func mustURL(raw string) *url.URL {
	u, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	return u
}

// asDryRun serves r through run as a dry run when it asks for one (?dryRun=true)
// and is not one already, and reports whether it did.
func asDryRun(w http.ResponseWriter, r *http.Request, run func(http.ResponseWriter, *http.Request)) bool {
	if isDryRun(r) || r.URL.Query().Get("dryRun") != "true" {
		return false
	}
	rec := &dryRunWriter{header: http.Header{}, status: http.StatusOK}
	run(rec, r.WithContext(context.WithValue(r.Context(), dryRunKey{}, true)))
	rec.finish(w)
	return true
}

// dryRunWriter buffers a handler's answer so it can be reshaped.
type dryRunWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (d *dryRunWriter) Header() http.Header         { return d.header }
func (d *dryRunWriter) WriteHeader(code int)        { d.status = code }
func (d *dryRunWriter) Write(b []byte) (int, error) { return d.body.Write(b) }

// webhookDenial pulls the message out of an admission denial.
var webhookDenial = regexp.MustCompile(`(?s)denied the request: (.*)$`)

func (d *dryRunWriter) finish(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	if d.status < http.StatusBadRequest {
		var obj map[string]any
		if json.Unmarshal(d.body.Bytes(), &obj) != nil {
			obj = map[string]any{}
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"valid": true, "object": obj})
		return
	}
	out := map[string]any{"valid": false, "errors": []string{d.errorMessage()}}
	var body map[string]any
	if json.Unmarshal(d.body.Bytes(), &body) == nil {
		if v, ok := body["currentResourceVersion"]; ok {
			out["currentResourceVersion"] = v
		}
	}
	w.WriteHeader(d.status)
	_ = json.NewEncoder(w).Encode(out)
}

// errorMessage is the human text of a failed answer, whether the handler wrote
// {"error": "..."} or plain text. A webhook denial is reduced to its own message.
func (d *dryRunWriter) errorMessage() string {
	msg := strings.TrimSpace(d.body.String())
	var body struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(d.body.Bytes(), &body) == nil && body.Error != "" {
		msg = body.Error
	}
	if m := webhookDenial.FindStringSubmatch(msg); m != nil {
		msg = strings.TrimSpace(m[1])
	}
	if msg == "" {
		msg = http.StatusText(d.status)
	}
	return msg
}
