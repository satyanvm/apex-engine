//go:build integration

package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/satyanvm/apex-engine/internal/runtime"
)

// fake is a scriptable upstream API that records every call it gets.
type fake struct {
	mu    sync.Mutex
	calls []*http.Request
	srv   *httptest.Server
}

func newFake(t *testing.T, h func(f *fake, w http.ResponseWriter, r *http.Request)) *fake {
	f := &fake{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls = append(f.calls, r.Clone(context.Background()))
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		h(f, w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake) to(method, path string) []*http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*http.Request
	for _, r := range f.calls {
		if r.Method == method && r.URL.Path == path {
			out = append(out, r)
		}
	}
	return out
}

func op(id string, params ...string) map[string]any {
	var ps []any
	for _, p := range params {
		in, name, _ := strings.Cut(p, ":")
		ps = append(ps, map[string]any{"name": name, "in": in, "required": in == "path"})
	}
	return map[string]any{"operationId": id, "summary": id, "parameters": ps, "responses": map[string]any{"200": map[string]any{"description": "ok"}}}
}

func (h *harness) addAPI(key, name, base string, paths map[string]any) string {
	h.t.Helper()
	spec := map[string]any{"openapi": "3.0.0", "servers": []any{map[string]any{"url": base}}, "paths": paths}
	var c map[string]any
	if code := h.do(key, "POST", "/v1/catalogs", map[string]any{"name": name, "source": map[string]any{"type": "openapi", "spec": spec}}, &c); code != 201 {
		h.t.Fatalf("import %s: %d %v", name, code, c)
	}
	return c["id"].(string)
}

func (h *harness) start(key string, catalogs []string, nodes ...any) string {
	h.t.Helper()
	var run map[string]any
	if code := h.do(key, "POST", "/v1/runs", map[string]any{"dag": map[string]any{"nodes": nodes}, "catalogIds": catalogs}, &run); code != 202 {
		h.t.Fatalf("start: %d %v", code, run)
	}
	return run["id"].(string)
}

func (h *harness) newKey(name, role string) string {
	h.t.Helper()
	var k map[string]any
	if code := h.do(keyA, "POST", "/v1/keys", map[string]string{"name": name, "role": role}, &k); code != 201 {
		h.t.Fatalf("create key %s: %d %v", name, code, k)
	}
	return k["key"].(string)
}

func (h *harness) audit(key, query string) []map[string]any {
	h.t.Helper()
	var out struct {
		Entries []map[string]any `json:"entries"`
	}
	if code := h.do(key, "GET", "/v1/audit?"+query, nil, &out); code != 200 {
		h.t.Fatalf("audit: %d", code)
	}
	return out.Entries
}

func ordersAPI(t *testing.T, failRefund func(id string) bool) *fake {
	return newFake(t, func(f *fake, w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/orders":
			_, _ = io.WriteString(w, `[{"id":"a"},{"id":"b"},{"id":"c"}]`)
		case strings.HasSuffix(r.URL.Path, "/refund"):
			id := strings.Split(r.URL.Path, "/")[2]
			if failRefund != nil && failRefund(id) {
				w.WriteHeader(422)
				_, _ = io.WriteString(w, `{"error":"card declined"}`)
				return
			}
			_, _ = io.WriteString(w, `{"refunded":"`+id+`"}`)
		}
	})
}

var orderPaths = map[string]any{
	"/orders":             map[string]any{"get": op("listOrders")},
	"/orders/{id}/refund": map[string]any{"post": op("refund", "path:id")},
}

func TestRolesKeysAndAuditLog(t *testing.T) {
	h := setup(t)
	viewer, approver, editor := h.newKey("vik", "viewer"), h.newKey("asha", "approver"), h.newKey("ed", "editor")

	var keys struct {
		Keys []map[string]any `json:"keys"`
	}
	h.do(keyA, "GET", "/v1/keys", nil, &keys)
	if raw, _ := json.Marshal(keys); len(keys.Keys) != 3 || strings.Contains(string(raw), viewer) {
		t.Fatalf("the key list must hold three keys and no secret: %s", raw)
	}
	if code := h.do(keyA, "POST", "/v1/keys", map[string]string{"name": "vik", "role": "viewer"}, nil); code != 409 {
		t.Errorf("duplicate name = %d", code)
	}
	if code := h.do(keyA, "POST", "/v1/keys", map[string]string{"name": "x", "role": "owner"}, nil); code != 400 {
		t.Errorf("unknown role = %d", code)
	}

	dag := map[string]any{"nodes": []any{
		nd("t", "trigger.manual", map[string]any{}),
		nd("ok", "logic.approval", map[string]any{"message": "go?"}, "t"),
		nd("say", "agent.llm", map[string]any{"prompt": "done"}, "ok"),
	}}
	for _, c := range []struct {
		who, key, method, path string
		body                   any
		want                   int
	}{
		{"viewer reads", viewer, "GET", "/v1/runs", nil, 200},
		{"viewer cannot run", viewer, "POST", "/v1/runs", map[string]any{"dag": dag}, 403},
		{"viewer cannot list keys", viewer, "GET", "/v1/keys", nil, 403},
		{"viewer cannot read the audit log", viewer, "GET", "/v1/audit", nil, 403},
		{"approver cannot run", approver, "POST", "/v1/runs", map[string]any{"dag": dag}, 403},
		{"editor cannot add tools", editor, "POST", "/v1/catalogs", map[string]any{"name": "x"}, 403},
		{"editor cannot make keys", editor, "POST", "/v1/keys", map[string]string{"name": "x", "role": "admin"}, 403},
	} {
		if code := h.do(c.key, c.method, c.path, c.body, nil); code != c.want {
			t.Errorf("%s: %d, want %d", c.who, code, c.want)
		}
	}

	var run map[string]any
	if code := h.do(editor, "POST", "/v1/runs", map[string]any{"dag": dag}, &run); code != 202 {
		t.Fatalf("editor run: %d %v", code, run)
	}
	id := run["id"].(string)
	if run["startedBy"] != "ed" {
		t.Errorf("startedBy = %v", run["startedBy"])
	}
	h.waitRun(editor, id, "waiting_approval")
	decision := map[string]any{"approved": true, "note": "fine"}
	if code := h.do(editor, "POST", "/v1/runs/"+id+"/nodes/ok/approve", decision, nil); code != 403 {
		t.Errorf("an editor must not approve their own run: %d", code)
	}
	if code := h.do(approver, "POST", "/v1/runs/"+id+"/nodes/ok/approve", decision, nil); code != 200 {
		t.Fatalf("approver: %d", code)
	}
	done := h.waitRun(viewer, id, "succeeded")
	if out := node(done, "ok")["output"].(map[string]any); out["decidedBy"] != "asha" || out["approved"] != true {
		t.Errorf("approval output = %v", out)
	}

	entries := h.audit(keyA, "target="+id)
	seen := map[string]string{}
	for _, e := range entries {
		seen[e["action"].(string)] = e["actor"].(string)
	}
	if seen["run.start"] != "ed" || seen["run.approve"] != "asha" {
		t.Errorf("audit for the run = %v", entries)
	}
	if got := h.audit(keyA, "action=key."); len(got) != 3 || got[0]["actor"] != "root" {
		t.Errorf("key audit = %v", got)
	}
	if got := h.audit(keyB, ""); len(got) != 0 {
		t.Errorf("tenant B sees tenant A's audit log: %v", got)
	}

	var first map[string]any
	h.do(keyA, "GET", "/v1/keys", nil, &keys)
	for _, k := range keys.Keys {
		if k["name"] == "ed" {
			first = k
		}
	}
	if code := h.do(keyA, "DELETE", "/v1/keys/"+first["id"].(string), nil, nil); code != 200 {
		t.Fatalf("revoke: %d", code)
	}
	if code := h.do(editor, "GET", "/v1/runs", nil, nil); code != 401 {
		t.Errorf("a revoked key still works: %d", code)
	}
	if code := h.do(keyB, "DELETE", "/v1/keys/"+first["id"].(string), nil, nil); code != 404 {
		t.Errorf("tenant B revoking tenant A's key = %d", code)
	}
}

func refundLoop(cat string, loopCfg map[string]any, withApproval bool) []any {
	loopCfg["items"] = "outputs.list.body"
	nodes := []any{
		nd("t", "trigger.manual", map[string]any{}),
		nd("list", "api.shop.listOrders", map[string]any{}, "t"),
		nd("each", "logic.loop", loopCfg, "list"),
	}
	dep := "each"
	if withApproval {
		nodes = append(nodes, nd("ok", "logic.approval", map[string]any{"message": "refund {{item.id}}?"}, "each"))
		dep = "ok"
	}
	return append(nodes, nd("refund", "api.shop.refund", map[string]any{"id": "{{item.id}}"}, dep))
}

// The bug this guards against: a decision delivered twice approved the next
// item of the loop, with nobody having looked at it.
func TestARepeatedDecisionDoesNotApproveTheNextLoopItem(t *testing.T) {
	h := setup(t)
	api := ordersAPI(t, nil)
	cat := h.addAPI(keyA, "Shop", api.srv.URL, orderPaths)
	id := h.start(keyA, []string{cat}, refundLoop(cat, map[string]any{}, true)...)

	run := h.waitRun(keyA, id, "waiting_approval")
	token := node(run, "ok")["output"].(map[string]any)["token"].(string)
	if token != "ok#0" {
		t.Fatalf("token = %q", token)
	}
	if code := h.do(keyA, "POST", "/v1/runs/"+id+"/nodes/ok/approve", map[string]any{"approved": true}, nil); code != 200 {
		t.Fatalf("approve: %d", code)
	}
	// The same decision again, delivered straight to the run as a retried
	// request or a second click would be.
	dup := runtime.ApprovalSignal{Approved: true, Token: token, By: "root"}
	if err := h.tc.SignalWorkflow(context.Background(), "apex-run-"+id, "", runtime.SignalName("ok"), dup); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		h.do(keyA, "GET", "/v1/runs/"+id, nil, &run)
		if out, _ := node(run, "ok")["output"].(map[string]any); out["token"] == "ok#1" && node(run, "ok")["status"] == "waiting_approval" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	time.Sleep(time.Second) // time for a wrongly approved refund to go out
	h.do(keyA, "GET", "/v1/runs/"+id, nil, &run)
	if run["status"] != "waiting_approval" {
		t.Fatalf("the run moved past the second item without a decision: %v", run["status"])
	}
	if got := len(api.to("POST", "/orders/b/refund")); got != 0 {
		t.Fatalf("order b was refunded %d time(s) without anyone approving it", got)
	}
	if got := len(api.to("POST", "/orders/a/refund")); got != 1 {
		t.Errorf("order a refunded %d times", got)
	}
	h.do(keyA, "POST", "/v1/runs/"+id+"/nodes/ok/approve", map[string]any{"approved": false}, nil)
	h.waitRun(keyA, id, "waiting_approval", "succeeded")
}

func TestLoopSetToContinueRecordsEachItem(t *testing.T) {
	h := setup(t)
	api := ordersAPI(t, func(id string) bool { return id == "b" })
	cat := h.addAPI(keyA, "Shop", api.srv.URL, orderPaths)
	id := h.start(keyA, []string{cat}, refundLoop(cat, map[string]any{"onError": "continue"}, false)...)
	run := h.waitRun(keyA, id, "succeeded", "failed")
	if run["status"] != "succeeded" || run["failedItems"] != float64(1) {
		t.Fatalf("status=%v failedItems=%v error=%v", run["status"], run["failedItems"], run["error"])
	}
	out := node(run, "each")["output"].(map[string]any)
	if out["succeeded"] != float64(2) || out["failed"] != float64(1) {
		t.Errorf("loop output = %v", out)
	}
	refund := node(run, "refund")
	if refund["itemsSucceeded"] != float64(2) || refund["itemsFailed"] != float64(1) || refund["status"] != "failed" {
		t.Errorf("refund step = %v", refund)
	}
	var items struct {
		Items []map[string]any `json:"items"`
	}
	h.do(keyA, "GET", "/v1/runs/"+id+"/nodes/refund/items", nil, &items)
	if len(items.Items) != 3 || items.Items[1]["status"] != "failed" || !strings.Contains(fmt.Sprint(items.Items[1]["error"]), "card declined") {
		t.Errorf("items = %v", items.Items)
	}
	h.do(keyA, "GET", "/v1/runs/"+id+"/nodes/refund/items?status=failed", nil, &items)
	if len(items.Items) != 1 || items.Items[0]["index"] != float64(1) {
		t.Errorf("failed items = %v", items.Items)
	}
	if code := h.do(keyB, "GET", "/v1/runs/"+id+"/nodes/refund/items", nil, nil); code != 404 {
		t.Errorf("tenant B reading items = %d", code)
	}
	for _, o := range []string{"a", "b", "c"} {
		if n := len(api.to("POST", "/orders/"+o+"/refund")); n != 1 {
			t.Errorf("order %s was called %d times", o, n)
		}
	}
}

func TestRetryContinuesAFailedRunWithoutRepeatingWhatSucceeded(t *testing.T) {
	h := setup(t)
	var mu sync.Mutex
	broken := true
	api := ordersAPI(t, func(id string) bool {
		mu.Lock()
		defer mu.Unlock()
		return broken && id == "b"
	})
	cat := h.addAPI(keyA, "Shop", api.srv.URL, orderPaths)
	id := h.start(keyA, []string{cat}, refundLoop(cat, map[string]any{}, false)...)
	if run := h.waitRun(keyA, id, "succeeded", "failed"); run["status"] != "failed" {
		t.Fatalf("the first run should fail at order b: %v", run["status"])
	}
	if code := h.do(keyA, "POST", "/v1/runs/"+id+"/cancel", nil, nil); code != 200 {
		t.Errorf("cancel of a finished run = %d", code)
	}

	mu.Lock()
	broken = false
	mu.Unlock()
	var second, again map[string]any
	if code := h.do(keyA, "POST", "/v1/runs/"+id+"/retry", nil, &second); code != 202 {
		t.Fatalf("retry: %d %v", code, second)
	}
	if code := h.do(keyA, "POST", "/v1/runs/"+id+"/retry", nil, &again); code != 200 || again["id"] != second["id"] {
		t.Errorf("a second retry should return the same run: %d %v", code, again["id"])
	}
	if second["resumeOf"] != id || second["source"] != "retry" {
		t.Errorf("second run = %v", second)
	}
	done := h.waitRun(keyA, second["id"].(string), "succeeded", "failed")
	if done["status"] != "succeeded" {
		t.Fatalf("resumed run: %v %v", done["status"], done["error"])
	}
	if node(done, "list")["reused"] != true {
		t.Errorf("the list step should be reused, got %v", node(done, "list"))
	}
	if n := len(api.to("GET", "/orders")); n != 1 {
		t.Errorf("the list was fetched %d times", n)
	}
	a, b, c := api.to("POST", "/orders/a/refund"), api.to("POST", "/orders/b/refund"), api.to("POST", "/orders/c/refund")
	if len(a) != 1 {
		t.Fatalf("order a was refunded %d times: the resume repeated a step that had succeeded", len(a))
	}
	if len(b) != 2 || len(c) != 1 {
		t.Fatalf("calls: b=%d c=%d", len(b), len(c))
	}
	if k1, k2 := b[0].Header.Get("Idempotency-Key"), b[1].Header.Get("Idempotency-Key"); k1 == "" || k1 != k2 || !strings.HasPrefix(k1, id) {
		t.Errorf("the retried call must carry the first run's idempotency key: %q then %q", k1, k2)
	}
	out := node(done, "each")["output"].(map[string]any)
	if results, _ := out["results"].([]any); len(results) != 3 {
		t.Errorf("results = %v", out["results"])
	}
	if code := h.do(keyA, "POST", "/v1/runs/"+second["id"].(string)+"/retry", nil, nil); code != 409 {
		t.Errorf("retrying a run that succeeded = %d", code)
	}
	if code := h.do(keyB, "POST", "/v1/runs/"+id+"/retry", nil, nil); code != 404 {
		t.Errorf("tenant B retrying tenant A's run = %d", code)
	}
}

func TestPagingTransformAndConditionsEndToEnd(t *testing.T) {
	h := setup(t)
	api := newFake(t, func(f *fake, w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page == 0 {
			page = 1
		}
		if page > 3 {
			_, _ = io.WriteString(w, `{"data":[]}`)
			return
		}
		fmt.Fprintf(w, `{"data":[{"id":%d,"total":%d},{"id":%d,"total":%d}]}`, page*2-1, page*100, page*2, page*10)
	})
	cat := h.addAPI(keyA, "Shop", api.srv.URL, map[string]any{"/things": map[string]any{"get": op("listThings", "query:page", "query:per_page")}})
	id := h.start(keyA, []string{cat},
		nd("t", "trigger.manual", map[string]any{}),
		nd("list", "api.shop.listThings", map[string]any{"allPages": "true"}, "t"),
		nd("big", "data.transform", map[string]any{"expression": `[.outputs.list.items[] | select(.total >= 100) | .id]`}, "list"),
		nd("gate", "logic.branch", map[string]any{"condition": "outputs.list.pages == 3 && (outputs.list.truncated == false || outputs.list.status == 500)"}, "list"),
		nd("pause", "logic.wait", map[string]any{"seconds": "1"}, "gate"),
		nd("say", "agent.llm", map[string]any{"prompt": "ids {{outputs.big.result}}"}, "pause", "big"),
	)
	run := h.waitRun(keyA, id, "succeeded", "failed")
	if run["status"] != "succeeded" {
		t.Fatalf("run: %v %v", run["status"], run["error"])
	}
	list := node(run, "list")["output"].(map[string]any)
	if items, _ := list["items"].([]any); len(items) != 6 || list["pages"] != float64(3) {
		t.Errorf("list = %v", list)
	}
	if got := fmt.Sprint(node(run, "big")["output"].(map[string]any)["result"]); got != "[1 3 5]" {
		t.Errorf("transform = %s", got)
	}
	if got := node(run, "say")["output"].(map[string]any)["text"]; got != "model said: ids [1,3,5]" {
		t.Errorf("say = %v", got)
	}
	if node(run, "pause")["status"] != "succeeded" {
		t.Errorf("wait = %v", node(run, "pause"))
	}
	// Bad graphs are refused before anything runs.
	for name, bad := range map[string]map[string]any{
		"bad jq":        nd("x", "data.transform", map[string]any{"expression": ".outputs.list.items | map("}, "list"),
		"bad condition": nd("x", "logic.branch", map[string]any{"condition": "outputs.list.pages == 3 &&"}, "list"),
		"nested loop":   nd("x", "logic.loop", map[string]any{"items": "item.lines"}, "outer"),
	} {
		nodes := []any{nd("t", "trigger.manual", map[string]any{}), nd("list", "api.shop.listThings", map[string]any{}, "t"),
			nd("outer", "logic.loop", map[string]any{"items": "outputs.list.items"}, "list"), bad}
		var resp map[string]any
		if code := h.do(keyA, "POST", "/v1/runs", map[string]any{"dag": map[string]any{"nodes": nodes}, "catalogIds": []string{cat}}, &resp); code != 422 {
			t.Errorf("%s: %d %v", name, code, resp)
		}
	}
}

// oauthProvider is a token endpoint and a protected API in one.
type oauthProvider struct {
	mu        sync.Mutex
	issued    int
	valid     string
	forms     []url.Values
	challenge string
	*fake
}

func newOAuthProvider(t *testing.T) *oauthProvider {
	p := &oauthProvider{}
	p.fake = newFake(t, func(f *fake, w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		switch r.URL.Path {
		case "/token":
			_ = r.ParseForm()
			p.forms = append(p.forms, r.PostForm)
			if r.PostForm.Get("client_secret") != "s3cret" {
				w.WriteHeader(401)
				_, _ = io.WriteString(w, `{"error":"invalid_client","error_description":"bad secret"}`)
				return
			}
			if r.PostForm.Get("grant_type") == "authorization_code" {
				sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
				if r.PostForm.Get("code") != "the-code" || base64.RawURLEncoding.EncodeToString(sum[:]) != p.challenge {
					w.WriteHeader(400)
					_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
					return
				}
			}
			p.issued++
			p.valid = fmt.Sprintf("tok%d", p.issued)
			fmt.Fprintf(w, `{"access_token":"%s","refresh_token":"ref%d","expires_in":3600}`, p.valid, p.issued)
		case "/me":
			if r.Header.Get("Authorization") != "Bearer "+p.valid {
				w.WriteHeader(401)
				_, _ = io.WriteString(w, `{"error":"expired"}`)
				return
			}
			_, _ = io.WriteString(w, `{"name":"acme"}`)
		}
	})
	return p
}

func (p *oauthProvider) revoke() {
	p.mu.Lock()
	p.valid = "nothing-is-valid"
	p.mu.Unlock()
}

func TestOAuthClientCredentialsFetchRefreshAndNeverLeak(t *testing.T) {
	h := setup(t)
	p := newOAuthProvider(t)
	cat := h.addAPI(keyA, "Idp", p.srv.URL, map[string]any{"/me": map[string]any{"get": op("me")}})
	oauthCfg := func(secret string) map[string]any {
		return map[string]any{"oauth": map[string]any{"grant": "client_credentials", "tokenUrl": p.srv.URL + "/token", "clientId": "cid", "clientSecret": secret, "scope": "read"}}
	}
	var resp map[string]any
	if code := h.do(keyA, "PUT", "/v1/catalogs/"+cat+"/credentials", oauthCfg("wrong"), &resp); code != 422 || !strings.Contains(fmt.Sprint(resp), "bad secret") {
		t.Fatalf("wrong secret should be refused with the reason: %d %v", code, resp)
	}
	if code := h.do(keyA, "PUT", "/v1/catalogs/"+cat+"/credentials", oauthCfg("s3cret"), &resp); code != 200 || resp["auth"] != "oauth2" {
		t.Fatalf("credentials: %d %v", code, resp)
	}
	var got map[string]any
	h.do(keyA, "GET", "/v1/catalogs/"+cat, nil, &got)
	if raw, _ := json.Marshal(got); strings.Contains(string(raw), "s3cret") || strings.Contains(string(raw), "tok1") {
		t.Fatalf("the API returned a secret: %s", raw)
	}
	call := func() map[string]any {
		id := h.start(keyA, []string{cat}, nd("t", "trigger.manual", map[string]any{}), nd("me", "api.idp.me", map[string]any{}, "t"))
		return h.waitRun(keyA, id, "succeeded", "failed")
	}
	if run := call(); run["status"] != "succeeded" {
		t.Fatalf("first call: %v", run["error"])
	}
	if p.issued != 1 {
		t.Errorf("tokens issued = %d; the token fetched when the settings were saved should be reused", p.issued)
	}
	// The provider drops the token early. The engine sees a 401, fetches a new
	// one and repeats the call, all inside the same step.
	p.revoke()
	run := call()
	if run["status"] != "succeeded" || node(run, "me")["attempts"] != float64(1) {
		t.Fatalf("after revoke: %v attempts=%v", run["error"], node(run, "me")["attempts"])
	}
	if p.issued != 2 {
		t.Errorf("tokens issued = %d, want 2", p.issued)
	}
	var history []map[string]any
	_ = json.Unmarshal(mustJSON(t, run), &history)
	if raw := string(mustJSON(t, run)); strings.Contains(raw, "tok2") {
		t.Errorf("a token reached the run record: %s", raw)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestOAuthConsentFlow(t *testing.T) {
	h := setupWith(t, func(s *Server, _ *runtime.Activities) { s.PublicURL = "https://engine.example" })
	p := newOAuthProvider(t)
	cat := h.addAPI(keyA, "Idp", p.srv.URL, map[string]any{"/me": map[string]any{"get": op("me")}})
	var start map[string]any
	body := map[string]any{"authUrl": p.srv.URL + "/authorize", "tokenUrl": p.srv.URL + "/token", "clientId": "cid", "clientSecret": "s3cret", "scope": "read",
		"authParams": map[string]string{"access_type": "offline"}}
	if code := h.do(h.newKey("ed", "editor"), "POST", "/v1/catalogs/"+cat+"/oauth/authorize", body, nil); code != 403 {
		t.Errorf("an editor starting a consent = %d", code)
	}
	if code := h.do(keyA, "POST", "/v1/catalogs/"+cat+"/oauth/authorize", body, &start); code != 200 {
		t.Fatalf("authorize: %d %v", code, start)
	}
	u, err := url.Parse(start["authorizeUrl"].(string))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("redirect_uri") != "https://engine.example/oauth/callback" || q.Get("access_type") != "offline" || q.Get("code_challenge_method") != "S256" || q.Get("state") == "" {
		t.Fatalf("authorize url = %s", u)
	}
	p.challenge = q.Get("code_challenge")
	var c map[string]any
	h.do(keyA, "GET", "/v1/catalogs/"+cat, nil, &c)
	if c["auth"] != "oauth2_pending" {
		t.Errorf("before consent auth = %v", c["auth"])
	}
	page := func(query string) (int, string) {
		resp, err := http.Get(h.url + "/oauth/callback?" + query)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw)
	}
	if code, _ := page("state=made-up&code=the-code"); code != 400 {
		t.Errorf("an unknown state = %d", code)
	}
	if code, body := page("state=" + url.QueryEscape(q.Get("state")) + "&code=the-code"); code != 200 || !strings.Contains(body, "Connected") {
		t.Fatalf("callback: %d %s", code, body)
	}
	if code, _ := page("state=" + url.QueryEscape(q.Get("state")) + "&code=the-code"); code != 400 {
		t.Errorf("a state must work once, second use = %d", code)
	}
	h.do(keyA, "GET", "/v1/catalogs/"+cat, nil, &c)
	if c["auth"] != "oauth2" {
		t.Fatalf("after consent auth = %v", c["auth"])
	}
	id := h.start(keyA, []string{cat}, nd("t", "trigger.manual", map[string]any{}), nd("me", "api.idp.me", map[string]any{}, "t"))
	if run := h.waitRun(keyA, id, "succeeded", "failed"); run["status"] != "succeeded" {
		t.Fatalf("call with the consented token: %v", run["error"])
	}
	// When the token stops working the engine uses the refresh token.
	p.revoke()
	id = h.start(keyA, []string{cat}, nd("t", "trigger.manual", map[string]any{}), nd("me", "api.idp.me", map[string]any{}, "t"))
	if run := h.waitRun(keyA, id, "succeeded", "failed"); run["status"] != "succeeded" {
		t.Fatalf("call after refresh: %v", run["error"])
	}
	last := p.forms[len(p.forms)-1]
	if last.Get("grant_type") != "refresh_token" || last.Get("refresh_token") != "ref1" {
		t.Errorf("refresh form = %v", last)
	}
	if got := h.audit(keyA, "action=catalog.oauth"); len(got) != 2 {
		t.Errorf("audit = %v", got)
	}
}

func TestMetrics(t *testing.T) {
	h := setupWith(t, func(s *Server, _ *runtime.Activities) { s.MetricsToken = "scrape-me" })
	id := h.start(keyA, nil, nd("t", "trigger.manual", map[string]any{}))
	h.waitRun(keyA, id, "succeeded")
	get := func(token string) (int, string) {
		req, _ := http.NewRequest("GET", h.url+"/metrics", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw)
	}
	if code, _ := get(""); code != 401 {
		t.Errorf("no token = %d", code)
	}
	if code, _ := get(keyA); code != 401 {
		t.Errorf("a tenant's API key must not read every tenant's metrics: %d", code)
	}
	code, body := get("scrape-me")
	if code != 200 {
		t.Fatalf("metrics = %d", code)
	}
	for _, want := range []string{`apex_runs_started_total{tenant="a",source="api"} 1`, `apex_runs_finished_total{tenant="a",status="succeeded"} 1`, "# TYPE apex_runs_open gauge"} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics lack %q:\n%s", want, body)
		}
	}
	off := setup(t)
	if resp, err := http.Get(off.url + "/metrics"); err != nil || resp.StatusCode != 404 {
		t.Errorf("metrics should be off without a token")
	}
}

// Needs the pieces runner: node pieces/server.mjs, with APEX_TEST_PIECES_URL
// (and APEX_TEST_PIECES_TOKEN) pointing at it.
func TestActivepiecesPieceRunsAsATool(t *testing.T) {
	base, token := os.Getenv("APEX_TEST_PIECES_URL"), os.Getenv("APEX_TEST_PIECES_TOKEN")
	if base == "" {
		t.Skip("APEX_TEST_PIECES_URL is not set")
	}
	h := setupWith(t, func(s *Server, a *runtime.Activities) {
		s.PiecesURL, s.PiecesToken, a.PiecesURL, a.PiecesToken = base, token, base, token
	})
	var list struct {
		Pieces []map[string]any `json:"pieces"`
	}
	if code := h.do(keyA, "GET", "/v1/pieces?q=json", nil, &list); code != 200 || len(list.Pieces) == 0 {
		t.Fatalf("pieces: %d %v", code, list)
	}
	var c map[string]any
	if code := h.do(keyA, "POST", "/v1/catalogs", map[string]any{"name": "Json", "source": map[string]any{"type": "piece", "piece": "json"}}, &c); code != 201 {
		t.Fatalf("add piece: %d %v", code, c)
	}
	if c["kind"] != "piece" || c["nodeCount"].(float64) < 2 {
		t.Fatalf("catalog = %v", c)
	}
	if code := h.do(keyA, "POST", "/v1/catalogs", map[string]any{"name": "Nope", "source": map[string]any{"type": "piece", "piece": "no-such-piece"}}, nil); code != 422 {
		t.Errorf("unknown piece = %d", code)
	}
	cat := c["id"].(string)
	id := h.start(keyA, []string{cat},
		nd("t", "trigger.manual", map[string]any{}),
		nd("parse", "piece.json.convert_text_to_json", map[string]any{"text": `{"n": 41, "tags": ["x"]}`}, "t"),
		nd("next", "data.transform", map[string]any{"expression": ".outputs.parse.result.n + 1"}, "parse"),
	)
	run := h.waitRun(keyA, id, "succeeded", "failed")
	if run["status"] != "succeeded" {
		t.Fatalf("run: %v", run["error"])
	}
	if got := node(run, "next")["output"].(map[string]any)["result"]; got != float64(42) {
		t.Errorf("result = %v", got)
	}
	// A piece that fails says why, and the step is not retried.
	id = h.start(keyA, []string{cat}, nd("t", "trigger.manual", map[string]any{}),
		nd("parse", "piece.json.convert_text_to_json", map[string]any{"text": "not json"}, "t"))
	run = h.waitRun(keyA, id, "succeeded", "failed")
	if run["status"] != "failed" || node(run, "parse")["attempts"] != float64(1) {
		t.Errorf("bad input: status=%v attempts=%v error=%v", run["status"], node(run, "parse")["attempts"], run["error"])
	}
}
