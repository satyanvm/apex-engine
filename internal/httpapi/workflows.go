package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/satyanvm/apex-engine/internal/catalog"
	"github.com/satyanvm/apex-engine/internal/compiler"
	"github.com/satyanvm/apex-engine/internal/runtime"
	"github.com/satyanvm/apex-engine/internal/store"
)

// hookRunsPerMinute caps runs started through one workflow's webhook.
const hookRunsPerMinute = 120

const demoWorkflowName = "Refund late orders"

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"models": compiler.Status(), "demoAvailable": s.DemoURL != "", "piecesAvailable": s.PiecesURL != "",
		"actor": who(r).Actor, "role": who(r).Role})
}

// callbackSecret returns the key that signs this tenant's run events.
func (s *Server) callbackSecret(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"secret": hex.EncodeToString(s.Store.Box.Derive(runtime.CallbackLabel(tenant(r))))})
}

func validCallback(w http.ResponseWriter, target string) bool {
	if target != "" && !validURL(target) {
		fail(w, http.StatusBadRequest, "bad_request", "callbackUrl must be a valid http(s) url")
		return false
	}
	return true
}

/* -------------------------------- workflows -------------------------------- */

type workflowBody struct {
	Name         string               `json:"name"`
	Description  string               `json:"description"`
	Prompt       string               `json:"prompt"`
	Nodes        []compiler.GraphNode `json:"nodes"`
	DefaultInput map[string]any       `json:"defaultInput"`
	CatalogIDs   []string             `json:"catalogIds"`
	CallbackURL  string               `json:"callbackUrl"`
}

// readWorkflow decodes and validates a workflow body; a stored workflow is
// always one that passed validation.
func (s *Server) readWorkflow(w http.ResponseWriter, r *http.Request) (*workflowBody, bool) {
	var body workflowBody
	if !decode(w, r, maxJSONBody, &body) {
		return nil, false
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" || len(body.Name) > 120 {
		fail(w, http.StatusBadRequest, "bad_request", "name is required (at most 120 characters)")
		return nil, false
	}
	if len(body.Nodes) == 0 {
		fail(w, http.StatusBadRequest, "bad_request", "a workflow needs at least one node")
		return nil, false
	}
	if !validCallback(w, body.CallbackURL) {
		return nil, false
	}
	if len(body.Prompt) > maxPrompt {
		fail(w, http.StatusBadRequest, "bad_request", "prompt is too long")
		return nil, false
	}
	problems, ok := s.problems(r, w, dag{Nodes: body.Nodes, DefaultInput: body.DefaultInput}, nil, body.CatalogIDs)
	if !ok {
		return nil, false
	}
	if len(problems) > 0 {
		invalidDag(w, problems)
		return nil, false
	}
	return &body, true
}

func (s *Server) createWorkflow(w http.ResponseWriter, r *http.Request) {
	body, ok := s.readWorkflow(w, r)
	if !ok {
		return
	}
	wf := &store.Workflow{ID: store.NewID("wfl"), Tenant: tenant(r), Name: body.Name, Description: body.Description,
		Prompt: body.Prompt, Nodes: body.Nodes, DefaultInput: body.DefaultInput, CatalogIDs: body.CatalogIDs, CallbackURL: body.CallbackURL}
	if err := s.Store.CreateWorkflow(r.Context(), wf); err != nil {
		s.failErr(w, err)
		return
	}
	s.audit(r, "workflow.create", wf.ID, map[string]any{"name": wf.Name, "steps": len(wf.Nodes)})
	writeJSON(w, http.StatusCreated, wf)
}

func (s *Server) listWorkflows(w http.ResponseWriter, r *http.Request) {
	out, err := s.Store.ListWorkflows(r.Context(), tenant(r))
	if err != nil {
		s.failErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"workflows": out})
}

func (s *Server) getWorkflow(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	wf, err := s.Store.GetWorkflow(r.Context(), tenant(r), id)
	if err != nil {
		s.failErr(w, err)
		return
	}
	schedules, err := s.Store.WorkflowSchedules(r.Context(), tenant(r), id)
	if err != nil {
		s.failErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		*store.Workflow
		Schedules []*store.Schedule `json:"schedules"`
	}{wf, schedules})
}

func (s *Server) updateWorkflow(w http.ResponseWriter, r *http.Request) {
	body, ok := s.readWorkflow(w, r)
	if !ok {
		return
	}
	wf := &store.Workflow{ID: chi.URLParam(r, "id"), Tenant: tenant(r), Name: body.Name, Description: body.Description,
		Prompt: body.Prompt, Nodes: body.Nodes, DefaultInput: body.DefaultInput, CatalogIDs: body.CatalogIDs, CallbackURL: body.CallbackURL}
	if err := s.Store.UpdateWorkflow(r.Context(), wf); err != nil {
		s.failErr(w, err)
		return
	}
	saved, err := s.Store.GetWorkflow(r.Context(), tenant(r), wf.ID)
	if err != nil {
		s.failErr(w, err)
		return
	}
	s.audit(r, "workflow.update", wf.ID, map[string]any{"name": wf.Name, "steps": len(wf.Nodes)})
	writeJSON(w, http.StatusOK, saved)
}

func (s *Server) deleteWorkflow(w http.ResponseWriter, r *http.Request) {
	id, t := chi.URLParam(r, "id"), tenant(r)
	if _, err := s.Store.GetWorkflow(r.Context(), t, id); err != nil {
		s.failErr(w, err)
		return
	}
	schedules, err := s.Store.WorkflowSchedules(r.Context(), t, id)
	if err != nil {
		s.failErr(w, err)
		return
	}
	for _, sc := range schedules {
		if err := s.Temporal.ScheduleClient().GetHandle(r.Context(), scheduleID(t, sc.ID)).Delete(r.Context()); err != nil {
			s.Log.Error("delete schedule", "error", err.Error())
			fail(w, http.StatusServiceUnavailable, "unavailable", "execution backend unavailable")
			return
		}
		if err := s.Store.DeleteSchedule(r.Context(), t, sc.ID); err != nil {
			s.failErr(w, err)
			return
		}
	}
	if err := s.Store.DeleteWorkflow(r.Context(), t, id); err != nil {
		s.failErr(w, err)
		return
	}
	s.audit(r, "workflow.delete", id, nil)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) runWorkflow(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Input          map[string]any `json:"input"`
		IdempotencyKey string         `json:"idempotencyKey"`
		CallbackURL    string         `json:"callbackUrl"`
		Preview        bool           `json:"preview"`
	}
	if !decode(w, r, maxJSONBody, &req) {
		return
	}
	wf, err := s.Store.GetWorkflow(r.Context(), tenant(r), chi.URLParam(r, "id"))
	if err != nil {
		s.failErr(w, err)
		return
	}
	if req.CallbackURL == "" {
		req.CallbackURL = wf.CallbackURL
	}
	run, code, ok := s.launch(w, r, dag{Nodes: wf.Nodes, DefaultInput: wf.DefaultInput}, wf.CatalogIDs, req.Input, req.IdempotencyKey,
		store.RunMeta{Name: wf.Name, SavedWorkflow: wf.ID, Source: "api", CallbackURL: req.CallbackURL, Preview: req.Preview})
	if ok {
		writeJSON(w, code, run)
	}
}

/* --------------------------------- webhooks -------------------------------- */

func newWebhookToken() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return "whk_" + hex.EncodeToString(b)
}

// enableWebhook turns the webhook on, or issues a new URL if it was on.
func (s *Server) enableWebhook(w http.ResponseWriter, r *http.Request) {
	token := newWebhookToken()
	if err := s.Store.SetWebhookToken(r.Context(), tenant(r), chi.URLParam(r, "id"), &token); err != nil {
		s.failErr(w, err)
		return
	}
	s.audit(r, "workflow.webhook_on", chi.URLParam(r, "id"), nil)
	writeJSON(w, http.StatusOK, map[string]string{"webhookPath": "/hooks/" + token})
}

func (s *Server) disableWebhook(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.SetWebhookToken(r.Context(), tenant(r), chi.URLParam(r, "id"), nil); err != nil {
		s.failErr(w, err)
		return
	}
	s.audit(r, "workflow.webhook_off", chi.URLParam(r, "id"), nil)
	w.WriteHeader(http.StatusNoContent)
}

// webhook starts a workflow from an outside system. The unguessable URL is the
// credential. The JSON body becomes the run input, and the response says only
// that a run started, never what it produced.
func (s *Server) webhook(w http.ResponseWriter, r *http.Request) {
	wf, err := s.Store.WorkflowByWebhook(r.Context(), chi.URLParam(r, "token"))
	if errors.Is(err, store.ErrNotFound) {
		fail(w, http.StatusNotFound, "not_found", "no such webhook")
		return
	}
	if err != nil {
		s.failErr(w, err)
		return
	}
	if !s.hooks.allow(wf.ID) {
		fail(w, http.StatusTooManyRequests, "rate_limited", "too many runs from this webhook; try again in a minute")
		return
	}
	input, ok := webhookInput(w, r)
	if !ok {
		return
	}
	r = withPrincipal(r, wf.Tenant, principal{Actor: "webhook", Role: store.RoleEditor})
	idem := ""
	if key := strings.TrimSpace(r.Header.Get("Idempotency-Key")); key != "" {
		idem = "hook:" + wf.ID + ":" + key
	}
	run, _, ok := s.launch(w, r, dag{Nodes: wf.Nodes, DefaultInput: wf.DefaultInput}, wf.CatalogIDs, input, idem,
		store.RunMeta{Name: wf.Name, SavedWorkflow: wf.ID, Source: "webhook", CallbackURL: wf.CallbackURL})
	if ok {
		writeJSON(w, http.StatusAccepted, map[string]string{"runId": run.ID, "status": run.Status})
	}
}

// webhookInput reads an optional JSON object body.
func webhookInput(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxJSONBody))
	if err != nil {
		fail(w, http.StatusRequestEntityTooLarge, "too_large", "body is larger than 2 MB")
		return nil, false
	}
	input := map[string]any{}
	if strings.TrimSpace(string(raw)) == "" {
		return input, true
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		fail(w, http.StatusBadRequest, "bad_request", "the body must be a JSON object")
		return nil, false
	}
	return input, true
}

/* ----------------------------------- demo ---------------------------------- */

// installDemo adds the sample tools and a sample workflow. Running it again
// changes nothing, so the console can offer it freely.
func (s *Server) installDemo(w http.ResponseWriter, r *http.Request) {
	if s.DemoURL == "" {
		fail(w, http.StatusNotFound, "not_found", "the sample tools are not running on this install")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	t := tenant(r)
	base := strings.TrimSuffix(s.DemoURL, "/")

	orders, policies, err := s.demoCatalogs(ctx, t)
	if err != nil {
		s.failErr(w, err)
		return
	}
	if orders == nil {
		var req catalogRequest
		req.Name, req.Source.Type, req.Source.URL = "Demo Orders", "openapi", base+"/openapi.json"
		c, apiErr, err := s.importCatalog(ctx, t, req)
		if apiErr != nil {
			fail(w, apiErr.status, apiErr.code, apiErr.msg)
			return
		}
		if err != nil {
			s.failErr(w, err)
			return
		}
		orders = c
	}
	if err := s.Store.SetCredentials(ctx, t, orders.ID, &store.Credentials{Headers: map[string]string{"Authorization": "Bearer demo-token"}}); err != nil {
		s.failErr(w, err)
		return
	}
	if policies == nil {
		var req catalogRequest
		req.Name, req.Source.Type, req.Source.URL = "Demo Policies", "mcp", base+"/mcp"
		c, apiErr, err := s.importCatalog(ctx, t, req)
		if apiErr != nil {
			fail(w, apiErr.status, apiErr.code, apiErr.msg)
			return
		}
		if err != nil {
			s.failErr(w, err)
			return
		}
		policies = c
	}

	existing, err := s.Store.ListWorkflows(ctx, t)
	if err != nil {
		s.failErr(w, err)
		return
	}
	for _, wf := range existing {
		if wf.Name == demoWorkflowName {
			writeJSON(w, http.StatusOK, map[string]any{"workflowId": wf.ID, "catalogIds": []string{orders.ID, policies.ID}})
			return
		}
	}
	wf := &store.Workflow{ID: store.NewID("wfl"), Tenant: t, Name: demoWorkflowName,
		Description: "Finds late orders, checks the refund policy for each, asks you before refunding the ones that qualify.",
		Prompt:      demoPrompt, Nodes: demoGraph(), DefaultInput: map[string]any{}, CatalogIDs: []string{orders.ID, policies.ID}}
	if err := s.Store.CreateWorkflow(ctx, wf); err != nil {
		s.failErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"workflowId": wf.ID, "catalogIds": []string{orders.ID, policies.ID}})
}

func (s *Server) demoCatalogs(ctx context.Context, t string) (orders, policies *store.Catalog, err error) {
	all, err := s.Store.ListCatalogs(ctx, t)
	if err != nil {
		return nil, nil, err
	}
	for _, c := range all {
		switch c.Slug {
		case catalog.Slug("Demo Orders"):
			orders = c
		case catalog.Slug("Demo Policies"):
			policies = c
		}
	}
	return orders, policies, nil
}

const demoPrompt = "Get the late orders, check the refund policy for each one, and for any that qualify ask me to approve before refunding it."

func demoGraph() []compiler.GraphNode {
	node := func(key, typ, name string, cfg map[string]any, deps ...string) compiler.GraphNode {
		if deps == nil {
			deps = []string{}
		}
		return compiler.GraphNode{Key: key, Type: typ, Name: name, DependsOn: deps, Config: cfg}
	}
	return []compiler.GraphNode{
		node("start", "trigger.manual", "Start", map[string]any{}),
		node("late", "api.demo_orders.listOrders", "List late orders", map[string]any{"status": "late"}, "start"),
		node("each", "logic.loop", "For each order", map[string]any{"items": "outputs.late.body"}, "late"),
		node("policy", "mcp.demo_policies.lookup_policy", "Check refund policy", map[string]any{"total": "{{item.total}}"}, "each"),
		node("qualifies", "logic.branch", "Does it qualify?", map[string]any{"condition": "outputs.policy.result.refundable == true"}, "policy"),
		node("approve", "logic.approval", "Ask a human", map[string]any{"message": "Refund ${{item.total}} to {{item.customer}} for order {{item.id}}?"}, "qualifies"),
		node("refund", "api.demo_orders.refundOrder", "Refund the order", map[string]any{"id": "{{item.id}}"}, "approve"),
	}
}
