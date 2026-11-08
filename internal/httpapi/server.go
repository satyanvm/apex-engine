// Package httpapi serves the engine's public API. docs/API.md is the contract.
package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/satyanvm/apex-engine/internal/catalog"
	"github.com/satyanvm/apex-engine/internal/compiler"
	"github.com/satyanvm/apex-engine/internal/netguard"
	"github.com/satyanvm/apex-engine/internal/runtime"
	"github.com/satyanvm/apex-engine/internal/store"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
)

const (
	maxJSONBody    = 2 << 20
	maxCatalogBody = 16 << 20
	maxSpecBytes   = 16 << 20
	maxPrompt      = 20000
	maxNodes       = 500
	compileTimeout = 8 * time.Minute
)

// Server holds the API's dependencies.
type Server struct {
	Store    *store.Store
	Temporal client.Client
	Keys     map[string]string // API key -> tenant
	Compile  func(ctx context.Context, cat *compiler.Catalog, prompt, name string) (compiler.Plan, error)
	Log      *slog.Logger
	limiter  *limiter
}

// ParseKeys reads "key:tenant,key2:tenant2".
func ParseKeys(raw string) (map[string]string, error) {
	out := map[string]string{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key, tenant, ok := strings.Cut(part, ":")
		if !ok || len(key) < 16 || tenant == "" {
			return nil, errors.New("APEX_API_KEYS entries must be <key of 16+ chars>:<tenant>")
		}
		out[key] = tenant
	}
	if len(out) == 0 {
		return nil, errors.New("APEX_API_KEYS is required")
	}
	return out, nil
}

// Handler builds the router.
func (s *Server) Handler(compilesPerMinute int) http.Handler {
	s.limiter = newLimiter(compilesPerMinute, time.Minute)
	r := chi.NewRouter()
	r.Use(recoverer(s.Log))
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	r.Route("/v1", func(r chi.Router) {
		r.Use(s.auth)
		r.Get("/catalogs", s.listCatalogs)
		r.Post("/catalogs", s.createCatalog)
		r.Get("/catalogs/{id}", s.getCatalog)
		r.Delete("/catalogs/{id}", s.deleteCatalog)
		r.Put("/catalogs/{id}/credentials", s.putCredentials)
		r.Post("/compile", s.compile)
		r.Post("/compile/stream", s.compileStream)
		r.Post("/validate", s.validate)
		r.Post("/runs", s.createRun)
		r.Get("/runs", s.listRuns)
		r.Get("/runs/{id}", s.getRun)
		r.Post("/runs/{id}/cancel", s.cancelRun)
		r.Post("/runs/{id}/nodes/{key}/approve", s.approve)
		r.Post("/schedules", s.createSchedule)
		r.Get("/schedules", s.listSchedules)
		r.Delete("/schedules/{id}", s.deleteSchedule)
	})
	return r
}

type tenantKey struct{}

func tenant(r *http.Request) string { return r.Context().Value(tenantKey{}).(string) }

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		given, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if ok {
			for key, t := range s.Keys { // constant time per key
				if subtle.ConstantTimeCompare([]byte(given), []byte(key)) == 1 {
					next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), tenantKey{}, t)))
					return
				}
			}
		}
		fail(w, http.StatusUnauthorized, "unauthorized", "missing or invalid API key")
	})
}

func recoverer(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if v := recover(); v != nil {
					log.Error("panic", "path", r.URL.Path, "value", fmt.Sprint(v))
					fail(w, http.StatusInternalServerError, "internal", "internal error")
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"code": code, "message": msg}})
}

func (s *Server) failErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		fail(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, store.ErrConflict):
		fail(w, http.StatusConflict, "conflict", err.Error())
	default:
		s.Log.Error("request failed", "error", err.Error())
		fail(w, http.StatusInternalServerError, "internal", "internal error")
	}
}

func decode(w http.ResponseWriter, r *http.Request, limit int64, into any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	if err := json.NewDecoder(r.Body).Decode(into); err != nil {
		fail(w, http.StatusBadRequest, "bad_request", "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

/* --------------------------------- catalogs -------------------------------- */

func (s *Server) listCatalogs(w http.ResponseWriter, r *http.Request) {
	out, err := s.Store.ListCatalogs(r.Context(), tenant(r))
	if err != nil {
		s.failErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"catalogs": out})
}

func (s *Server) getCatalog(w http.ResponseWriter, r *http.Request) {
	c, err := s.Store.GetCatalog(r.Context(), tenant(r), chi.URLParam(r, "id"))
	if err != nil {
		s.failErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) deleteCatalog(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.DeleteCatalog(r.Context(), tenant(r), chi.URLParam(r, "id")); err != nil {
		s.failErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type catalogRequest struct {
	Name    string `json:"name"`
	BaseURL string `json:"baseUrl"`
	Source  struct {
		Type    string            `json:"type"`
		URL     string            `json:"url"`
		Spec    json.RawMessage   `json:"spec"`
		Headers map[string]string `json:"headers"`
	} `json:"source"`
}

func validURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

func (s *Server) createCatalog(w http.ResponseWriter, r *http.Request) {
	var req catalogRequest
	if !decode(w, r, maxCatalogBody, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len(req.Name) > 80 {
		fail(w, http.StatusBadRequest, "bad_request", "name is required (at most 80 characters)")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	c := &store.Catalog{ID: store.NewID("cat"), Tenant: tenant(r), Name: req.Name, Slug: catalog.Slug(req.Name), Kind: req.Source.Type}
	var headers map[string]string
	switch req.Source.Type {
	case "openapi":
		raw := []byte(req.Source.Spec)
		if len(raw) == 0 {
			if !validURL(req.Source.URL) {
				fail(w, http.StatusBadRequest, "bad_request", "source needs spec, or a valid http(s) url")
				return
			}
			var err error
			if raw, err = fetchSpec(ctx, req.Source.URL); err != nil {
				fail(w, http.StatusUnprocessableEntity, "import_failed", err.Error())
				return
			}
		}
		api, err := catalog.ParseOpenAPI(raw, c.Slug, req.Source.URL, req.BaseURL)
		if err != nil {
			fail(w, http.StatusUnprocessableEntity, "import_failed", err.Error())
			return
		}
		c.BaseURL, c.Nodes = api.BaseURL, api.Nodes
	case "mcp":
		if !validURL(req.Source.URL) {
			fail(w, http.StatusBadRequest, "bad_request", "source.url must be a valid http(s) url")
			return
		}
		headers = req.Source.Headers
		mc, err := catalog.NewMCPClient(ctx, req.Source.URL, headers, 30*time.Second)
		if err != nil {
			fail(w, http.StatusUnprocessableEntity, "import_failed", err.Error())
			return
		}
		tools, err := mc.ListTools(ctx)
		if err != nil {
			fail(w, http.StatusUnprocessableEntity, "import_failed", err.Error())
			return
		}
		if len(tools) == 0 {
			fail(w, http.StatusUnprocessableEntity, "import_failed", "the MCP server advertises no tools")
			return
		}
		c.BaseURL, c.Nodes = req.Source.URL, catalog.MCPNodes(c.Slug, tools)
	default:
		fail(w, http.StatusBadRequest, "bad_request", `source.type must be "openapi" or "mcp"`)
		return
	}
	if err := s.Store.CreateCatalog(ctx, c, headers); err != nil {
		if errors.Is(err, store.ErrConflict) {
			fail(w, http.StatusConflict, "conflict", "a catalog with this name already exists")
			return
		}
		s.failErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

func fetchSpec(ctx context.Context, target string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	resp, err := netguard.Client(30*time.Second, true).Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not fetch spec: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching spec returned HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxSpecBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxSpecBytes {
		return nil, errors.New("spec is larger than 16 MB")
	}
	return raw, nil
}

func (s *Server) putCredentials(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Headers map[string]string `json:"headers"`
	}
	if !decode(w, r, maxJSONBody, &req) {
		return
	}
	for name := range req.Headers {
		if !validHeaderName(name) {
			fail(w, http.StatusBadRequest, "bad_request", "invalid header name: "+name)
			return
		}
	}
	id := chi.URLParam(r, "id")
	if err := s.Store.SetCredentials(r.Context(), tenant(r), id, req.Headers); err != nil {
		s.failErr(w, err)
		return
	}
	c, err := s.Store.GetCatalog(r.Context(), tenant(r), id)
	if err != nil {
		s.failErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for _, c := range name {
		if !(c == '-' || c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
			return false
		}
	}
	return true
}

/* ---------------------------------- compile -------------------------------- */

type compileRequest struct {
	Prompt     string   `json:"prompt"`
	CatalogIDs []string `json:"catalogIds"`
	Name       string   `json:"name"`
}

type stageTrace struct {
	Stage      string `json:"stage"`
	Status     string `json:"status"`
	DurationMs int64  `json:"durationMs"`
	Detail     string `json:"detail"`
}

type trace struct {
	DurationMs int64             `json:"durationMs"`
	Stages     []stageTrace      `json:"stages"`
	Repairs    int               `json:"repairs"`
	Models     map[string]string `json:"models"`
}

type recorder struct {
	mu      sync.Mutex
	started map[string]time.Time
	begin   time.Time
	t       trace
	emit    func(compiler.StageEvent)
}

func newRecorder(emit func(compiler.StageEvent)) *recorder {
	mapName, reduce := compiler.Models()
	return &recorder{started: map[string]time.Time{}, begin: time.Now(), emit: emit,
		t: trace{Stages: []stageTrace{}, Models: map[string]string{"map": mapName, "reduce": reduce}}}
}

func (rec *recorder) on(ev compiler.StageEvent) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if ev.Status == "start" {
		rec.started[ev.Stage] = time.Now()
		if ev.Stage == "repair" {
			rec.t.Repairs++
		}
	} else {
		var ms int64
		if at, ok := rec.started[ev.Stage]; ok {
			ms = time.Since(at).Milliseconds()
		}
		rec.t.Stages = append(rec.t.Stages, stageTrace{Stage: ev.Stage, Status: ev.Status, DurationMs: ms, Detail: ev.Detail})
	}
	if rec.emit != nil {
		rec.emit(ev)
	}
}

func (rec *recorder) done() trace {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.t.DurationMs = time.Since(rec.begin).Milliseconds()
	return rec.t
}

// prepareCompile validates a compile request and writes any failure.
func (s *Server) prepareCompile(w http.ResponseWriter, r *http.Request) (*compileRequest, *compiler.Catalog, bool) {
	var req compileRequest
	if !decode(w, r, maxJSONBody, &req) {
		return nil, nil, false
	}
	req.Prompt = strings.TrimSpace(req.Prompt)
	if req.Prompt == "" || len(req.Prompt) > maxPrompt {
		fail(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("prompt is required (at most %d characters)", maxPrompt))
		return nil, nil, false
	}
	if !s.limiter.allow(tenant(r)) {
		fail(w, http.StatusTooManyRequests, "rate_limited", "too many compiles; try again in a minute")
		return nil, nil, false
	}
	cat, ok := s.plannerCatalog(w, r, req.CatalogIDs)
	return &req, cat, ok
}

func (s *Server) plannerCatalog(w http.ResponseWriter, r *http.Request, ids []string) (*compiler.Catalog, bool) {
	cats, err := s.Store.GetCatalogs(r.Context(), tenant(r), ids)
	if err != nil {
		s.failErr(w, err)
		return nil, false
	}
	return store.BuildPlannerCatalog(cats), true
}

func (s *Server) compile(w http.ResponseWriter, r *http.Request) {
	req, cat, ok := s.prepareCompile(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), compileTimeout)
	defer cancel()
	rec := newRecorder(nil)
	plan, err := s.Compile(compiler.WithProgress(ctx, rec.on), cat, req.Prompt, req.Name)
	if err != nil {
		fail(w, http.StatusUnprocessableEntity, "compile_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"plan": plan, "trace": rec.done()})
}

func (s *Server) compileStream(w http.ResponseWriter, r *http.Request) {
	req, cat, ok := s.prepareCompile(w, r)
	if !ok {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		fail(w, http.StatusInternalServerError, "internal", "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	var mu sync.Mutex
	send := func(event string, v any) {
		raw, err := json.Marshal(v)
		if err != nil {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, raw)
		flusher.Flush()
	}
	ctx, cancel := context.WithTimeout(r.Context(), compileTimeout)
	defer cancel()

	// Heartbeat comments keep proxies from closing a quiet stream.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				mu.Lock()
				fmt.Fprint(w, ": keepalive\n\n")
				flusher.Flush()
				mu.Unlock()
			}
		}
	}()

	rec := newRecorder(func(ev compiler.StageEvent) { send("stage", ev) })
	plan, err := s.Compile(compiler.WithProgress(ctx, rec.on), cat, req.Prompt, req.Name)
	if err != nil {
		send("error", map[string]string{"message": err.Error()})
		return
	}
	send("result", map[string]any{"plan": plan, "trace": rec.done()})
}

type dag struct {
	Nodes        []compiler.GraphNode `json:"nodes"`
	DefaultInput map[string]any       `json:"defaultInput"`
}

func (s *Server) problems(r *http.Request, w http.ResponseWriter, d dag, input map[string]any, ids []string) ([]string, bool) {
	if len(d.Nodes) > maxNodes {
		return []string{fmt.Sprintf("graph has %d nodes, more than the limit of %d", len(d.Nodes), maxNodes)}, true
	}
	cat, ok := s.plannerCatalog(w, r, ids)
	if !ok {
		return nil, false
	}
	keys := map[string]bool{}
	for k := range d.DefaultInput {
		keys[k] = true
	}
	for k := range input {
		keys[k] = true
	}
	return compiler.Validate(d.Nodes, cat, keys), true
}

func (s *Server) validate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Dag        dag      `json:"dag"`
		CatalogIDs []string `json:"catalogIds"`
	}
	if !decode(w, r, maxJSONBody, &req) {
		return
	}
	problems, ok := s.problems(r, w, req.Dag, nil, req.CatalogIDs)
	if !ok {
		return
	}
	if problems == nil {
		problems = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"valid": len(problems) == 0, "problems": problems})
}

func merged(defaults, input map[string]any) map[string]any {
	out := make(map[string]any, len(defaults)+len(input))
	for k, v := range defaults {
		out[k] = v
	}
	for k, v := range input {
		out[k] = v
	}
	return out
}

func invalidDag(w http.ResponseWriter, problems []string) {
	writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": map[string]any{
		"code": "invalid_dag", "message": "the DAG failed validation", "problems": problems,
	}})
}

/* ------------------------------------ runs --------------------------------- */

func (s *Server) createRun(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Dag            dag            `json:"dag"`
		CatalogIDs     []string       `json:"catalogIds"`
		Input          map[string]any `json:"input"`
		IdempotencyKey string         `json:"idempotencyKey"`
	}
	if !decode(w, r, maxJSONBody, &req) {
		return
	}
	problems, ok := s.problems(r, w, req.Dag, req.Input, req.CatalogIDs)
	if !ok {
		return
	}
	if len(problems) > 0 {
		invalidDag(w, problems)
		return
	}
	id := store.NewID("run")
	wid := "apex-run-" + id
	input := merged(req.Dag.DefaultInput, req.Input)
	run, created, err := s.Store.CreateRun(r.Context(), tenant(r), id, wid, req.IdempotencyKey, req.Dag.Nodes, input, req.CatalogIDs)
	if err != nil {
		s.failErr(w, err)
		return
	}
	if !created {
		writeJSON(w, http.StatusOK, run)
		return
	}
	params := runtime.RunParams{RunID: id, Tenant: tenant(r), Nodes: req.Dag.Nodes, Input: input, CatalogIDs: req.CatalogIDs}
	if _, err := s.Temporal.ExecuteWorkflow(r.Context(), client.StartWorkflowOptions{ID: wid, TaskQueue: runtime.TaskQueue}, runtime.RunWorkflow, params); err != nil {
		s.Log.Error("start workflow", "run", id, "error", err.Error())
		_ = s.Store.FinishRun(context.WithoutCancel(r.Context()), id, store.StatusFailed, "could not start the run: execution backend unavailable")
		fail(w, http.StatusServiceUnavailable, "unavailable", "execution backend unavailable")
		return
	}
	writeJSON(w, http.StatusAccepted, run)
}

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v <= 200 {
		limit = v
	}
	out, err := s.Store.ListRuns(r.Context(), tenant(r), limit)
	if err != nil {
		s.failErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": out})
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.Store.GetRun(r.Context(), tenant(r), chi.URLParam(r, "id"))
	if err != nil {
		s.failErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) cancelRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.Store.GetRun(r.Context(), tenant(r), chi.URLParam(r, "id"))
	if err != nil {
		s.failErr(w, err)
		return
	}
	if run.FinishedAt == nil {
		if err := s.Temporal.CancelWorkflow(r.Context(), run.WorkflowID, ""); err != nil {
			s.Log.Error("cancel workflow", "run", run.ID, "error", err.Error())
			fail(w, http.StatusServiceUnavailable, "unavailable", "execution backend unavailable")
			return
		}
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) approve(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Approved *bool  `json:"approved"`
		Note     string `json:"note"`
	}
	if !decode(w, r, maxJSONBody, &req) {
		return
	}
	if req.Approved == nil {
		fail(w, http.StatusBadRequest, "bad_request", "approved is required")
		return
	}
	id, key := chi.URLParam(r, "id"), chi.URLParam(r, "key")
	run, err := s.Store.GetRun(r.Context(), tenant(r), id)
	if err != nil {
		s.failErr(w, err)
		return
	}
	waiting := false
	for _, n := range run.Nodes {
		waiting = waiting || (n.Key == key && n.Status == store.StatusWaiting)
	}
	if !waiting {
		fail(w, http.StatusConflict, "not_waiting", "that node is not waiting for approval")
		return
	}
	sig := runtime.ApprovalSignal{Approved: *req.Approved, Note: req.Note}
	if err := s.Temporal.SignalWorkflow(r.Context(), run.WorkflowID, "", runtime.SignalName(key), sig); err != nil {
		s.Log.Error("signal workflow", "run", id, "error", err.Error())
		fail(w, http.StatusServiceUnavailable, "unavailable", "execution backend unavailable")
		return
	}
	writeJSON(w, http.StatusOK, run)
}

/* --------------------------------- schedules ------------------------------- */

func scheduleID(tenantName, id string) string { return "apex-" + tenantName + "-" + id }

func (s *Server) createSchedule(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Cron       string         `json:"cron"`
		Dag        dag            `json:"dag"`
		CatalogIDs []string       `json:"catalogIds"`
		Input      map[string]any `json:"input"`
	}
	if !decode(w, r, maxJSONBody, &req) {
		return
	}
	req.Cron = strings.TrimSpace(req.Cron)
	if len(strings.Fields(req.Cron)) != 5 {
		fail(w, http.StatusBadRequest, "bad_request", "cron must have five fields, e.g. 0 9 * * 1")
		return
	}
	problems, ok := s.problems(r, w, req.Dag, req.Input, req.CatalogIDs)
	if !ok {
		return
	}
	if len(problems) > 0 {
		invalidDag(w, problems)
		return
	}
	sc := &store.Schedule{ID: store.NewID("sch"), Cron: req.Cron}
	t := tenant(r)
	params := runtime.RunParams{Tenant: t, Nodes: req.Dag.Nodes, Input: merged(req.Dag.DefaultInput, req.Input), CatalogIDs: req.CatalogIDs}
	_, err := s.Temporal.ScheduleClient().Create(r.Context(), client.ScheduleOptions{
		ID:      scheduleID(t, sc.ID),
		Spec:    client.ScheduleSpec{CronExpressions: []string{req.Cron}},
		Overlap: 1, // skip a tick while the previous run is still going
		Action: &client.ScheduleWorkflowAction{
			ID: "apex-sched-" + sc.ID, Workflow: runtime.ScheduledWorkflow, Args: []any{params}, TaskQueue: runtime.TaskQueue,
			RetryPolicy: &temporal.RetryPolicy{MaximumAttempts: 1},
		},
	})
	if err != nil {
		if strings.Contains(err.Error(), "InvalidArgument") || strings.Contains(strings.ToLower(err.Error()), "invalid") {
			fail(w, http.StatusBadRequest, "bad_request", "invalid cron expression")
			return
		}
		s.Log.Error("create schedule", "error", err.Error())
		fail(w, http.StatusServiceUnavailable, "unavailable", "execution backend unavailable")
		return
	}
	if err := s.Store.CreateSchedule(r.Context(), t, sc); err != nil {
		_ = s.Temporal.ScheduleClient().GetHandle(context.WithoutCancel(r.Context()), scheduleID(t, sc.ID)).Delete(context.WithoutCancel(r.Context()))
		s.failErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, sc)
}

func (s *Server) listSchedules(w http.ResponseWriter, r *http.Request) {
	out, err := s.Store.ListSchedules(r.Context(), tenant(r))
	if err != nil {
		s.failErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"schedules": out})
}

func (s *Server) deleteSchedule(w http.ResponseWriter, r *http.Request) {
	id, t := chi.URLParam(r, "id"), tenant(r)
	schedules, err := s.Store.ListSchedules(r.Context(), t)
	if err != nil {
		s.failErr(w, err)
		return
	}
	found := false
	for _, sc := range schedules {
		found = found || sc.ID == id
	}
	if !found {
		s.failErr(w, store.ErrNotFound)
		return
	}
	if err := s.Temporal.ScheduleClient().GetHandle(r.Context(), scheduleID(t, id)).Delete(r.Context()); err != nil {
		s.Log.Error("delete schedule", "error", err.Error())
		fail(w, http.StatusServiceUnavailable, "unavailable", "execution backend unavailable")
		return
	}
	if err := s.Store.DeleteSchedule(r.Context(), t, id); err != nil {
		s.failErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

/* ---------------------------------- limiter -------------------------------- */

type limiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	hits   map[string][]time.Time
}

func newLimiter(max int, window time.Duration) *limiter {
	return &limiter{max: max, window: window, hits: map[string][]time.Time{}}
}

func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := time.Now().Add(-l.window)
	kept := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= l.max {
		l.hits[key] = kept
		return false
	}
	l.hits[key] = append(kept, time.Now())
	return true
}
