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

// Package devtools gives a coding agent a shell and file tools over one
// workspace directory: shell, read_file, write_file, edit_file, list_dir and
// grep. Register them with [Register].
//
// Paths are confined to the workspace root so the model gets a clear error for
// a path outside it. This is not a security boundary (a shell command can go
// anywhere the process can); run the agent in a sandbox for that.
package devtools

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/heddles/agent-orca/pkg/agent"
)

// Options tunes the tools. Zero values pick the defaults.
type Options struct {
	// Shell runs commands as Shell + "-c" + command (default: bash if present, else sh).
	Shell string
	// DefaultTimeout is a command's timeout when the model sets none (default 10m).
	DefaultTimeout time.Duration
	// MaxTimeout caps the timeout the model may ask for (default 1h).
	MaxTimeout time.Duration
	// MaxOutputBytes caps a tool result; longer output keeps its head and tail
	// (default 16 KiB).
	MaxOutputBytes int
	// Env is extra environment for commands, as KEY=VALUE.
	Env []string
}

// Tools are the devtools bound to a workspace root.
type Tools struct {
	root string
	opts Options
}

// New returns the tools for the workspace at root.
func New(root string, opts Options) (*Tools, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if info, err := os.Stat(abs); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("workspace %q is not a directory", root)
	}
	if opts.Shell == "" {
		opts.Shell = "/bin/sh"
		if p, err := exec.LookPath("bash"); err == nil {
			opts.Shell = p
		}
	}
	if opts.DefaultTimeout <= 0 {
		opts.DefaultTimeout = 10 * time.Minute
	}
	if opts.MaxTimeout <= 0 {
		opts.MaxTimeout = time.Hour
	}
	if opts.MaxOutputBytes <= 0 {
		opts.MaxOutputBytes = 16 << 10
	}
	return &Tools{root: abs, opts: opts}, nil
}

// Register adds the tools to a.
func (t *Tools) Register(a *agent.Agent) {
	a.Tool("shell", "Run a shell command in the workspace and return its exit code and combined output. "+
		"Use it to build, test, run git, install packages and inspect the system.",
		object(map[string]any{
			"command":         str("The command, run with -c by bash (or sh)."),
			"cwd":             str("Working directory, relative to the workspace root. Default: the root."),
			"timeout_seconds": map[string]any{"type": "integer", "description": "Timeout in seconds. Default 600."},
		}, "command"), t.shell)
	a.Tool("read_file", "Read a text file. Returns numbered lines.",
		object(map[string]any{
			"path":   str("File path, relative to the workspace root."),
			"offset": map[string]any{"type": "integer", "description": "First line to return, 1-based. Default 1."},
			"limit":  map[string]any{"type": "integer", "description": "Maximum lines to return. Default 2000."},
		}, "path"), t.readFile)
	a.Tool("write_file", "Create or overwrite a file with the given content. Parent directories are created.",
		object(map[string]any{
			"path":    str("File path, relative to the workspace root."),
			"content": str("The complete file content."),
		}, "path", "content"), t.writeFile)
	a.Tool("edit_file", "Replace an exact string in a file. "+
		"old_string must occur exactly once unless replace_all is true.",
		object(map[string]any{
			"path":        str("File path, relative to the workspace root."),
			"old_string":  str("The exact text to replace, including whitespace."),
			"new_string":  str("The replacement text."),
			"replace_all": map[string]any{"type": "boolean", "description": "Replace every occurrence."},
		}, "path", "old_string", "new_string"), t.editFile)
	a.Tool("list_dir", "List a directory. Directories end with /.",
		object(map[string]any{
			"path": str("Directory, relative to the workspace root. Default: the root."),
		}), t.listDir)
	a.Tool("grep", "Search file contents with a regular expression (Go RE2 syntax). Returns path:line: text matches.",
		object(map[string]any{
			"pattern": str("The regular expression."),
			"path":    str("File or directory to search, relative to the workspace root. Default: the root."),
			"glob":    str("Only search files whose name matches this glob, e.g. *.go."),
		}, "pattern"), t.grep)
}

func object(props map[string]any, required ...string) map[string]any {
	o := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		o["required"] = required
	}
	return o
}

func str(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

// resolve turns a model-supplied path into an absolute path inside the root.
func (t *Tools) resolve(p string) (string, error) {
	if p == "" {
		p = "."
	}
	abs := p
	if !filepath.IsAbs(p) {
		abs = filepath.Join(t.root, p)
	}
	abs = filepath.Clean(abs)
	rel, err := filepath.Rel(t.root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q is outside the workspace %s", p, t.root)
	}
	return abs, nil
}

func (t *Tools) shell(args map[string]any) (string, error) {
	command := stringArg(args, "command")
	if strings.TrimSpace(command) == "" {
		return "", errors.New("command is required")
	}
	dir, err := t.resolve(stringArg(args, "cwd"))
	if err != nil {
		return "", err
	}
	timeout := t.opts.DefaultTimeout
	if s := intArg(args, "timeout_seconds"); s > 0 {
		timeout = min(time.Duration(s)*time.Second, t.opts.MaxTimeout)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, t.opts.Shell, "-c", command)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), t.opts.Env...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	// Kill the whole process group on timeout, not just the shell.
	setProcessGroup(cmd)
	cmd.WaitDelay = 5 * time.Second
	start := time.Now()
	runErr := cmd.Run()

	exit := 0
	var exitErr *exec.ExitError
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		return fmt.Sprintf("timed out after %s\n%s", timeout, t.capOutput(out.String())), nil
	case errors.As(runErr, &exitErr):
		exit = exitErr.ExitCode()
	case runErr != nil:
		return "", runErr
	}
	return fmt.Sprintf("exit code: %d (%.1fs)\n%s", exit, time.Since(start).Seconds(), t.capOutput(out.String())), nil
}

// capOutput keeps the head and tail of long output: build and test failures
// are usually at the end, and the command's context at the start.
func (t *Tools) capOutput(s string) string {
	limit := t.opts.MaxOutputBytes
	if len(s) <= limit {
		return s
	}
	head := limit / 4
	tail := limit - head
	return s[:head] + fmt.Sprintf("\n[... %d bytes omitted ...]\n", len(s)-limit) + s[len(s)-tail:]
}

func (t *Tools) readFile(args map[string]any) (string, error) {
	path, err := t.resolve(stringArg(args, "path"))
	if err != nil {
		return "", err
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	offset := max(intArg(args, "offset"), 1)
	limit := intArg(args, "limit")
	if limit <= 0 {
		limit = 2000
	}
	var b strings.Builder
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	line, shown := 0, 0
	for scanner.Scan() {
		line++
		if line < offset {
			continue
		}
		if shown == limit || b.Len() > t.opts.MaxOutputBytes {
			fmt.Fprintf(&b, "[... more lines; read again with offset %d ...]\n", line)
			break
		}
		fmt.Fprintf(&b, "%6d\t%s\n", line, scanner.Text())
		shown++
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	if line == 0 {
		return "(empty file)", nil
	}
	return b.String(), nil
}

func (t *Tools) writeFile(args map[string]any) (string, error) {
	path, err := t.resolve(stringArg(args, "path"))
	if err != nil {
		return "", err
	}
	content, ok := args["content"].(string)
	if !ok {
		return "", errors.New("content is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", err
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(content), t.rel(path)), nil
}

func (t *Tools) editFile(args map[string]any) (string, error) {
	path, err := t.resolve(stringArg(args, "path"))
	if err != nil {
		return "", err
	}
	oldStr, newStr := stringArg(args, "old_string"), stringArg(args, "new_string")
	if oldStr == "" {
		return "", errors.New("old_string is required")
	}
	if oldStr == newStr {
		return "", errors.New("old_string and new_string are identical")
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	text := string(data)
	n := strings.Count(text, oldStr)
	replaceAll, _ := args["replace_all"].(bool)
	switch {
	case n == 0:
		return "", fmt.Errorf("old_string not found in %s", t.rel(path))
	case n > 1 && !replaceAll:
		return "", fmt.Errorf("old_string occurs %d times in %s; add context to make it unique or set replace_all",
			n, t.rel(path))
	}
	if !replaceAll {
		n = 1
	}
	text = strings.Replace(text, oldStr, newStr, n)
	if err := os.WriteFile(path, []byte(text), info.Mode().Perm()); err != nil {
		return "", err
	}
	return fmt.Sprintf("replaced %d occurrence(s) in %s", n, t.rel(path)), nil
}

func (t *Tools) listDir(args map[string]any) (string, error) {
	path, err := t.resolve(stringArg(args, "path"))
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return "", err
	}
	const maxEntries = 1000
	var b strings.Builder
	for i, e := range entries {
		if i == maxEntries {
			fmt.Fprintf(&b, "[... %d more entries ...]\n", len(entries)-maxEntries)
			break
		}
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		b.WriteString(name + "\n")
	}
	if b.Len() == 0 {
		return "(empty directory)", nil
	}
	return b.String(), nil
}

// skipDirs are not searched by grep: they are large and rarely what the model wants.
var skipDirs = map[string]bool{".git": true, "node_modules": true, "vendor": true}

func (t *Tools) grep(args map[string]any) (string, error) {
	re, err := regexp.Compile(stringArg(args, "pattern"))
	if err != nil {
		return "", fmt.Errorf("invalid pattern: %w", err)
	}
	start, err := t.resolve(stringArg(args, "path"))
	if err != nil {
		return "", err
	}
	glob := stringArg(args, "glob")
	const maxMatches = 200
	var matches []string
	errStop := errors.New("stop")
	walkErr := filepath.WalkDir(start, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // unreadable entries are skipped
		}
		if d.IsDir() {
			if path != start && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if glob != "" {
			if ok, _ := filepath.Match(glob, d.Name()); !ok {
				return nil
			}
		}
		f, err := os.Open(path)
		if err != nil {
			return nil //nolint:nilerr // unreadable files are skipped
		}
		defer func() { _ = f.Close() }()
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		line := 0
		for scanner.Scan() {
			line++
			text := scanner.Text()
			if line == 1 && strings.IndexByte(text, 0) >= 0 {
				return nil // binary
			}
			if re.MatchString(text) {
				if len(text) > 300 {
					text = text[:300] + "…"
				}
				matches = append(matches, fmt.Sprintf("%s:%d: %s", t.rel(path), line, text))
				if len(matches) == maxMatches {
					return errStop
				}
			}
		}
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, errStop) {
		return "", walkErr
	}
	if len(matches) == 0 {
		return "no matches", nil
	}
	sort.Strings(matches)
	out := strings.Join(matches, "\n")
	if len(matches) == maxMatches {
		out += fmt.Sprintf("\n[stopped at %d matches; narrow the pattern or path]", maxMatches)
	}
	return t.capOutput(out), nil
}

func (t *Tools) rel(path string) string {
	if rel, err := filepath.Rel(t.root, path); err == nil {
		return rel
	}
	return path
}

func stringArg(args map[string]any, key string) string {
	s, _ := args[key].(string)
	return s
}

// intArg reads a JSON number argument (decoded as float64).
func intArg(args map[string]any, key string) int {
	switch v := args[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return 0
}
