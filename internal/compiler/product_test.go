package compiler

import (
	"strings"
	"testing"
)

func node(key, typ string, cfg map[string]any, deps ...string) GraphNode {
	if cfg == nil {
		cfg = map[string]any{}
	}
	return GraphNode{Key: key, Type: typ, Name: key, DependsOn: deps, Config: cfg}
}

func problemsOf(nodes ...GraphNode) string {
	return strings.Join(graphProblems(nodes, BuiltinCatalog(), map[string]bool{"min": true}), "; ")
}

func TestNestedLoopsAreRejectedAtCompileTime(t *testing.T) {
	got := problemsOf(
		node("t", "trigger.manual", nil),
		node("outer", "logic.loop", map[string]any{"items": "input.min"}, "t"),
		node("inner", "logic.loop", map[string]any{"items": "item.lines"}, "outer"),
		node("do", "agent.llm", map[string]any{"prompt": "{{item}}"}, "inner"),
	)
	if !strings.Contains(got, "nested loops are not supported") {
		t.Errorf("got %q", got)
	}
}

func TestLoopAfterALoopIsNotNested(t *testing.T) {
	got := problemsOf(
		node("t", "trigger.manual", nil),
		node("first", "logic.loop", map[string]any{"items": "input.min"}, "t"),
		node("do", "agent.llm", map[string]any{"prompt": "{{item}}"}, "first"),
		node("second", "logic.loop", map[string]any{"items": "outputs.first.results"}, "first"),
		node("again", "agent.llm", map[string]any{"prompt": "{{item}}"}, "second"),
	)
	if got != "" {
		t.Errorf("a loop over another loop's results is fine, got %q", got)
	}
}

func TestLoopOnErrorMustBeKnown(t *testing.T) {
	bad := problemsOf(node("t", "trigger.manual", nil), node("each", "logic.loop", map[string]any{"items": "input.min", "onError": "ignore"}, "t"))
	if !strings.Contains(bad, "onError") {
		t.Errorf("got %q", bad)
	}
	ok := problemsOf(node("t", "trigger.manual", nil), node("each", "logic.loop", map[string]any{"items": "input.min", "onError": "continue"}, "t"))
	if ok != "" {
		t.Errorf("got %q", ok)
	}
}

func TestTransformIsCheckedAtCompileTime(t *testing.T) {
	fetch := node("fetch", "tool.http", map[string]any{"url": "https://example.com"}, "t")
	start := node("t", "trigger.manual", nil)
	cases := map[string]string{
		`[.outputs.fetch.body[] | select(.total > 1)]`:             "",
		`.input.min as $m | .outputs["fetch"].body | map(.x + $m)`: "",
		`.outputs.fetch.body | map(`:                               "not valid jq",
		`{{outputs.fetch.body}}`:                                   "",
		`.outputs.fetch.rows`:                                      "has no output 'rows'",
		`.outputs.other.body`:                                      "there is no node 'other'",
		`.input.limit`:                                             "input.limit is not in defaultInput",
	}
	for expr, want := range cases {
		got := problemsOf(start, fetch, node("shape", "data.transform", map[string]any{"expression": expr}, "fetch"))
		if want == "" && expr == `{{outputs.fetch.body}}` {
			if got == "" {
				t.Errorf("%s: a template in a jq expression should be rejected", expr)
			}
			continue
		}
		if (want == "") != (got == "") || !strings.Contains(got, want) {
			t.Errorf("%s: got %q, want %q", expr, got, want)
		}
	}
	undeclared := problemsOf(start, fetch, node("x", "agent.llm", map[string]any{"prompt": "a"}, "t"),
		node("shape", "data.transform", map[string]any{"expression": ".outputs.x.text"}, "fetch"))
	if !strings.Contains(undeclared, "not in its dependsOn") {
		t.Errorf("got %q", undeclared)
	}
}

func TestTransformReadingAFieldThatCannotExistIsRejected(t *testing.T) {
	bad := map[string]string{
		`[.body[] | {id: .id}]`:                              ".body does not exist",
		`.items | length`:                                    ".items does not exist",
		`{count: (.results | length)}`:                       ".results does not exist",
		`.input.min as $m | [.body[] | select(.total > $m)]`: ".body does not exist",
		`if .ok then .outputs.fetch.body else [] end`:        ".ok does not exist",
	}
	for expr, want := range bad {
		got := strings.Join(jqRootProblems(expr), "; ")
		if !strings.Contains(got, want) {
			t.Errorf("%s: got %q, want %q", expr, got, want)
		}
	}
	for _, good := range []string{
		`[.outputs.fetch.body[] | {id: .id, total: .total}]`,
		`.outputs.fetch.body | map(select(.status == "late")) | length`,
		`.input.min as $m | [.outputs.fetch.body[] | select(.total > $m) | .id]`,
		`.item.total * 2`,
		`[.outputs.a.body[], .outputs.b.body[]] | unique_by(.id)`,
		`.outputs.fetch.body | group_by(.customer_id) | map({customer: .[0].customer_id, n: length})`,
		`.`,
	} {
		if got := jqRootProblems(good); len(got) != 0 {
			t.Errorf("%s: %v", good, got)
		}
	}
	fetch := node("fetch", "tool.http", map[string]any{"url": "https://example.com"}, "t")
	got := problemsOf(node("t", "trigger.manual", nil), fetch, node("shape", "data.transform", map[string]any{"expression": "[.body[] | .id]"}, "fetch"))
	if !strings.Contains(got, "node 'shape': .body does not exist") {
		t.Errorf("got %q", got)
	}
}

func TestGateConditionIsParsedAtCompileTime(t *testing.T) {
	ConditionChecker = func(expr string) error {
		if strings.HasSuffix(strings.TrimSpace(expr), "&&") {
			return errString("condition ends too early")
		}
		return nil
	}
	defer func() { ConditionChecker = nil }()
	fetch := node("fetch", "tool.http", map[string]any{"url": "https://example.com"}, "t")
	bad := problemsOf(node("t", "trigger.manual", nil), fetch, node("gate", "logic.branch", map[string]any{"condition": "outputs.fetch.status == 200 &&"}, "fetch"))
	if !strings.Contains(bad, "ends too early") {
		t.Errorf("got %q", bad)
	}
	good := problemsOf(node("t", "trigger.manual", nil), fetch, node("gate", "logic.branch", map[string]any{"condition": "outputs.fetch.status == 200 && outputs.fetch.body"}, "fetch"))
	if good != "" {
		t.Errorf("got %q", good)
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func TestWaitNeedsADuration(t *testing.T) {
	if got := problemsOf(node("t", "trigger.manual", nil), node("w", "logic.wait", nil, "t")); !strings.Contains(got, "needs seconds or until") {
		t.Errorf("got %q", got)
	}
	if got := problemsOf(node("t", "trigger.manual", nil), node("w", "logic.wait", map[string]any{"seconds": 60}, "t")); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestNewBuiltinsAreInThePrompt(t *testing.T) {
	prompt := reduceSystemPrompt(BuiltinCatalog(), nil)
	for _, want := range []string{"### data.transform", "### logic.wait", "onError", "allPages", "&&"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the authoring prompt does not mention %s", want)
		}
	}
}

/* ------------- checks added after the first eval run (2026-11-11) ------------ */

// shopCatalog is a catalog with a paged list whose response the spec describes.
func shopCatalog() *Catalog {
	cat := BuiltinCatalog()
	cat.AddCluster("shop", "Shop", "", []CatalogNode{
		{ID: "api.shop.listOrders", Kind: "action", Outputs: []string{"status", "body", "items", "pages", "truncated"},
			Config: []CatalogField{{Key: "status"}, {Key: "allPages"}},
			Exec:   map[string]any{"method": "GET", "bodyType": "array", "pagination": map[string]any{"style": "page"}}},
		{ID: "api.shop.getOrder", Kind: "action", Outputs: []string{"status", "body"}, Config: []CatalogField{{Key: "id", Required: true}},
			Exec: map[string]any{"method": "GET", "bodyType": "object", "bodyFields": []any{"id", "total", "status"}}},
		{ID: "api.shop.refundOrder", Kind: "action", Outputs: []string{"status", "body"}, Config: []CatalogField{{Key: "id", Required: true}},
			Exec: map[string]any{"method": "POST"}},
	})
	return cat
}

func shopProblems(nodes ...GraphNode) string {
	all := append([]GraphNode{node("t", "trigger.manual", nil)}, nodes...)
	return strings.Join(graphProblems(all, shopCatalog(), map[string]bool{}), "; ")
}

func TestFieldOfAListIsRejected(t *testing.T) {
	list := node("list", "api.shop.listOrders", nil, "t")
	got := shopProblems(list, node("gate", "logic.filter", map[string]any{"condition": "outputs.list.body.total > 1000"}, "list"))
	if !strings.Contains(got, "is a list, so it has no field 'total'") {
		t.Errorf("condition on a list: %q", got)
	}
	got = shopProblems(list, node("x", "data.transform", map[string]any{"expression": ".outputs.list.items.total"}, "list"))
	if !strings.Contains(got, "'items' of 'list' is a list") {
		t.Errorf("jq path into a list: %q", got)
	}
	for _, fine := range []string{"[.outputs.list.items[] | select(.total > 100)]", ".outputs.list.items | length", ".outputs.list.body[0].id"} {
		if got := shopProblems(list, node("x", "data.transform", map[string]any{"expression": fine}, "list")); got != "" {
			t.Errorf("%s: %q", fine, got)
		}
	}
	one := node("one", "api.shop.getOrder", map[string]any{"id": "o1"}, "t")
	if got := shopProblems(one, node("say", "agent.llm", map[string]any{"prompt": "{{outputs.one.body.total}}"}, "one")); got != "" {
		t.Errorf("a field the spec lists: %q", got)
	}
	got = shopProblems(one, node("say", "agent.llm", map[string]any{"prompt": "{{outputs.one.body.email}}"}, "one"))
	if !strings.Contains(got, "has no field 'email' (fields: id, total, status)") {
		t.Errorf("a field the spec does not list: %q", got)
	}
}

func TestAllPagesMeansReadItemsNotBody(t *testing.T) {
	paged := node("list", "api.shop.listOrders", map[string]any{"allPages": "true"}, "t")
	got := shopProblems(paged, node("x", "data.transform", map[string]any{"expression": "[.outputs.list.body[] | .id]"}, "list"))
	if !strings.Contains(got, "body is only the first page. Use outputs.list.items") {
		t.Errorf("got %q", got)
	}
	if got := shopProblems(paged, node("x", "data.transform", map[string]any{"expression": "[.outputs.list.items[] | .id]"}, "list")); got != "" {
		t.Errorf("got %q", got)
	}
	single := node("list", "api.shop.listOrders", nil, "t")
	if got := shopProblems(single, node("x", "data.transform", map[string]any{"expression": "[.outputs.list.body[] | .id]"}, "list")); got != "" {
		t.Errorf("without allPages body is the whole answer: %q", got)
	}
}

func TestLoopMustBeOverAList(t *testing.T) {
	list := node("list", "api.shop.listOrders", nil, "t")
	gate := node("gate", "logic.filter", map[string]any{"condition": "outputs.list.status == 200"}, "list")
	got := shopProblems(list, gate, node("each", "logic.loop", map[string]any{"items": "outputs.gate.passed"}, "gate"))
	if !strings.Contains(got, "single value, not a list") || !strings.Contains(got, "data.transform") {
		t.Errorf("got %q", got)
	}
	one := node("one", "api.shop.getOrder", map[string]any{"id": "o1"}, "t")
	got = shopProblems(one, node("each", "logic.loop", map[string]any{"items": "outputs.one.body"}, "one"))
	if !strings.Contains(got, "is an object (fields: id, total, status), not a list") {
		t.Errorf("got %q", got)
	}
}

func TestItemOutsideALoopIsRejected(t *testing.T) {
	list := node("list", "api.shop.listOrders", nil, "t")
	for name, n := range map[string]GraphNode{
		"template":  node("refund", "api.shop.refundOrder", map[string]any{"id": "{{item.id}}"}, "list"),
		"condition": node("gate", "logic.branch", map[string]any{"condition": "item.total > 5"}, "list"),
		"jq":        node("x", "data.transform", map[string]any{"expression": ".item.total * 2"}, "list"),
	} {
		if got := shopProblems(list, n); !strings.Contains(got, "is not inside a loop") {
			t.Errorf("%s: %q", name, got)
		}
	}
	each := node("each", "logic.loop", map[string]any{"items": "outputs.list.items"}, "list")
	inside := shopProblems(list, each,
		node("gate", "logic.branch", map[string]any{"condition": "item.total > 5"}, "each"),
		node("refund", "api.shop.refundOrder", map[string]any{"id": "{{item.id}}"}, "gate"))
	if inside != "" {
		t.Errorf("inside a loop: %q", inside)
	}
	// ".item" after a pipe is a field of the element, not the loop item.
	if got := shopProblems(list, node("x", "data.transform", map[string]any{"expression": "[.outputs.list.items[] | .item]"}, "list")); got != "" {
		t.Errorf("a field called item: %q", got)
	}
}

func TestToolConfigThatWouldSendTheWrongValue(t *testing.T) {
	ok := node("ok", "logic.approval", map[string]any{"message": "delete?"}, "t")
	got := shopProblems(ok, node("refund", "api.shop.refundOrder", map[string]any{"id": "{{outputs.ok.approved}}"}, "ok"))
	if !strings.Contains(got, "only true or false") {
		t.Errorf("a decision used as an id: %q", got)
	}
	list := node("list", "api.shop.listOrders", nil, "t")
	each := node("each", "logic.loop", map[string]any{"items": "outputs.list.items"}, "list")
	for _, literal := range []string{"outputs.each.item.id", "item.id", "outputs.ok.passed ? outputs.one.body.id : null"} {
		got := shopProblems(list, each, node("refund", "api.shop.refundOrder", map[string]any{"id": literal}, "each"))
		if !strings.Contains(got, "is the literal text") {
			t.Errorf("%s: %q", literal, got)
		}
	}
	nested := shopProblems(list, each, node("refund", "api.shop.refundOrder", map[string]any{"id": "{{item.id}}", "body": map[string]any{"note": "input.reason"}}, "each"))
	if !strings.Contains(got+nested, "config 'body.note' is the literal text") {
		t.Errorf("nested: %q", nested)
	}
	if got := shopProblems(list, each, node("refund", "api.shop.refundOrder", map[string]any{"id": "{{item.id}}", "body": map[string]any{"reason": "outputs were late"}}, "each")); got != "" {
		t.Errorf("ordinary text that begins with a similar word: %q", got)
	}
}

func TestJqFeaturesATransformCannotUse(t *testing.T) {
	for expr, want := range map[string]string{
		`inputs as $input | [.outputs.list.items[] | .id]`: "'inputs' is not available",
		`input | .x`: "'input' is not available",
		`$ENV.HOME`:  "cannot read the environment",
		`[.outputs.list.items[] | select(.total > env.MIN)]`: "cannot read the environment",
	} {
		if got := jqUnavailable(expr); !strings.Contains(got, want) {
			t.Errorf("%s: %q", expr, got)
		}
	}
	for _, fine := range []string{`.input.min`, `{input: .input.min}`, `[.outputs.a.items[] | .inputs]`, `"input"`, `.outputs.a.body.environment`} {
		if got := jqUnavailable(fine); got != "" {
			t.Errorf("%s: %q", fine, got)
		}
	}
}

func TestConditionWithAPipeGetsAUsefulHint(t *testing.T) {
	ConditionChecker = func(string) error { return errString(`condition has an unexpected "|"`) }
	defer func() { ConditionChecker = nil }()
	list := node("list", "api.shop.listOrders", nil, "t")
	got := shopProblems(list, node("gate", "logic.filter", map[string]any{"condition": "outputs.list.body | length > 0"}, "list"))
	if !strings.Contains(got, "A condition is not jq") {
		t.Errorf("got %q", got)
	}
}

func TestGateCannotNarrowAList(t *testing.T) {
	cat := LoadCatalog()
	nodes := []GraphNode{
		{Key: "list", Type: "tool.http", Config: map[string]any{"url": "https://x.test", "method": "GET"}},
		{Key: "keep", Type: "logic.filter", DependsOn: []string{"list"}, Config: map[string]any{"condition": "outputs.list.body.items[].assignee == null"}},
		{Key: "ids", Type: "data.transform", DependsOn: []string{"keep"}, Config: map[string]any{"expression": "[.outputs.keep.passed[] | {id: .id}]"}},
	}
	errs := strings.Join(validateRefs(nodes, cat, map[string]bool{}), "\n")
	for _, want := range []string{"cannot look inside a list", "'passed' is a single value, not a list", "A gate does not narrow a list"} {
		if !strings.Contains(errs, want) {
			t.Errorf("missing %q in:\n%s", want, errs)
		}
	}
	// A quoted bracket is text, and a real list may be iterated.
	ok := []GraphNode{
		{Key: "list", Type: "tool.http", Config: map[string]any{"url": "https://x.test", "method": "GET"}},
		{Key: "keep", Type: "logic.filter", DependsOn: []string{"list"}, Config: map[string]any{"condition": "outputs.list.body.tag == \"[x]\""}},
		{Key: "ids", Type: "data.transform", DependsOn: []string{"list"}, Config: map[string]any{"expression": "[.outputs.list.body.items[] | .id]"}},
	}
	if errs := validateRefs(ok, cat, map[string]bool{}); len(errs) > 0 {
		t.Errorf("valid graph rejected: %v", errs)
	}
}

func TestTemplatePathsUseDotsNotBrackets(t *testing.T) {
	cat := LoadCatalog()
	nodes := []GraphNode{
		{Key: "list", Type: "tool.http", Config: map[string]any{"url": "https://x.test", "method": "GET"}},
		{Key: "send", Type: "tool.http", DependsOn: []string{"list"}, Config: map[string]any{"url": "https://x.test/{{outputs.list.body.items[0].id}}", "method": "POST"}},
	}
	errs := strings.Join(validateRefs(nodes, cat, map[string]bool{}), "\n")
	if !strings.Contains(errs, "a path uses dots, not [ ]") {
		t.Errorf("bracket path accepted: %s", errs)
	}
}

func reviewCatalog() *Catalog {
	cat := BuiltinCatalog()
	cat.AddCluster("shop", "Shop", "orders", []CatalogNode{
		{ID: "api.shop.list", Label: "List orders", Kind: "action", Outputs: []string{"status", "body", "items"}, Exec: map[string]any{"method": "GET"}},
		{ID: "api.shop.refund", Label: "Refund an order", Kind: "action", Outputs: []string{"status", "body"}, Exec: map[string]any{"method": "POST"}},
		{ID: "api.shop.remove", Label: "Delete an order", Kind: "action", Outputs: []string{"status", "body"}, Exec: map[string]any{"method": "DELETE"}},
	})
	cat.AddCluster("chat", "Chat", "messages", []CatalogNode{
		{ID: "piece.chat.send", Label: "Send a message", Kind: "action", Outputs: []string{"result"}},
		{ID: "mcp.chat.search", Label: "Search messages", Kind: "action", Outputs: []string{"result"}, Exec: map[string]any{"readOnly": true}},
	})
	return cat
}

func TestReviewSaysWhatAGraphDoesAndWhatNobodyApproves(t *testing.T) {
	cat := reviewCatalog()
	nodes := []GraphNode{
		{Key: "t", Type: "trigger.manual"},
		{Key: "list", Type: "api.shop.list", Name: "Late orders", DependsOn: []string{"t"}, Config: map[string]any{"allPages": "true"}},
		{Key: "find", Type: "mcp.chat.search", Name: "Find the thread", DependsOn: []string{"t"}},
		{Key: "each", Type: "logic.loop", DependsOn: []string{"list"}, Config: map[string]any{"items": "outputs.list.items", "onError": "continue"}},
		{Key: "ask", Type: "logic.approval", DependsOn: []string{"each"}, Config: map[string]any{"message": "Refund {{item.id}}?"}},
		{Key: "refund", Type: "api.shop.refund", Name: "Refund it", DependsOn: []string{"ask"}},
		{Key: "tell", Type: "piece.chat.send", Name: "Tell the team", DependsOn: []string{"refund"}},
		{Key: "purge", Type: "api.shop.remove", Name: "Remove test orders", DependsOn: []string{"list"}},
		{Key: "ping", Type: "tool.http", DependsOn: []string{"t"}, Config: map[string]any{"method": "post", "url": "https://x.test/hook"}},
	}
	rv := ReviewGraph(nodes, cat)
	text := map[string]ReviewStep{}
	for _, s := range rv.Steps {
		text[s.Node] = s
	}
	for key, want := range map[string]string{
		"list":   "Read from Shop: Late orders (every page)",
		"find":   "Read from Chat: Find the thread",
		"each":   "Repeat the steps after this for each item of outputs.list.items, carrying on if one fails",
		"ask":    `Ask a person: "Refund {{item.id}}?"`,
		"refund": "Change Shop: Refund it, once for each item, after a person approves",
		"tell":   "Use Chat: Tell the team, once for each item, after a person approves",
		"purge":  "Delete in Shop: Remove test orders",
		"ping":   "POST https://x.test/hook",
	} {
		if text[key].Text != want {
			t.Errorf("%s: %q, want %q", key, text[key].Text, want)
		}
	}
	if rv.Reads != 2 || rv.Changes != 4 || rv.Approvals != 1 {
		t.Errorf("counts: reads=%d changes=%d approvals=%d", rv.Reads, rv.Changes, rv.Approvals)
	}
	warned := strings.Join(rv.Warnings, "\n")
	if len(rv.Warnings) != 2 || !strings.Contains(warned, "'Remove test orders' deletes data in Shop and no person is asked first") ||
		!strings.Contains(warned, "'HTTP request' changes data and no person is asked first") {
		t.Errorf("warnings: %v", rv.Warnings)
	}
}

func TestApprovalPolicyIsAValidationProblem(t *testing.T) {
	cat := reviewCatalog()
	nodes := []GraphNode{
		{Key: "t", Type: "trigger.manual"},
		{Key: "refund", Type: "api.shop.refund", DependsOn: []string{"t"}},
		{Key: "purge", Type: "api.shop.remove", DependsOn: []string{"t"}},
	}
	if errs := graphProblems(nodes, cat, map[string]bool{}); len(errs) != 0 {
		t.Fatalf("no policy, but: %v", errs)
	}
	cat.RequireApproval = ApproveDeletes
	errs := graphProblems(nodes, cat, map[string]bool{})
	if len(errs) != 1 || !strings.Contains(errs[0], "node 'purge' deletes data") || !strings.Contains(errs[0], "add a logic.approval") {
		t.Fatalf("deletes policy: %v", errs)
	}
	cat.RequireApproval = ApproveChanges
	if errs := graphProblems(nodes, cat, map[string]bool{}); len(errs) != 2 {
		t.Fatalf("changes policy: %v", errs)
	}
	guarded := []GraphNode{
		{Key: "t", Type: "trigger.manual"},
		{Key: "ok", Type: "logic.approval", DependsOn: []string{"t"}, Config: map[string]any{"message": "go?"}},
		{Key: "refund", Type: "api.shop.refund", DependsOn: []string{"ok"}},
		{Key: "purge", Type: "api.shop.remove", DependsOn: []string{"refund"}},
	}
	if errs := graphProblems(guarded, cat, map[string]bool{}); len(errs) != 0 {
		t.Fatalf("an approval upstream satisfies the policy, but: %v", errs)
	}
}

func TestDiffGraphs(t *testing.T) {
	before := []GraphNode{
		{Key: "a", Type: "trigger.manual"},
		{Key: "b", Type: "agent.llm", DependsOn: []string{"a"}, Config: map[string]any{"prompt": "x"}},
		{Key: "c", Type: "agent.llm", DependsOn: []string{"b"}, Config: map[string]any{"prompt": "y"}},
	}
	after := []GraphNode{
		{Key: "a", Type: "trigger.manual", Col: 9, Row: 9},
		{Key: "b", Type: "agent.llm", DependsOn: []string{"a"}, Config: map[string]any{"prompt": "changed"}},
		{Key: "d", Type: "agent.llm", DependsOn: []string{"b"}, Config: map[string]any{"prompt": "new"}},
	}
	d := DiffGraphs(before, after)
	if strings.Join(d.Added, ",") != "d" || strings.Join(d.Removed, ",") != "c" || strings.Join(d.Changed, ",") != "b" || d.Unchanged != 1 {
		t.Errorf("diff = %+v", d)
	}
}

func TestRefineSendsTheCurrentGraphAndKeepsItsTools(t *testing.T) {
	base := Plan{Name: "Refunds", Description: "refund late orders", DefaultInput: map[string]any{"days": 3}, Nodes: []GraphNode{
		{Key: "t", Type: "trigger.manual"},
		{Key: "list", Type: "api.shop.list", Name: "Late orders", DependsOn: []string{"t"}},
	}}
	req := refineRequest(base, "also tell the team")
	for _, want := range []string{`"key": "list"`, `"type": "api.shop.list"`, "The change to make:\nalso tell the team", "Copy every node the change does not concern exactly"} {
		if !strings.Contains(req, want) {
			t.Errorf("refine request is missing %q", want)
		}
	}
}
