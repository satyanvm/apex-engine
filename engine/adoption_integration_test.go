//go:build integration

package engine_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/satyanvm/apex-engine/client"
	"github.com/satyanvm/apex-engine/engine"
)

// desk is a customer's help-desk API, the system the automation acts on.
type desk struct {
	srv   *httptest.Server
	mu    sync.Mutex
	calls []string
}

func newDesk(t *testing.T) *desk {
	d := &desk{}
	d.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		d.calls = append(d.calls, r.Method+" "+r.URL.Path)
		d.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/tickets" {
			_, _ = io.WriteString(w, `[{"id":"t1","subject":"refund please"},{"id":"t2","subject":"where is my order"}]`)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(d.srv.Close)
	return d
}

func (d *desk) writes() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []string
	for _, c := range d.calls {
		if !strings.HasPrefix(c, "GET ") {
			out = append(out, c)
		}
	}
	return out
}

func (d *desk) spec() map[string]any {
	ok := map[string]any{"200": map[string]any{"description": "ok"}}
	return map[string]any{"openapi": "3.0.0", "servers": []any{map[string]any{"url": d.srv.URL}}, "paths": map[string]any{
		"/tickets": map[string]any{"get": map[string]any{"operationId": "listTickets", "summary": "List open tickets", "responses": ok}},
		"/tickets/{id}/close": map[string]any{"post": map[string]any{"operationId": "closeTicket", "summary": "Close a ticket", "responses": ok,
			"parameters": []any{map[string]any{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}}}}},
	}}
}

func n(key, typ string, cfg map[string]any, deps ...string) client.Node {
	if deps == nil {
		deps = []string{}
	}
	return client.Node{Key: key, Type: typ, Name: key, DependsOn: deps, Config: cfg}
}

// TestAProductWithItsOwnCustomers walks the path of a product that puts the
// engine inside its own Go server and offers automation to its customers. It
// uses only the public packages, no API key, and no per-customer setup.
func TestAProductWithItsOwnCustomers(t *testing.T) {
	db, hp := os.Getenv("APEX_TEST_DATABASE_URL"), os.Getenv("TEMPORAL_TEST_HOSTPORT")
	if db == "" || hp == "" {
		t.Skip("APEX_TEST_DATABASE_URL and TEMPORAL_TEST_HOSTPORT are required")
	}
	t.Setenv("APEX_ALLOW_PRIVATE_NETWORK", "1") // the customer's API is on localhost here
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	e, err := engine.New(ctx, engine.Config{
		DatabaseURL: db, TemporalHostPort: hp,
		SecretKey:   base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)),
		MaxOpenRuns: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	// A customer signs up. Their id in the product is their tenant; nothing to create.
	acme, globex := e.Client("cust-acme-"+fmt.Sprint(time.Now().UnixNano())), e.Client("cust-globex-"+fmt.Sprint(time.Now().UnixNano()))
	defer func() { _ = globex.DeleteTenant(context.Background(), globex.Tenant) }()

	// The customer connects their help desk.
	api := newDesk(t)
	tool, err := acme.As("dana@acme.test", "admin").CreateCatalog(ctx, client.CatalogRequest{Name: "Helpdesk", Source: client.CatalogSource{Type: "openapi", Spec: api.spec()}})
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{tool.ID}

	// A plan, as a compile would return it: close every open ticket, asking first.
	dag := client.DAG{Nodes: []client.Node{
		n("start", "trigger.manual", map[string]any{}),
		n("tickets", "api.helpdesk.listTickets", map[string]any{}, "start"),
		n("each", "logic.loop", map[string]any{"items": "outputs.tickets.body"}, "tickets"),
		n("ask", "logic.approval", map[string]any{"message": "Close ticket {{item.id}}: {{item.subject}}?"}, "each"),
		n("close", "api.helpdesk.closeTicket", map[string]any{"id": "{{item.id}}"}, "ask"),
	}}
	dana := acme.As("dana@acme.test", "editor")

	// 1. What will it do, in words a customer can read?
	review, problems, err := dana.Review(ctx, dag, ids)
	if err != nil || len(problems) > 0 {
		t.Fatalf("review: %v %v", err, problems)
	}
	var lines []string
	for _, s := range review.Steps {
		lines = append(lines, s.Text)
	}
	text := strings.Join(lines, "\n")
	for _, want := range []string{"Read from Helpdesk: tickets", `Ask a person: "Close ticket {{item.id}}: {{item.subject}}?"`,
		"Change Helpdesk: close, once for each item, after a person approves"} {
		if !strings.Contains(text, want) {
			t.Errorf("review is missing %q:\n%s", want, text)
		}
	}
	if review.Changes != 1 || review.Reads != 1 || review.Approvals != 1 || len(review.Warnings) != 0 {
		t.Errorf("review counts = %+v", review)
	}
	// Without the approval, the same graph is flagged.
	bare := client.DAG{Nodes: append(append([]client.Node{}, dag.Nodes[:3]...), n("close", "api.helpdesk.closeTicket", map[string]any{"id": "{{item.id}}"}, "each"))}
	if rv, _, _ := dana.Review(ctx, bare, ids); rv == nil || len(rv.Warnings) != 1 || !strings.Contains(rv.Warnings[0], "no person is asked first") {
		t.Errorf("a write with no approval should be flagged: %+v", rv)
	}

	// 2. What would it do to this customer's real data, right now? Nothing is changed.
	pv, err := dana.StartRun(ctx, client.RunRequest{DAG: dag, CatalogIDs: ids, Preview: true})
	if err != nil {
		t.Fatal(err)
	}
	if pv, err = dana.WaitRun(ctx, pv.ID, client.WaitOptions{Poll: 50 * time.Millisecond}); err != nil || pv.Status != client.StatusSucceeded {
		t.Fatalf("preview run = %+v, err = %v", pv, err)
	}
	plan, err := dana.Preview(ctx, pv.ID)
	if err != nil {
		t.Fatal(err)
	}
	var would []string
	for _, a := range plan.Actions {
		switch a.Kind {
		case "call":
			would = append(would, fmt.Sprintf("%v %s", a.Detail["method"], strings.TrimPrefix(fmt.Sprint(a.Detail["url"]), api.srv.URL)))
		case "approval":
			would = append(would, "ask: "+fmt.Sprint(a.Detail["message"]))
		}
	}
	if got := strings.Join(would, " | "); got != "ask: Close ticket t1: refund please? | POST /tickets/t1/close | ask: Close ticket t2: where is my order? | POST /tickets/t2/close" {
		t.Errorf("preview = %s", got)
	}
	if w := api.writes(); len(w) != 0 {
		t.Fatalf("the preview changed the customer's data: %v", w)
	}

	// 3. Save it and run it for real. An editor cannot approve; an approver can,
	//    and is the name on the decision.
	wf, err := dana.CreateWorkflow(ctx, client.Workflow{Name: "Close open tickets", Nodes: dag.Nodes, CatalogIDs: ids})
	if err != nil {
		t.Fatal(err)
	}
	run, err := dana.RunWorkflow(ctx, wf.ID, client.WorkflowRunRequest{})
	if err != nil {
		t.Fatal(err)
	}
	priya := acme.As("priya@acme.test", "approver")
	for i := 0; i < 2; i++ {
		if run, err = dana.WaitRun(ctx, run.ID, client.WaitOptions{Poll: 50 * time.Millisecond, StopAtApproval: true}); err != nil || len(run.Waiting()) != 1 {
			t.Fatalf("waiting for approval %d: %+v %v", i, run, err)
		}
		var denied *client.Error
		if err := dana.Decide(ctx, run.ID, "ask", true, ""); !errors.As(err, &denied) || denied.Status != 403 {
			t.Fatalf("an editor approving: %v", err)
		}
		if err := priya.Decide(ctx, run.ID, "ask", true, "ok"); err != nil {
			t.Fatal(err)
		}
		// The next item's question, or the end.
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			run, _ = dana.GetRun(ctx, run.ID)
			if run.Done() || (len(run.Waiting()) == 1 && fmt.Sprint(run.Waiting()[0].Output) != "" && strings.Contains(fmt.Sprint(run.Waiting()[0].Output), fmt.Sprintf("ask#%d", i+1))) {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	if run, err = dana.WaitRun(ctx, run.ID, client.WaitOptions{Poll: 50 * time.Millisecond}); err != nil || run.Status != client.StatusSucceeded {
		t.Fatalf("run = %+v, err = %v", run, err)
	}
	if run.StartedBy != "dana@acme.test" {
		t.Errorf("startedBy = %q", run.StartedBy)
	}
	if got := strings.Join(api.writes(), " | "); got != "POST /tickets/t1/close | POST /tickets/t2/close" {
		t.Errorf("writes = %s", got)
	}

	// 4. Another customer sees none of it.
	if list, err := globex.ListWorkflows(ctx); err != nil || len(list) != 0 {
		t.Errorf("globex sees %d workflows, err %v", len(list), err)
	}
	if _, err := globex.GetRun(ctx, run.ID); !client.IsNotFound(err) {
		t.Errorf("globex reading acme's run: %v", err)
	}

	// 5. One customer cannot fill the queue: the third unfinished run is refused.
	var open []string
	for i := 0; i < 2; i++ {
		r, err := dana.RunWorkflow(ctx, wf.ID, client.WorkflowRunRequest{})
		if err != nil {
			t.Fatalf("run %d under the cap: %v", i, err)
		}
		open = append(open, r.ID)
	}
	var busy *client.Error
	if _, err := dana.RunWorkflow(ctx, wf.ID, client.WorkflowRunRequest{}); !errors.As(err, &busy) || busy.Status != 429 || busy.Code != "too_many_runs" {
		t.Errorf("a third open run: %v", err)
	}
	if _, err := globex.StartRun(ctx, client.RunRequest{DAG: client.DAG{Nodes: []client.Node{n("start", "trigger.manual", map[string]any{})}}}); err != nil {
		t.Errorf("acme's cap stopped globex: %v", err)
	}

	// 6. The customer leaves. Everything of theirs goes, running work included.
	if err := acme.DeleteTenant(ctx, "wrong-name"); err == nil {
		t.Fatal("deleting a tenant without its name should be refused")
	}
	if err := acme.DeleteTenant(ctx, acme.Tenant); err != nil {
		t.Fatal(err)
	}
	if list, _ := acme.ListWorkflows(ctx); len(list) != 0 {
		t.Errorf("workflows left after offboarding: %d", len(list))
	}
	if cats, _ := acme.ListCatalogs(ctx); len(cats) != 0 {
		t.Errorf("tools left after offboarding: %d", len(cats))
	}
	if _, err := acme.GetRun(ctx, open[0]); !client.IsNotFound(err) {
		t.Errorf("a run survived offboarding: %v", err)
	}
	if runs, err := globex.ListRuns(ctx, client.RunFilter{}); err != nil || len(runs) != 1 {
		t.Errorf("globex lost its run: %d %v", len(runs), err)
	}
}

// A compile streams its progress through the in-process client too. With no
// model connected it must say so, not hang.
func TestInProcessClientStreams(t *testing.T) {
	db, hp := os.Getenv("APEX_TEST_DATABASE_URL"), os.Getenv("TEMPORAL_TEST_HOSTPORT")
	if db == "" || hp == "" {
		t.Skip("APEX_TEST_DATABASE_URL and TEMPORAL_TEST_HOSTPORT are required")
	}
	for _, k := range []string{"GEMINI_API_KEY", "ANTHROPIC_API_KEY", "OPENAI_API_KEY", "MOONSHOT_API_KEY", "PLANNER_MODEL", "PLANNER_MAP_MODEL", "OLLAMA_HOST"} {
		t.Setenv(k, "")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	e, err := engine.New(ctx, engine.Config{DatabaseURL: db, TemporalHostPort: hp,
		SecretKey: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32))})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	c := e.Client("stream-test")
	if who, err := c.As("sam", "viewer").Whoami(ctx); err != nil || who.Actor != "sam" || who.Role != "viewer" {
		t.Fatalf("whoami = %+v, %v", who, err)
	}
	_, err = c.CompileStream(ctx, client.CompileRequest{Prompt: "close old tickets"}, func(client.Stage) {})
	var failed *client.Error
	if !errors.As(err, &failed) || !strings.Contains(strings.ToLower(failed.Message), "model") {
		t.Fatalf("a compile with no model should fail with the reason: %v", err)
	}
}
