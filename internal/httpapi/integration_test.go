//go:build integration

// Integration tests run the real API and a real Temporal worker against real
// Postgres and a real Temporal server. Only the model calls are stubbed:
//
//	APEX_TEST_DATABASE_URL=postgres://... TEMPORAL_TEST_HOSTPORT=localhost:7233 \
//	  go test -tags integration ./internal/httpapi
package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/satyanvm/apex-engine/internal/compiler"
	"github.com/satyanvm/apex-engine/internal/runtime"
	"github.com/satyanvm/apex-engine/internal/secret"
	"github.com/satyanvm/apex-engine/internal/store"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

const (
	keyA = "tenant-a-key-0123456789"
	keyB = "tenant-b-key-0123456789"
)

type harness struct {
	t        *testing.T
	url      string
	upstream *upstream
	st       *store.Store
	tc       client.Client
}

type upstream struct {
	mu      sync.Mutex
	headers map[string][]http.Header
	srv     *httptest.Server
}

func (u *upstream) calls(path string) []http.Header {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.headers[path]
}

func newUpstream() *upstream {
	u := &upstream{headers: map[string][]http.Header{}}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.headers[r.Method+" "+r.URL.Path] = append(u.headers[r.Method+" "+r.URL.Path], r.Header.Clone())
		u.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/orders":
			_, _ = io.WriteString(w, `[{"id":"a","total":10},{"id":"b","total":20}]`)
		case strings.HasSuffix(r.URL.Path, "/refund"):
			_, _ = io.WriteString(w, `{"ok":true}`)
		case r.URL.Path == "/missing":
			w.WriteHeader(404)
			_, _ = io.WriteString(w, `{"error":"no such thing"}`)
		default:
			_, _ = io.WriteString(w, `{}`)
		}
	}))
	return u
}

func (u *upstream) spec() map[string]any {
	op := func(id string, params ...any) map[string]any {
		return map[string]any{"operationId": id, "summary": id, "parameters": params, "responses": map[string]any{"200": map[string]any{"description": "ok"}}}
	}
	return map[string]any{
		"openapi": "3.0.0", "servers": []any{map[string]any{"url": u.srv.URL}},
		"paths": map[string]any{
			"/orders":             map[string]any{"get": op("listOrders")},
			"/orders/{id}/refund": map[string]any{"post": op("refund", map[string]any{"name": "id", "in": "path", "required": true})},
			"/missing":            map[string]any{"get": op("missing")},
		},
	}
}

func setup(t *testing.T) *harness {
	t.Helper()
	t.Setenv("APEX_ALLOW_PRIVATE_NETWORK", "1")
	dbURL, hp := os.Getenv("APEX_TEST_DATABASE_URL"), os.Getenv("TEMPORAL_TEST_HOSTPORT")
	if dbURL == "" || hp == "" {
		t.Skip("APEX_TEST_DATABASE_URL and TEMPORAL_TEST_HOSTPORT are required")
	}
	box, err := secret.New(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	st, err := store.Connect(ctx, dbURL, box)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx, `TRUNCATE catalogs, runs, schedules CASCADE`); err != nil {
		t.Fatal(err)
	}
	tc, err := client.Dial(client.Options{HostPort: hp})
	if err != nil {
		t.Fatal(err)
	}
	w := worker.New(tc, runtime.TaskQueue, worker.Options{})
	w.RegisterWorkflow(runtime.RunWorkflow)
	w.RegisterWorkflow(runtime.ScheduledWorkflow)
	w.RegisterActivity(&runtime.Activities{Store: st, Complete: func(_ context.Context, model, system, user string) (string, error) {
		return "model said: " + user, nil
	}})
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	u := newUpstream()
	api := &Server{Store: st, Temporal: tc, Keys: map[string]string{keyA: "a", keyB: "b"}, Log: slog.Default(),
		Compile: func(ctx context.Context, cat *compiler.Catalog, prompt, name string) (compiler.Plan, error) {
			return compiler.Plan{Name: "stub", Nodes: []compiler.GraphNode{{Key: "t", Type: "trigger.manual"}}, DefaultInput: map[string]any{}}, nil
		}}
	srv := httptest.NewServer(api.Handler(1000))
	t.Cleanup(func() { srv.Close(); w.Stop(); tc.Close(); u.srv.Close(); st.Pool.Close() })
	return &harness{t: t, url: srv.URL, upstream: u, st: st, tc: tc}
}

func (h *harness) do(key, method, path string, body any, into any) int {
	h.t.Helper()
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, h.url+path, rd)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if into != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, into); err != nil {
			h.t.Fatalf("%s %s: %v in %s", method, path, err, raw)
		}
	}
	return resp.StatusCode
}

func (h *harness) importCatalog(key string) string {
	h.t.Helper()
	var c map[string]any
	code := h.do(key, "POST", "/v1/catalogs", map[string]any{"name": "Acme", "source": map[string]any{"type": "openapi", "spec": h.upstream.spec()}}, &c)
	if code != 201 {
		h.t.Fatalf("import: %d %v", code, c)
	}
	id := c["id"].(string)
	var withCreds map[string]any
	if code := h.do(key, "PUT", "/v1/catalogs/"+id+"/credentials", map[string]any{"headers": map[string]string{"Authorization": "Bearer sekret-token"}}, &withCreds); code != 200 {
		h.t.Fatalf("credentials: %d", code)
	}
	if raw, _ := json.Marshal(withCreds); strings.Contains(string(raw), "sekret-token") {
		h.t.Fatal("API returned a credential value")
	}
	if withCreds["hasCredentials"] != true {
		h.t.Fatalf("hasCredentials: %v", withCreds)
	}
	return id
}

func (h *harness) waitRun(key, id string, want ...string) map[string]any {
	h.t.Helper()
	deadline := time.Now().Add(40 * time.Second)
	var run map[string]any
	for time.Now().Before(deadline) {
		h.do(key, "GET", "/v1/runs/"+id, nil, &run)
		for _, w := range want {
			if run["status"] == w {
				return run
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	h.t.Fatalf("run stuck: %v", run)
	return nil
}

func node(run map[string]any, key string) map[string]any {
	for _, raw := range run["nodes"].([]any) {
		if n := raw.(map[string]any); n["key"] == key {
			return n
		}
	}
	return nil
}

func nd(key, typ string, cfg map[string]any, deps ...string) map[string]any {
	if deps == nil {
		deps = []string{}
	}
	return map[string]any{"key": key, "type": typ, "name": key, "dependsOn": deps, "config": cfg}
}

func TestAuthAndTenantIsolation(t *testing.T) {
	h := setup(t)
	if code := h.do("", "GET", "/v1/catalogs", nil, nil); code != 401 {
		t.Errorf("no key: %d", code)
	}
	if code := h.do("wrong-key-wrong-key", "GET", "/v1/catalogs", nil, nil); code != 401 {
		t.Errorf("bad key: %d", code)
	}
	if code := h.do("", "GET", "/healthz", nil, nil); code != 200 {
		t.Errorf("healthz: %d", code)
	}
	cat := h.importCatalog(keyA)
	dag := map[string]any{"nodes": []any{nd("t", "trigger.manual", map[string]any{})}}
	var run map[string]any
	if code := h.do(keyA, "POST", "/v1/runs", map[string]any{"dag": dag}, &run); code != 202 {
		t.Fatalf("run: %d", code)
	}
	id := run["id"].(string)
	h.waitRun(keyA, id, "succeeded")

	for _, c := range []struct{ method, path string }{
		{"GET", "/v1/catalogs/" + cat}, {"DELETE", "/v1/catalogs/" + cat}, {"GET", "/v1/runs/" + id}, {"POST", "/v1/runs/" + id + "/cancel"},
	} {
		if code := h.do(keyB, c.method, c.path, nil, nil); code != 404 {
			t.Errorf("tenant B %s %s = %d, want 404", c.method, c.path, code)
		}
	}
	// Tenant B cannot compile with or run against tenant A's catalog.
	if code := h.do(keyB, "POST", "/v1/compile", map[string]any{"prompt": "x", "catalogIds": []string{cat}}, nil); code != 404 {
		t.Errorf("tenant B compile = %d", code)
	}
	if code := h.do(keyB, "POST", "/v1/runs", map[string]any{"dag": dag, "catalogIds": []string{cat}}, nil); code != 404 {
		t.Errorf("tenant B run = %d", code)
	}
	var list struct {
		Catalogs []any `json:"catalogs"`
		Runs     []any `json:"runs"`
	}
	h.do(keyB, "GET", "/v1/catalogs", nil, &list)
	h.do(keyB, "GET", "/v1/runs", nil, &list)
	if len(list.Catalogs) != 0 || len(list.Runs) != 0 {
		t.Errorf("tenant B sees %v", list)
	}
}

func TestFullRun(t *testing.T) {
	h := setup(t)
	cat := h.importCatalog(keyA)
	dag := map[string]any{
		"defaultInput": map[string]any{"who": "ops"},
		"nodes": []any{
			nd("start", "trigger.manual", map[string]any{}),
			nd("orders", "api.acme.listOrders", map[string]any{}, "start"),
			nd("gate", "logic.branch", map[string]any{"condition": "outputs.orders.status == 200"}, "orders"),
			nd("review", "logic.approval", map[string]any{"message": "Refund for {{input.who}}?"}, "gate"),
			nd("each", "logic.loop", map[string]any{"items": "outputs.orders.body"}, "review", "orders"),
			nd("refund", "api.acme.refund", map[string]any{"id": "{{item.id}}"}, "each"),
			nd("sum", "agent.llm", map[string]any{"prompt": "count {{outputs.each.count}} / {{input.who}}"}, "each"),
			nd("closed", "logic.filter", map[string]any{"condition": "input.who == nobody"}, "sum"),
			nd("never", "agent.llm", map[string]any{"prompt": "x"}, "closed"),
		},
	}
	var run map[string]any
	if code := h.do(keyA, "POST", "/v1/runs", map[string]any{"dag": dag, "catalogIds": []string{cat}}, &run); code != 202 {
		t.Fatalf("run: %d %v", code, run)
	}
	id := run["id"].(string)
	run = h.waitRun(keyA, id, "waiting_approval", "failed")
	if run["status"] != "waiting_approval" {
		t.Fatalf("run = %v", run)
	}
	if out := node(run, "review")["output"].(map[string]any); out["message"] != "Refund for ops?" {
		t.Errorf("approval message = %v", out)
	}
	if len(h.upstream.calls("POST /orders/a/refund")) != 0 {
		t.Fatal("refund ran before approval")
	}
	if code := h.do(keyA, "POST", "/v1/runs/"+id+"/nodes/gate/approve", map[string]any{"approved": true}, nil); code != 409 {
		t.Errorf("approving a non-waiting node = %d", code)
	}
	if code := h.do(keyA, "POST", "/v1/runs/"+id+"/nodes/review/approve", map[string]any{"approved": true, "note": "ok"}, nil); code != 200 {
		t.Fatalf("approve: %d", code)
	}
	run = h.waitRun(keyA, id, "succeeded", "failed")
	if run["status"] != "succeeded" {
		t.Fatalf("run = %v", run)
	}

	if got := h.upstream.calls("GET /orders")[0].Get("Authorization"); got != "Bearer sekret-token" {
		t.Errorf("credential not sent: %q", got)
	}
	a, b := h.upstream.calls("POST /orders/a/refund"), h.upstream.calls("POST /orders/b/refund")
	if len(a) != 1 || len(b) != 1 {
		t.Fatalf("refund calls a=%d b=%d", len(a), len(b))
	}
	if k := a[0].Get("Idempotency-Key"); k != id+":refund:0" {
		t.Errorf("Idempotency-Key = %q", k)
	}
	if out := node(run, "sum")["output"].(map[string]any); out["text"] != "model said: count 2 / ops" {
		t.Errorf("sum = %v", out)
	}
	if results := node(run, "each")["output"].(map[string]any)["results"].([]any); len(results) != 2 {
		t.Errorf("results = %v", results)
	}
	if node(run, "never")["status"] != "skipped" {
		t.Errorf("never = %v", node(run, "never"))
	}

	// The credential is sealed at rest and absent from workflow history.
	var sealed []byte
	if err := h.st.Pool.QueryRow(context.Background(), `SELECT credentials FROM catalogs WHERE id=$1`, cat).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("sekret-token")) {
		t.Error("credential stored in plaintext")
	}
	iter := h.tc.GetWorkflowHistory(context.Background(), "apex-run-"+id, "", false, 0)
	for iter.HasNext() {
		ev, err := iter.Next()
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(ev.String(), "sekret-token") {
			t.Fatal("credential leaked into workflow history")
		}
	}
}

func TestClientErrorFailsRunWithReasonAndNoRetry(t *testing.T) {
	h := setup(t)
	cat := h.importCatalog(keyA)
	dag := map[string]any{"nodes": []any{nd("t", "trigger.manual", map[string]any{}), nd("m", "api.acme.missing", map[string]any{}, "t"), nd("after", "agent.llm", map[string]any{"prompt": "x"}, "m")}}
	var run map[string]any
	h.do(keyA, "POST", "/v1/runs", map[string]any{"dag": dag, "catalogIds": []string{cat}}, &run)
	run = h.waitRun(keyA, run["id"].(string), "failed", "succeeded")
	if run["status"] != "failed" || !strings.Contains(fmt.Sprint(run["error"]), "HTTP 404 from Acme") {
		t.Fatalf("run = %v", run)
	}
	if n := len(h.upstream.calls("GET /missing")); n != 1 {
		t.Errorf("a 404 was called %d times", n)
	}
	if node(run, "m")["status"] != "failed" || node(run, "after")["status"] != "skipped" {
		t.Errorf("nodes = %v", run["nodes"])
	}
}

func TestInvalidDagIdempotencyCancel(t *testing.T) {
	h := setup(t)
	var e map[string]any
	bad := map[string]any{"nodes": []any{nd("a", "no.such.type", map[string]any{})}}
	if code := h.do(keyA, "POST", "/v1/runs", map[string]any{"dag": bad}, &e); code != 422 {
		t.Fatalf("invalid dag = %d", code)
	}
	if e["error"].(map[string]any)["code"] != "invalid_dag" || len(e["error"].(map[string]any)["problems"].([]any)) == 0 {
		t.Errorf("error = %v", e)
	}
	var v map[string]any
	h.do(keyA, "POST", "/v1/validate", map[string]any{"dag": bad}, &v)
	if v["valid"] != false {
		t.Errorf("validate = %v", v)
	}

	dag := map[string]any{"nodes": []any{nd("t", "trigger.manual", map[string]any{}), nd("wait", "logic.approval", map[string]any{"message": "m"}, "t"), nd("x", "agent.llm", map[string]any{"prompt": "x"}, "wait")}}
	var first, second map[string]any
	if code := h.do(keyA, "POST", "/v1/runs", map[string]any{"dag": dag, "idempotencyKey": "abc"}, &first); code != 202 {
		t.Fatalf("first: %d", code)
	}
	if code := h.do(keyA, "POST", "/v1/runs", map[string]any{"dag": dag, "idempotencyKey": "abc"}, &second); code != 200 {
		t.Fatalf("second: %d", code)
	}
	if first["id"] != second["id"] {
		t.Errorf("idempotency: %v vs %v", first["id"], second["id"])
	}
	id := first["id"].(string)
	h.waitRun(keyA, id, "waiting_approval")
	if code := h.do(keyA, "POST", "/v1/runs/"+id+"/cancel", nil, nil); code != 200 {
		t.Fatalf("cancel: %d", code)
	}
	run := h.waitRun(keyA, id, "cancelled")
	if node(run, "x")["status"] != "skipped" || node(run, "wait")["status"] != "skipped" {
		t.Errorf("nodes = %v", run["nodes"])
	}
}

func TestSchedule(t *testing.T) {
	h := setup(t)
	dag := map[string]any{"nodes": []any{nd("t", "trigger.manual", map[string]any{}), nd("x", "agent.llm", map[string]any{"prompt": "tick"}, "t")}}
	if code := h.do(keyA, "POST", "/v1/schedules", map[string]any{"cron": "nope", "dag": dag}, nil); code != 400 {
		t.Errorf("bad cron = %d", code)
	}
	var sc map[string]any
	if code := h.do(keyA, "POST", "/v1/schedules", map[string]any{"cron": "0 3 1 1 *", "dag": dag}, &sc); code != 201 {
		t.Fatalf("create: %d %v", code, sc)
	}
	id := sc["id"].(string)
	handle := h.tc.ScheduleClient().GetHandle(context.Background(), "apex-a-"+id)
	if err := handle.Trigger(context.Background(), client.ScheduleTriggerOptions{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	done := false
	for time.Now().Before(deadline) && !done {
		var list struct {
			Runs []map[string]any `json:"runs"`
		}
		h.do(keyA, "GET", "/v1/runs", nil, &list)
		for _, r := range list.Runs {
			done = done || r["status"] == "succeeded"
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !done {
		t.Fatal("the scheduled run never succeeded")
	}
	if code := h.do(keyB, "DELETE", "/v1/schedules/"+id, nil, nil); code != 404 {
		t.Errorf("tenant B delete = %d", code)
	}
	if code := h.do(keyA, "DELETE", "/v1/schedules/"+id, nil, nil); code != 204 {
		t.Errorf("delete = %d", code)
	}
}

func TestCompileRoute(t *testing.T) {
	h := setup(t)
	var out map[string]any
	if code := h.do(keyA, "POST", "/v1/compile", map[string]any{"prompt": "do a thing"}, &out); code != 200 {
		t.Fatalf("compile: %d %v", code, out)
	}
	if out["plan"].(map[string]any)["name"] != "stub" || out["trace"] == nil {
		t.Errorf("out = %v", out)
	}
	if code := h.do(keyA, "POST", "/v1/compile", map[string]any{"prompt": "  "}, nil); code != 400 {
		t.Errorf("empty prompt = %d", code)
	}
}
