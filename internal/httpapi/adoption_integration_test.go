//go:build integration

package httpapi

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/satyanvm/apex-engine/internal/runtime"
)

const platformKey = "platform-key-0123456789abcdef0123456789"

func as(tenant, actor, role string) string {
	return platformKey + "|" + tenant + "|" + actor + "|" + role
}

// A product with its own customers holds one key. It names the customer and the
// person on each request; nothing is set up per customer.
func TestPlatformKeyActsForAnyTenantAsAnEndUser(t *testing.T) {
	h := setupWith(t, func(s *Server, _ *runtime.Activities) { s.PlatformKeys = []string{platformKey, ""} })

	if code := h.do(platformKey, "GET", "/v1/workflows", nil, nil); code != 400 {
		t.Errorf("a platform key with no tenant = %d, want 400", code)
	}
	if code := h.do(as("bad tenant!", "", ""), "GET", "/v1/workflows", nil, nil); code != 400 {
		t.Errorf("an invalid tenant name = %d, want 400", code)
	}
	if code := h.do(as("acme", "", "owner"), "GET", "/v1/workflows", nil, nil); code != 400 {
		t.Errorf("an unknown role = %d, want 400", code)
	}
	if code := h.do("|acme", "GET", "/v1/workflows", nil, nil); code != 401 {
		t.Errorf("an empty key must not match the unset platform key: %d", code)
	}

	// Two customers appear by being named.
	dag := map[string]any{"nodes": []any{
		nd("t", "trigger.manual", map[string]any{}),
		nd("ok", "logic.approval", map[string]any{"message": "go?"}, "t"),
		nd("say", "agent.llm", map[string]any{"prompt": "done"}, "ok"),
	}}
	var wf map[string]any
	if code := h.do(as("acme", "dana@acme.test", "editor"), "POST", "/v1/workflows", map[string]any{"name": "Acme flow", "nodes": dag["nodes"]}, &wf); code != 201 {
		t.Fatalf("create workflow for acme: %d %v", code, wf)
	}
	var list struct {
		Workflows []map[string]any `json:"workflows"`
	}
	h.do(as("globex", "", ""), "GET", "/v1/workflows", nil, &list)
	if len(list.Workflows) != 0 {
		t.Errorf("globex sees acme's workflows: %v", list.Workflows)
	}
	if code := h.do(as("globex", "", ""), "GET", "/v1/workflows/"+wf["id"].(string), nil, nil); code != 404 {
		t.Errorf("globex reading acme's workflow = %d", code)
	}
	h.do(keyA, "GET", "/v1/workflows", nil, &list)
	if len(list.Workflows) != 0 {
		t.Errorf("tenant a sees acme's workflows: %v", list.Workflows)
	}

	// The role on the request is enforced.
	if code := h.do(as("acme", "vik@acme.test", "viewer"), "POST", "/v1/workflows/"+wf["id"].(string)+"/runs", map[string]any{}, nil); code != 403 {
		t.Errorf("a viewer starting a run = %d", code)
	}
	var run map[string]any
	if code := h.do(as("acme", "dana@acme.test", "editor"), "POST", "/v1/workflows/"+wf["id"].(string)+"/runs", map[string]any{}, &run); code != 202 {
		t.Fatalf("an editor starting a run: %d %v", code, run)
	}
	id := run["id"].(string)
	if run["startedBy"] != "dana@acme.test" {
		t.Errorf("startedBy = %v", run["startedBy"])
	}
	h.waitRun(as("acme", "", ""), id, "waiting_approval")
	if code := h.do(as("acme", "dana@acme.test", "editor"), "POST", "/v1/runs/"+id+"/nodes/ok/approve", map[string]any{"approved": true}, nil); code != 403 {
		t.Errorf("an editor approving = %d", code)
	}
	if code := h.do(as("acme", "priya@acme.test", "approver"), "POST", "/v1/runs/"+id+"/nodes/ok/approve", map[string]any{"approved": true}, nil); code != 200 {
		t.Fatalf("an approver approving: %d", code)
	}
	done := h.waitRun(as("acme", "", ""), id, "succeeded", "failed")
	if by := node(done, "ok")["output"].(map[string]any)["decidedBy"]; by != "priya@acme.test" {
		t.Errorf("decidedBy = %v", by)
	}

	// The end user, not the platform, is on the audit trail.
	seen := map[string]string{}
	for _, e := range h.audit(as("acme", "", ""), "limit=50") {
		seen[fmt.Sprint(e["action"])] = fmt.Sprint(e["actor"])
	}
	if seen["run.start"] != "dana@acme.test" || seen["run.approve"] != "priya@acme.test" {
		t.Errorf("audit actors = %v", seen)
	}
	if len(h.audit(as("globex", "", ""), "limit=50")) != 0 {
		t.Error("globex has audit entries from acme")
	}
}

// A preview does every read and sends no write. It lists what it would have
// done, so a person can agree to the effects and not only to the graph.
func TestPreviewDoesTheReadsAndRecordsTheWrites(t *testing.T) {
	h := setup(t)
	api := ordersAPI(t, nil)
	cat := h.addAPI(keyA, "Shop", api.srv.URL, orderPaths)
	nodes := append(refundLoop(cat, map[string]any{}, true),
		// Uses what the refund returned, which a preview does not have.
		nd("note", "tool.http", map[string]any{"method": "POST", "url": api.srv.URL + "/notes", "body": `{"about":"{{outputs.refund.body.refunded}}"}`}, "refund"))

	var run map[string]any
	if code := h.do(keyA, "POST", "/v1/runs", map[string]any{"dag": map[string]any{"nodes": nodes}, "catalogIds": []string{cat}, "preview": true}, &run); code != 202 {
		t.Fatalf("start preview: %d %v", code, run)
	}
	id := run["id"].(string)
	// It never stops for a person.
	done := h.waitRun(keyA, id, "succeeded", "failed", "waiting_approval")
	if done["status"] != "succeeded" || done["preview"] != true {
		t.Fatalf("preview run: status=%v preview=%v error=%v", done["status"], done["preview"], done["error"])
	}
	if n := len(api.to("GET", "/orders")); n != 1 {
		t.Errorf("the read was made %d times, want 1", n)
	}
	for _, o := range []string{"a", "b", "c"} {
		if n := len(api.to("POST", "/orders/"+o+"/refund")); n != 0 {
			t.Errorf("a preview refunded order %s", o)
		}
	}
	if n := len(api.to("POST", "/notes")); n != 0 {
		t.Error("a preview posted a note")
	}

	var pv struct {
		Done    bool             `json:"done"`
		Counts  map[string]int   `json:"counts"`
		Actions []map[string]any `json:"actions"`
	}
	if code := h.do(keyA, "GET", "/v1/runs/"+id+"/preview", nil, &pv); code != 200 {
		t.Fatalf("read preview: %d", code)
	}
	if !pv.Done || pv.Counts["approval"] != 3 || pv.Counts["call"] != 3 || pv.Counts["unknown"] != 3 {
		t.Fatalf("counts = %v, actions = %v", pv.Counts, pv.Actions)
	}
	var calls, asks []string
	for _, a := range pv.Actions {
		d := a["detail"].(map[string]any)
		switch a["kind"] {
		case "call":
			calls = append(calls, fmt.Sprintf("%v %v item=%v", d["method"], strings.TrimPrefix(fmt.Sprint(d["url"]), api.srv.URL), a["item"]))
			if d["tool"] != "Shop" {
				t.Errorf("call detail = %v", d)
			}
		case "approval":
			asks = append(asks, fmt.Sprint(d["message"]))
		case "unknown":
			if a["node"] != "note" || !strings.Contains(fmt.Sprint(d["reason"]), "did not run") {
				t.Errorf("unknown action = %v", a)
			}
		}
	}
	if got := strings.Join(calls, "; "); got != "POST /orders/a/refund item=0; POST /orders/b/refund item=1; POST /orders/c/refund item=2" {
		t.Errorf("planned calls = %s", got)
	}
	if got := strings.Join(asks, "; "); got != "refund a?; refund b?; refund c?" {
		t.Errorf("planned questions = %s", got)
	}
	if code := h.do(keyB, "GET", "/v1/runs/"+id+"/preview", nil, nil); code != 404 {
		t.Errorf("tenant B reading the preview = %d", code)
	}

	// The same graph, for real, does stop for a person and does send.
	real := h.start(keyA, []string{cat}, refundLoop(cat, map[string]any{}, true)...)
	h.waitRun(keyA, real, "waiting_approval")
	if code := h.do(keyA, "GET", "/v1/runs/"+real+"/preview", nil, nil); code != 409 {
		t.Errorf("preview of a real run = %d, want 409", code)
	}
	h.do(keyA, "POST", "/v1/runs/"+real+"/cancel", nil, nil)
}

// Finished runs hold customer data in every step's output. Retention deletes
// them, with their steps and loop items, and leaves unfinished runs alone.
func TestRetentionDeletesOldFinishedRunsOnly(t *testing.T) {
	h := setup(t)
	api := ordersAPI(t, nil)
	cat := h.addAPI(keyA, "Shop", api.srv.URL, orderPaths)
	old := h.start(keyA, []string{cat}, refundLoop(cat, map[string]any{}, false)...)
	h.waitRun(keyA, old, "succeeded")
	recent := h.start(keyA, []string{cat}, refundLoop(cat, map[string]any{}, false)...)
	h.waitRun(keyA, recent, "succeeded")
	waiting := h.start(keyA, []string{cat}, refundLoop(cat, map[string]any{}, true)...)
	h.waitRun(keyA, waiting, "waiting_approval")

	ctx := context.Background()
	if _, err := h.st.Pool.Exec(ctx, `UPDATE runs SET finished_at = now() - interval '40 days', created_at = now() - interval '41 days' WHERE id = $1`, old); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.Pool.Exec(ctx, `UPDATE runs SET created_at = now() - interval '41 days' WHERE id = $1`, waiting); err != nil {
		t.Fatal(err)
	}
	n, err := h.st.PurgeRuns(ctx, time.Now().AddDate(0, 0, -30))
	if err != nil || n != 1 {
		t.Fatalf("purged %d runs, err %v; want 1", n, err)
	}
	if code := h.do(keyA, "GET", "/v1/runs/"+old, nil, nil); code != 404 {
		t.Errorf("the old run is still readable: %d", code)
	}
	var left int
	if err := h.st.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM node_runs WHERE run_id=$1) + (SELECT count(*) FROM node_item_runs WHERE run_id=$1)`, old).Scan(&left); err != nil || left != 0 {
		t.Errorf("%d step rows of the old run were left behind (err %v)", left, err)
	}
	for _, id := range []string{recent, waiting} {
		if code := h.do(keyA, "GET", "/v1/runs/"+id, nil, nil); code != 200 {
			t.Errorf("run %s should have been kept: %d", id, code)
		}
	}
	if n, err := h.st.PurgeAudit(ctx, time.Now().Add(time.Hour)); err != nil || n == 0 {
		t.Errorf("audit purge removed %d entries, err %v", n, err)
	}
	h.do(keyA, "POST", "/v1/runs/"+waiting+"/cancel", nil, nil)
}
