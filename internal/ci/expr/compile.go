package expr

import (
	"fmt"
	"strings"
)

// ShellCompile compiles an `if:` expression into a POSIX shell snippet that
// EXITS 0 when the condition holds.
//
// Why this exists: a job's steps used to run one executor job at a time, so Go
// evaluated each step's `if:` between them. Now all steps of a job run inside ONE
// container script (they share a workspace), so a condition that depends on an
// earlier step's output must be evaluated IN the container. Values that are
// already known (easyvcs/matrix/needs/env/literals) are substituted as literals;
// the only runtime lookup is `steps.<id>.outputs.<key>`, read from the per-step
// output file the supervisor writes.
//
// An expression that references runtime state in any OTHER way is rejected: a
// silent wrong answer is worse than a clear plan-time error.
func ShellCompile(src string, ctx *Ctx, stepIndex map[string]int) (string, error) {
	src = strings.TrimSpace(src)
	if src == "" {
		return "true", nil
	}
	if inner, ok := unwrap(src); ok {
		src = inner
	}
	p, err := Parse(src)
	if err != nil {
		return "", err
	}
	c := &shellCompiler{ctx: ctx, steps: stepIndex}
	out, err := c.node(p.root)
	if err != nil {
		return "", err
	}
	return out, nil
}

type shellCompiler struct {
	ctx   *Ctx
	steps map[string]int
}

// node returns a shell snippet that exits 0 / non-zero (a "test").
func (c *shellCompiler) node(n Node) (string, error) {
	switch x := n.(type) {
	case *litNode:
		if Truthy(x.v) {
			return "true", nil
		}
		return "false", nil
	case *identNode, *fieldNode, *indexNode:
		v, err := c.value(n)
		if err != nil {
			return "", err
		}
		if v.runtime != "" {
			return "test -n " + v.runtime, nil
		}
		if Truthy(v.literal) {
			return "true", nil
		}
		return "false", nil
	case *unaryNode:
		if x.op == "!" {
			inner, err := c.node(x.x)
			if err != nil {
				return "", err
			}
			return "! { " + inner + "; }", nil
		}
		return "", fmt.Errorf("expr: unsupported unary %q in a shell gate", x.op)
	case *binaryNode:
		return c.binary(x)
	case *callNode:
		return c.call(x)
	}
	return "", fmt.Errorf("expr: unsupported node in a shell gate")
}

func (c *shellCompiler) binary(x *binaryNode) (string, error) {
	switch x.op {
	case "&&", "||":
		l, err := c.node(x.l)
		if err != nil {
			return "", err
		}
		r, err := c.node(x.r)
		if err != nil {
			return "", err
		}
		if x.op == "&&" {
			return "{ " + l + "; } && { " + r + "; }", nil
		}
		return "{ " + l + "; } || { " + r + "; }", nil
	}
	// A comparison. Resolve both sides; if either is a runtime value, emit a
	// shell string comparison, else decide now.
	lv, err := c.value(x.l)
	if err != nil {
		return "", err
	}
	rv, err := c.value(x.r)
	if err != nil {
		return "", err
	}
	if lv.runtime == "" && rv.runtime == "" {
		switch x.op {
		case "==":
			if equal(lv.literal, rv.literal) {
				return "true", nil
			}
			return "false", nil
		case "!=":
			if !equal(lv.literal, rv.literal) {
				return "true", nil
			}
			return "false", nil
		}
		cp, ok := compare(lv.literal, rv.literal)
		if !ok {
			return "false", nil
		}
		switch x.op {
		case "<":
			return shellBool(!(cp >= 0)), nil
		case "<=":
			return shellBool(cp <= 0), nil
		case ">":
			return shellBool(cp > 0), nil
		case ">=":
			return shellBool(cp >= 0), nil
		}
	}
	lq := c.shellValue(lv)
	rq := c.shellValue(rv)
	switch x.op {
	case "==":
		return "test " + lq + " = " + rq, nil
	case "!=":
		return "test " + lq + " != " + rq, nil
	}
	return "", fmt.Errorf("expr: %q is not supported on a runtime value in a shell gate", x.op)
}

func (c *shellCompiler) call(x *callNode) (string, error) {
	switch strings.ToLower(x.name) {
	case "always":
		return "true", nil
	case "success":
		return `test "${EASYDEV_FAILED:-0}" = "0"`, nil
	case "failure":
		return `test "${EASYDEV_FAILED:-0}" != "0"`, nil
	case "cancelled", "canceled":
		return `test "${EASYDEV_CANCELLED:-0}" != "0"`, nil
	}
	// Any other function must be resolvable NOW (it cannot see runtime state).
	v, err := c.value(x)
	if err != nil {
		return "", err
	}
	if v.runtime != "" {
		return "", fmt.Errorf("expr: function %q cannot be evaluated at run time in a shell gate", x.name)
	}
	if Truthy(v.literal) {
		return "true", nil
	}
	return "false", nil
}

// shellValue is one side of a runtime comparison.
type shellValue struct {
	literal any
	// runtime, when non-empty, is a shell command substitution producing the
	// value (e.g. `"$(easydev_out 2 version)"`).
	runtime string
}

func (c *shellCompiler) shellValue(v shellValue) string {
	if v.runtime != "" {
		return v.runtime
	}
	return shellQuote(ToString(v.literal))
}

// value resolves a node to either a literal or a runtime substitution.
func (c *shellCompiler) value(n Node) (shellValue, error) {
	if k, ok := c.stepOutputRef(n); ok {
		return shellValue{runtime: `"$(easydev_out ` + k + `)"`}, nil
	}
	v, err := n.eval(c.ctx)
	if err != nil {
		return shellValue{}, err
	}
	return shellValue{literal: v}, nil
}

// stepOutputRef recognises `steps.<id>.outputs.<key>` and returns the
// supervisor's step index plus the key.
//
// The AST for `steps.meta.outputs.version` is
// field(field(field(ident(steps), "meta"), "outputs"), "version").
func (c *shellCompiler) stepOutputRef(n Node) (string, bool) {
	key, ok := n.(*fieldNode)
	if !ok {
		return "", false
	}
	outputs, ok := key.base.(*fieldNode)
	if !ok || outputs.name != "outputs" {
		return "", false
	}
	stepField, ok := outputs.base.(*fieldNode)
	if !ok {
		return "", false
	}
	root, ok := stepField.base.(*identNode)
	if !ok || root.name != "steps" {
		return "", false
	}
	idx, ok := c.indexOfStep(stepField.name)
	if !ok {
		return "", false
	}
	return fmt.Sprintf("%d %s", idx, shellQuote(key.name)), true
}

// indexOfStep resolves a step id to its supervisor index.
func (c *shellCompiler) indexOfStep(id string) (int, bool) {
	i, ok := c.steps[id]
	return i, ok
}

// shellQuote single-quotes a string for POSIX sh.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// shellBool renders a shell truth value.
func shellBool(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
