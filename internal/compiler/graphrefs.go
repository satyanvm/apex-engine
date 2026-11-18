package compiler

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/itchyny/gojq"
)

// graphrefs.go checks the {{...}} references inside a compiled graph's config
// against what the reasoner will actually have at run time, so a bad reference
// is caught by the reduce repair loop instead of failing the user's first run.
//
// The rules mirror reasoner/nodes.py render(): a node sees the run input and the
// outputs of its DIRECT dependencies only (engine.py / workflow.py build
// `upstream` from dependsOn), and an unknown reference raises.

var tplRef = regexp.MustCompile(`\{\{\s*([^}]+?)\s*\}\}`)

// validateRefs returns one human-readable complaint per bad reference. inputKeys
// are the run-input fields the plan seeds (defaultInput); input.* references are
// not checked when the graph starts from a webhook, whose payload is arbitrary.
func validateRefs(nodes []GraphNode, cat *Catalog, inputKeys map[string]bool) []string {
	typeOf := map[string]string{}
	byKey := map[string]GraphNode{}
	webhook := false
	inLoop := map[string]bool{}
	for _, n := range nodes {
		typeOf[n.Key] = n.Type
		byKey[n.Key] = n
		if n.Type == "trigger.webhook" {
			webhook = true
		}
	}
	for _, n := range nodes {
		if n.Type == "logic.loop" {
			for _, k := range LoopBody(nodes, n.Key) {
				inLoop[k] = true
			}
		}
	}
	var errs []string
	seen := map[string]bool{}
	add := func(msg string) {
		if msg != "" && !seen[msg] {
			seen[msg] = true
			errs = append(errs, msg)
		}
	}
	for _, n := range nodes {
		deps := map[string]bool{}
		for _, d := range n.DependsOn {
			deps[d] = true
		}
		refs := configRefs(n.Config)
		usesItem := false
		switch n.Type {
		case "logic.branch", "logic.filter":
			if cond, ok := n.Config["condition"].(string); ok {
				refs = append(refs, conditionRefs(cond)...)
				usesItem = bareItem.MatchString(quoted.ReplaceAllString(cond, ""))
				if strings.Contains(quoted.ReplaceAllString(cond, ""), "[") {
					add("node '" + n.Key + "': a condition tests ONE value and cannot look inside a list with [ ]." +
						" A gate does not narrow a list. To keep only some items, replace this gate with a data.transform such as" +
						" [.outputs.<key>.items[] | select(.field == \"value\")] and loop over its result")
				}
			}
		case "data.transform":
			// A jq expression reads .outputs.key.field or .outputs["key"].field.
			if expr, ok := n.Config["expression"].(string); ok {
				refs = jqRefs(expr)
				for _, msg := range jqScalarIterated(n, expr, typeOf) {
					add(msg)
				}
				for _, name := range jqRootNames(expr) {
					usesItem = usesItem || name == "item" || name == "loop"
				}
			}
		case "logic.loop":
			// items is a bare dotted ref ("outputs.fetch.result"), not a template.
			if items, ok := n.Config["items"].(string); ok && strings.TrimSpace(items) != "" {
				ref := strings.Trim(strings.TrimSpace(items), "{} ")
				refs = append(refs, ref)
				add(checkLoopItems(n, ref, typeOf, cat))
			}
		}
		if isToolType(n.Type) {
			for _, msg := range checkToolConfig(n, typeOf) {
				add(msg)
			}
		}
		for _, expr := range refs {
			add(checkRef(n, expr, deps, typeOf, byKey, cat, inputKeys, webhook))
			root := strings.SplitN(strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(expr, "json "), "urlencode ")), ".", 2)[0]
			usesItem = usesItem || root == "item" || root == "loop"
		}
		if usesItem && !inLoop[n.Key] {
			add("node '" + n.Key + "' uses the loop item, but it is not inside a loop. {{item}} exists only in nodes that depend on a logic.loop:" +
				" add a logic.loop over the list and make this node depend on it")
		}
	}
	return errs
}

var bareItem = regexp.MustCompile(`(^|[^\w.])item(\.[\w.-]+)?\b`)

func isToolType(t string) bool {
	return strings.HasPrefix(t, "api.") || strings.HasPrefix(t, "mcp.") || strings.HasPrefix(t, "piece.") || t == "tool.http"
}

func isGateType(t string) bool {
	return t == "logic.branch" || t == "logic.filter" || t == "logic.approval"
}

var (
	bareRefValue = regexp.MustCompile(`^\s*(outputs|input|item|loop)\.[A-Za-z0-9_]`)
	soleRef      = regexp.MustCompile(`^\s*\{\{\s*outputs\.([A-Za-z0-9_-]+)\.([A-Za-z0-9_]+)\s*\}\}\s*$`)
)

// checkToolConfig catches two ways a tool call ends up with the wrong value:
// a reference written without {{ }}, which is sent as literal text, and a
// gate's yes/no answer used as if it were data.
func checkToolConfig(n GraphNode, typeOf map[string]string) []string {
	var out []string
	var walk func(field string, v any)
	walk = func(field string, v any) {
		switch t := v.(type) {
		case string:
			if bareRefValue.MatchString(t) && !strings.Contains(t, "{{") {
				out = append(out, "node '"+n.Key+"' config '"+field+"' is the literal text \""+truncateStr(strings.TrimSpace(t), 60)+
					"\", not a reference. Write references as {{outputs.<key>.<field>}} or {{item.<field>}}; there are no expressions or ?: in config values")
			}
			if m := soleRef.FindStringSubmatch(t); m != nil && isGateType(typeOf[m[1]]) && (m[2] == "passed" || m[2] == "approved") {
				out = append(out, "node '"+n.Key+"' config '"+field+"' is set to {{outputs."+m[1]+"."+m[2]+"}}, which is only true or false."+
					" A gate decides whether this node runs; it carries no data. Put the real value here: a literal, {{input.<field>}}, {{item.<field>}} or another node's output")
			}
		case map[string]any:
			for k, x := range t {
				walk(field+"."+k, x)
			}
		case []any:
			for _, x := range t {
				walk(field, x)
			}
		}
	}
	keys := make([]string, 0, len(n.Config))
	for k := range n.Config {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		walk(k, n.Config[k])
	}
	return out
}

// checkLoopItems rejects a loop over something that is known not to be a list.
func checkLoopItems(n GraphNode, ref string, typeOf map[string]string, cat *Catalog) string {
	parts := strings.Split(ref, ".")
	if len(parts) != 3 || parts[0] != "outputs" {
		return ""
	}
	src := typeOf[parts[1]]
	where := "node '" + n.Key + "' loops over " + ref
	if isScalarOutput(src, parts[2]) {
		hint := ""
		if isGateType(src) {
			hint = " A gate does not produce a list. To act on only some items, build the smaller list with data.transform" +
				" ([.outputs.<key>.items[] | select(...)]) and loop over its result"
		}
		return where + ", which is a single value, not a list." + hint
	}
	if spec, ok := cat.ByID[src]; ok && parts[2] == "body" {
		if kind, _ := spec.Exec["bodyType"].(string); kind == "object" {
			return where + ", which is an object" + fieldList(spec) + ", not a list. Point items at the list inside it"
		}
	}
	return ""
}

func fieldList(spec CatalogNode) string {
	fields, _ := spec.Exec["bodyFields"].([]any)
	if len(fields) == 0 {
		return ""
	}
	names := make([]string, 0, len(fields))
	for _, f := range fields {
		names = append(names, fmt.Sprint(f))
	}
	return " (fields: " + strings.Join(names, ", ") + ")"
}

func checkRef(n GraphNode, expr string, deps map[string]bool, typeOf map[string]string, byKey map[string]GraphNode,
	cat *Catalog, inputKeys map[string]bool, webhook bool) string {
	for _, helper := range []string{"json ", "urlencode "} {
		expr = strings.TrimSpace(strings.TrimPrefix(expr, helper))
	}
	parts := strings.Split(expr, ".")
	where := fmt.Sprintf("node '%s' references {{%s}}", n.Key, expr)
	if strings.ContainsAny(expr, "[]") {
		return where + ": a path uses dots, not [ ] — write items.0.id for the first element. To act on every element use a logic.loop" +
			" and {{item.<field>}}; to keep only some, use a data.transform"
	}
	switch parts[0] {
	case "input":
		if webhook || len(parts) < 2 || inputKeys[parts[1]] {
			return ""
		}
		return where + ": input." + parts[1] + " is not in defaultInput (" + keyList(inputKeys) +
			") — add it to defaultInput or reference an upstream output"
	case "outputs":
		if len(parts) < 2 {
			return where + ": use outputs.<dependency key>.<field>"
		}
		key := parts[1]
		if !deps[key] {
			if _, exists := typeOf[key]; exists {
				return where + ": '" + key + "' is not in its dependsOn — a node only sees its direct dependencies' outputs"
			}
			return where + ": there is no node '" + key + "'"
		}
		if len(parts) < 3 {
			return ""
		}
		spec, ok := cat.ByID[typeOf[key]]
		if !ok || len(spec.Outputs) == 0 {
			return "" // undeclared outputs: nothing to check against
		}
		for _, o := range spec.Outputs {
			if o == parts[2] {
				if len(parts) > 3 && isScalarOutput(typeOf[key], parts[2]) {
					return where + ": '" + parts[2] + "' is a plain value with no field '" + parts[3] +
						"'. Gates do not pass data through: add the node that produced the data to dependsOn and reference" +
						" outputs.<that key>.<field>, or use {{item.<field>}} inside a loop"
				}
				return checkShape(where, key, parts, spec, byKey[key])
			}
		}
		return where + ": '" + typeOf[key] + "' has no output '" + parts[2] + "' (its outputs: " +
			strings.Join(spec.Outputs, ", ") + ")"
	case "item", "loop":
		return "" // loop-body roots; validateRefs checks the node is in a loop
	}
	return where + ": unknown root '" + parts[0] + "' — use input.<field> or outputs.<dependency key>.<field>"
}

// checkShape uses what the tool's spec says about its answer, and how the node
// is configured, to reject a path that cannot resolve.
func checkShape(where, key string, parts []string, spec CatalogNode, src GraphNode) string {
	field := parts[2]
	numeric := func(s string) bool { _, err := strconv.Atoi(s); return err == nil }
	if field == "body" && isTrue(src.Config["allPages"]) {
		return where + ": '" + key + "' has allPages on, so every page is in outputs." + key + ".items and outputs." + key +
			".body is only the first page. Use outputs." + key + ".items"
	}
	if len(parts) < 4 {
		return ""
	}
	next := parts[3]
	list := field == "items" || (field == "body" && spec.Exec["bodyType"] == "array")
	if list && !numeric(next) {
		return where + ": '" + field + "' of '" + key + "' is a list, so it has no field '" + next + "'." +
			" To use each element, loop over it with logic.loop and read {{item." + next + "}};" +
			" to filter or total it, use data.transform ([.outputs." + key + "." + field + "[] | select(." + next + " ...)])"
	}
	if field == "body" && spec.Exec["bodyType"] == "object" {
		fields, _ := spec.Exec["bodyFields"].([]any)
		if len(fields) == 0 {
			return ""
		}
		for _, f := range fields {
			if fmt.Sprint(f) == next {
				return ""
			}
		}
		return where + ": the response of '" + key + "' has no field '" + next + "'" + fieldList(spec)
	}
	return ""
}

func isTrue(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return strings.EqualFold(strings.TrimSpace(t), "true")
	}
	return false
}

var (
	quoted  = regexp.MustCompile(`"[^"]*"|'[^']*'`)
	bareRef = regexp.MustCompile(`\b(?:outputs|input)\.[\w.-]+`)
)

var (
	jqDot     = regexp.MustCompile(`\.(outputs|input)((?:\.[A-Za-z_][\w]*)+)`)
	jqBracket = regexp.MustCompile(`\.outputs\[\s*"([^"]+)"\s*\]((?:\.[A-Za-z_][\w]*)*)`)
	jqString  = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)
	jqBanned  = regexp.MustCompile(`(^|[^.\w$])(inputs?|input_line_number|env|halt|halt_error|getpath\(\["?ENV)\b|\$ENV\b`)
)

// jqRefs lists the upstream data a jq expression reads, as dotted references.
func jqRefs(expr string) []string {
	var out []string
	for _, m := range jqDot.FindAllStringSubmatch(expr, -1) {
		out = append(out, m[1]+m[2])
	}
	for _, m := range jqBracket.FindAllStringSubmatch(expr, -1) {
		out = append(out, "outputs."+m[1]+m[2])
	}
	return out
}

// jqScalarIterated reports a transform that iterates or indexes an output which
// is always a single value, such as a gate's yes/no answer.
func jqScalarIterated(n GraphNode, expr string, typeOf map[string]string) []string {
	var out []string
	for _, loc := range jqDot.FindAllStringSubmatchIndex(expr, -1) {
		if loc[1] >= len(expr) || expr[loc[1]] != '[' || expr[loc[2]:loc[3]] != "outputs" {
			continue
		}
		parts := strings.Split(strings.TrimPrefix(expr[loc[4]:loc[5]], "."), ".")
		if len(parts) != 2 || !isScalarOutput(typeOf[parts[0]], parts[1]) {
			continue
		}
		msg := "node '" + n.Key + "' reads .outputs." + parts[0] + "." + parts[1] + "[...], but '" + parts[1] + "' is a single value, not a list."
		if isGateType(typeOf[parts[0]]) {
			msg += " A gate does not narrow a list or pass data through. Remove the gate, depend on the node that produced the list," +
				" and narrow it here: [.outputs.<that key>.items[] | select(.field == \"value\")]"
		}
		out = append(out, msg)
	}
	return out
}

// jqUnavailable reports jq features a transform cannot use, so the mistake is
// reported when the graph is written and not when it runs.
func jqUnavailable(expr string) string {
	bare := jqString.ReplaceAllString(expr, `""`)
	for _, loc := range jqBanned.FindAllStringIndex(bare, -1) {
		word := strings.Trim(bare[loc[0]:loc[1]], " \t\n([|,;")
		// "{input: 1}" names a field; it does not call anything.
		if rest := strings.TrimLeft(bare[loc[1]:], " \t\n"); strings.HasPrefix(rest, ":") {
			continue
		}
		if strings.HasPrefix(word, "input") {
			return "jq's '" + word + "' is not available here. The run input is .input.<field>, and upstream data is .outputs.<dependency key>.<field>"
		}
		return "'" + word + "' is not available in a transform: it cannot read the environment or stop the run"
	}
	return ""
}

// jqRootProblems finds field reads at the top of a jq expression that cannot
// exist. A transform's input is {input, outputs, item, loop}, so ".body" at the
// top is always null, and the mistake only shows when the run fails. Inside a
// pipe, a function argument or a loop the dot means something else, and is not
// checked.
func jqRootProblems(expr string) []string {
	var out []string
	for _, name := range jqRootNames(expr) {
		if !jqRoots[name] {
			out = append(out, "."+name+" does not exist at the top of a jq expression, where only .input, .outputs, .item and .loop do."+
				" Read upstream data as .outputs.<dependency key>."+name)
		}
	}
	return out
}

// jqRootNames lists the fields a jq expression reads from its own input.
func jqRootNames(expr string) []string {
	query, err := gojq.Parse(expr)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	var walkQuery func(q *gojq.Query)
	var walkTerm func(t *gojq.Term)
	walkQuery = func(q *gojq.Query) {
		if q == nil {
			return
		}
		switch {
		case q.Term != nil:
			walkTerm(q.Term)
		case q.Op == gojq.OpPipe:
			walkQuery(q.Left)      // the right side sees what the left side produced
			if q.Patterns != nil { // "<source> as $x | <body>": the body still sees the top
				walkQuery(q.Right)
			}
		default:
			walkQuery(q.Left)
			walkQuery(q.Right)
		}
	}
	walkTerm = func(t *gojq.Term) {
		if t == nil {
			return
		}
		switch t.Type {
		case gojq.TermTypeIndex:
			if name := t.Index.Name; name != "" && !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		case gojq.TermTypeArray:
			if t.Array != nil {
				walkQuery(t.Array.Query)
			}
		case gojq.TermTypeObject:
			if t.Object != nil {
				for _, kv := range t.Object.KeyVals {
					walkQuery(kv.Val)
				}
			}
		case gojq.TermTypeQuery:
			walkQuery(t.Query)
		case gojq.TermTypeIf:
			if t.If != nil {
				walkQuery(t.If.Cond)
				walkQuery(t.If.Then)
				walkQuery(t.If.Else)
			}
		case gojq.TermTypeUnary:
			if t.Unary != nil {
				walkTerm(t.Unary.Term)
			}
		}
	}
	walkQuery(query)
	return out
}

var jqRoots = map[string]bool{"input": true, "outputs": true, "item": true, "loop": true}

// conditionRefs finds the bare references in a gate's condition, which is
// written "outputs.a.b == 1" rather than as a template.
func conditionRefs(cond string) []string {
	return bareRef.FindAllString(quoted.ReplaceAllString(tplRef.ReplaceAllString(cond, "$1"), ""), -1)
}

// isScalarOutput reports whether an output is always a plain value, so a path
// cannot continue into it. agent.llm text is excluded: it may hold JSON.
func isScalarOutput(nodeType, output string) bool {
	switch nodeType {
	case "logic.branch", "logic.filter":
		return output == "passed" || output == "condition"
	case "logic.approval":
		return output == "approved" || output == "note" || output == "decidedBy" || output == "message"
	case "logic.loop":
		return output == "count" || output == "succeeded" || output == "failed"
	case "logic.wait":
		return output == "seconds"
	}
	if strings.HasPrefix(nodeType, "api.") && (output == "pages" || output == "truncated") {
		return true
	}
	return (strings.HasPrefix(nodeType, "api.") || nodeType == "tool.http") && output == "status"
}

// configRefs collects every template expression in string config values,
// descending into nested maps and lists.
func configRefs(v any) []string {
	var out []string
	switch t := v.(type) {
	case string:
		for _, m := range tplRef.FindAllStringSubmatch(t, -1) {
			out = append(out, m[1])
		}
	case map[string]any:
		for _, x := range t {
			out = append(out, configRefs(x)...)
		}
	case []any:
		for _, x := range t {
			out = append(out, configRefs(x)...)
		}
	}
	return out
}

func keyList(m map[string]bool) string {
	if len(m) == 0 {
		return "empty"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}
