package main

// A GraphQL v4 subset: just enough of the language (operations, variables,
// arguments, aliases, fragment expansion) and of the schema (viewer,
// repository, issues, pull requests, node, introspection, and the five
// mutations the suite exercises) to serve the real queries byte-faithfully.

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
)

// ---- global ids ----

func globalID(typ string, dbID int) string {
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%s:%d", typ, dbID)))
}

func parseGlobalID(s string) (string, int, bool) {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		// Tolerate raw "Type:123" too.
		raw = []byte(s)
	}
	typ, num, ok := strings.Cut(string(raw), ":")
	if !ok {
		return "", 0, false
	}
	n, err := strconv.Atoi(num)
	if err != nil {
		return "", 0, false
	}
	return typ, n, true
}

// ---- lexer ----

type gqlToken struct {
	kind string // name, string, int, float, punct, var, eof
	val  string
}

func gqlLex(src string) ([]gqlToken, error) {
	var out []gqlToken
	i := 0
	for i < len(src) {
		ch := src[i]
		switch {
		case ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' || ch == ',':
			i++
		case ch == '#':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case ch == '"':
			if strings.HasPrefix(src[i:], `"""`) {
				end := strings.Index(src[i+3:], `"""`)
				if end < 0 {
					return nil, fmt.Errorf("unterminated string")
				}
				out = append(out, gqlToken{"string", src[i+3 : i+3+end]})
				i += 3 + end + 3
			} else {
				j := i + 1
				var sb strings.Builder
				for j < len(src) && src[j] != '"' {
					if src[j] == '\\' && j+1 < len(src) {
						j++
						switch src[j] {
						case 'n':
							sb.WriteByte('\n')
						case 't':
							sb.WriteByte('\t')
						case 'r':
							sb.WriteByte('\r')
						case 'u':
							if j+4 < len(src) {
								if v, err := strconv.ParseUint(src[j+1:j+5], 16, 32); err == nil {
									sb.WriteRune(rune(v))
									j += 4
								}
							}
						default:
							sb.WriteByte(src[j])
						}
						j++
					} else {
						sb.WriteByte(src[j])
						j++
					}
				}
				if j >= len(src) {
					return nil, fmt.Errorf("unterminated string")
				}
				out = append(out, gqlToken{"string", sb.String()})
				i = j + 1
			}
		case ch == '$':
			j := i + 1
			for j < len(src) && (isNameChar(src[j])) {
				j++
			}
			out = append(out, gqlToken{"var", src[i+1 : j]})
			i = j
		case ch == '.' && strings.HasPrefix(src[i:], "..."):
			out = append(out, gqlToken{"punct", "..."})
			i += 3
		case strings.IndexByte("{}()[]:=@|&!", ch) >= 0:
			out = append(out, gqlToken{"punct", string(ch)})
			i++
		case ch == '-' || (ch >= '0' && ch <= '9'):
			j := i
			if src[j] == '-' {
				j++
			}
			isFloat := false
			for j < len(src) && ((src[j] >= '0' && src[j] <= '9') || src[j] == '.' || src[j] == 'e' || src[j] == 'E' || src[j] == '+' || src[j] == '-') {
				if src[j] == '.' || src[j] == 'e' || src[j] == 'E' {
					isFloat = true
				}
				j++
			}
			kind := "int"
			if isFloat {
				kind = "float"
			}
			out = append(out, gqlToken{kind, src[i:j]})
			i = j
		case isNameStart(ch):
			j := i
			for j < len(src) && isNameChar(src[j]) {
				j++
			}
			out = append(out, gqlToken{"name", src[i:j]})
			i = j
		default:
			return nil, fmt.Errorf("unexpected character %q", ch)
		}
	}
	out = append(out, gqlToken{"eof", ""})
	return out, nil
}

func isNameStart(ch byte) bool {
	return ch == '_' || (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z')
}

func isNameChar(ch byte) bool {
	return isNameStart(ch) || (ch >= '0' && ch <= '9')
}

// ---- parser ----

type gqlField struct {
	alias    string
	name     string
	args     map[string]any
	children []*gqlField
	spread   string // fragment spread name, when non-empty
}

type gqlOperation struct {
	typ       string // query | mutation
	name      string
	selection []*gqlField
}

type gqlParser struct {
	toks []gqlToken
	pos  int
	vars map[string]any
}

func (p *gqlParser) peek() gqlToken { return p.toks[p.pos] }
func (p *gqlParser) next() gqlToken { t := p.toks[p.pos]; p.pos++; return t }
func (p *gqlParser) expectPunct(v string) error {
	t := p.next()
	if t.kind != "punct" || t.val != v {
		return fmt.Errorf("expected %q, got %q", v, t.val)
	}
	return nil
}

func (p *gqlParser) parseValue() (any, error) {
	t := p.next()
	switch t.kind {
	case "string":
		return t.val, nil
	case "int":
		n, err := strconv.Atoi(t.val)
		return n, err
	case "float":
		return strconv.ParseFloat(t.val, 64)
	case "var":
		return p.vars[t.val], nil
	case "name":
		switch t.val {
		case "true":
			return true, nil
		case "false":
			return false, nil
		case "null":
			return nil, nil
		default:
			return gqlEnum(t.val), nil
		}
	case "punct":
		if t.val == "[" {
			var out []any
			for p.peek().val != "]" {
				v, err := p.parseValue()
				if err != nil {
					return nil, err
				}
				out = append(out, v)
			}
			p.next()
			return out, nil
		}
		if t.val == "{" {
			out := map[string]any{}
			for p.peek().val != "}" {
				nameTok := p.next()
				if nameTok.kind != "name" {
					return nil, fmt.Errorf("expected object field name")
				}
				if err := p.expectPunct(":"); err != nil {
					return nil, err
				}
				v, err := p.parseValue()
				if err != nil {
					return nil, err
				}
				out[nameTok.val] = v
			}
			p.next()
			return out, nil
		}
	}
	return nil, fmt.Errorf("unexpected value token %q", t.val)
}

type gqlEnum string

func (p *gqlParser) parseSelectionSet() ([]*gqlField, error) {
	if err := p.expectPunct("{"); err != nil {
		return nil, err
	}
	var out []*gqlField
	for p.peek().val != "}" {
		if p.peek().kind == "eof" {
			return nil, fmt.Errorf("unterminated selection set")
		}
		f, err := p.parseField()
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	p.next()
	return out, nil
}

func (p *gqlParser) parseField() (*gqlField, error) {
	if p.peek().val == "..." {
		p.next()
		nameTok := p.next()
		if nameTok.kind == "name" && nameTok.val == "on" {
			// Inline fragment: parse its selection set as this field's children
			// under a synthetic spread of the type condition.
			typTok := p.next()
			children, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			return &gqlField{spread: "on:" + typTok.val, children: children}, nil
		}
		return &gqlField{spread: nameTok.val}, nil
	}
	nameTok := p.next()
	if nameTok.kind != "name" {
		return nil, fmt.Errorf("expected field name, got %q", nameTok.val)
	}
	f := &gqlField{name: nameTok.val}
	if p.peek().val == ":" {
		p.next()
		realTok := p.next()
		if realTok.kind != "name" {
			return nil, fmt.Errorf("expected field name after alias")
		}
		f.alias = f.name
		f.name = realTok.val
	}
	if p.peek().val == "(" {
		p.next()
		f.args = map[string]any{}
		for p.peek().val != ")" {
			argTok := p.next()
			if argTok.kind != "name" {
				return nil, fmt.Errorf("expected argument name")
			}
			if err := p.expectPunct(":"); err != nil {
				return nil, err
			}
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			f.args[argTok.val] = v
		}
		p.next()
	}
	// Directives: skip (@include(if: ...) etc.).
	for p.peek().val == "@" {
		p.next()
		p.next() // directive name
		if p.peek().val == "(" {
			depth := 0
			for {
				t := p.next()
				if t.val == "(" {
					depth++
				} else if t.val == ")" {
					depth--
					if depth == 0 {
						break
					}
				} else if t.kind == "eof" {
					break
				}
			}
		}
	}
	if p.peek().val == "{" {
		children, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		f.children = children
	}
	return f, nil
}

// parseDocument returns the operations and fragment definitions.
func parseDocument(src string, vars map[string]any) ([]*gqlOperation, map[string][]*gqlField, error) {
	toks, err := gqlLex(src)
	if err != nil {
		return nil, nil, err
	}
	p := &gqlParser{toks: toks, vars: vars}
	var ops []*gqlOperation
	fragments := map[string][]*gqlField{}
	for p.peek().kind != "eof" {
		t := p.peek()
		if t.val == "{" {
			sel, err := p.parseSelectionSet()
			if err != nil {
				return nil, nil, err
			}
			ops = append(ops, &gqlOperation{typ: "query", selection: sel})
			continue
		}
		if t.kind != "name" {
			return nil, nil, fmt.Errorf("unexpected token %q", t.val)
		}
		switch t.val {
		case "query", "mutation", "subscription":
			p.next()
			op := &gqlOperation{typ: t.val}
			if p.peek().kind == "name" {
				op.name = p.next().val
			}
			if p.peek().val == "(" {
				// Variable definitions: skip the parenthesised block.
				depth := 0
				for {
					tok := p.next()
					if tok.val == "(" {
						depth++
					} else if tok.val == ")" {
						depth--
						if depth == 0 {
							break
						}
					} else if tok.kind == "eof" {
						break
					}
				}
			}
			sel, err := p.parseSelectionSet()
			if err != nil {
				return nil, nil, err
			}
			op.selection = sel
			ops = append(ops, op)
		case "fragment":
			p.next()
			nameTok := p.next()
			p.next() // "on"
			p.next() // type name
			sel, err := p.parseSelectionSet()
			if err != nil {
				return nil, nil, err
			}
			fragments[nameTok.val] = sel
		default:
			return nil, nil, fmt.Errorf("unexpected definition %q", t.val)
		}
	}
	return ops, fragments, nil
}

// expandSelections inlines fragment spreads (recursively, cycle-safe).
func expandSelections(sel []*gqlField, fragments map[string][]*gqlField, depth int) []*gqlField {
	if depth > 16 {
		return sel
	}
	var out []*gqlField
	for _, f := range sel {
		if f.spread != "" {
			if strings.HasPrefix(f.spread, "on:") {
				merged := *f
				merged.spread = ""
				merged.name = ""
				merged.children = expandSelections(f.children, fragments, depth+1)
				out = append(out, &merged)
				continue
			}
			if frag, ok := fragments[f.spread]; ok {
				out = append(out, expandSelections(frag, fragments, depth+1)...)
				continue
			}
			continue
		}
		cp := *f
		if len(f.children) > 0 {
			cp.children = expandSelections(f.children, fragments, depth+1)
		}
		out = append(out, &cp)
	}
	return out
}
