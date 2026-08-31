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

package state

import "testing"

func TestResolveTokenStreamCap(t *testing.T) {
	if got := resolveTokenStreamCap(0); got != defaultTokenStreamMaxLen {
		t.Fatalf("zero -> default %d, got %d", defaultTokenStreamMaxLen, got)
	}
	if got := resolveTokenStreamCap(50000); got != 50000 {
		t.Fatalf("explicit 50000 -> 50000, got %d", got)
	}
	if defaultTokenStreamMaxLen <= 10000 {
		t.Fatalf("default should exceed the old 10000 floor, got %d", defaultTokenStreamMaxLen)
	}
}
