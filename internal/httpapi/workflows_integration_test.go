//go:build integration

package httpapi

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
)

func TestSavedWorkflowLifecycle(t *testing.T) {
	h := setup(t)
	cat := h.importCatalog(keyA)
	nodes := []any{
		nd("start", "trigger.manual", map[string]any{}),
		nd("orders", "api.acme.listOrders", map[string]any{}, "start"),
	}
	body := map[string]any{"name": "Orders", "prompt": "list orders", "nodes": nodes, "catalogIds": []string{cat}, "defaultInput": map[string]any{"who": "ops"}}

	var wf map[string]any
	if code := h.do(keyA, "POST", "/v1/workflows", body, &wf); code != 201 {
		t.Fatalf("create: %d %v", code, wf)
	}
	id := wf["id"].(string)
	if wf["webhookPath"] != nil {
		t.Errorf("webhook must be off by default: %v", wf["webhookPath"])
	}

	// A graph that fails validation is refused, not stored.
	bad := map[string]any{"name": "Bad", "nodes": []any{nd("x", "api.acme.madeUp", map[string]any{})}, "catalogIds": []string{cat}}
	var refused map[string]any
	if code := h.do(keyA, "POST", "/v1/workflows", bad, &refused); code != 422 {
		t.Fatalf("invalid graph: %d %v", code, refused)
	}

	// Another tenant cannot see, run or delete it.
	for _, c := range []struct{ method, path string }{
		{"GET", "/v1/workflows/" + id}, {"DELETE", "/v1/workflows/" + id}, {"PUT", "/v1/workflows/" + id + "/webhook"},
	} {
		if code := h.do(keyB, c.method, c.path, nil, nil); code != 404 {
			t.Errorf("tenant B %s %s = %d", c.method, c.path, code)
		}
	}
	if code := h.do(keyB, "POST", "/v1/workflows/"+id+"/runs", map[string]any{}, nil); code != 404 {
		t.Errorf("tenant B run = %d", code)
	}

	var run map[string]any
	if code := h.do(keyA, "POST", "/v1/workflows/"+id+"/runs", map[string]any{"input": map[string]any{"who": "me"}}, &run); code != 202 {
		t.Fatalf("run: %d %v", code, run)
	}
	done := h.waitRun(keyA, run["id"].(string), "succeeded", "failed")
	if done["status"] != "succeeded" || done["name"] != "Orders" || done["workflowId"] != id || done["source"] != "api" {
		t.Fatalf("run = %v", done)
	}
	if len(done["dag"].([]any)) != 2 {
		t.Errorf("run detail should carry its graph: %v", done["dag"])
	}
	if in := done["input"].(map[string]any); in["who"] != "me" {
		t.Errorf("per-run input should win: %v", in)
	}

	var runs struct {
		Runs []map[string]any `json:"runs"`
	}
	h.do(keyA, "GET", "/v1/runs?workflowId="+id, nil, &runs)
	if len(runs.Runs) != 1 {
		t.Errorf("runs filtered by workflow: %v", runs.Runs)
	}
	h.do(keyA, "GET", "/v1/runs?workflowId=wfl_other", nil, &runs)
	if len(runs.Runs) != 0 {
		t.Errorf("filter for another workflow: %v", runs.Runs)
	}

	var list struct {
		Workflows []map[string]any `json:"workflows"`
	}
	h.do(keyA, "GET", "/v1/workflows", nil, &list)
	if len(list.Workflows) != 1 || list.Workflows[0]["lastRun"] == nil {
		t.Fatalf("list = %v", list.Workflows)
	}

	body["name"] = "Renamed"
	var updated map[string]any
	if code := h.do(keyA, "PUT", "/v1/workflows/"+id, body, &updated); code != 200 || updated["name"] != "Renamed" {
		t.Fatalf("update: %d %v", code, updated)
	}
	if code := h.do(keyA, "DELETE", "/v1/workflows/"+id, nil, nil); code != 204 {
		t.Fatalf("delete: %d", code)
	}
	if code := h.do(keyA, "GET", "/v1/workflows/"+id, nil, nil); code != 404 {
		t.Fatalf("after delete: %d", code)
	}
	if code := h.do(keyA, "GET", "/v1/runs/"+run["id"].(string), nil, nil); code != 200 {
		t.Errorf("runs stay as history after delete: %d", code)
	}
}

func hook(t *testing.T, url, body, idem string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	if idem != "" {
		req.Header.Set("Idempotency-Key", idem)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b bytes.Buffer
	_, _ = b.ReadFrom(resp.Body)
	return resp.StatusCode, b.String()
}

func TestWebhookStartsRunWithPayloadAsInput(t *testing.T) {
	h := setup(t)
	cat := h.importCatalog(keyA)
	nodes := []any{
		nd("start", "trigger.manual", map[string]any{}),
		nd("say", "agent.llm", map[string]any{"prompt": "hello {{input.who}}"}, "start"),
	}
	var wf map[string]any
	if code := h.do(keyA, "POST", "/v1/workflows", map[string]any{"name": "Hook", "nodes": nodes, "catalogIds": []string{cat}, "defaultInput": map[string]any{"who": "nobody"}}, &wf); code != 201 {
		t.Fatalf("create: %d %v", code, wf)
	}
	id := wf["id"].(string)

	var on map[string]string
	if code := h.do(keyA, "PUT", "/v1/workflows/"+id+"/webhook", nil, &on); code != 200 {
		t.Fatalf("enable: %d", code)
	}
	url := h.url + on["webhookPath"]

	code, resp := hook(t, url, `{"who":"ada"}`, "evt-1")
	if code != 202 || !strings.Contains(resp, "runId") {
		t.Fatalf("hook: %d %s", code, resp)
	}
	if strings.Contains(resp, "ada") {
		t.Errorf("the webhook response must not echo run data: %s", resp)
	}
	var runs struct {
		Runs []map[string]any `json:"runs"`
	}
	h.do(keyA, "GET", "/v1/runs?workflowId="+id, nil, &runs)
	if len(runs.Runs) != 1 || runs.Runs[0]["source"] != "webhook" {
		t.Fatalf("runs = %v", runs.Runs)
	}
	run := h.waitRun(keyA, runs.Runs[0]["id"].(string), "succeeded", "failed")
	if out := node(run, "say")["output"].(map[string]any); !strings.Contains(out["text"].(string), "hello ada") {
		t.Errorf("payload did not reach the node: %v", out)
	}

	// A repeated delivery with the same key starts nothing new.
	if code, resp := hook(t, url, `{"who":"ada"}`, "evt-1"); code != 202 {
		t.Fatalf("repeat: %d %s", code, resp)
	}
	h.do(keyA, "GET", "/v1/runs?workflowId="+id, nil, &runs)
	if len(runs.Runs) != 1 {
		t.Errorf("duplicate delivery started a run: %d", len(runs.Runs))
	}

	// An empty body uses the defaults; anything but an object is refused.
	if code, resp := hook(t, url, "", ""); code != 202 {
		t.Errorf("empty body: %d %s", code, resp)
	}
	if code, _ := hook(t, url, `[1,2]`, ""); code != 400 {
		t.Errorf("non-object body: %d", code)
	}

	// Rotating kills the old URL; disabling kills the new one.
	var next map[string]string
	h.do(keyA, "PUT", "/v1/workflows/"+id+"/webhook", nil, &next)
	if code, _ := hook(t, url, `{}`, ""); code != 404 {
		t.Errorf("old URL after rotate: %d", code)
	}
	if code := h.do(keyA, "DELETE", "/v1/workflows/"+id+"/webhook", nil, nil); code != 204 {
		t.Fatalf("disable: %d", code)
	}
	if code, _ := hook(t, h.url+next["webhookPath"], `{}`, ""); code != 404 {
		t.Errorf("URL after disable: %d", code)
	}
}

func TestScheduleOnWorkflowRunsLatestSavedGraph(t *testing.T) {
	h := setup(t)
	mk := func(prompt string) []any {
		return []any{
			nd("start", "trigger.manual", map[string]any{}),
			nd("say", "agent.llm", map[string]any{"prompt": prompt}, "start"),
		}
	}
	var wf map[string]any
	if code := h.do(keyA, "POST", "/v1/workflows", map[string]any{"name": "Tick", "nodes": mk("first")}, &wf); code != 201 {
		t.Fatalf("create: %d %v", code, wf)
	}
	id := wf["id"].(string)
	var sc map[string]any
	if code := h.do(keyA, "POST", "/v1/schedules", map[string]any{"cron": "* * * * *", "workflowId": id}, &sc); code != 201 {
		t.Fatalf("schedule: %d %v", code, sc)
	}
	if sc["workflowId"] != id {
		t.Errorf("schedule should name its workflow: %v", sc)
	}
	if code := h.do(keyA, "PUT", "/v1/workflows/"+id, map[string]any{"name": "Tick", "nodes": mk("edited")}, nil); code != 200 {
		t.Fatalf("edit: %d", code)
	}
	handle := h.tc.ScheduleClient().GetHandle(ctxBG(), scheduleID("a", sc["id"].(string)))
	if err := handle.Trigger(ctxBG(), scheduleTriggerNow()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(40 * time.Second)
	var runs struct {
		Runs []map[string]any `json:"runs"`
	}
	for time.Now().Before(deadline) {
		h.do(keyA, "GET", "/v1/runs?workflowId="+id, nil, &runs)
		if len(runs.Runs) > 0 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if len(runs.Runs) == 0 {
		t.Fatal("the schedule produced no run")
	}
	run := h.waitRun(keyA, runs.Runs[0]["id"].(string), "succeeded", "failed")
	if run["source"] != "schedule" || run["name"] != "Tick" {
		t.Errorf("run = %v", run)
	}
	if out := node(run, "say")["output"].(map[string]any); !strings.Contains(out["text"].(string), "edited") {
		t.Errorf("schedule ran a stale graph: %v", out)
	}

	// Deleting the workflow takes its schedule with it.
	if code := h.do(keyA, "DELETE", "/v1/workflows/"+id, nil, nil); code != 204 {
		t.Fatalf("delete: %d", code)
	}
	var list struct {
		Schedules []any `json:"schedules"`
	}
	h.do(keyA, "GET", "/v1/schedules", nil, &list)
	if len(list.Schedules) != 0 {
		t.Errorf("schedule outlived its workflow: %v", list.Schedules)
	}
}

func TestConsoleIsServedWithoutAuthAndAPIStaysProtected(t *testing.T) {
	h := setup(t)
	resp, err := http.Get(h.url + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "script-src 'self'") {
		t.Errorf("console: %d csp=%q", resp.StatusCode, resp.Header.Get("Content-Security-Policy"))
	}
	if code := h.do("", "GET", "/v1/status", nil, nil); code != 401 {
		t.Errorf("status without a key: %d", code)
	}
	if code := h.do("", "GET", "/v1/workflows", nil, nil); code != 401 {
		t.Errorf("workflows without a key: %d", code)
	}
}

func ctxBG() context.Context { return context.Background() }

func scheduleTriggerNow() client.ScheduleTriggerOptions {
	return client.ScheduleTriggerOptions{Overlap: enumspb.SCHEDULE_OVERLAP_POLICY_ALLOW_ALL}
}
