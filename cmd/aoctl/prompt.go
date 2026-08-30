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
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// loginMethodOAuth and loginMethodOIDC are the auth-method values wired through
// the selection picker and the --auth-method flag.
const (
	loginMethodOAuth = "oauth"
	loginMethodOIDC  = "oidc"
)

// promptOption is a single row in an interactive selection menu.
type promptOption struct {
	Value string // canonical value returned when this option is chosen
	// Label is the description shown beside the option's number.
	Label string
}

// promptSelection renders a numbered menu to out and reads a choice from r.
// It re-prompts until a valid selection is made. Returns the chosen option's
// Value. This mirrors the selection-based menu the UI login picker presents.
func promptSelection(out io.Writer, r *bufio.Reader, title string, opts []promptOption) (string, error) {
	if len(opts) == 0 {
		return "", fmt.Errorf("no options to present")
	}
	for {
		_, _ = fmt.Fprintln(out)
		_, _ = fmt.Fprintln(out, title)
		_, _ = fmt.Fprintln(out)
		for i, o := range opts {
			_, _ = fmt.Fprintf(out, "  %d. %s\n", i+1, o.Label)
		}
		_, _ = fmt.Fprintln(out)
		_, _ = fmt.Fprintf(out, "Select an option [1-%d]: ", len(opts))
		if err := flushWriter(out); err != nil {
			return "", err
		}
		line, err := r.ReadString('\n')
		if err != nil && line == "" {
			return "", fmt.Errorf("reading selection: %w", err)
		}
		line = strings.TrimSpace(strings.TrimRight(line, "\r"))
		if line == "" {
			continue
		}
		// Allow entering the value directly as well as the index.
		for i, o := range opts {
			if line == o.Value {
				return o.Value, nil
			}
			if n, perr := strconv.Atoi(line); perr == nil && n == i+1 {
				return o.Value, nil
			}
		}
		_, _ = fmt.Fprintf(out, "  %q is not a valid choice.\n", line)
	}
}

// promptLine writes label (no trailing newline) to out and reads a single line
// from r, trimming surrounding whitespace. It is used for non-secret inputs.
func promptLine(out io.Writer, r *bufio.Reader, label string) (string, error) {
	_, _ = fmt.Fprint(out, label)
	if err := flushWriter(out); err != nil {
		return "", err
	}
	line, err := r.ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("reading input: %w", err)
	}
	return strings.TrimSpace(strings.TrimRight(line, "\r")), err
}

// promptSecret prompts for sensitive input. When stdin is a real terminal it
// reads without echo (readPassword, e.g. term.ReadPassword); otherwise it reads
// a line from r — which makes it fully testable with an injected buffer. The
// returned value is trimmed of surrounding whitespace.
func promptSecret(out io.Writer, r *bufio.Reader, label string, isTerminal func() bool, readPassword func(int) ([]byte, error)) (string, error) { //nolint:lll
	_, _ = fmt.Fprint(out, label)
	if err := flushWriter(out); err != nil {
		return "", err
	}
	if isTerminal != nil && isTerminal() && readPassword != nil {
		b, err := readPassword(int(os.Stdin.Fd()))
		_, _ = fmt.Fprintln(out) // echo is off; terminate the prompt line after the secret
		if err != nil {
			return "", fmt.Errorf("reading secret: %w", err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	line, err := r.ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("reading secret: %w", err)
	}
	return strings.TrimSpace(strings.TrimRight(line, "\r")), err
}

// flushWriter calls Flush on the writer if it implements it, otherwise is a no-op.
func flushWriter(w io.Writer) error {
	if f, ok := w.(interface{ Flush() error }); ok {
		return f.Flush()
	}
	return nil
}
