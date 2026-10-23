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

package devtools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTools(t *testing.T, opts Options) (*Tools, string) {
	t.Helper()
	root := t.TempDir()
	tools, err := New(root, opts)
	if err != nil {
		t.Fatal(err)
	}
	return tools, root
}

func TestPathsStayInWorkspace(t *testing.T) {
	tools, root := newTools(t, Options{})
	for _, p := range []string{"../escape", "/etc/passwd", "a/../../b"} {
		if _, err := tools.resolve(p); err == nil {
			t.Errorf("%q resolved inside the workspace", p)
		}
	}
	inside := map[string]string{
		"":                       root,
		".":                      root,
		"a/b":                    filepath.Join(root, "a/b"),
		filepath.Join(root, "c"): filepath.Join(root, "c"),
	}
	for p, want := range inside {
		if got, err := tools.resolve(p); err != nil || got != want {
			t.Errorf("resolve(%q) = %q, %v; want %q", p, got, err, want)
		}
	}
}

func TestShell(t *testing.T) {
	tools, root := newTools(t, Options{})
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := tools.shell(map[string]any{"command": "pwd; echo oops >&2; exit 3", "cwd": "sub"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "exit code: 3") || !strings.Contains(out, "sub") || !strings.Contains(out, "oops") {
		t.Errorf("output = %q, want exit code 3, the sub dir and stderr", out)
	}
	if _, err := tools.shell(map[string]any{"command": "true", "cwd": "../.."}); err == nil {
		t.Error("cwd outside the workspace was accepted")
	}
}

func TestShellTimeoutKillsProcessGroup(t *testing.T) {
	tools, root := newTools(t, Options{})
	marker := filepath.Join(root, "late")
	start := time.Now()
	// A background child would keep running if only the shell were killed.
	out, err := tools.shell(map[string]any{
		"command":         "(sleep 2; touch " + marker + ") & sleep 30",
		"timeout_seconds": float64(1),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "timed out") {
		t.Errorf("output = %q, want a timeout", out)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("timeout took %s", d)
	}
	time.Sleep(2500 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Error("a child of the timed-out command kept running")
	}
}

func TestCapOutputKeepsHeadAndTail(t *testing.T) {
	tools, _ := newTools(t, Options{MaxOutputBytes: 100})
	s := "HEAD" + strings.Repeat("x", 1000) + "TAIL"
	got := tools.capOutput(s)
	if !strings.HasPrefix(got, "HEAD") || !strings.HasSuffix(got, "TAIL") ||
		!strings.Contains(got, "bytes omitted") || len(got) > 160 {
		t.Errorf("capped = %q", got)
	}
}

func TestFileTools(t *testing.T) {
	tools, root := newTools(t, Options{})
	write := func(path, content string) {
		t.Helper()
		if _, err := tools.writeFile(map[string]any{"path": path, "content": content}); err != nil {
			t.Fatal(err)
		}
	}
	edit := func(path, oldStr, newStr string, all bool) (string, error) {
		return tools.editFile(map[string]any{"path": path, "old_string": oldStr, "new_string": newStr, "replace_all": all})
	}

	write("pkg/a.go", "package a\n\nfunc A() int { return 1 }\n")
	out, err := tools.readFile(map[string]any{"path": "pkg/a.go", "offset": float64(3)})
	if err != nil || !strings.Contains(out, "3\tfunc A() int { return 1 }") || strings.Contains(out, "package a") {
		t.Errorf("read = %q, %v", out, err)
	}

	if _, err := edit("pkg/a.go", "return 1", "return 2", false); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(root, "pkg/a.go"))
	if !strings.Contains(string(data), "return 2") {
		t.Errorf("edit not applied: %s", data)
	}
	if _, err := edit("pkg/a.go", "missing", "x", false); err == nil {
		t.Error("edit of a missing string succeeded")
	}
	write("dup.txt", "a a")
	if _, err := edit("dup.txt", "a", "b", false); err == nil {
		t.Error("ambiguous edit succeeded")
	}
	if out, err := edit("dup.txt", "a", "b", true); err != nil || !strings.Contains(out, "2 occurrence") {
		t.Errorf("replace_all = %q, %v", out, err)
	}

	list, err := tools.listDir(map[string]any{})
	if err != nil || !strings.Contains(list, "pkg/\n") || !strings.Contains(list, "dup.txt\n") {
		t.Errorf("list = %q, %v", list, err)
	}
}

func TestGrep(t *testing.T) {
	tools, root := newTools(t, Options{})
	files := map[string]string{
		"a.go":              "package a\nfunc Foo() {}\n",
		"b.txt":             "Foo in text\n",
		".git/config":       "Foo in git\n",
		"sub/c.go":          "func FooBar() {}\n",
		"node_modules/x.js": "Foo\n",
	}
	for p, c := range files {
		full := filepath.Join(root, p)
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out, err := tools.grep(map[string]any{"pattern": `func Foo`, "glob": "*.go"})
	if err != nil {
		t.Fatal(err)
	}
	if out != "a.go:2: func Foo() {}\nsub/c.go:1: func FooBar() {}" {
		t.Errorf("grep = %q", out)
	}
	out, _ = tools.grep(map[string]any{"pattern": "Foo"})
	if strings.Contains(out, ".git") || strings.Contains(out, "node_modules") {
		t.Errorf("grep searched skipped dirs: %q", out)
	}
	if _, err := tools.grep(map[string]any{"pattern": "("}); err == nil {
		t.Error("invalid pattern accepted")
	}
}
