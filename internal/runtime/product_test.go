package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/satyanvm/apex-engine/internal/compiler"
	"github.com/satyanvm/apex-engine/internal/store"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
)

func boom() error { return temporal.NewNonRetryableApplicationError("boom", "NodeError", nil) }

func loopNode(key string, cfg map[string]any, deps ...string) compiler.GraphNode {
	return compiler.GraphNode{Key: key, Type: "logic.loop", Name: key, DependsOn: deps, Config: cfg}
}

func TestLoopContinueRecordsFailedItemsAndCarriesOn(t *testing.T) {
	var after map[string]any
	summary := compiler.GraphNode{Key: "summary", Type: "agent.llm", Name: "summary", DependsOn: []string{"each"},
		Config: map[string]any{"prompt": "{{outputs.each.results}}"}}
	rec, _ := exec(t, []compiler.GraphNode{loopNode("each", map[string]any{"onError": "continue"}), n("do", "agent.llm", "each"), n("then", "agent.llm", "do"), summary},
		func(in NodeInput) (NodeResult, error) {
			switch in.Node.Key {
			case "each":
				return NodeResult{Output: map[string]any{"items": []any{"a", "b", "c"}, "count": float64(3)}}, nil
			case "do":
				if in.Item == "b" {
					return NodeResult{}, boom()
				}
			case "summary":
				after = in.Upstream["each"]
			}
			return NodeResult{Output: map[string]any{"text": in.Node.Key}}, nil
		}, nil)
	if rec.run.Status != statusSucceeded {
		t.Fatalf("run = %+v", rec.run)
	}
	counts := map[string]int{}
	for _, k := range rec.ran {
		counts[k]++
	}
	if counts["do"] != 3 || counts["then"] != 2 || counts["summary"] != 1 {
		t.Errorf("counts = %v; the step after the failed one must not run for that item", counts)
	}
	if after["failed"] != float64(1) || after["succeeded"] != float64(2) {
		t.Errorf("loop output = %v", after)
	}
	results, _ := after["results"].([]any)
	if len(results) != 3 {
		t.Fatalf("results = %v", results)
	}
	if bad, _ := results[1].(map[string]any); bad["failedAt"] != "do" || !strings.Contains(fmt.Sprint(bad["error"]), "boom") {
		t.Errorf("failed item = %v", results[1])
	}
	if rec.finished["do"] != statusFailed || rec.finished["each"] != statusSucceeded {
		t.Errorf("finished = %v; the body step should read failed and the loop succeeded", rec.finished)
	}
}

func TestLoopStopsAtFirstFailureByDefault(t *testing.T) {
	rec, _ := exec(t, []compiler.GraphNode{n("each", "logic.loop"), n("do", "agent.llm", "each")}, func(in NodeInput) (NodeResult, error) {
		if in.Node.Key == "each" {
			return NodeResult{Output: map[string]any{"items": []any{"a", "b", "c"}}}, nil
		}
		return NodeResult{}, boom()
	}, nil)
	ran := 0
	for _, k := range rec.ran {
		if k == "do" {
			ran++
		}
	}
	if rec.run.Status != statusFailed || ran != 1 {
		t.Fatalf("run=%+v do ran %d times", rec.run, ran)
	}
}

func TestLoopBodyDoesNotCarryTheWholeList(t *testing.T) {
	reads := compiler.GraphNode{Key: "reads", Type: "agent.llm", Name: "reads", DependsOn: []string{"each"},
		Config: map[string]any{"prompt": "{{item}} of {{outputs.each.items}}"}}
	var lean, full map[string]any
	exec(t, []compiler.GraphNode{n("each", "logic.loop"), n("do", "agent.llm", "each"), reads}, func(in NodeInput) (NodeResult, error) {
		switch in.Node.Key {
		case "each":
			return NodeResult{Output: map[string]any{"items": []any{"a"}, "count": float64(1)}}, nil
		case "do":
			lean = in.Upstream["each"]
		case "reads":
			full = in.Upstream["each"]
		}
		return NodeResult{Output: map[string]any{}}, nil
	}, nil)
	if _, has := lean["items"]; has || lean["count"] != float64(1) {
		t.Errorf("a body step that does not ask for the list got %v", lean)
	}
	if items, _ := full["items"].([]any); len(items) != 1 {
		t.Errorf("a body step that asks for the list got %v", full)
	}
}

func TestRunStopsWithAReasonBeforeItsHistoryIsTooLarge(t *testing.T) {
	items := make([]any, 60)
	for i := range items {
		items[i] = i
	}
	big := strings.Repeat("x", 600<<10) // 60 items x 600 KB is past the 30 MB budget
	rec, _ := exec(t, []compiler.GraphNode{loopNode("each", map[string]any{"onError": "continue"}), n("do", "agent.llm", "each")},
		func(in NodeInput) (NodeResult, error) {
			if in.Node.Key == "each" {
				return NodeResult{Output: map[string]any{"items": items}}, nil
			}
			return NodeResult{Output: map[string]any{"text": big}}, nil
		}, nil)
	if rec.run.Status != statusFailed || !strings.Contains(rec.run.Error, "more data") {
		t.Fatalf("run = %+v", rec.run)
	}
}

func TestWaitPausesThenContinues(t *testing.T) {
	var s testsuite.WorkflowTestSuite
	_ = s
	rec, env := exec(t, []compiler.GraphNode{n("pause", "logic.wait"), n("after", "agent.llm", "pause")}, func(in NodeInput) (NodeResult, error) {
		if in.Node.Key == "pause" {
			return NodeResult{Output: map[string]any{"seconds": float64(3600)}}, nil
		}
		return NodeResult{Output: map[string]any{}}, nil
	}, nil)
	if rec.run.Status != statusSucceeded || rec.finished["pause"] != statusSucceeded {
		t.Fatalf("run=%+v finished=%v", rec.run, rec.finished)
	}
	if rec.ran[len(rec.ran)-1] != "after" {
		t.Errorf("ran = %v", rec.ran)
	}
	_ = env
}

func TestResumeSkipsTheWaitForAnApprovalAlreadyDecided(t *testing.T) {
	// No signal is ever sent: the decision comes from the run being resumed.
	rec, _ := exec(t, approvalDag(), func(in NodeInput) (NodeResult, error) {
		if in.Node.Key == "ok" {
			return NodeResult{Output: map[string]any{"approved": true, "decidedBy": "asha"}, Reused: true}, nil
		}
		return NodeResult{Output: map[string]any{}}, nil
	}, nil)
	if rec.run.Status != statusSucceeded {
		t.Fatalf("run = %+v", rec.run)
	}
	if got := sorted(rec.ran); !equal(got, []string{"ok", "send", "t"}) {
		t.Errorf("ran = %v", got)
	}
}

func TestResumedLoopThatAlreadyFinishedIsNotFannedOutAgain(t *testing.T) {
	var copied CopyPriorInput
	rec, _ := exec(t, []compiler.GraphNode{n("each", "logic.loop"), n("do", "agent.llm", "each")}, func(in NodeInput) (NodeResult, error) {
		return NodeResult{Output: map[string]any{"items": []any{"a", "b"}, "results": []any{1, 2}}, Reused: true}, nil
	}, func(env *testsuite.TestWorkflowEnvironment) {
		env.OnActivity("CopyPrior", mockCtx, mockAny).Return(func(_ context.Context, in CopyPriorInput) error {
			copied = in
			return nil
		})
	})
	if rec.run.Status != statusSucceeded {
		t.Fatalf("run = %+v", rec.run)
	}
	for _, k := range rec.ran {
		if k == "do" {
			t.Error("the body ran again")
		}
	}
	if len(copied.Keys) != 1 || copied.Keys[0] != "do" {
		t.Errorf("copied = %+v", copied)
	}
}

/* --------------------------------- conditions ------------------------------- */

func TestConditions(t *testing.T) {
	s := Scope{
		Input:   map[string]any{"region": "eu", "vip": true, "tags": []any{"late", "big"}},
		Outputs: map[string]map[string]any{"fetch": {"status": float64(200), "body": map[string]any{"total": float64(150), "name": "New York", "note": ""}}},
	}
	cases := map[string]bool{
		"outputs.fetch.status == 200":                                              true,
		"outputs.fetch.status == 200 && outputs.fetch.body.total > 100":            true,
		"outputs.fetch.status == 200 && outputs.fetch.body.total > 500":            false,
		"outputs.fetch.status == 500 || input.region == 'eu'":                      true,
		"outputs.fetch.status == 500 or input.region == \"us\"":                    false,
		"!(outputs.fetch.status == 200)":                                           false,
		"not input.vip":                                                            false,
		"input.vip and outputs.fetch.body.total >= 150":                            true,
		"(input.region == us || input.region == eu) && outputs.fetch.status < 300": true,
		"outputs.fetch.body.name == New York":                                      true,
		"outputs.fetch.body.name contains york":                                    true,
		"input.tags contains late":                                                 true,
		"input.tags contains small":                                                false,
		"outputs.fetch.body.note":                                                  false,
		"{{outputs.fetch.status}} == 200":                                          true,
	}
	for expr, want := range cases {
		got, err := s.EvalCondition(expr)
		if err != nil {
			t.Errorf("%s: %v", expr, err)
			continue
		}
		if got != want {
			t.Errorf("%s = %v, want %v", expr, got, want)
		}
		if err := CheckCondition(expr); err != nil {
			t.Errorf("CheckCondition(%s): %v", expr, err)
		}
	}
	for _, bad := range []string{"", "outputs.fetch.status ==", "(outputs.fetch.status == 200", "outputs.fetch.status == 200 &&", "== 3", "outputs.fetch.status == 'x"} {
		if _, err := s.EvalCondition(bad); err == nil {
			t.Errorf("%q should not evaluate", bad)
		}
		if err := CheckCondition(bad); err == nil {
			t.Errorf("%q should not parse", bad)
		}
	}
	if _, err := s.EvalCondition("outputs.missing.status == 200"); err == nil {
		t.Error("a missing reference must fail, not read as false")
	}
}

/* ---------------------------------- transform ------------------------------- */

func transform(t *testing.T, expr string, s Scope) (any, error) {
	t.Helper()
	res, err := transformNode(context.Background(), NodeInput{Node: compiler.GraphNode{Key: "x", Type: "data.transform", Config: map[string]any{"expression": expr}}}, s)
	if err != nil {
		return nil, err
	}
	return res.Output["result"], nil
}

func TestTransform(t *testing.T) {
	s := Scope{Input: map[string]any{"min": float64(100)}, Outputs: map[string]map[string]any{"orders": {"body": []any{
		map[string]any{"id": "a", "total": float64(50), "status": "late"},
		map[string]any{"id": "b", "total": float64(150), "status": "late"},
		map[string]any{"id": "c", "total": float64(300), "status": "ok"},
	}}}}
	got, err := transform(t, `.input.min as $min | [.outputs.orders.body[] | select(.status == "late" and .total > $min) | {id, total}]`, s)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(got)
	if string(raw) != `[{"id":"b","total":150}]` {
		t.Errorf("got %s", raw)
	}
	if got, _ := transform(t, `.outputs.orders.body | map(.total) | add`, s); got != float64(500) {
		t.Errorf("sum = %v", got)
	}
	if got, _ := transform(t, `.outputs.orders.body[] | .id`, s); fmt.Sprint(got) != "[a b c]" {
		t.Errorf("several values should come back as one list, got %v", got)
	}
	if _, err := transform(t, `.outputs.orders.body | map(`, s); err == nil || !strings.Contains(err.Error(), "not valid jq") {
		t.Errorf("bad syntax: %v", err)
	}
	if got, err := transform(t, `[$ENV.HOME, env.HOME, $ENV.PATH]`, s); err == nil && fmt.Sprint(got) != "[<nil> <nil> <nil>]" {
		t.Errorf("the environment must not be readable, got %v", got)
	}
	if _, err := transform(t, `input`, s); err == nil {
		t.Error("reading further input must fail")
	}
	withItem := Scope{HasItem: true, Item: map[string]any{"total": float64(7)}}
	if got, _ := transform(t, `.item.total * 2`, withItem); got != float64(14) {
		t.Errorf("item = %v", got)
	}
}

func TestWaitNode(t *testing.T) {
	wait := func(cfg map[string]any) (NodeResult, error) {
		return waitNode(NodeInput{Node: compiler.GraphNode{Key: "w", Type: "logic.wait", Config: cfg}}, Scope{Input: map[string]any{"n": "90"}})
	}
	if res, err := wait(map[string]any{"seconds": "{{input.n}}"}); err != nil || res.Output["seconds"] != float64(90) {
		t.Errorf("seconds: %v %v", res, err)
	}
	if res, err := wait(map[string]any{"until": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}); err != nil || res.Output["seconds"].(float64) < 3500 {
		t.Errorf("until: %v %v", res, err)
	}
	if res, err := wait(map[string]any{"until": "2001-01-01T00:00:00Z"}); err != nil || res.Output["seconds"] != float64(0) {
		t.Errorf("a time in the past should not wait: %v %v", res, err)
	}
	for _, bad := range []map[string]any{{}, {"seconds": "soon"}, {"seconds": "-1"}, {"seconds": "99999999"}, {"until": "tomorrow"}} {
		if _, err := wait(bad); err == nil {
			t.Errorf("%v should fail", bad)
		}
	}
}

/* --------------------------------- pagination ------------------------------- */

func listSpec(style, param, limit string) compiler.CatalogNode {
	params := []any{map[string]any{"name": param, "in": "query"}}
	if limit != "" {
		params = append(params, map[string]any{"name": limit, "in": "query"})
	}
	return compiler.CatalogNode{ID: "api.acme.list", Exec: map[string]any{"method": "GET", "path": "/things", "params": params,
		"pagination": map[string]any{"style": style, "param": param, "limit": limit}}}
}

func pagedCall(t *testing.T, h http.HandlerFunc, spec compiler.CatalogNode, cfg map[string]any, creds *store.Credentials) (NodeResult, error, *int) {
	t.Helper()
	t.Setenv("APEX_ALLOW_PRIVATE_NETWORK", "1")
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	if creds == nil {
		creds = &store.Credentials{}
	}
	c := &apiCall{a: &Activities{}, in: NodeInput{RunID: "r", Node: compiler.GraphNode{Key: "list"}}, cat: &store.Catalog{Name: "Acme", BaseURL: srv.URL}, creds: creds}
	res, err := c.paged(context.Background(), spec, cfg, paginationOf(spec))
	return res, err, &calls
}

func ids(res NodeResult) string {
	var out []string
	for _, it := range res.Output["items"].([]any) {
		out = append(out, fmt.Sprint(it.(map[string]any)["id"]))
	}
	return strings.Join(out, ",")
}

func writeJSONBody(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(v) }

func things(from, to int) []any {
	var out []any
	for i := from; i <= to; i++ {
		out = append(out, map[string]any{"id": i})
	}
	return out
}

func TestPagingByPageNumber(t *testing.T) {
	h := func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page == 0 {
			page = 1
		}
		if r.URL.Query().Get("status") != "late" {
			t.Errorf("page %d lost the caller's own parameter", page)
		}
		if page > 3 {
			writeJSONBody(w, []any{})
			return
		}
		writeJSONBody(w, things(page*2-1, page*2))
	}
	spec := listSpec("page", "page", "")
	spec.Exec["params"] = append(spec.Exec["params"].([]any), map[string]any{"name": "status", "in": "query"})
	res, err, calls := pagedCall(t, h, spec, map[string]any{"allPages": "true", "status": "late"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ids(res) != "1,2,3,4,5,6" || res.Output["pages"] != float64(3) || res.Output["truncated"] != false || *calls != 4 {
		t.Errorf("items=%s pages=%v truncated=%v calls=%d", ids(res), res.Output["pages"], res.Output["truncated"], *calls)
	}
	first, _ := res.Output["body"].([]any)
	if len(first) != 2 {
		t.Errorf("body should stay the first page, got %v", res.Output["body"])
	}
}

func TestWithoutAllPagesOnlyTheFirstPageIsFetched(t *testing.T) {
	h := func(w http.ResponseWriter, r *http.Request) {
		writeJSONBody(w, map[string]any{"data": things(1, 2), "next_cursor": "abc"})
	}
	res, err, calls := pagedCall(t, h, listSpec("cursor", "cursor", ""), map[string]any{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ids(res) != "1,2" || *calls != 1 {
		t.Errorf("items=%s calls=%d", ids(res), *calls)
	}
}

func TestPagingByOffsetStopsOnAShortPage(t *testing.T) {
	h := func(w http.ResponseWriter, r *http.Request) {
		off, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		end := off + 3
		if end > 7 {
			end = 7
		}
		writeJSONBody(w, map[string]any{"results": things(off+1, end), "total": 7})
	}
	res, err, calls := pagedCall(t, h, listSpec("offset", "offset", "limit"), map[string]any{"allPages": true, "limit": "3"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ids(res) != "1,2,3,4,5,6,7" || *calls != 3 {
		t.Errorf("items=%s calls=%d", ids(res), *calls)
	}
}

func TestPagingByCursorInTheBody(t *testing.T) {
	h := func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("cursor") {
		case "":
			writeJSONBody(w, map[string]any{"items": things(1, 2), "meta": map[string]any{"next_cursor": "p2"}})
		case "p2":
			writeJSONBody(w, map[string]any{"items": things(3, 4), "meta": map[string]any{"next_cursor": ""}})
		}
	}
	res, err, calls := pagedCall(t, h, listSpec("cursor", "cursor", ""), map[string]any{"allPages": "true"}, nil)
	if err != nil || ids(res) != "1,2,3,4" || *calls != 2 {
		t.Errorf("items=%v calls=%d err=%v", res.Output["items"], *calls, err)
	}
}

func TestPagingStripeStyle(t *testing.T) {
	h := func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("starting_after") {
		case "":
			writeJSONBody(w, map[string]any{"object": "list", "data": things(1, 2), "has_more": true})
		case "2":
			writeJSONBody(w, map[string]any{"object": "list", "data": things(3, 3), "has_more": false})
		default:
			t.Errorf("unexpected cursor %q", r.URL.Query().Get("starting_after"))
		}
	}
	res, err, calls := pagedCall(t, h, listSpec("cursor", "starting_after", "limit"), map[string]any{"allPages": "true"}, nil)
	if err != nil || ids(res) != "1,2,3" || *calls != 2 {
		t.Errorf("items=%v calls=%d err=%v", res.Output["items"], *calls, err)
	}
}

func TestPagingFollowsALinkHeaderOnTheSameHostOnly(t *testing.T) {
	h := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "" {
			w.Header().Set("Link", `<http://`+r.Host+`/things?page=2>; rel="next", <http://`+r.Host+`/things?page=9>; rel="last"`)
			writeJSONBody(w, things(1, 2))
			return
		}
		if r.Header.Get("Authorization") != "Bearer k" {
			t.Error("the second page was fetched without credentials")
		}
		writeJSONBody(w, things(3, 4))
	}
	creds := &store.Credentials{Headers: map[string]string{"Authorization": "Bearer k"}}
	res, err, _ := pagedCall(t, h, listSpec("page", "page", ""), map[string]any{"allPages": "true"}, creds)
	if err != nil || ids(res) != "1,2,3,4" {
		t.Fatalf("items=%v err=%v", res.Output["items"], err)
	}
	elsewhere := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", `<https://evil.example/things?page=2>; rel="next"`)
		writeJSONBody(w, things(1, 2))
	}
	if _, err, _ := pagedCall(t, elsewhere, listSpec("page", "page", ""), map[string]any{"allPages": "true"}, creds); err == nil || !strings.Contains(err.Error(), "another host") {
		t.Errorf("a next page on another host must be refused, got %v", err)
	}
}

func TestPagingStopsWhenTheAPIIgnoresThePageParameter(t *testing.T) {
	h := func(w http.ResponseWriter, r *http.Request) { writeJSONBody(w, things(1, 2)) }
	res, err, calls := pagedCall(t, h, listSpec("page", "page", ""), map[string]any{"allPages": "true"}, nil)
	if err != nil || ids(res) != "1,2" || *calls != 2 {
		t.Errorf("items=%v calls=%d err=%v", res.Output["items"], *calls, err)
	}
}

func TestPagingReportsWhenItStopsAtThePageLimit(t *testing.T) {
	h := func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page == 0 {
			page = 1
		}
		writeJSONBody(w, things(page, page))
	}
	res, err, calls := pagedCall(t, h, listSpec("page", "page", ""), map[string]any{"allPages": "true", "maxPages": "3"}, nil)
	if err != nil || ids(res) != "1,2,3" || res.Output["truncated"] != true || *calls != 3 {
		t.Errorf("items=%v truncated=%v calls=%d err=%v", res.Output["items"], res.Output["truncated"], *calls, err)
	}
}

func TestAllPagesWithNoListInTheResponseFails(t *testing.T) {
	h := func(w http.ResponseWriter, r *http.Request) { writeJSONBody(w, map[string]any{"ok": true}) }
	if _, err, _ := pagedCall(t, h, listSpec("page", "page", ""), map[string]any{"allPages": "true"}, nil); err == nil {
		t.Error("expected a failure, not an empty list")
	}
}

func TestQueryParameterCredentialsAndIdempotencyKey(t *testing.T) {
	t.Setenv("APEX_ALLOW_PRIVATE_NETWORK", "1")
	var seen *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := &apiCall{a: &Activities{}, cat: &store.Catalog{Name: "Acme", BaseURL: srv.URL}, creds: &store.Credentials{Query: map[string]string{"api_key": "k1"}},
		in: NodeInput{RunID: "run_2", IdemRoot: "run_1", HasItem: true, Index: 4, Node: compiler.GraphNode{Key: "refund"}}}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/refund?x=1", strings.NewReader(`{}`))
	if _, _, _, err := c.do(req); err != nil {
		t.Fatal(err)
	}
	if seen.URL.Query().Get("api_key") != "k1" || seen.URL.Query().Get("x") != "1" {
		t.Errorf("query = %s", seen.URL.RawQuery)
	}
	if got := seen.Header.Get("Idempotency-Key"); got != "run_1:refund:4" {
		t.Errorf("a resumed run must keep the first run's key, got %q", got)
	}
}
