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

package inbound

import "testing"

func TestPairingCode(t *testing.T) {
	seen := map[string]bool{}
	for range 200 {
		code, err := NewPairingCode()
		if err != nil {
			t.Fatal(err)
		}
		if PairingCode("pair "+code) != code {
			t.Fatalf("code %q is not read back", code)
		}
		seen[code] = true
	}
	if len(seen) < 199 {
		t.Errorf("only %d distinct codes in 200", len(seen))
	}
	for text, want := range map[string]string{
		"pair K7M2QX": "K7M2QX", "  PAIR k7m2qx \n": "K7M2QX",
		"please pair K7M2QX": "", "pair K7M2QX please": "", "pair K7M2Q": "", "pair K7M2QXY": "", "pair K7M2Q0": "", "pairK7M2QX": "", "": "",
	} {
		if got := PairingCode(text); got != want {
			t.Errorf("PairingCode(%q) = %q, want %q", text, got, want)
		}
	}
	if PairingHash("a", "K7M2QX") == PairingHash("b", "K7M2QX") {
		t.Error("a code's digest must be bound to the connection")
	}
}
