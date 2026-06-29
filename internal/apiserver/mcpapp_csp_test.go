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
