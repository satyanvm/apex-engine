// Package catalog turns OpenAPI documents and MCP servers into planner nodes.
package catalog

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/satyanvm/apex-engine/internal/compiler"
	"gopkg.in/yaml.v3"
)

const (
	maxOperations = 1500
	maxDescLen    = 400
)

// ImportedAPI is the result of parsing an OpenAPI document.
type ImportedAPI struct {
	BaseURL string
	Nodes   []compiler.CatalogNode
}

var (
	nonSlug  = regexp.MustCompile(`[^a-z0-9_]+`)
	nonIdent = regexp.MustCompile(`[^a-zA-Z0-9_]+`)
	varRe    = regexp.MustCompile(`\{([^{}]+)\}`)
	methods  = []string{"get", "post", "put", "patch", "delete"}
	skipHdr  = map[string]bool{"authorization": true, "accept": true, "content-type": true}
)

// Slug makes a node-id-safe name.
func Slug(s string) string {
	s = strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(s), "_"), "_")
	if s == "" {
		return "tool"
	}
	return s
}

// normalize converts YAML's map[any]any (response codes parse as ints) to
// string-keyed maps, so the walk below has one shape.
func normalize(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			t[k] = normalize(x)
		}
		return t
	case map[any]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[fmt.Sprint(k)] = normalize(x)
		}
		return out
	case []any:
		for i, x := range t {
			t[i] = normalize(x)
		}
	}
	return v
}

type doc struct{ root map[string]any }

// deref follows a local $ref, with a depth limit for circular documents.
func (d doc) deref(v any) map[string]any {
	m, _ := v.(map[string]any)
	for depth := 0; m != nil && depth < 12; depth++ {
		ref, ok := m["$ref"].(string)
		if !ok {
			return m
		}
		if !strings.HasPrefix(ref, "#/") {
			return nil
		}
		var cur any = d.root
		for _, part := range strings.Split(ref[2:], "/") {
			part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
			next, _ := cur.(map[string]any)
			if next == nil {
				return nil
			}
			cur = next[part]
		}
		m, _ = cur.(map[string]any)
	}
	return m
}

// ParseOpenAPI reads an OpenAPI 3.x document (JSON or YAML). specURL resolves a
// relative servers entry; baseOverride, when set, replaces servers altogether.
func ParseOpenAPI(raw []byte, slug, specURL, baseOverride string) (*ImportedAPI, error) {
	var parsed any
	if err := yaml.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("spec is neither valid JSON nor YAML: %w", err)
	}
	root, _ := normalize(parsed).(map[string]any)
	if root == nil {
		return nil, errors.New("spec is not an object")
	}
	version, _ := root["openapi"].(string)
	if !strings.HasPrefix(version, "3.") {
		if _, swagger := root["swagger"]; swagger {
			return nil, errors.New("Swagger 2.0 is not supported; convert the spec to OpenAPI 3")
		}
		return nil, errors.New("not an OpenAPI 3 document (missing openapi: 3.x)")
	}
	d := doc{root: root}
	base, err := baseURL(root, specURL, baseOverride)
	if err != nil {
		return nil, err
	}
	paths, _ := root["paths"].(map[string]any)
	if len(paths) == 0 {
		return nil, errors.New("spec has no paths")
	}
	pathKeys := make([]string, 0, len(paths))
	for p := range paths {
		pathKeys = append(pathKeys, p)
	}
	sort.Strings(pathKeys)

	used := map[string]bool{}
	var nodes []compiler.CatalogNode
	for _, p := range pathKeys {
		item := d.deref(paths[p])
		if item == nil {
			continue
		}
		for _, method := range methods {
			op, _ := item[method].(map[string]any)
			if op == nil {
				continue
			}
			if len(nodes) == maxOperations {
				return nil, fmt.Errorf("spec has more than %d operations", maxOperations)
			}
			nodes = append(nodes, d.operationNode(slug, p, method, item, op, used))
		}
	}
	if len(nodes) == 0 {
		return nil, errors.New("spec has no operations")
	}
	return &ImportedAPI{BaseURL: base, Nodes: nodes}, nil
}

func baseURL(root map[string]any, specURL, override string) (string, error) {
	raw := strings.TrimSpace(override)
	if raw == "" {
		if servers, _ := root["servers"].([]any); len(servers) > 0 {
			s, _ := servers[0].(map[string]any)
			raw, _ = s["url"].(string)
			vars, _ := s["variables"].(map[string]any)
			raw = varRe.ReplaceAllStringFunc(raw, func(m string) string {
				v, _ := vars[m[1:len(m)-1]].(map[string]any)
				if def, ok := v["default"]; ok {
					return fmt.Sprint(def)
				}
				return m
			})
		}
	}
	if raw == "" {
		return "", errors.New("spec has no servers entry; pass baseUrl")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid server URL %q: %w", raw, err)
	}
	if !u.IsAbs() {
		if specURL == "" {
			return "", fmt.Errorf("server URL %q is relative; pass baseUrl", raw)
		}
		parent, err := url.Parse(specURL)
		if err != nil {
			return "", err
		}
		u = parent.ResolveReference(u)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("server URL must be http or https, got %q", u.Scheme)
	}
	return strings.TrimSuffix(u.String(), "/"), nil
}

func (d doc) operationNode(slug, path, method string, item, op map[string]any, used map[string]bool) compiler.CatalogNode {
	id, _ := op["operationId"].(string)
	id = strings.Trim(nonIdent.ReplaceAllString(id, "_"), "_")
	if id == "" {
		id = strings.Trim(nonIdent.ReplaceAllString(method+"/"+path, "_"), "_")
	}
	for base, i := id, 2; used[id]; i++ {
		id = fmt.Sprintf("%s_%d", base, i)
	}
	used[id] = true

	var (
		fields []compiler.CatalogField
		params []any
		seen   = map[string]bool{}
	)
	all, _ := op["parameters"].([]any)
	shared, _ := item["parameters"].([]any)
	for _, raw := range append(append([]any{}, all...), shared...) { // operation wins
		p := d.deref(raw)
		if p == nil {
			continue
		}
		name, _ := p["name"].(string)
		in, _ := p["in"].(string)
		if name == "" || seen[in+":"+name] || in == "cookie" || (in == "header" && skipHdr[strings.ToLower(name)]) {
			continue
		}
		seen[in+":"+name] = true
		required, _ := p["required"].(bool)
		desc, _ := p["description"].(string)
		fields = append(fields, compiler.CatalogField{
			Key: name, Label: name, Control: "text", Required: required || in == "path",
			Help: clip(strings.TrimSpace(in+" parameter. "+desc), 200),
		})
		params = append(params, map[string]any{"name": name, "in": in})
	}
	hasBody := false
	if body := d.deref(op["requestBody"]); body != nil {
		hasBody = true
		required, _ := body["required"].(bool)
		fields = append(fields, compiler.CatalogField{
			Key: "body", Label: "Request body", Control: "json", Required: required,
			Help: clip("JSON request body. "+d.bodyHint(body), 500),
		})
	}

	summary, _ := op["summary"].(string)
	desc, _ := op["description"].(string)
	text := strings.TrimSpace(summary + " " + desc)
	if text == "" {
		text = strings.ToUpper(method) + " " + path
	}
	if shape := d.responseHint(op); shape != "" {
		text += " Returns: " + shape
	}
	label := summary
	if label == "" {
		label = id
	}
	group := ""
	if tags, _ := op["tags"].([]any); len(tags) > 0 {
		group, _ = tags[0].(string)
	}
	return compiler.CatalogNode{
		ID: "api." + slug + "." + id, Label: clip(label, 80), Description: clip(text, maxDescLen),
		Kind: "action", Runtime: "http", Config: fields, Outputs: []string{"status", "body"},
		Exec: map[string]any{"method": strings.ToUpper(method), "path": path, "params": params, "hasBody": hasBody, "group": group},
	}
}

func (d doc) jsonSchema(holder map[string]any) map[string]any {
	content, _ := holder["content"].(map[string]any)
	for _, mt := range []string{"application/json", "application/*+json", "*/*"} {
		if media, ok := content[mt].(map[string]any); ok {
			return d.deref(media["schema"])
		}
	}
	return nil
}

func (d doc) schemaSummary(s map[string]any) string {
	if s == nil {
		return ""
	}
	if t, _ := s["type"].(string); t == "array" {
		if inner := d.schemaSummary(d.deref(s["items"])); inner != "" {
			return "array of {" + inner + "}"
		}
		return "array"
	}
	props, _ := s["properties"].(map[string]any)
	names := make([]string, 0, len(props))
	for k := range props {
		names = append(names, k)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, k := range names {
		t, _ := d.deref(props[k])["type"].(string)
		if t == "" {
			t = "object"
		}
		parts = append(parts, k+": "+t)
	}
	return strings.Join(parts, ", ")
}

func (d doc) bodyHint(body map[string]any) string {
	if s := d.schemaSummary(d.jsonSchema(body)); s != "" {
		return "Fields: " + s
	}
	return ""
}

func (d doc) responseHint(op map[string]any) string {
	responses, _ := op["responses"].(map[string]any)
	for _, code := range []string{"200", "201", "202"} {
		if r := d.deref(responses[code]); r != nil {
			if s := d.schemaSummary(d.jsonSchema(r)); s != "" {
				return "body has " + s
			}
		}
	}
	return ""
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
