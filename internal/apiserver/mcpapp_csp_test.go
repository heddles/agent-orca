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
	"strings"
	"testing"
)

func TestInjectMCPAppCSPNonces(t *testing.T) {
	const in = `<!doctype html><html><head><style>body{}</style></head><body><script>console.log(1)</script></body></html>`
	nonce := "testnonce123"
	out, err := injectMCPAppCSPNonces([]byte(in), nonce)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, `nonce="testnonce123"`) {
		t.Fatalf("missing nonce in output: %s", s)
	}
}

func TestMcpAppCSPHeader(t *testing.T) {
	h := mcpAppCSPHeader("abc")
	if strings.Contains(h, "script-src 'unsafe-inline'") {
		t.Fatalf("script-src must not allow unsafe-inline: %s", h)
	}
	if !strings.Contains(h, "script-src 'nonce-abc'") {
		t.Fatalf("expected nonce in script-src: %s", h)
	}
}
