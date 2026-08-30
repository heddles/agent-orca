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

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"
)

// maxJSONBodyBytes bounds how much of a response body we read into memory when
// building a diagnostic error message. Successful decodes stream straight from
// resp.Body; only the error paths (non-2xx, non-JSON) read a snippet.
const maxJSONBodyBytes = 200

// decodeJSON decodes an HTTP response that must be JSON into v. Unlike a bare
// json.Decode, it produces a *diagnostic* error when the server did not return
// JSON — the cause of the cryptic "invalid character '<' looking for beginning
// of value" messages reported when --endpoint points at the wrong surface
// (e.g. the UI proxy on :8080, whose SPA fallback serves index.html for unknown
// paths). It also surfaces the HTTP status for non-2xx responses so the user
// knows *why* (401, 404, …) instead of only that parsing failed.
func decodeJSON(resp *http.Response, v any, wantStatus int) error {
	if resp == nil {
		return errors.New("no response")
	}
	ct := resp.Header.Get("Content-Type")
	u := requestURL(resp)

	if resp.StatusCode != wantStatus {
		return fmt.Errorf("request to %s failed (HTTP %d, Content-Type %q)%s",
			u, resp.StatusCode, ct, snippet(resp.Body))
	}

	// A successful status with a non-JSON body is the SPA-fallback / wrong-
	// endpoint case: fail fast with an actionable message rather than letting
	// the JSON decoder emit "invalid character '<'".
	if !strings.HasPrefix(ct, "application/json") {
		return fmt.Errorf("non-JSON response (HTTP %d, Content-Type %q) from %s; "+
			"is --endpoint or --acp-endpoint correct? the External Task API is :8084 and the "+
			"ACP API is :8000 (the UI proxy on :8080 serves HTML, not JSON)", resp.StatusCode, ct, u) //nolint:lll
	}

	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		return fmt.Errorf("decoding %s: %w (HTTP %d, Content-Type %q)%s",
			responsePath(resp), err, resp.StatusCode, ct, snippet(resp.Body))
	}
	return nil
}

// snippet reads up to maxJSONBodyBytes from r and formats it for an error message.
func snippet(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, maxJSONBodyBytes))
	s := strings.TrimSpace(string(b))
	if s == "" {
		return ""
	}
	if len(s) > maxJSONBodyBytes {
		s = s[:maxJSONBodyBytes] + "…"
	}
	return " — body: " + singleLine(s)
}

// singleLine collapses a body to a single line for compact error display.
func singleLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	s = strings.TrimSpace(s)
	const maxLen = 160
	if len(s) > maxLen {
		return s[:maxLen] + "…"
	}
	return s
}

func requestURL(resp *http.Response) string {
	if resp.Request != nil && resp.Request.URL != nil {
		return resp.Request.URL.Redacted()
	}
	return "<unknown>"
}

func responsePath(resp *http.Response) string {
	if resp.Request != nil && resp.Request.URL != nil {
		return resp.Request.URL.Path
	}
	return "<unknown>"
}

// tableWriter is a minimal, dependency-free fixed-width table renderer for
// human-readable command output (header row + dashed separator + left-aligned rows).
type tableWriter struct {
	headerRow []string
	rows      [][]string
}

func newTableWriter() *tableWriter { return &tableWriter{} }

func (t *tableWriter) header(cols ...string) *tableWriter {
	t.headerRow = cols
	return t
}

// row appends a data row. It does not return the writer (callers render once at
// the end), which keeps the chaining to a single header setup call.
func (t *tableWriter) row(cols ...string) {
	t.rows = append(t.rows, cols)
}

// render writes the table to w. Empty tables (no rows and no header) print
// nothing; a header with no rows prints just the header.
func (t *tableWriter) render(w io.Writer) error {
	cols := len(t.headerRow)
	if cols == 0 && len(t.rows) > 0 {
		cols = len(t.rows[0])
	}
	widths := make([]int, cols)
	for i, h := range t.headerRow {
		widths[i] = max(widths[i], utf8.RuneCountInString(h))
	}
	for _, r := range t.rows {
		for i := 0; i < cols && i < len(r); i++ {
			widths[i] = max(widths[i], utf8.RuneCountInString(r[i]))
		}
	}
	fmtRow := func(cells []string) {
		for i := 0; i < cols; i++ {
			cell := ""
			if i < len(cells) {
				cell = cells[i]
			}
			_, _ = fmt.Fprint(w, ljust(cell, widths[i]))
			if i < cols-1 {
				_, _ = fmt.Fprint(w, "  ")
			}
		}
		_, _ = fmt.Fprintln(w)
	}
	if len(t.headerRow) > 0 {
		fmtRow(t.headerRow)
		for i := 0; i < cols; i++ {
			_, _ = fmt.Fprint(w, strings.Repeat("-", widths[i]))
			if i < cols-1 {
				_, _ = fmt.Fprint(w, "  ")
			}
		}
		_, _ = fmt.Fprintln(w)
	}
	for _, r := range t.rows {
		fmtRow(r)
	}
	return nil
}

func ljust(s string, width int) string {
	n := max(0, width-utf8.RuneCountInString(s))
	return s + strings.Repeat(" ", n)
}
