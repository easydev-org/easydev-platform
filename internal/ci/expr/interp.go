package expr

import (
	"fmt"
	"strings"
)

// Ctx is the evaluation context: a variable tree plus the ambient job status.
//
// Variable names are matched case-insensitively at the top level (so `github`
// and `GitHub` both resolve), because the workflow schema is written by humans.
type Ctx struct {
	// Vars is the variable tree, e.g. {"github": {"ref": "main"}}.
	Vars map[string]any
	// Status is the current job status for success()/failure()/always().
	Status Status
	// HashFiles backs the hashFiles() function. nil makes it return "".
	HashFiles func(patterns []string) (string, error)
}

// Lookup returns a top-level variable (nil when absent).
func (c *Ctx) Lookup(name string) any {
	if c == nil {
		return nil
	}
	if v, ok := c.Vars[name]; ok {
		return v
	}
	for k, v := range c.Vars {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return nil
}

// With returns a shallow copy of the context with a variable overridden.
func (c *Ctx) With(name string, v any) *Ctx {
	next := &Ctx{Status: c.Status, HashFiles: c.HashFiles, Vars: map[string]any{}}
	for k, val := range c.Vars {
		next.Vars[k] = val
	}
	next.Vars[name] = v
	return next
}

// Interpolate expands every `${{ expr }}` occurrence in s.
//
// String literals may contain `}}` (e.g. `${{ 'a}}b' }}`); the scanner honours
// quotes so that a literal never terminates the expression early. A parse error
// is returned rather than silently emitting the raw text, so a typo in a
// workflow fails the run instead of producing a wrong shell command.
func Interpolate(s string, ctx *Ctx) (string, error) {
	if !strings.Contains(s, "${{") {
		return s, nil
	}
	var b strings.Builder
	i := 0
	for i < len(s) {
		j := strings.Index(s[i:], "${{")
		if j < 0 {
			b.WriteString(s[i:])
			break
		}
		j += i
		b.WriteString(s[i:j])
		end, err := findClose(s, j+3)
		if err != nil {
			return "", fmt.Errorf("expr: %w in %q", err, s)
		}
		src := strings.TrimSpace(s[j+3 : end])
		prog, err := Parse(src)
		if err != nil {
			return "", err
		}
		v, err := prog.Eval(ctx)
		if err != nil {
			return "", err
		}
		b.WriteString(ToString(v))
		i = end + 2
	}
	return b.String(), nil
}

// findClose returns the index of the `}}` that closes an expression starting at
// from, skipping over single-quoted and double-quoted literals.
func findClose(s string, from int) (int, error) {
	var quote byte
	for i := from; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == '\\' {
				i++ // skip the escaped byte
				continue
			}
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
		case '}':
			if i+1 < len(s) && s[i+1] == '}' {
				return i, nil
			}
		}
	}
	return 0, fmt.Errorf("unterminated ${{ }}")
}

// Evaluate parses and evaluates a standalone expression (no `${{ }}`).
func Evaluate(src string, ctx *Ctx) (any, error) {
	p, err := Parse(src)
	if err != nil {
		return nil, err
	}
	return p.Eval(ctx)
}

// EvalBool evaluates an expression and applies truthiness. It is what `if:`
// uses. An empty expression is true (a missing `if:` means "always run").
//
// Both spellings are accepted: `if: needs.a.outputs.x != ”` and the wrapped
// `if: ${{ needs.a.outputs.x != ” }}`. The wrapper is stripped BEFORE parsing
// (not interpolated), so a literal "false" cannot become a truthy string.
func EvalBool(src string, ctx *Ctx) (bool, error) {
	src = strings.TrimSpace(src)
	if src == "" {
		return true, nil
	}
	if inner, ok := unwrap(src); ok {
		src = inner
	}
	v, err := Evaluate(src, ctx)
	if err != nil {
		return false, err
	}
	return Truthy(v), nil
}

// unwrap strips a single `${{ ... }}` that spans the whole expression.
func unwrap(s string) (string, bool) {
	if !strings.HasPrefix(s, "${{") {
		return s, false
	}
	end, err := findClose(s, 3)
	if err != nil || strings.TrimSpace(s[end+2:]) != "" {
		return s, false
	}
	return strings.TrimSpace(s[3:end]), true
}
