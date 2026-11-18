// Saas is a product that offers automation to its own customers. The engine
// runs inside it; the product's users never see an engine key, and a customer
// needs no setup: their id in the product is their tenant.
//
//	DATABASE_URL=... APEX_SECRET_KEY=... go run ./examples/saas
//
// The three calls a product makes for an end user are below: describe a plan in
// words they can read, show what it would do to their data without doing it,
// and run it with their name on every decision.
package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os/signal"
	"syscall"

	"github.com/satyanvm/apex-engine/client"
	"github.com/satyanvm/apex-engine/engine"
)

// session is whoever your own login says the request is from.
type session struct{ Customer, User, Role string }

// whoIs stands in for your authentication. Map your roles onto the engine's:
// viewer, approver, editor, operator, admin.
func whoIs(r *http.Request) session {
	return session{Customer: r.Header.Get("X-Demo-Customer"), User: r.Header.Get("X-Demo-User"), Role: "operator"}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := engine.ConfigFromEnvWithoutKeys()
	if err != nil {
		log.Fatal(err)
	}
	cfg.MaxOpenRuns = 50            // one customer cannot fill the queue
	cfg.RetentionDays = 90          // run history, with customer data in it, is not kept forever
	cfg.RequireApproval = "deletes" // no workflow may delete without a person saying yes
	e, err := engine.New(ctx, cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer e.Close()

	// The engine for this request's customer, acting as this request's user.
	apex := func(r *http.Request) *client.Client {
		s := whoIs(r)
		return e.Client(s.Customer).As(s.User, s.Role)
	}
	reply := func(w http.ResponseWriter, v any, err error) {
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		_ = json.NewEncoder(w).Encode(v)
	}

	mux := http.NewServeMux()

	// "Describe what you want." The answer carries the plan, and a review of it
	// in plain words, with every change nobody would be asked about.
	mux.HandleFunc("POST /automations/draft", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Prompt string       `json:"prompt"`
			Tools  []string     `json:"tools"`
			Change *client.Plan `json:"change"` // an earlier draft to change, or nothing
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		res, err := apex(r).Compile(r.Context(), client.CompileRequest{Prompt: in.Prompt, CatalogIDs: in.Tools, Base: in.Change})
		reply(w, res, err)
	})

	// "Show me what it would do." Reads happen, writes do not.
	mux.HandleFunc("POST /automations/preview", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Plan  client.Plan `json:"plan"`
			Tools []string    `json:"tools"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		c := apex(r)
		run, err := c.StartRun(r.Context(), client.RunRequest{DAG: in.Plan.DAG(), CatalogIDs: in.Tools, Preview: true})
		if err == nil {
			run, err = c.WaitRun(r.Context(), run.ID, client.WaitOptions{})
		}
		if err != nil {
			reply(w, nil, err)
			return
		}
		pv, err := c.Preview(r.Context(), run.ID)
		reply(w, pv, err)
	})

	// "Run it." The user's name goes on the run and on each approval they give.
	mux.HandleFunc("POST /automations/run", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Plan  client.Plan `json:"plan"`
			Tools []string    `json:"tools"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		run, err := apex(r).StartRun(r.Context(), client.RunRequest{DAG: in.Plan.DAG(), CatalogIDs: in.Tools, Name: in.Plan.Name})
		reply(w, run, err)
	})
	mux.HandleFunc("POST /automations/runs/{run}/approve/{step}", func(w http.ResponseWriter, r *http.Request) {
		err := apex(r).Decide(r.Context(), r.PathValue("run"), r.PathValue("step"), true, "")
		reply(w, map[string]bool{"ok": err == nil}, err)
	})

	// A customer leaves: everything of theirs goes, running work included.
	mux.HandleFunc("DELETE /customers/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		reply(w, map[string]string{"deleted": id}, e.Client(id).DeleteTenant(r.Context(), id))
	})

	srv := &http.Server{Addr: ":9000", Handler: mux}
	go func() { <-ctx.Done(); _ = srv.Close() }()
	log.Print("your product on :9000, with the engine inside it")
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
