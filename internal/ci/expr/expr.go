// Package expr implements the expression language used by workflow conditions
// (`if:`), `${{ }}` interpolation, matrix values and step outputs.
//
// It is a deliberately small, side-effect-free subset of the language GitHub
// Actions uses:
//
//	expr    := or
//	or      := and ( "||" and )*
//	and     := cmp ( "&&" cmp )*
//	cmp     := unary ( ( "==" | "!=" | "<" | "<=" | ">" | ">=" ) unary )?
//	unary   := "!" unary | postfix
//	postfix := primary ( "." ident | "[" expr "]" )*
//	primary := literal | call | "(" expr ")" | ident
//
// There is no shell, no eval and no filesystem access: a value can only reach
// the outside world through `${{ }}` interpolation, and the caller decides what
// to do with it (the platform only ever puts it into an environment variable).
// The single "effectful" function is hashFiles, whose implementation is
// supplied by the caller.
package expr

import (
	"fmt"
	"strconv"
	"strings"
)

// A Program is a parsed expression. Parsing is separated from evaluation so a
// workflow is validated ONCE at load time and then evaluated per job and step.
type Program struct {
	src  string
	root Node
}

// Parse compiles one expression (without the surrounding `${{ }}`).
func Parse(src string) (*Program, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	n, err := p.parseOr()
	if err != nil {
		return nil, fmt.Errorf("expr: %w (in %q)", err, src)
	}
	if t := p.peek(); t.kind != kEOF {
		return nil, fmt.Errorf("expr: unexpected %q at %d in %q", t.text, t.pos, src)
	}
	return &Program{src: src, root: n}, nil
}

// Eval evaluates a parsed program against ctx (nil = no variables).
func (p *Program) Eval(ctx *Ctx) (any, error) { return p.root.eval(ctx) }

// String returns the source the program was parsed from.
func (p *Program) String() string { return p.src }

// ---- lexer ----

type kind int

const (
	kEOF kind = iota
	kIdent
	kString
	kNumber
	kOp
)

type token struct {
	kind kind
	text string
	pos  int
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentPart(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9') || c == '-'
}

func lex(src string) ([]token, error) {
	out := make([]token, 0, 16)
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++

		case c == '\'' || c == '"':
			q := c
			start := i
			i++
			var b strings.Builder
			closed := false
			for i < len(src) {
				if src[i] == q {
					// '' or "" is an escaped quote (SQL/GitHub style).
					if i+1 < len(src) && src[i+1] == q {
						b.WriteByte(q)
						i += 2
						continue
					}
					i++
					closed = true
					break
				}
				// A backslash escape keeps regexes readable: '\d+'.
				if src[i] == '\\' && i+1 < len(src) && (src[i+1] == q || src[i+1] == '\\') {
					b.WriteByte(src[i+1])
					i += 2
					continue
				}
				b.WriteByte(src[i])
				i++
			}
			if !closed {
				return nil, fmt.Errorf("expr: unterminated string at %d", start)
			}
			out = append(out, token{kString, b.String(), start})

		case c >= '0' && c <= '9':
			j := i
			for j < len(src) && ((src[j] >= '0' && src[j] <= '9') || src[j] == '.') {
				j++
			}
			out = append(out, token{kNumber, src[i:j], i})
			i = j

		case isIdentStart(c):
			j := i + 1
			for j < len(src) && isIdentPart(src[j]) {
				j++
			}
			out = append(out, token{kIdent, src[i:j], i})
			i = j

		default:
			if i+1 < len(src) {
				switch two := src[i : i+2]; two {
				case "||", "&&", "==", "!=", "<=", ">=":
					out = append(out, token{kOp, two, i})
					i += 2
					continue
				}
			}
			switch c {
			case '<', '>', '!', '(', ')', '[', ']', '.', ',', '*':
				out = append(out, token{kOp, string(c), i})
				i++
			default:
				return nil, fmt.Errorf("expr: unexpected character %q at %d", c, i)
			}
		}
	}
	out = append(out, token{kEOF, "", len(src)})
	return out, nil
}

// ---- parser ----

type parser struct {
	toks []token
	pos  int
}

func (p *parser) peek() token { return p.toks[p.pos] }

func (p *parser) next() token {
	t := p.toks[p.pos]
	if t.kind != kEOF {
		p.pos++
	}
	return t
}

func (p *parser) isOp(text string) bool {
	t := p.peek()
	return t.kind == kOp && t.text == text
}

func (p *parser) acceptOp(text string) bool {
	if p.isOp(text) {
		p.pos++
		return true
	}
	return false
}

func (p *parser) expectOp(text string) error {
	if !p.acceptOp(text) {
		t := p.peek()
		return fmt.Errorf("expected %q, got %q", text, t.text)
	}
	return nil
}

func (p *parser) parseOr() (Node, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.acceptOp("||") {
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = &binaryNode{op: "||", l: left, r: right}
	}
	return left, nil
}

func (p *parser) parseAnd() (Node, error) {
	left, err := p.parseCmp()
	if err != nil {
		return nil, err
	}
	for p.acceptOp("&&") {
		right, err := p.parseCmp()
		if err != nil {
			return nil, err
		}
		left = &binaryNode{op: "&&", l: left, r: right}
	}
	return left, nil
}

func (p *parser) parseCmp() (Node, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for _, op := range []string{"==", "!=", "<=", ">=", "<", ">"} {
		if p.acceptOp(op) {
			right, err := p.parseUnary()
			if err != nil {
				return nil, err
			}
			return &binaryNode{op: op, l: left, r: right}, nil
		}
	}
	return left, nil
}

func (p *parser) parseUnary() (Node, error) {
	if p.acceptOp("!") {
		x, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return &unaryNode{op: "!", x: x}, nil
	}
	return p.parsePostfix()
}

func (p *parser) parsePostfix() (Node, error) {
	n, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}
	for {
		switch {
		case p.acceptOp("."):
			t := p.next()
			if t.kind != kIdent {
				return nil, fmt.Errorf("expected a property name after '.', got %q", t.text)
			}
			n = &fieldNode{base: n, name: t.text}
		case p.acceptOp("["):
			key, err := p.parseOr()
			if err != nil {
				return nil, err
			}
			if err := p.expectOp("]"); err != nil {
				return nil, err
			}
			n = &indexNode{base: n, key: key}
		default:
			return n, nil
		}
	}
}

func (p *parser) parsePrimary() (Node, error) {
	t := p.next()
	switch t.kind {
	case kNumber:
		f, err := strconv.ParseFloat(t.text, 64)
		if err != nil {
			return nil, fmt.Errorf("bad number %q", t.text)
		}
		return &litNode{v: f}, nil

	case kString:
		return &litNode{v: t.text}, nil

	case kIdent:
		switch strings.ToLower(t.text) {
		case "true":
			return &litNode{v: true}, nil
		case "false":
			return &litNode{v: false}, nil
		case "null":
			return &litNode{v: nil}, nil
		}
		if p.isOp("(") {
			return p.parseCall(t.text)
		}
		return &identNode{name: t.text}, nil

	case kOp:
		if t.text == "(" {
			n, err := p.parseOr()
			if err != nil {
				return nil, err
			}
			if err := p.expectOp(")"); err != nil {
				return nil, err
			}
			return n, nil
		}
	}

	return nil, fmt.Errorf("unexpected %q at %d", t.text, t.pos)
}

func (p *parser) parseCall(name string) (Node, error) {
	if err := p.expectOp("("); err != nil {
		return nil, err
	}
	var args []Node
	for !p.isOp(")") {
		a, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		args = append(args, a)
		if !p.acceptOp(",") {
			break
		}
	}
	if err := p.expectOp(")"); err != nil {
		return nil, err
	}
	return &callNode{name: name, args: args}, nil
}
