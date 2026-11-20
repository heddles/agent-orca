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

package redact

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"testing"
)

const tok = "ghu_SuperSecretAccessToken/with+odd=chars\"and\\slash"

func TestStringMasksAllForms(t *testing.T) {
	r := &Registry{}
	r.Register(tok)
	jsonForm, _ := json.Marshal(tok)
	for name, in := range map[string]string{
		"raw":      "Authorization: Bearer " + tok,
		"json":     "{\"error\":" + string(jsonForm) + "}",
		"query":    "https://x.example/?access_token=" + url.QueryEscape(tok),
		"twice":    tok + " and " + tok,
		"embedded": "x" + tok + "y",
	} {
		if got := r.String(in); strings.Contains(got, "SuperSecret") || !strings.Contains(got, Mask) {
			t.Errorf("%s: %q", name, got)
		}
	}
	if got := r.String("nothing to see"); got != "nothing to see" {
		t.Errorf("clean text changed: %q", got)
	}
	r.Forget(tok)
	if got := r.String(tok); got != tok {
		t.Error("forgotten secret still masked")
	}
}

func TestShortSecretsAreIgnoredAndLongestWins(t *testing.T) {
	r := &Registry{}
	r.Register("short")
	if r.String("short") != "short" {
		t.Error("a short value was registered")
	}
	r.Register("abcdefgh")
	r.Register("abcdefgh-longer-token")
	if got := r.String("t=abcdefgh-longer-token"); got != "t="+Mask {
		t.Errorf("got %q", got)
	}
}

func TestHandlerMasksMessagesAttrsErrorsAndGroups(t *testing.T) {
	Default.Register(tok)
	defer Default.Forget(tok)
	var out bytes.Buffer
	log := slog.New(Handler(slog.NewJSONHandler(&out, nil))).With("preset", tok)
	log.Info("calling with "+tok, "token", tok, "err", errors.New("bad "+tok), "n", 3,
		slog.Group("g", slog.String("inner", tok)), "any", fmt.Errorf("wrapped: %w", errors.New(tok)), "obj", map[string]string{"k": tok})
	if strings.Contains(out.String(), "SuperSecret") {
		t.Fatalf("log leaks the token: %s", out.String())
	}
	if !strings.Contains(out.String(), Mask) || !strings.Contains(out.String(), "\"n\":3") {
		t.Errorf("log = %s", out.String())
	}
}

func TestWriterMasks(t *testing.T) {
	Default.Register(tok)
	defer Default.Forget(tok)
	var out bytes.Buffer
	w := Writer(&out)
	n, err := w.Write([]byte("line with " + tok + "\n"))
	if err != nil || n != len("line with "+tok+"\n") {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if strings.Contains(out.String(), "SuperSecret") {
		t.Errorf("writer leaks: %q", out.String())
	}
}
