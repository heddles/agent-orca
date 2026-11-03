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

package controller

// Workflow templates (AgentWorkflow spec.mode: template). A template is a
// definition: it is validated here and never run. A workflow is created from it
// by name (spec.fromTemplate), and on its first reconcile the controller fills in
// the steps with the template's parameters substituted into the step inputs.
//
// Parameters are plain text from outside the cluster (a ticket title, say), so
// they go only where text belongs: step inputs, never an agentRef, a step name, a
// dependency or a condition, where they could redirect the workflow or inject
// into an expression. A value is also stopped from smuggling in a
// {{steps.x.output}} placeholder that would later be expanded.

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

// defaultParamMaxLength bounds a parameter value when the template sets no limit.
const defaultParamMaxLength = 8192

var (
	paramRefPattern = regexp.MustCompile(`\{\{\s*params\.([A-Za-z][A-Za-z0-9_]*)\s*\}\}`)
	stepRefPattern  = regexp.MustCompile(`\{\{steps\.([a-z0-9][a-z0-9\-]*)\.output\}\}`)
	// anyParamPrefix finds a params reference that is not well formed.
	anyParamPrefix = regexp.MustCompile(`\{\{\s*params\.`)
)

// validateWorkflowStructure checks what must hold for a workflow to run at all:
// unique step names and dependencies that exist and form no cycle. It reports
// every problem it finds.
func validateWorkflowStructure(steps []agentorcav1alpha1.WorkflowStep) []string {
	var problems []string
	if len(steps) == 0 {
		return []string{"there are no steps"}
	}
	names := make(map[string]bool, len(steps))
	for _, s := range steps {
		if names[s.Name] {
			problems = append(problems, fmt.Sprintf("step %q is defined more than once", s.Name))
		}
		names[s.Name] = true
		problems = append(problems, validateStepShape(&s)...)
	}
	for _, s := range steps {
		for _, d := range s.DependsOn {
			switch {
			case d == s.Name:
				problems = append(problems, fmt.Sprintf("step %q depends on itself", s.Name))
			case !names[d]:
				problems = append(problems, fmt.Sprintf("step %q depends on %q, which does not exist", s.Name, d))
			}
		}
	}
	if len(problems) == 0 {
		if cycle := findCycle(steps); cycle != "" {
			problems = append(problems, "the dependencies form a cycle: "+cycle)
		} else {
			problems = append(problems, validateWorkflowLoops(steps)...)
		}
	}
	return problems
}

// validateStepShape checks that a step has what its type needs and nothing of
// another type. The CRD's rules enforce this at admission; this covers steps
// that reach the controller another way (dynamic steps, objects created
// before the rules existed).
func validateStepShape(s *agentorcav1alpha1.WorkflowStep) []string {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	switch stepType(s) {
	case agentorcav1alpha1.WorkflowStepTypeAgent:
		if s.AgentRef == "" {
			add("step %q is an agent step and needs agentRef", s.Name)
		}
		if s.Container != nil || s.Approval != nil {
			add("step %q is an agent step but has a container or approval block", s.Name)
		}
	case agentorcav1alpha1.WorkflowStepTypeContainer:
		if s.Container == nil || s.Container.Image == "" {
			add("step %q is a container step and needs container.image", s.Name)
		}
		if s.AgentRef != "" || s.Input != "" || s.Approval != nil {
			add("step %q is a container step but has agentRef, input or an approval block", s.Name)
		}
	case agentorcav1alpha1.WorkflowStepTypeApproval:
		if s.Approval == nil || s.Approval.Message == "" {
			add("step %q is an approval step and needs approval.message", s.Name)
		}
		if s.AgentRef != "" || s.Input != "" || s.Container != nil {
			add("step %q is an approval step but has agentRef, input or a container block", s.Name)
		}
	default:
		add("step %q has an unknown type %q", s.Name, s.Type)
	}
	return problems
}

// stepTexts lists the text fields of a step that may use {{params.x}} and
// {{steps.x.output}}: the agent input, the environment of a container, the
// message of an approval.
func stepTexts(s *agentorcav1alpha1.WorkflowStep) []struct{ where, text string } {
	var out []struct{ where, text string }
	add := func(where, text string) {
		out = append(out, struct{ where, text string }{fmt.Sprintf("step %q %s", s.Name, where), text})
	}
	if s.Input != "" || stepType(s) == agentorcav1alpha1.WorkflowStepTypeAgent {
		add("input", s.Input)
	}
	if s.Container != nil {
		for _, e := range s.Container.Env {
			add("env "+e.Name, e.Value)
		}
	}
	if s.Approval != nil {
		add("approval message", s.Approval.Message)
	}
	return out
}

// findCycle returns a description of a dependency cycle ("a -> b -> a"), or "".
func findCycle(steps []agentorcav1alpha1.WorkflowStep) string {
	deps := make(map[string][]string, len(steps))
	for _, s := range steps {
		deps[s.Name] = s.DependsOn
	}
	const (
		unseen = iota
		inPath
		done
	)
	state := make(map[string]int, len(steps))
	var path []string
	var visit func(string) string
	visit = func(n string) string {
		state[n] = inPath
		path = append(path, n)
		for _, d := range deps[n] {
			switch state[d] {
			case inPath:
				i := slices.Index(path, d)
				return strings.Join(append(slices.Clone(path[i:]), d), " -> ")
			case unseen:
				if c := visit(d); c != "" {
					return c
				}
			}
		}
		path = path[:len(path)-1]
		state[n] = done
		return ""
	}
	for _, s := range steps {
		if state[s.Name] == unseen {
			if c := visit(s.Name); c != "" {
				return c
			}
		}
	}
	return ""
}

// ancestors returns, for each step, every step it transitively depends on.
func ancestors(steps []agentorcav1alpha1.WorkflowStep) map[string]map[string]bool {
	direct := make(map[string][]string, len(steps))
	for _, s := range steps {
		direct[s.Name] = s.DependsOn
	}
	out := make(map[string]map[string]bool, len(steps))
	var collect func(string, map[string]bool)
	collect = func(n string, acc map[string]bool) {
		for _, d := range direct[n] {
			if !acc[d] {
				acc[d] = true
				collect(d, acc)
			}
		}
	}
	for _, s := range steps {
		acc := map[string]bool{}
		collect(s.Name, acc)
		out[s.Name] = acc
	}
	return out
}

// validateTemplate reports every problem that makes a template unusable.
func validateTemplate(spec *agentorcav1alpha1.AgentWorkflowSpec) []string {
	problems := append(validateWorkflowStructure(spec.Steps), validateWorkflowSandbox(spec)...)

	declared := make(map[string]*agentorcav1alpha1.WorkflowParameter, len(spec.Parameters))
	for i := range spec.Parameters {
		p := &spec.Parameters[i]
		if _, dup := declared[p.Name]; dup {
			problems = append(problems, fmt.Sprintf("parameter %q is declared more than once", p.Name))
			continue
		}
		declared[p.Name] = p
		problems = append(problems, validateParameterDeclaration(p)...)
	}

	anc := ancestors(spec.Steps)
	stepNames := make(map[string]bool, len(spec.Steps))
	for _, s := range spec.Steps {
		stepNames[s.Name] = true
	}
	checkParamRefs := func(where, text string) {
		for _, m := range paramRefPattern.FindAllStringSubmatch(text, -1) {
			if _, ok := declared[m[1]]; !ok {
				problems = append(problems, fmt.Sprintf("%s uses {{params.%s}}, which is not declared in spec.parameters", where, m[1]))
			}
		}
		if len(anyParamPrefix.FindAllString(text, -1)) != len(paramRefPattern.FindAllString(text, -1)) {
			problems = append(problems, fmt.Sprintf("%s has a malformed {{params.…}} placeholder", where))
		}
	}
	noParams := func(where, text string) {
		if anyParamPrefix.MatchString(text) {
			problems = append(problems, fmt.Sprintf("%s may not use {{params.…}}: parameters go only in step inputs, container environments and approval messages", where))
		}
	}
	checkParamRefs("spec.description", spec.Description)
	if sb := spec.Sandbox; sb != nil {
		// Source and image selection are configuration, not untrusted text. An
		// instance can override the whole sandbox block when it is created.
		noParams("spec.sandbox.image", sb.Image)
		noParams("spec.sandbox.classRef", sb.ClassRef)
		noParams("spec.sandbox.repo", sb.Repo)
		noParams("spec.sandbox.ref", sb.Ref)
		noParams("spec.sandbox.branch", sb.Branch)
	}
	for _, s := range spec.Steps {
		noParams(fmt.Sprintf("step %q agentRef", s.Name), s.AgentRef)
		noParams(fmt.Sprintf("step %q condition", s.Name), s.Condition)
		noParams(fmt.Sprintf("step %q name", s.Name), s.Name)
		for _, d := range s.DependsOn {
			noParams(fmt.Sprintf("step %q dependsOn", s.Name), d)
		}
		if c := s.Container; c != nil {
			// What a job runs is never decided by a parameter.
			noParams(fmt.Sprintf("step %q container.image", s.Name), c.Image)
			for _, v := range append(append([]string{}, c.Command...), c.Args...) {
				noParams(fmt.Sprintf("step %q container command or args", s.Name), v)
			}
			for _, e := range c.Env {
				noParams(fmt.Sprintf("step %q env name %q", s.Name, e.Name), e.Name)
			}
		}
		if a := s.Approval; a != nil {
			for _, g := range a.ApproverGroups {
				noParams(fmt.Sprintf("step %q approverGroups", s.Name), g)
			}
		}
		for _, t := range stepTexts(&s) {
			checkParamRefs(t.where, t.text)
			for _, m := range stepRefPattern.FindAllStringSubmatch(t.text, -1) {
				switch {
				case !stepNames[m[1]]:
					problems = append(problems, fmt.Sprintf("%s uses {{steps.%s.output}}, but there is no step %q", t.where, m[1], m[1]))
				case !anc[s.Name][m[1]]:
					problems = append(problems, fmt.Sprintf("%s uses {{steps.%s.output}}, but %q is not one of the step's dependencies, so it would be empty", t.where, m[1], m[1]))
				}
			}
		}
		if s.Condition != "" {
			if _, err := evalCondition(s.Condition, nil); err != nil {
				problems = append(problems, fmt.Sprintf("step %q has an unsupported condition: %v", s.Name, err))
			}
		}
	}
	return problems
}

func validateParameterDeclaration(p *agentorcav1alpha1.WorkflowParameter) []string {
	var problems []string
	typ := paramType(p)
	if p.Pattern != "" {
		if typ != "string" {
			problems = append(problems, fmt.Sprintf("parameter %q: pattern only applies to string parameters", p.Name))
		} else if _, err := regexp.Compile("^(?:" + p.Pattern + ")$"); err != nil {
			problems = append(problems, fmt.Sprintf("parameter %q: invalid pattern: %v", p.Name, err))
		}
	}
	if p.Default != nil {
		if _, err := checkParameterValue(p, *p.Default); err != nil {
			problems = append(problems, fmt.Sprintf("parameter %q: its default is not valid: %v", p.Name, err))
		}
	}
	for _, e := range p.Enum {
		if typ == "integer" {
			if _, err := strconv.ParseInt(e, 10, 64); err != nil {
				problems = append(problems, fmt.Sprintf("parameter %q: enum value %q is not an integer", p.Name, e))
			}
		}
		if typ == "boolean" && e != strconv.FormatBool(true) && e != strconv.FormatBool(false) {
			problems = append(problems, fmt.Sprintf("parameter %q: enum value %q is not true or false", p.Name, e))
		}
	}
	return problems
}

func paramType(p *agentorcav1alpha1.WorkflowParameter) string {
	if p.Type == "" {
		return "string"
	}
	return p.Type
}

// checkParameterValue validates one value against its declaration and returns it
// in canonical form (integers without leading zeros, booleans lowercase).
func checkParameterValue(p *agentorcav1alpha1.WorkflowParameter, v string) (string, error) {
	max := p.MaxLength
	if max <= 0 {
		max = defaultParamMaxLength
	}
	if len(v) > max {
		return "", fmt.Errorf("%d bytes is over the limit of %d", len(v), max)
	}
	switch paramType(p) {
	case "integer":
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			return "", fmt.Errorf("%q is not an integer", v)
		}
		v = strconv.FormatInt(n, 10)
	case "boolean":
		b, err := strconv.ParseBool(strings.TrimSpace(v))
		if err != nil {
			return "", fmt.Errorf("%q is not true or false", v)
		}
		v = strconv.FormatBool(b)
	default:
		if p.Pattern != "" {
			re, err := regexp.Compile("^(?:" + p.Pattern + ")$")
			if err != nil {
				return "", fmt.Errorf("invalid pattern: %v", err)
			}
			if !re.MatchString(v) {
				return "", fmt.Errorf("%q does not match %q", truncateForMessage(v), p.Pattern)
			}
		}
	}
	if len(p.Enum) > 0 && !slices.Contains(p.Enum, v) {
		return "", fmt.Errorf("%q is not one of %s", truncateForMessage(v), strings.Join(p.Enum, ", "))
	}
	return v, nil
}

func truncateForMessage(s string) string {
	if len(s) > 60 {
		return s[:60] + "…"
	}
	return s
}

// resolveParameters checks the instance's parameters against the template's
// declarations and returns the value of every declared parameter.
func resolveParameters(declared []agentorcav1alpha1.WorkflowParameter, given map[string]string) (map[string]string, error) {
	byName := make(map[string]*agentorcav1alpha1.WorkflowParameter, len(declared))
	for i := range declared {
		byName[declared[i].Name] = &declared[i]
	}
	var problems []string
	for name := range given {
		if _, ok := byName[name]; !ok {
			problems = append(problems, fmt.Sprintf("the template has no parameter %q", name))
		}
	}
	values := make(map[string]string, len(declared))
	for _, p := range declared {
		raw, ok := given[p.Name]
		if !ok {
			switch {
			case p.Default != nil:
				raw = *p.Default
			case p.Required:
				problems = append(problems, fmt.Sprintf("parameter %q is required", p.Name))
				continue
			default:
				// Optional with no default: substitutes as empty text.
				values[p.Name] = ""
				continue
			}
		}
		v, err := checkParameterValue(&p, raw)
		if err != nil {
			problems = append(problems, fmt.Sprintf("parameter %q: %v", p.Name, err))
			continue
		}
		values[p.Name] = v
	}
	if len(problems) > 0 {
		slices.Sort(problems)
		return nil, fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	return values, nil
}

// neutralizePlaceholders stops a value from being read as a placeholder later:
// it puts a zero-width space between the braces of any "{{", so the text looks
// the same to a reader but no longer matches {{steps.x.output}}.
func neutralizePlaceholders(v string) string {
	return strings.ReplaceAll(v, "{{", "{\u200b{")
}

func substituteParameters(text string, values map[string]string) string {
	return paramRefPattern.ReplaceAllStringFunc(text, func(m string) string {
		name := paramRefPattern.FindStringSubmatch(m)[1]
		return neutralizePlaceholders(values[name])
	})
}

// instantiateTemplate builds the spec of a workflow from a template and the
// instance's parameters. Anything the instance sets itself (description, timeout,
// budget cap, failure policy, adaptive policy) wins over the template's.
func instantiateTemplate(tmpl, inst *agentorcav1alpha1.AgentWorkflow) (agentorcav1alpha1.AgentWorkflowSpec, error) {
	if tmpl.Spec.Mode != agentorcav1alpha1.WorkflowModeTemplate {
		return agentorcav1alpha1.AgentWorkflowSpec{}, fmt.Errorf("%q is not a template (spec.mode is %q)", tmpl.Name, orDefaultMode(tmpl.Spec.Mode))
	}
	if problems := validateTemplate(&tmpl.Spec); len(problems) > 0 {
		return agentorcav1alpha1.AgentWorkflowSpec{}, fmt.Errorf("template %q is not valid: %s", tmpl.Name, strings.Join(problems, "; "))
	}
	values, err := resolveParameters(tmpl.Spec.Parameters, inst.Spec.FromTemplate.Parameters)
	if err != nil {
		return agentorcav1alpha1.AgentWorkflowSpec{}, err
	}

	out := inst.Spec
	out.Mode = agentorcav1alpha1.WorkflowModeWorkflow
	out.Parameters = nil
	out.Steps = make([]agentorcav1alpha1.WorkflowStep, len(tmpl.Spec.Steps))
	for i, s := range tmpl.Spec.Steps {
		s = *s.DeepCopy()
		s.Input = substituteParameters(s.Input, values)
		if s.Container != nil {
			for j := range s.Container.Env {
				s.Container.Env[j].Value = substituteParameters(s.Container.Env[j].Value, values)
			}
		}
		if s.Approval != nil {
			s.Approval.Message = substituteParameters(s.Approval.Message, values)
		}
		out.Steps[i] = s
	}
	if out.Description == "" {
		out.Description = substituteParameters(tmpl.Spec.Description, values)
	}
	if out.Timeout == nil && tmpl.Spec.Timeout != nil {
		out.Timeout = tmpl.Spec.Timeout.DeepCopy()
	}
	if out.BudgetCap == nil && tmpl.Spec.BudgetCap != nil {
		out.BudgetCap = tmpl.Spec.BudgetCap.DeepCopy()
	}
	if out.OnStepFailure == "" {
		out.OnStepFailure = tmpl.Spec.OnStepFailure
	}
	if out.AdaptivePolicy == nil && tmpl.Spec.AdaptivePolicy != nil {
		out.AdaptivePolicy = tmpl.Spec.AdaptivePolicy.DeepCopy()
	}
	if out.Sandbox == nil && tmpl.Spec.Sandbox != nil {
		out.Sandbox = tmpl.Spec.Sandbox.DeepCopy()
	}
	return out, nil
}

func orDefaultMode(m agentorcav1alpha1.WorkflowMode) agentorcav1alpha1.WorkflowMode {
	if m == "" {
		return agentorcav1alpha1.WorkflowModeWorkflow
	}
	return m
}
