//go:build integration

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/satyanvm/apex-engine/client"
)

type received struct {
	event     client.Event
	signature string
	body      []byte
}

type receiver struct {
	mu       sync.Mutex
	got      []received
	failures int // answer 500 this many times first
	srv      *httptest.Server
}

func newReceiver(t *testing.T, failures int) *receiver {
	r := &receiver{failures: failures}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		raw, _ := io.ReadAll(req.Body)
		var ev client.Event
		_ = json.Unmarshal(raw, &ev)
		r.mu.Lock()
		defer r.mu.Unlock()
		r.got = append(r.got, received{event: ev, signature: req.Header.Get("X-Apex-Signature"), body: raw})
		if r.failures > 0 {
			r.failures--
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *receiver) waitFor(t *testing.T, name string, n int) []received {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		var match []received
		for _, g := range r.got {
			if g.event.Event == name {
				match = append(match, g)
			}
		}
		r.mu.Unlock()
		if len(match) >= n {
			return match
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("event %s never arrived %d times", name, n)
	return nil
}

func TestGoClientDrivesARunAndReceivesSignedEvents(t *testing.T) {
	h := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c := client.New(h.url, keyA)

	cat, err := c.CreateCatalog(ctx, client.CatalogRequest{Name: "Acme", Source: client.CatalogSource{Type: "openapi", Spec: h.upstream.spec()}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SetCredentials(ctx, cat.ID, map[string]string{"Authorization": "Bearer x"}); err != nil {
		t.Fatal(err)
	}
	ids := []string{cat.ID}

	// A compile comes back typed, through both paths.
	if res, err := c.Compile(ctx, client.CompileRequest{Prompt: "x", CatalogIDs: ids}); err != nil || res.Plan.Name != "stub" {
		t.Fatalf("compile: %+v %v", res, err)
	}
	if res, err := c.CompileStream(ctx, client.CompileRequest{Prompt: "x", CatalogIDs: ids}, nil); err != nil || res.Plan.Name != "stub" {
		t.Fatalf("compile stream: %+v %v", res, err)
	}

	// An invalid graph is refused with its problems, typed.
	bad := client.DAG{Nodes: []client.Node{{Key: "x", Type: "api.acme.madeUp", DependsOn: []string{}, Config: map[string]any{}}}}
	if problems, err := c.Validate(ctx, bad, ids); err != nil || len(problems) == 0 {
		t.Fatalf("validate: %v %v", problems, err)
	}
	_, err = c.StartRun(ctx, client.RunRequest{DAG: bad, CatalogIDs: ids})
	var apiErr *client.Error
	if !errors.As(err, &apiErr) || apiErr.Code != "invalid_dag" || len(apiErr.Problems) == 0 {
		t.Fatalf("invalid run: %#v", err)
	}
	if _, err := c.GetRun(ctx, "run_missing"); !client.IsNotFound(err) {
		t.Fatalf("missing run: %v", err)
	}

	secret, err := c.CallbackSecret(ctx)
	if err != nil || secret == "" {
		t.Fatalf("secret: %q %v", secret, err)
	}
	other, err := client.New(h.url, keyB).CallbackSecret(ctx)
	if err != nil || other == secret {
		t.Fatalf("tenants must not share a signing key: %v", err)
	}

	hook := newReceiver(t, 1) // the first delivery fails, so a retry must follow
	dag := client.DAG{Nodes: []client.Node{
		{Key: "start", Type: "trigger.manual", DependsOn: []string{}, Config: map[string]any{}},
		{Key: "orders", Type: "api.acme.listOrders", DependsOn: []string{"start"}, Config: map[string]any{}},
		{Key: "review", Type: "logic.approval", DependsOn: []string{"orders"}, Config: map[string]any{"message": "Go ahead?"}},
		{Key: "say", Type: "agent.llm", DependsOn: []string{"review"}, Config: map[string]any{"prompt": "done"}},
	}}
	run, err := c.StartRun(ctx, client.RunRequest{DAG: dag, CatalogIDs: ids, Name: "from go", CallbackURL: hook.srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	run, err = c.WaitRun(ctx, run.ID, client.WaitOptions{Poll: 50 * time.Millisecond, StopAtApproval: true})
	if err != nil || run.Status != client.StatusWaiting || len(run.Waiting()) != 1 {
		t.Fatalf("wait: %+v %v", run, err)
	}

	// The failed first delivery is retried with the same delivery id.
	waits := hook.waitFor(t, client.EventWaitingApproval, 2)
	if waits[0].event.Delivery != waits[1].event.Delivery || waits[0].event.Delivery == "" {
		t.Errorf("a retry must keep its delivery id: %q %q", waits[0].event.Delivery, waits[1].event.Delivery)
	}
	if w := waits[1]; w.event.Node != "review" || w.event.Message != "Go ahead?" || w.event.RunID != run.ID {
		t.Errorf("event = %+v", w.event)
	}
	if !client.VerifySignature(secret, waits[1].body, waits[1].signature) {
		t.Error("the event signature did not verify")
	}
	if client.VerifySignature(other, waits[1].body, waits[1].signature) {
		t.Error("another tenant's key verified this event")
	}

	if err := c.Decide(ctx, run.ID, "review", true, "yes"); err != nil {
		t.Fatal(err)
	}
	run, err = c.WaitRun(ctx, run.ID, client.WaitOptions{Poll: 50 * time.Millisecond})
	if err != nil || run.Status != client.StatusSucceeded || run.Node("say") == nil {
		t.Fatalf("finish: %+v %v", run, err)
	}
	done := hook.waitFor(t, client.EventFinished, 1)
	if done[0].event.Status != client.StatusSucceeded || done[0].event.RunID != run.ID {
		t.Errorf("finished event = %+v", done[0].event)
	}

	// A saved workflow carries its callback to every run.
	saved := newReceiver(t, 0)
	wf, err := c.CreateWorkflow(ctx, client.Workflow{Name: "Saved", Nodes: dag.Nodes[:2], CatalogIDs: ids, CallbackURL: saved.srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	path, err := c.EnableWebhook(ctx, wf.ID)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(h.url+path, "application/json", nil)
	if err != nil || resp.StatusCode != http.StatusAccepted {
		t.Fatalf("webhook: %v %v", resp, err)
	}
	_ = resp.Body.Close()
	if ev := saved.waitFor(t, client.EventFinished, 1)[0]; ev.event.Status != client.StatusSucceeded {
		t.Errorf("webhook run event = %+v", ev.event)
	}

	// A receiver that rejects the event outright does not fail the run.
	gone := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusGone) }))
	defer gone.Close()
	run, err = c.StartRun(ctx, client.RunRequest{DAG: client.DAG{Nodes: dag.Nodes[:2]}, CatalogIDs: ids, CallbackURL: gone.URL})
	if err != nil {
		t.Fatal(err)
	}
	if run, err = c.WaitRun(ctx, run.ID, client.WaitOptions{Poll: 50 * time.Millisecond}); err != nil || run.Status != client.StatusSucceeded {
		t.Fatalf("callback failure leaked into the run: %+v %v", run, err)
	}

	if _, err := c.StartRun(ctx, client.RunRequest{DAG: client.DAG{Nodes: dag.Nodes[:2]}, CatalogIDs: ids, CallbackURL: "not a url"}); err == nil {
		t.Error("a bad callback URL should be refused")
	}
}
