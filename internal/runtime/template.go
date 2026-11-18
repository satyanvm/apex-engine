// Package runtime executes DAGs durably on Temporal.
package runtime

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/satyanvm/apex-engine/internal/compiler"
)

func init() { compiler.ConditionChecker = CheckCondition }

var token = regexp.MustCompile(`\{\{\s*([^}]+?)\s*\}\}`)

// Scope is what a node's templates can see.
type Scope struct {
	Input   map[string]any
	Outputs map[string]map[string]any // direct dependencies only
	Item    any
	HasItem bool
	Loop    map[string]any
}

func (s Scope) root() map[string]any {
	outputs := make(map[string]any, len(s.Outputs))
	for k, v := range s.Outputs {
		outputs[k] = v
	}
	root := map[string]any{"input": s.Input, "outputs": outputs}
	if s.HasItem {
		root["item"] = s.Item
		root["loop"] = s.Loop
	}
	return root
}

// Lookup resolves a dotted reference. A string holding JSON is entered when the
// path continues into it. Missing references are errors, never empty strings.
func (s Scope) Lookup(expr string) (any, error) {
	var cur any = s.root()
	for _, part := range strings.Split(strings.TrimSpace(expr), ".") {
		cur = decodeJSONString(cur)
		switch t := cur.(type) {
		case map[string]any:
			v, ok := t[part]
			if !ok {
				return nil, fmt.Errorf("reference not found: %s", expr)
			}
			cur = v
		case []any:
			i, err := strconv.Atoi(part)
			if err != nil || i < 0 || i >= len(t) {
				return nil, fmt.Errorf("reference not found: %s", expr)
			}
			cur = t[i]
		default:
			return nil, fmt.Errorf("reference not found: %s", expr)
		}
	}
	return cur, nil
}

func decodeJSONString(v any) any {
	s, ok := v.(string)
	if !ok {
		return v
	}
	trimmed := strings.TrimSpace(s)
	if trimmed == "" || (trimmed[0] != '{' && trimmed[0] != '[') {
		return v
	}
	var out any
	if json.Unmarshal([]byte(trimmed), &out) == nil {
		return out
	}
	return v
}

func (s Scope) resolve(raw string) (any, string, error) {
	expr, helper := strings.TrimSpace(raw), ""
	for _, h := range []string{"json ", "urlencode "} {
		if strings.HasPrefix(expr, h) {
			helper, expr = strings.TrimSpace(h), strings.TrimSpace(expr[len(h):])
		}
	}
	v, err := s.Lookup(expr)
	if err != nil {
		return nil, "", err
	}
	if v == nil {
		return nil, "", fmt.Errorf("reference is null: %s", expr)
	}
	return v, helper, nil
}

func stringify(v any) (string, error) {
	if s, ok := v.(string); ok {
		return s, nil
	}
	raw, err := json.Marshal(v)
	return string(raw), err
}

// Render expands a config value. A string that is exactly one reference keeps
// the referenced value's type, so a list stays a list; anything else renders as text.
func (s Scope) Render(v any) (any, error) {
	switch t := v.(type) {
	case string:
		if m := token.FindStringSubmatch(t); m != nil && m[0] == strings.TrimSpace(t) {
			val, helper, err := s.resolve(m[1])
			if err != nil {
				return nil, err
			}
			if helper == "" {
				return val, nil
			}
		}
		return s.RenderString(t)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			r, err := s.Render(x)
			if err != nil {
				return nil, err
			}
			out[k] = r
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			r, err := s.Render(x)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	}
	return v, nil
}

// RenderString expands every {{...}} in a string.
func (s Scope) RenderString(tpl string) (string, error) {
	var firstErr error
	out := token.ReplaceAllStringFunc(tpl, func(m string) string {
		if firstErr != nil {
			return ""
		}
		val, helper, err := s.resolve(token.FindStringSubmatch(m)[1])
		if err != nil {
			firstErr = err
			return ""
		}
		switch helper {
		case "json":
			raw, err := json.Marshal(val)
			if err != nil {
				firstErr = err
			}
			return string(raw)
		case "urlencode":
			text, err := stringify(val)
			if err != nil {
				firstErr = err
			}
			return url.QueryEscape(text)
		}
		text, err := stringify(val)
		if err != nil {
			firstErr = err
		}
		return text
	})
	return out, firstErr
}

// EvalCondition evaluates a gate's condition. A condition is a comparison
// ("<lhs> <op> <rhs>", op one of == != > < >= <= contains), a bare reference
// tested for truthiness, or several of those joined with && and ||, negated
// with ! and grouped with parentheses. Anything that does not parse fails,
// rather than opening a branch.
func (s Scope) EvalCondition(expr string) (bool, error) {
	expr = strings.TrimSpace(token.ReplaceAllString(expr, "$1"))
	if expr == "" {
		return false, fmt.Errorf("condition is empty")
	}
	toks, err := condTokens(expr)
	if err != nil {
		return false, err
	}
	p := &condParser{s: s, toks: toks, expr: expr}
	v, err := p.or()
	if err != nil {
		return false, err
	}
	if p.pos != len(p.toks) {
		return false, fmt.Errorf("condition has an unexpected %q: %s", p.toks[p.pos].text, expr)
	}
	return v, nil
}

// CheckCondition reports whether a condition parses, without evaluating it.
func CheckCondition(expr string) error {
	expr = strings.TrimSpace(token.ReplaceAllString(expr, "$1"))
	if expr == "" {
		return fmt.Errorf("condition is empty")
	}
	toks, err := condTokens(expr)
	if err != nil {
		return err
	}
	p := &condParser{toks: toks, expr: expr, dry: true}
	if _, err := p.or(); err != nil {
		return err
	}
	if p.pos != len(p.toks) {
		return fmt.Errorf("condition has an unexpected %q: %s", p.toks[p.pos].text, expr)
	}
	return nil
}

type condTok struct {
	kind string // op, logic, not, lparen, rparen, str, word
	text string
}

func condTokens(expr string) ([]condTok, error) {
	var out []condTok
	for i := 0; i < len(expr); {
		c := expr[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n':
			i++
		case c == '(':
			out, i = append(out, condTok{"lparen", "("}), i+1
		case c == ')':
			out, i = append(out, condTok{"rparen", ")"}), i+1
		case c == '"' || c == '\'':
			end := strings.IndexByte(expr[i+1:], c)
			if end < 0 {
				return nil, fmt.Errorf("condition has an unclosed quote: %s", expr)
			}
			out, i = append(out, condTok{"str", expr[i+1 : i+1+end]}), i+end+2
		case strings.HasPrefix(expr[i:], "&&") || strings.HasPrefix(expr[i:], "||"):
			out, i = append(out, condTok{"logic", expr[i : i+2]}), i+2
		case strings.HasPrefix(expr[i:], "==") || strings.HasPrefix(expr[i:], "!=") || strings.HasPrefix(expr[i:], ">=") || strings.HasPrefix(expr[i:], "<="):
			out, i = append(out, condTok{"op", expr[i : i+2]}), i+2
		case c == '>' || c == '<':
			out, i = append(out, condTok{"op", string(c)}), i+1
		case c == '!':
			out, i = append(out, condTok{"not", "!"}), i+1
		default:
			j := i
			for j < len(expr) && !strings.ContainsRune(" \t\n()\"'!<>=&|", rune(expr[j])) {
				j++
			}
			if j == i {
				return nil, fmt.Errorf("condition has an unexpected %q: %s", string(c), expr)
			}
			word := expr[i:j]
			switch strings.ToLower(word) {
			case "and":
				out = append(out, condTok{"logic", "&&"})
			case "or":
				out = append(out, condTok{"logic", "||"})
			case "not":
				out = append(out, condTok{"not", "!"})
			case "contains":
				out = append(out, condTok{"op", "contains"})
			default:
				out = append(out, condTok{"word", word})
			}
			i = j
		}
	}
	return out, nil
}

type condParser struct {
	s    Scope
	toks []condTok
	pos  int
	expr string
	dry  bool // parse only
}

func (p *condParser) peek() (condTok, bool) {
	if p.pos < len(p.toks) {
		return p.toks[p.pos], true
	}
	return condTok{}, false
}

func (p *condParser) or() (bool, error) {
	v, err := p.and()
	if err != nil {
		return false, err
	}
	for {
		t, ok := p.peek()
		if !ok || t.kind != "logic" || t.text != "||" {
			return v, nil
		}
		p.pos++
		r, err := p.and()
		if err != nil {
			return false, err
		}
		v = v || r
	}
}

func (p *condParser) and() (bool, error) {
	v, err := p.unary()
	if err != nil {
		return false, err
	}
	for {
		t, ok := p.peek()
		if !ok || t.kind != "logic" || t.text != "&&" {
			return v, nil
		}
		p.pos++
		r, err := p.unary()
		if err != nil {
			return false, err
		}
		v = v && r
	}
}

func (p *condParser) unary() (bool, error) {
	t, ok := p.peek()
	if !ok {
		return false, fmt.Errorf("condition ends too early: %s", p.expr)
	}
	switch t.kind {
	case "not":
		p.pos++
		v, err := p.unary()
		return !v, err
	case "lparen":
		p.pos++
		v, err := p.or()
		if err != nil {
			return false, err
		}
		if c, ok := p.peek(); !ok || c.kind != "rparen" {
			return false, fmt.Errorf("condition is missing a closing parenthesis: %s", p.expr)
		}
		p.pos++
		return v, nil
	}
	return p.comparison()
}

// operand reads one side of a comparison. Unquoted words that are not
// references run together, so "outputs.x.name == New York" still works.
func (p *condParser) operand() (any, bool, error) {
	t, ok := p.peek()
	if !ok || (t.kind != "str" && t.kind != "word") {
		return nil, false, fmt.Errorf("condition is missing a value: %s", p.expr)
	}
	p.pos++
	if t.kind == "str" {
		return t.text, false, nil
	}
	text := t.text
	if isRef(text) {
		if p.dry {
			return nil, true, nil
		}
		v, err := p.s.Lookup(text)
		return v, true, err
	}
	for {
		n, ok := p.peek()
		if !ok || n.kind != "word" || isRef(n.text) {
			break
		}
		text += " " + n.text
		p.pos++
	}
	return text, false, nil
}

func isRef(tok string) bool {
	return strings.HasPrefix(tok, "input.") || strings.HasPrefix(tok, "outputs.") || tok == "item" || strings.HasPrefix(tok, "item.") || strings.HasPrefix(tok, "loop.")
}

func (p *condParser) comparison() (bool, error) {
	lhs, lref, err := p.operand()
	if err != nil {
		return false, err
	}
	t, ok := p.peek()
	if !ok || t.kind != "op" {
		if !lref {
			return false, fmt.Errorf("condition must compare something or test a reference: %s", p.expr)
		}
		return !p.dry && truthy(lhs), nil
	}
	p.pos++
	rhs, _, err := p.operand()
	if err != nil {
		return false, err
	}
	if p.dry {
		return false, nil
	}
	if t.text == "contains" {
		return contains(lhs, rhs), nil
	}
	return compare(lhs, t.text, rhs, p.expr)
}

// contains tests a substring of text, or membership of a list.
func contains(l, r any) bool {
	if list, ok := decodeJSONString(l).([]any); ok {
		for _, x := range list {
			if fmt.Sprint(coerce(x)) == fmt.Sprint(coerce(r)) {
				return true
			}
		}
		return false
	}
	ls, err := stringify(l)
	if err != nil {
		return false
	}
	rs, err := stringify(r)
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(ls), strings.ToLower(rs))
}

func coerce(v any) any {
	s, ok := v.(string)
	if !ok {
		return v
	}
	switch strings.ToLower(s) {
	case "true":
		return true
	case "false":
		return false
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	return s
}

func compare(l any, op string, r any, expr string) (bool, error) {
	l, r = coerce(l), coerce(r)
	lf, lnum := l.(float64)
	rf, rnum := r.(float64)
	if lnum && rnum {
		switch op {
		case "==":
			return lf == rf, nil
		case "!=":
			return lf != rf, nil
		case ">":
			return lf > rf, nil
		case "<":
			return lf < rf, nil
		case ">=":
			return lf >= rf, nil
		default:
			return lf <= rf, nil
		}
	}
	ls, lstr := l.(string)
	rs, rstr := r.(string)
	switch op {
	case "==", "!=":
		eq := fmt.Sprint(l) == fmt.Sprint(r)
		return eq == (op == "=="), nil
	}
	if lstr && rstr {
		switch op {
		case ">":
			return ls > rs, nil
		case "<":
			return ls < rs, nil
		case ">=":
			return ls >= rs, nil
		default:
			return ls <= rs, nil
		}
	}
	return false, fmt.Errorf("condition operands are not comparable: %s", expr)
}

func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case float64:
		return t != 0
	case []any:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	}
	return true
}
