package expr

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Node is one expression AST node.
type Node interface {
	eval(*Ctx) (any, error)
}

type litNode struct{ v any }

func (n *litNode) eval(*Ctx) (any, error) { return n.v, nil }

type identNode struct{ name string }

func (n *identNode) eval(ctx *Ctx) (any, error) {
	if ctx == nil {
		return nil, nil
	}
	return ctx.Lookup(n.name), nil
}

type fieldNode struct {
	base Node
	name string
}

func (n *fieldNode) eval(ctx *Ctx) (any, error) {
	b, err := n.base.eval(ctx)
	if err != nil {
		return nil, err
	}
	return getField(b, n.name), nil
}

type indexNode struct {
	base Node
	key  Node
}

func (n *indexNode) eval(ctx *Ctx) (any, error) {
	b, err := n.base.eval(ctx)
	if err != nil {
		return nil, err
	}
	k, err := n.key.eval(ctx)
	if err != nil {
		return nil, err
	}
	if list, ok := b.([]any); ok {
		i, ok := toInt(k)
		if !ok || i < 0 || i >= len(list) {
			return nil, nil
		}
		return list[i], nil
	}
	return getField(b, toString(k)), nil
}

type unaryNode struct {
	op string
	x  Node
}

func (n *unaryNode) eval(ctx *Ctx) (any, error) {
	x, err := n.x.eval(ctx)
	if err != nil {
		return nil, err
	}
	if n.op == "!" {
		return !Truthy(x), nil
	}
	return nil, fmt.Errorf("expr: unknown unary %q", n.op)
}

type binaryNode struct {
	op   string
	l, r Node
}

func (n *binaryNode) eval(ctx *Ctx) (any, error) {
	// Short-circuit, like the original language.
	switch n.op {
	case "&&":
		l, err := n.l.eval(ctx)
		if err != nil {
			return nil, err
		}
		if !Truthy(l) {
			return l, nil
		}
		return n.r.eval(ctx)
	case "||":
		l, err := n.l.eval(ctx)
		if err != nil {
			return nil, err
		}
		if Truthy(l) {
			return l, nil
		}
		return n.r.eval(ctx)
	}

	l, err := n.l.eval(ctx)
	if err != nil {
		return nil, err
	}
	r, err := n.r.eval(ctx)
	if err != nil {
		return nil, err
	}

	if n.op == "==" || n.op == "!=" {
		eq := equal(l, r)
		if n.op == "==" {
			return eq, nil
		}
		return !eq, nil
	}

	c, ok := compare(l, r)
	if !ok {
		return false, nil
	}
	switch n.op {
	case "<":
		return c < 0, nil
	case "<=":
		return c <= 0, nil
	case ">":
		return c > 0, nil
	case ">=":
		return c >= 0, nil
	}
	return nil, fmt.Errorf("expr: unknown operator %q", n.op)
}

type callNode struct {
	name string
	args []Node
}

func (n *callNode) eval(ctx *Ctx) (any, error) {
	args := make([]any, 0, len(n.args))
	for _, a := range n.args {
		v, err := a.eval(ctx)
		if err != nil {
			return nil, err
		}
		args = append(args, v)
	}
	return callFunc(ctx, n.name, args)
}

// ---- values ----

// Truthy applies the language's truthiness: nil/false/0/""/[]/{} are false.
func Truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0
	case int:
		return x != 0
	case int64:
		return x != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

func toString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'g', -1, 64)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case []any:
		parts := make([]string, 0, len(x))
		for _, e := range x {
			parts = append(parts, toString(e))
		}
		return strings.Join(parts, ",")
	}
	return fmt.Sprint(v)
}

// ToString is the exported form of toString (used by the interpolator).
func ToString(v any) string { return toString(v) }

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f, err == nil
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

func toInt(v any) (int, bool) {
	f, ok := toFloat(v)
	if !ok {
		return 0, false
	}
	return int(f), true
}

func equal(a, b any) bool {
	// Numeric comparison first: "1" == 1 is true, like the original language.
	if af, ok := toFloat(a); ok {
		if bf, ok := toFloat(b); ok {
			// Only compare as numbers when neither side is a plain string that
			// does not parse as a number.
			_, aStr := a.(string)
			_, bStr := b.(string)
			if !(aStr && bStr) {
				return af == bf
			}
		}
	}
	as, aIsStr := a.(string)
	bs, bIsStr := b.(string)
	if aIsStr && bIsStr {
		return strings.EqualFold(as, bs)
	}
	ab, aIsBool := a.(bool)
	bb, bIsBool := b.(bool)
	if aIsBool && bIsBool {
		return ab == bb
	}
	if a == nil && b == nil {
		return true
	}
	return toString(a) == toString(b)
}

func compare(a, b any) (int, bool) {
	af, aok := toFloat(a)
	bf, bok := toFloat(b)
	if aok && bok {
		switch {
		case af < bf:
			return -1, true
		case af > bf:
			return 1, true
		}
		return 0, true
	}
	as, bs := toString(a), toString(b)
	return strings.Compare(as, bs), true
}

// getField reads a property from a map (nil for anything else). Property
// lookup is case-insensitive because YAML/JSON context keys are written both
// ways in practice (e.g. `github.event` vs `github.Event`).
func getField(v any, name string) any {
	switch x := v.(type) {
	case map[string]any:
		if val, ok := x[name]; ok {
			return val
		}
		for k, val := range x {
			if strings.EqualFold(k, name) {
				return val
			}
		}
	}
	return nil
}

// callFunc dispatches the whitelisted function table. There is no user-defined
// function and no way to reach the outside world.
func callFunc(ctx *Ctx, name string, args []any) (any, error) {
	n := strings.ToLower(name)
	switch n {
	case "contains":
		if len(args) != 2 {
			return nil, fmt.Errorf("expr: contains(search, item) takes 2 arguments")
		}
		if list, ok := args[0].([]any); ok {
			for _, e := range list {
				if equal(e, args[1]) {
					return true, nil
				}
			}
			return false, nil
		}
		return strings.Contains(
			strings.ToLower(toString(args[0])),
			strings.ToLower(toString(args[1])),
		), nil

	case "startswith":
		if len(args) != 2 {
			return nil, fmt.Errorf("expr: startsWith(s, prefix) takes 2 arguments")
		}
		return strings.HasPrefix(strings.ToLower(toString(args[0])), strings.ToLower(toString(args[1]))), nil

	case "endswith":
		if len(args) != 2 {
			return nil, fmt.Errorf("expr: endsWith(s, suffix) takes 2 arguments")
		}
		return strings.HasSuffix(strings.ToLower(toString(args[0])), strings.ToLower(toString(args[1]))), nil

	case "format":
		if len(args) == 0 {
			return nil, fmt.Errorf("expr: format(fmt, ...) takes at least 1 argument")
		}
		return format(toString(args[0]), args[1:])

	case "join":
		if len(args) == 0 || len(args) > 2 {
			return nil, fmt.Errorf("expr: join(list, sep?) takes 1 or 2 arguments")
		}
		sep := ","
		if len(args) == 2 {
			sep = toString(args[1])
		}
		list, ok := args[0].([]any)
		if !ok {
			return toString(args[0]), nil
		}
		parts := make([]string, 0, len(list))
		for _, e := range list {
			parts = append(parts, toString(e))
		}
		return strings.Join(parts, sep), nil

	case "tojson":
		if len(args) != 1 {
			return nil, fmt.Errorf("expr: toJSON(v) takes 1 argument")
		}
		return toJSON(args[0]), nil

	case "fromjson":
		if len(args) != 1 {
			return nil, fmt.Errorf("expr: fromJSON(s) takes 1 argument")
		}
		return fromJSON(toString(args[0]))

	case "hashfiles":
		if ctx == nil || ctx.HashFiles == nil {
			return "", nil
		}
		pats := make([]string, 0, len(args))
		for _, a := range args {
			pats = append(pats, toString(a))
		}
		return ctx.HashFiles(pats)

	case "success":
		return ctxStatus(ctx) == StatusSuccess, nil
	case "failure":
		return ctxStatus(ctx) == StatusFailure, nil
	case "cancelled", "canceled":
		return ctxStatus(ctx) == StatusCancelled, nil
	case "always":
		return true, nil
	}
	return nil, fmt.Errorf("expr: unknown function %q", name)
}

// Status is the coarse state of the current job as seen by `if:` expressions.
type Status string

const (
	StatusSuccess   Status = "success"
	StatusFailure   Status = "failure"
	StatusCancelled Status = "cancelled"
)

func ctxStatus(ctx *Ctx) Status {
	if ctx == nil || ctx.Status == "" {
		return StatusSuccess
	}
	return ctx.Status
}

// format implements `format('{0} {1}', a, b)` with `{{` / `}}` escapes.
func format(f string, args []any) (string, error) {
	var b strings.Builder
	for i := 0; i < len(f); i++ {
		c := f[i]
		if c == '{' {
			if i+1 < len(f) && f[i+1] == '{' {
				b.WriteByte('{')
				i++
				continue
			}
			j := i + 1
			for j < len(f) && f[j] != '}' {
				j++
			}
			if j >= len(f) {
				return "", fmt.Errorf("expr: format: unterminated placeholder in %q", f)
			}
			idx, err := strconv.Atoi(strings.TrimSpace(f[i+1 : j]))
			if err != nil || idx < 0 || idx >= len(args) {
				return "", fmt.Errorf("expr: format: bad placeholder %q", f[i:j+1])
			}
			b.WriteString(toString(args[idx]))
			i = j
			continue
		}
		if c == '}' && i+1 < len(f) && f[i+1] == '}' {
			b.WriteByte('}')
			i++
			continue
		}
		b.WriteByte(c)
	}
	return b.String(), nil
}

func toJSON(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		if x {
			return "true"
		}
		return "false"
	case string:
		return quoteJSON(x)
	case float64, int, int64:
		return toString(x)
	case []any:
		parts := make([]string, 0, len(x))
		for _, e := range x {
			parts = append(parts, toJSON(e))
		}
		return "[" + strings.Join(parts, ",") + "]"
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, quoteJSON(k)+":"+toJSON(x[k]))
		}
		return "{" + strings.Join(parts, ",") + "}"
	}
	return quoteJSON(fmt.Sprint(v))
}

func quoteJSON(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// fromJSON parses a JSON document into the expression value model. Objects
// become map[string]any, arrays []any, numbers float64.
func fromJSON(s string) (any, error) {
	p := &jsonParser{s: s}
	p.ws()
	v, err := p.value()
	if err != nil {
		return nil, fmt.Errorf("expr: fromJSON: %w", err)
	}
	p.ws()
	if p.i != len(p.s) {
		return nil, fmt.Errorf("expr: fromJSON: trailing data at %d", p.i)
	}
	return v, nil
}

type jsonParser struct {
	s string
	i int
}

func (p *jsonParser) ws() {
	for p.i < len(p.s) {
		switch p.s[p.i] {
		case ' ', '\t', '\n', '\r':
			p.i++
		default:
			return
		}
	}
}

func (p *jsonParser) value() (any, error) {
	if p.i >= len(p.s) {
		return nil, fmt.Errorf("unexpected end of input")
	}
	switch c := p.s[p.i]; {
	case c == '{':
		return p.object()
	case c == '[':
		return p.array()
	case c == '"':
		return p.str()
	case c == 't' || c == 'f' || c == 'n':
		for _, lit := range []struct {
			txt string
			val any
		}{{"true", true}, {"false", false}, {"null", nil}} {
			if strings.HasPrefix(p.s[p.i:], lit.txt) {
				p.i += len(lit.txt)
				return lit.val, nil
			}
		}
		return nil, fmt.Errorf("bad literal at %d", p.i)
	default:
		return p.number()
	}
}

func (p *jsonParser) object() (any, error) {
	p.i++ // {
	out := map[string]any{}
	p.ws()
	if p.i < len(p.s) && p.s[p.i] == '}' {
		p.i++
		return out, nil
	}
	for {
		p.ws()
		k, err := p.str()
		if err != nil {
			return nil, err
		}
		p.ws()
		if p.i >= len(p.s) || p.s[p.i] != ':' {
			return nil, fmt.Errorf("expected ':' at %d", p.i)
		}
		p.i++
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		out[k] = v
		p.ws()
		if p.i >= len(p.s) {
			return nil, fmt.Errorf("unterminated object")
		}
		if p.s[p.i] == ',' {
			p.i++
			continue
		}
		if p.s[p.i] == '}' {
			p.i++
			return out, nil
		}
		return nil, fmt.Errorf("expected ',' or '}' at %d", p.i)
	}
}

func (p *jsonParser) array() (any, error) {
	p.i++ // [
	out := []any{}
	p.ws()
	if p.i < len(p.s) && p.s[p.i] == ']' {
		p.i++
		return out, nil
	}
	for {
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		p.ws()
		if p.i >= len(p.s) {
			return nil, fmt.Errorf("unterminated array")
		}
		if p.s[p.i] == ',' {
			p.i++
			p.ws()
			continue
		}
		if p.s[p.i] == ']' {
			p.i++
			return out, nil
		}
		return nil, fmt.Errorf("expected ',' or ']' at %d", p.i)
	}
}

func (p *jsonParser) str() (string, error) {
	if p.i >= len(p.s) || p.s[p.i] != '"' {
		return "", fmt.Errorf("expected a string at %d", p.i)
	}
	p.i++
	var b strings.Builder
	for p.i < len(p.s) {
		c := p.s[p.i]
		if c == '"' {
			p.i++
			return b.String(), nil
		}
		if c == '\\' && p.i+1 < len(p.s) {
			p.i++
			switch p.s[p.i] {
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			case 'u':
				if p.i+4 >= len(p.s) {
					return "", fmt.Errorf("bad \\u escape")
				}
				code, err := strconv.ParseUint(p.s[p.i+1:p.i+5], 16, 32)
				if err != nil {
					return "", fmt.Errorf("bad \\u escape")
				}
				b.WriteRune(rune(code))
				p.i += 4
			default:
				b.WriteByte(p.s[p.i])
			}
			p.i++
			continue
		}
		b.WriteByte(c)
		p.i++
	}
	return "", fmt.Errorf("unterminated string")
}

func (p *jsonParser) number() (any, error) {
	j := p.i
	for j < len(p.s) && strings.IndexByte("+-.eE0123456789", p.s[j]) >= 0 {
		j++
	}
	if j == p.i {
		return nil, fmt.Errorf("unexpected %q at %d", p.s[p.i], p.i)
	}
	f, err := strconv.ParseFloat(p.s[p.i:j], 64)
	if err != nil {
		return nil, fmt.Errorf("bad number %q", p.s[p.i:j])
	}
	p.i = j
	return f, nil
}
