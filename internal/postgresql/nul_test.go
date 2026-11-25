package postgresql

import "testing"

func TestStripNULJSON(t *testing.T) {
	for in, want := range map[string]string{
		`{"a":"x\u0000y"}`:          `{"a":"xy"}`,
		`{"a":"\u0000"}`:            `{"a":""}`,
		`{"a":"x\\u0000y"}`:         `{"a":"x\\u0000y"}`, // a backslash, then the text u0000
		`{"a":"\\\u0000"}`:          `{"a":"\\"}`,        // an escaped backslash, then a NUL
		`{"a":"\n\u0000\t"}`:        `{"a":"\n\t"}`,
		`{"a":"é \u0001"}`:          `{"a":"é \u0001"}`,
		`[{"k":"a"},{"k":"b"}]`:     `[{"k":"a"},{"k":"b"}]`,
		`{"a":"\u0000\u0000end\\"}`: `{"a":"end\\"}`,
	} {
		if got := string(stripNULJSON([]byte(in))); got != want {
			t.Errorf("stripNULJSON(%s) = %s, want %s", in, got, want)
		}
	}
	if stripNULJSON(nil) != nil {
		t.Error("nil changed")
	}
}

func TestStripNUL(t *testing.T) {
	if got := stripNUL("a\x00b\x00"); got != "ab" {
		t.Errorf("got %q", got)
	}
	if got := stripNUL("plain"); got != "plain" {
		t.Errorf("got %q", got)
	}
}
