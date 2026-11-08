// Package runtime executes DAGs durably on Temporal.
package runtime

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

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

var cond = regexp.MustCompile(`^\s*(.+?)\s*(==|!=|>=|<=|>|<)\s*(.+?)\s*$`)

// EvalCondition is a deliberately tiny comparison: "<lhs> <op> <rhs>" or a bare
// reference for truthiness. Invalid comparisons fail instead of opening a branch.
func (s Scope) EvalCondition(expr string) (bool, error) {
	expr = strings.TrimSpace(token.ReplaceAllString(expr, "$1"))
	if expr == "" {
		return false, fmt.Errorf("condition is empty")
	}
	m := cond.FindStringSubmatch(expr)
	if m == nil {
		v, err := s.Lookup(expr)
		if err != nil {
			return false, err
		}
		return truthy(v), nil
	}
	lhs, err := s.side(m[1])
	if err != nil {
		return false, err
	}
	rhs, err := s.side(m[3])
	if err != nil {
		return false, err
	}
	return compare(lhs, m[2], rhs, expr)
}

func (s Scope) side(tok string) (any, error) {
	tok = strings.TrimSpace(tok)
	if len(tok) >= 2 && (tok[0] == '"' || tok[0] == '\'') && tok[len(tok)-1] == tok[0] {
		return tok[1 : len(tok)-1], nil
	}
	if strings.HasPrefix(tok, "input.") || strings.HasPrefix(tok, "outputs.") || tok == "item" || strings.HasPrefix(tok, "item.") {
		return s.Lookup(tok)
	}
	return tok, nil
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
