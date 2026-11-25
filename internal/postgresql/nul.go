package postgresql

import (
	"bytes"
	"strings"
)

// PostgreSQL refuses NUL: a JSONB value cannot hold the escape \u0000 (SQLSTATE
// 22P05) and a text value cannot hold the byte 0x00 (22021). Tool output and
// model text can, so a run that holds one could never be archived. The NUL
// carries no meaning in what is stored (traces, answers), so it is dropped.

var jsonNUL = []byte(`\u0000`)

// stripNULJSON removes the JSON escape \u0000 from marshalled JSON. An escaped
// backslash followed by the text u0000 (`\\u0000`) is not that escape and stays.
func stripNULJSON(b []byte) []byte {
	if !bytes.Contains(b, jsonNUL) {
		return b
	}
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); {
		if b[i] == '\\' {
			if bytes.HasPrefix(b[i:], jsonNUL) {
				i += len(jsonNUL)
				continue
			}
			// Any other escape (including \\) is copied whole, so the second
			// backslash of \\ is never taken for the start of an escape.
			if i+1 < len(b) {
				out = append(out, b[i], b[i+1])
				i += 2
				continue
			}
		}
		out = append(out, b[i])
		i++
	}
	return out
}

// stripNUL removes the byte 0x00 from a text value.
func stripNUL(s string) string {
	if !strings.Contains(s, "\x00") {
		return s
	}
	return strings.ReplaceAll(s, "\x00", "")
}
