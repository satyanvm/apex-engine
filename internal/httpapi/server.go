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
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/satyanvm/apex-engine/internal/catalog"
	"github.com/satyanvm/apex-engine/internal/compiler"
	"github.com/satyanvm/apex-engine/internal/netguard"
	"github.com/satyanvm/apex-engine/internal/oauth"
	"github.com/satyanvm/apex-engine/internal/runtime"
	"github.com/satyanvm/apex-engine/internal/store"
	"github.com/satyanvm/apex-engine/internal/web"
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
	// PlatformKeys act for any tenant. The host application holds one and names
	// the tenant, and optionally the end user and their role, on each request.
	PlatformKeys []string
	Compile      func(ctx context.Context, cat *compiler.Catalog, prompt, name string) (compiler.Plan, error)
	Log          *slog.Logger
	// DemoURL is where the sample tools run; empty turns POST /v1/demo off.
	DemoURL string
	// Console also serves the web inspector at /.
	Console bool
	// PublicURL is the engine's address as a browser sees it. OAuth consent
	// redirects come back to it.
	PublicURL string
	// PiecesURL is the Activepieces runner; empty turns piece tools off.
	PiecesURL, PiecesToken string
	// MetricsToken guards GET /metrics; empty turns it off.
	MetricsToken string
	// RequireApproval is "deletes" or "changes": graphs that do that with no
	// approval step in front are refused, and compiles are told to add one.
	RequireApproval string
	// MaxOpenRuns caps how many unfinished runs one tenant may have, so one
	// customer cannot fill the queue for the rest. Zero means no cap.
	MaxOpenRuns int
	limiter     *limiter
	hooks       *limiter
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
	s.hooks = newLimiter(hookRunsPerMinute, time.Minute)
	r := chi.NewRouter()
	r.Use(recoverer(s.Log))
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	r.Post("/hooks/{token}", s.webhook)
	r.Get("/oauth/callback", s.oauthCallback)
	r.Get("/metrics", s.metrics)
	r.Get("/openapi.yaml", serveOpenAPI)
	if s.Console {
		r.Handle("/*", web.Handler())
	}
	r.Route("/v1", func(r chi.Router) {
		r.Use(s.auth)
		// Anyone with a key may read.
		r.Get("/status", s.status)
		r.Get("/workflows", s.listWorkflows)
		r.Get("/workflows/{id}", s.getWorkflow)
		r.Get("/catalogs", s.listCatalogs)
		r.Get("/catalogs/{id}", s.getCatalog)
		r.Get("/pieces", s.listPieces)
		r.Get("/runs", s.listRuns)
		r.Get("/runs/{id}", s.getRun)
		r.Get("/runs/{id}/nodes/{key}/items", s.listItems)
		r.Get("/runs/{id}/preview", s.runPreview)
		r.Get("/schedules", s.listSchedules)
		r.Post("/validate", s.validate)

		r.Group(func(r chi.Router) { // build and run
			r.Use(s.need(permWrite))
			r.Get("/callback-secret", s.callbackSecret)
			r.Post("/demo", s.installDemo)
			r.Post("/workflows", s.createWorkflow)
			r.Put("/workflows/{id}", s.updateWorkflow)
			r.Delete("/workflows/{id}", s.deleteWorkflow)
			r.Post("/workflows/{id}/runs", s.runWorkflow)
			r.Put("/workflows/{id}/webhook", s.enableWebhook)
			r.Delete("/workflows/{id}/webhook", s.disableWebhook)
			r.Post("/compile", s.compile)
			r.Post("/compile/stream", s.compileStream)
			r.Post("/runs", s.createRun)
			r.Post("/runs/{id}/cancel", s.cancelRun)
			r.Post("/runs/{id}/retry", s.retryRun)
			r.Post("/schedules", s.createSchedule)
			r.Delete("/schedules/{id}", s.deleteSchedule)
		})
		r.With(s.need(permApprove)).Post("/runs/{id}/nodes/{key}/approve", s.approve)
		r.Group(func(r chi.Router) { // connect systems, manage access
			r.Use(s.need(permAdmin))
			r.Post("/catalogs", s.createCatalog)
			r.Delete("/catalogs/{id}", s.deleteCatalog)
			r.Put("/catalogs/{id}/credentials", s.putCredentials)
			r.Post("/catalogs/{id}/oauth/authorize", s.oauthAuthorize)
			r.Get("/keys", s.listKeys)
			r.Post("/keys", s.createKey)
			r.Delete("/keys/{id}", s.revokeKey)
			r.Get("/audit", s.listAudit)
			r.Delete("/tenant", s.deleteTenant)
		})
	})
	return r
}

type tenantKey struct{}

func tenant(r *http.Request) string { return r.Context().Value(tenantKey{}).(string) }

// principal is who a request acts as.
type principal struct{ Actor, Role string }

type principalKey struct{}

func who(r *http.Request) principal {
	p, _ := r.Context().Value(principalKey{}).(principal)
	return p
}

type perm int

const (
	permWrite perm = iota + 1
	permApprove
	permAdmin
)

func (p perm) String() string {
	return map[perm]string{permWrite: "editor, operator or admin", permApprove: "approver, operator or admin", permAdmin: "admin"}[p]
}

func allowed(role string, p perm) bool {
	switch p {
	case permWrite:
		return role == store.RoleEditor || role == store.RoleOperator || role == store.RoleAdmin
	case permApprove:
		return role == store.RoleApprover || role == store.RoleOperator || role == store.RoleAdmin
	case permAdmin:
		return role == store.RoleAdmin
	}
	return false
}

// need rejects a request whose key lacks a permission.
func (s *Server) need(p perm) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if role := who(r).Role; !allowed(role, p) {
				fail(w, http.StatusForbidden, "forbidden", fmt.Sprintf("your role is %s; this needs %s", role, p))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// auth resolves the bearer key. Keys from configuration are administrators of
// their tenant; keys made through the API carry the role they were given.
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		given, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if ok && given != "" {
			for _, key := range s.PlatformKeys {
				if key == "" || subtle.ConstantTimeCompare([]byte(given), []byte(key)) != 1 {
					continue
				}
				p, t, problem := platformPrincipal(r)
				if problem != "" {
					fail(w, http.StatusBadRequest, "bad_request", problem)
					return
				}
				next.ServeHTTP(w, withPrincipal(r, t, p))
				return
			}
			for key, t := range s.Keys { // constant time per key
				if subtle.ConstantTimeCompare([]byte(given), []byte(key)) == 1 {
					next.ServeHTTP(w, withPrincipal(r, t, principal{Actor: "root", Role: store.RoleAdmin}))
					return
				}
			}
			if strings.HasPrefix(given, "apx_") {
				t, name, role, err := s.Store.LookupAPIKey(r.Context(), given)
				if err == nil {
					next.ServeHTTP(w, withPrincipal(r, t, principal{Actor: name, Role: role}))
					return
				}
				if !errors.Is(err, store.ErrNotFound) {
					s.failErr(w, err)
					return
				}
			}
		}
		fail(w, http.StatusUnauthorized, "unauthorized", "missing or invalid API key")
	})
}

// Headers a platform key uses to say whom a request is for.
const (
	HeaderTenant = "X-Apex-Tenant"
	HeaderActor  = "X-Apex-Actor"
	HeaderRole   = "X-Apex-Role"
)

var tenantName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,63}$`)

// ValidTenant reports whether a name can be a tenant.
func ValidTenant(name string) bool { return tenantName.MatchString(name) }

// platformPrincipal reads whom a platform key is acting for. A tenant needs no
// setup: the first request that names it creates it.
func platformPrincipal(r *http.Request) (principal, string, string) {
	t := r.Header.Get(HeaderTenant)
	if !ValidTenant(t) {
		return principal{}, "", "a platform key must name the tenant in " + HeaderTenant +
			": 1 to 64 letters, digits or . _ : - starting with a letter or digit"
	}
	actor := strings.TrimSpace(r.Header.Get(HeaderActor))
	if actor == "" {
		actor = "platform"
	}
	if len(actor) > 120 || strings.ContainsAny(actor, "\r\n") {
		return principal{}, "", HeaderActor + " must be one line of at most 120 characters"
	}
	role := r.Header.Get(HeaderRole)
	if role == "" {
		role = store.RoleAdmin
	}
	if !store.ValidRole(role) {
		return principal{}, "", HeaderRole + " must be viewer, approver, editor, operator or admin"
	}
	return principal{Actor: actor, Role: role}, t, ""
}

func withPrincipal(r *http.Request, tenantName string, p principal) *http.Request {
	ctx := context.WithValue(r.Context(), tenantKey{}, tenantName)
	return r.WithContext(context.WithValue(ctx, principalKey{}, p))
}

// audit records what a request did. A failure to record is logged loudly but
// does not undo the action, which has already happened.
func (s *Server) audit(r *http.Request, action, target string, detail map[string]any) {
	if err := s.Store.Audit(context.WithoutCancel(r.Context()), tenant(r), who(r).Actor, action, target, detail); err != nil {
		s.Log.Error("AUDIT WRITE FAILED", "action", action, "target", target, "actor", who(r).Actor, "error", err.Error())
	}
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
	s.audit(r, "catalog.delete", chi.URLParam(r, "id"), nil)
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
		// Piece is an Activepieces piece name such as "slack", and Auth the
		// connection value that piece expects.
		Piece string          `json:"piece"`
		Auth  json.RawMessage `json:"auth"`
	} `json:"source"`
}

func validURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// apiError is a failure a handler reports as is.
type apiError struct {
	status    int
	code, msg string
}

func (s *Server) createCatalog(w http.ResponseWriter, r *http.Request) {
	var req catalogRequest
	if !decode(w, r, maxCatalogBody, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	c, apiErr, err := s.importCatalog(ctx, tenant(r), req)
	if apiErr != nil {
		fail(w, apiErr.status, apiErr.code, apiErr.msg)
		return
	}
	if err != nil {
		s.failErr(w, err)
		return
	}
	s.audit(r, "catalog.create", c.ID, map[string]any{"name": c.Name, "kind": c.Kind, "tools": c.NodeCount})
	writeJSON(w, http.StatusCreated, c)
}

// importCatalog fetches, parses and stores a tool source.
func (s *Server) importCatalog(ctx context.Context, tenantName string, req catalogRequest) (*store.Catalog, *apiError, error) {
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len(req.Name) > 80 {
		return nil, &apiError{http.StatusBadRequest, "bad_request", "name is required (at most 80 characters)"}, nil
	}
	importFailed := func(msg string) (*store.Catalog, *apiError, error) {
		return nil, &apiError{http.StatusUnprocessableEntity, "import_failed", msg}, nil
	}
	c := &store.Catalog{ID: store.NewID("cat"), Tenant: tenantName, Name: req.Name, Slug: catalog.Slug(req.Name), Kind: req.Source.Type}
	creds := &store.Credentials{}
	switch req.Source.Type {
	case "openapi":
		raw := []byte(req.Source.Spec)
		var text string // a spec pasted as a string may be YAML
		if json.Unmarshal(raw, &text) == nil {
			raw = []byte(text)
		}
		if len(raw) == 0 {
			if !validURL(req.Source.URL) {
				return nil, &apiError{http.StatusBadRequest, "bad_request", "source needs spec, or a valid http(s) url"}, nil
			}
			var err error
			if raw, err = fetchSpec(ctx, req.Source.URL); err != nil {
				return importFailed(err.Error())
			}
		}
		api, err := catalog.ParseOpenAPI(raw, c.Slug, req.Source.URL, req.BaseURL)
		if err != nil {
			return importFailed(err.Error())
		}
		c.BaseURL, c.Nodes = api.BaseURL, api.Nodes
	case "mcp":
		if !validURL(req.Source.URL) {
			return nil, &apiError{http.StatusBadRequest, "bad_request", "source.url must be a valid http(s) url"}, nil
		}
		creds.Headers = req.Source.Headers
		mc, err := catalog.NewMCPClient(ctx, req.Source.URL, creds.Headers, 30*time.Second)
		if err != nil {
			return importFailed(err.Error())
		}
		tools, err := mc.ListTools(ctx)
		if err != nil {
			return importFailed(err.Error())
		}
		if len(tools) == 0 {
			return importFailed("the MCP server advertises no tools")
		}
		c.BaseURL, c.Nodes = req.Source.URL, catalog.MCPNodes(c.Slug, tools)
	case "piece":
		if s.PiecesURL == "" {
			return nil, &apiError{http.StatusServiceUnavailable, "pieces_unavailable", "this engine has no pieces runner; set APEX_PIECES_URL"}, nil
		}
		meta, err := catalog.FetchPiece(ctx, s.PiecesURL, s.PiecesToken, req.Source.Piece)
		if err != nil {
			return importFailed(err.Error())
		}
		if len(meta.Actions) == 0 {
			return importFailed("the piece " + meta.Name + " has no actions")
		}
		c.BaseURL, c.Nodes = "piece:"+meta.Name+"@"+meta.Version, catalog.PieceNodes(c.Slug, meta)
		if raw := strings.TrimSpace(string(req.Source.Auth)); raw != "" && raw != "null" {
			creds.Piece = req.Source.Auth
		}
	default:
		return nil, &apiError{http.StatusBadRequest, "bad_request", `source.type must be "openapi", "mcp" or "piece"`}, nil
	}
	if err := s.Store.CreateCatalog(ctx, c, creds); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return nil, &apiError{http.StatusConflict, "conflict", "a catalog with this name already exists"}, nil
		}
		return nil, nil, err
	}
	return c, nil, nil
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

// oauthRequest is an OAuth 2.0 client as a caller describes it.
type oauthRequest struct {
	Grant        string            `json:"grant"`
	TokenURL     string            `json:"tokenUrl"`
	AuthURL      string            `json:"authUrl"`
	ClientID     string            `json:"clientId"`
	ClientSecret string            `json:"clientSecret"`
	Scope        string            `json:"scope"`
	Audience     string            `json:"audience"`
	RefreshToken string            `json:"refreshToken"`
	ClientAuth   string            `json:"clientAuth"`
	AuthParams   map[string]string `json:"authParams"`
}

func (o *oauthRequest) config() (*store.OAuth, string) {
	if !validURL(o.TokenURL) {
		return nil, "oauth.tokenUrl must be a valid http(s) url"
	}
	if strings.TrimSpace(o.ClientID) == "" {
		return nil, "oauth.clientId is required"
	}
	if o.ClientAuth != "" && o.ClientAuth != "body" && o.ClientAuth != "basic" {
		return nil, `oauth.clientAuth must be "body" or "basic"`
	}
	return &store.OAuth{Grant: o.Grant, TokenURL: o.TokenURL, AuthURL: o.AuthURL, ClientID: o.ClientID, ClientSecret: o.ClientSecret,
		Scope: o.Scope, Audience: o.Audience, RefreshToken: o.RefreshToken, ClientAuth: o.ClientAuth, AuthParams: o.AuthParams}, ""
}

// putCredentials replaces how the engine authenticates to a tool source:
// headers, query parameters, an OAuth client, or a piece connection.
func (s *Server) putCredentials(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Headers map[string]string `json:"headers"`
		Query   map[string]string `json:"query"`
		OAuth   *oauthRequest     `json:"oauth"`
		Piece   json.RawMessage   `json:"piece"`
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
	creds := &store.Credentials{Headers: req.Headers, Query: req.Query}
	if raw := strings.TrimSpace(string(req.Piece)); raw != "" && raw != "null" {
		creds.Piece = req.Piece
	}
	if req.OAuth != nil {
		if req.OAuth.Grant != store.GrantClientCredentials && req.OAuth.Grant != store.GrantRefreshToken {
			fail(w, http.StatusBadRequest, "bad_request", `oauth.grant must be "client_credentials" or "refresh_token"; for a consent screen use POST /v1/catalogs/{id}/oauth/authorize`)
			return
		}
		o, problem := req.OAuth.config()
		if problem == "" && o.Grant == store.GrantRefreshToken && o.RefreshToken == "" {
			problem = "oauth.refreshToken is required for the refresh_token grant"
		}
		if problem != "" {
			fail(w, http.StatusBadRequest, "bad_request", problem)
			return
		}
		// Ask for a token now, so wrong settings are reported here and not by
		// the first run that needs them.
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		if err := oauth.Fetch(ctx, o); err != nil {
			fail(w, http.StatusUnprocessableEntity, "oauth_failed", err.Error())
			return
		}
		creds.OAuth = o
	}
	id := chi.URLParam(r, "id")
	if err := s.Store.SetCredentials(r.Context(), tenant(r), id, creds); err != nil {
		s.failErr(w, err)
		return
	}
	c, err := s.Store.GetCatalog(r.Context(), tenant(r), id)
	if err != nil {
		s.failErr(w, err)
		return
	}
	s.audit(r, "catalog.credentials", id, map[string]any{"auth": c.Auth, "headers": c.CredentialHeaders})
	c.Nodes = nil
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
	// Base, when set, makes the prompt a change to this graph: everything the
	// prompt does not mention is kept.
	Base *struct {
		Name         string               `json:"name"`
		Description  string               `json:"description"`
		Nodes        []compiler.GraphNode `json:"nodes"`
		DefaultInput map[string]any       `json:"defaultInput"`
	} `json:"base"`
}

// compileContext carries the base graph into the compiler when the request is
// a change, and reports whether it is one.
func (req *compileRequest) compileContext(ctx context.Context) (context.Context, bool) {
	if req.Base == nil || len(req.Base.Nodes) == 0 {
		return ctx, false
	}
	return compiler.WithBase(ctx, compiler.Plan{Name: req.Base.Name, Description: req.Base.Description,
		Nodes: req.Base.Nodes, DefaultInput: req.Base.DefaultInput}), true
}

// compiled is the body of a successful compile.
func (req *compileRequest) compiled(plan compiler.Plan, trace any, cat *compiler.Catalog) map[string]any {
	out := map[string]any{"plan": plan, "trace": trace, "review": compiler.ReviewGraph(plan.Nodes, cat)}
	if req.Base != nil && len(req.Base.Nodes) > 0 {
		out["diff"] = compiler.DiffGraphs(req.Base.Nodes, plan.Nodes)
	}
	return out
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
	if req.Base != nil && len(req.Base.Nodes) > maxNodes {
		fail(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("base has more than %d nodes", maxNodes))
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
	cat := store.BuildPlannerCatalog(cats)
	cat.RequireApproval = s.RequireApproval
	return cat, true
}

func (s *Server) compile(w http.ResponseWriter, r *http.Request) {
	req, cat, ok := s.prepareCompile(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), compileTimeout)
	defer cancel()
	rec := newRecorder(nil)
	ctx, _ = req.compileContext(ctx)
	plan, err := s.Compile(compiler.WithProgress(ctx, rec.on), cat, req.Prompt, req.Name)
	s.countCompile(r, err, rec)
	if err != nil {
		fail(w, http.StatusUnprocessableEntity, "compile_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, req.compiled(plan, rec.done(), cat))
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
	ctx, _ = req.compileContext(ctx)
	plan, err := s.Compile(compiler.WithProgress(ctx, rec.on), cat, req.Prompt, req.Name)
	s.countCompile(r, err, rec)
	if err != nil {
		send("error", map[string]string{"message": err.Error()})
		return
	}
	send("result", req.compiled(plan, rec.done(), cat))
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
	out := map[string]any{"valid": len(problems) == 0, "problems": problems}
	if len(problems) == 0 {
		// A valid graph also gets its plain-language description, so a host can
		// show one for a graph it stored or edited without compiling again.
		if cat, ok := s.plannerCatalog(w, r, req.CatalogIDs); ok {
			out["review"] = compiler.ReviewGraph(req.Dag.Nodes, cat)
		} else {
			return
		}
	}
	writeJSON(w, http.StatusOK, out)
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
		Name           string         `json:"name"`
		CallbackURL    string         `json:"callbackUrl"`
		// Preview does every read and records every write instead of sending it.
		Preview bool `json:"preview"`
	}
	if !decode(w, r, maxJSONBody, &req) {
		return
	}
	run, code, ok := s.launch(w, r, req.Dag, req.CatalogIDs, req.Input, req.IdempotencyKey, store.RunMeta{Name: req.Name, CallbackURL: req.CallbackURL, Preview: req.Preview})
	if ok {
		writeJSON(w, code, run)
	}
}

// launch validates a graph and starts it. On failure it has already written the
// response and returns ok=false.
func (s *Server) launch(w http.ResponseWriter, r *http.Request, d dag, catalogIDs []string, runInput map[string]any, idemKey string, meta store.RunMeta) (*store.Run, int, bool) {
	if !validCallback(w, meta.CallbackURL) {
		return nil, 0, false
	}
	problems, ok := s.problems(r, w, d, runInput, catalogIDs)
	if !ok {
		return nil, 0, false
	}
	if len(problems) > 0 {
		invalidDag(w, problems)
		return nil, 0, false
	}
	if s.MaxOpenRuns > 0 {
		open, err := s.Store.OpenRunCount(r.Context(), tenant(r))
		if err != nil {
			s.failErr(w, err)
			return nil, 0, false
		}
		if open >= s.MaxOpenRuns {
			w.Header().Set("Retry-After", "30")
			fail(w, http.StatusTooManyRequests, "too_many_runs", fmt.Sprintf(
				"this tenant already has %d unfinished runs, the most allowed; wait for some to finish, or cancel runs that are waiting for an approval nobody will give", open))
			return nil, 0, false
		}
	}
	id := store.NewID("run")
	wid := "apex-run-" + id
	input := merged(d.DefaultInput, runInput)
	if meta.StartedBy == "" {
		meta.StartedBy = who(r).Actor
	}
	run, created, err := s.Store.CreateRun(r.Context(), tenant(r), id, wid, idemKey, d.Nodes, input, catalogIDs, meta)
	if err != nil {
		s.failErr(w, err)
		return nil, 0, false
	}
	if !created {
		return run, http.StatusOK, true
	}
	detail := map[string]any{"source": run.Source, "name": meta.Name}
	if meta.Preview {
		detail["preview"] = true
	}
	if meta.SavedWorkflow != "" {
		detail["workflowId"] = meta.SavedWorkflow
	}
	if meta.ResumeOf != "" {
		detail["resumeOf"] = meta.ResumeOf
	}
	s.audit(r, "run.start", id, detail)
	params := runtime.RunParams{RunID: id, Tenant: tenant(r), Nodes: d.Nodes, Input: input, CatalogIDs: catalogIDs, Name: meta.Name, WorkflowRef: meta.SavedWorkflow,
		CallbackURL: meta.CallbackURL, ResumeOf: meta.ResumeOf, IdemRoot: meta.IdemRoot, Preview: meta.Preview}
	if _, err := s.Temporal.ExecuteWorkflow(r.Context(), client.StartWorkflowOptions{ID: wid, TaskQueue: runtime.TaskQueue}, runtime.RunWorkflow, params); err != nil {
		s.Log.Error("start workflow", "run", id, "error", err.Error())
		_ = s.Store.FinishRun(context.WithoutCancel(r.Context()), id, store.StatusFailed, "could not start the run: execution backend unavailable")
		fail(w, http.StatusServiceUnavailable, "unavailable", "execution backend unavailable")
		return nil, 0, false
	}
	return run, http.StatusAccepted, true
}

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v <= 200 {
		limit = v
	}
	q := r.URL.Query()
	out, err := s.Store.ListRuns(r.Context(), tenant(r), limit, q.Get("status"), q.Get("workflowId"))
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
		s.audit(r, "run.cancel", run.ID, nil)
	}
	writeJSON(w, http.StatusOK, run)
}

// retryRun continues a failed or cancelled run as a new run. Steps and loop
// items that succeeded are not executed again; their results are carried over.
// Idempotency keys keep the first run's root, so a call that was in flight when
// it failed is sent with the same key.
func (s *Server) retryRun(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CallbackURL string `json:"callbackUrl"`
	}
	if !decodeOptional(w, r, &req) {
		return
	}
	prior, err := s.Store.GetRun(r.Context(), tenant(r), chi.URLParam(r, "id"))
	if err != nil {
		s.failErr(w, err)
		return
	}
	if prior.FinishedAt == nil || (prior.Status != store.StatusFailed && prior.Status != store.RunCancelled) {
		fail(w, http.StatusConflict, "not_resumable", "only a failed or cancelled run can be retried; this one is "+prior.Status)
		return
	}
	root := prior.ID
	if prior.IdemRoot != nil && *prior.IdemRoot != "" {
		root = *prior.IdemRoot
	}
	// A preview stays a preview: continuing one must never start sending writes.
	meta := store.RunMeta{Name: prior.Name, Source: "retry", CallbackURL: req.CallbackURL, ResumeOf: prior.ID, IdemRoot: root, Preview: prior.Preview}
	if prior.SavedWorkflow != nil {
		meta.SavedWorkflow = *prior.SavedWorkflow
	}
	// One resume per failed run: asking twice returns the same new run.
	run, code, ok := s.launch(w, r, dag{Nodes: prior.Dag}, prior.CatalogIDs, prior.Input, "retry:"+prior.ID, meta)
	if ok {
		writeJSON(w, code, run)
	}
}

// runPreview lists what a preview run would have done: each write it did not
// send, each question it would have asked and each wait it skipped.
func (s *Server) runPreview(w http.ResponseWriter, r *http.Request) {
	run, err := s.Store.GetRun(r.Context(), tenant(r), chi.URLParam(r, "id"))
	if err != nil {
		s.failErr(w, err)
		return
	}
	if !run.Preview {
		fail(w, http.StatusConflict, "not_a_preview", "this run was not a preview; start one with \"preview\": true")
		return
	}
	const most = 2000
	actions, err := s.Store.PlannedActions(r.Context(), tenant(r), run.ID, most+1)
	if err != nil {
		s.failErr(w, err)
		return
	}
	truncated := len(actions) > most
	if truncated {
		actions = actions[:most]
	}
	orderForReading(actions, run.Dag)
	counts := map[string]int{"call": 0, "approval": 0, "wait": 0, "unknown": 0}
	for _, a := range actions {
		counts[a.Kind]++
	}
	writeJSON(w, http.StatusOK, map[string]any{"runId": run.ID, "status": run.Status, "error": run.Error, "done": run.FinishedAt != nil,
		"actions": actions, "counts": counts, "truncated": truncated})
}

// orderForReading sorts planned actions the way a person reads a run: steps in
// graph order, and inside a loop one item at a time, so the question about an
// item sits next to what would then be done to it.
func orderForReading(actions []store.PlannedAction, nodes []compiler.GraphNode) {
	pos, anchor := map[string]int{}, map[string]int{}
	for i, n := range nodes {
		pos[n.Key] = i
	}
	for _, n := range nodes {
		if n.Type == "logic.loop" {
			for _, k := range compiler.LoopBody(nodes, n.Key) {
				anchor[k] = pos[n.Key]
			}
		}
	}
	key := func(a store.PlannedAction) [3]int {
		if a.Item == nil {
			return [3]int{pos[a.Node], -1, pos[a.Node]}
		}
		at, ok := anchor[a.Node]
		if !ok {
			at = pos[a.Node]
		}
		return [3]int{at, *a.Item, pos[a.Node]}
	}
	sort.SliceStable(actions, func(i, j int) bool {
		a, b := key(actions[i]), key(actions[j])
		for k := range a {
			if a[k] != b[k] {
				return a[k] < b[k]
			}
		}
		return false
	})
}

// decodeOptional reads a JSON body that may be absent.
func decodeOptional(w http.ResponseWriter, r *http.Request, into any) bool {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxJSONBody))
	if err != nil {
		fail(w, http.StatusRequestEntityTooLarge, "too_large", "body is larger than 2 MB")
		return false
	}
	if strings.TrimSpace(string(raw)) == "" {
		return true
	}
	if err := json.Unmarshal(raw, into); err != nil {
		fail(w, http.StatusBadRequest, "bad_request", "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

// listItems returns the loop items of one node of a run.
func (s *Server) listItems(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, offset := 100, 0
	if v, err := strconv.Atoi(q.Get("limit")); err == nil && v > 0 && v <= 500 {
		limit = v
	}
	if v, err := strconv.Atoi(q.Get("offset")); err == nil && v > 0 {
		offset = v
	}
	if _, err := s.Store.GetRun(r.Context(), tenant(r), chi.URLParam(r, "id")); err != nil {
		s.failErr(w, err)
		return
	}
	items, err := s.Store.ListItems(r.Context(), tenant(r), chi.URLParam(r, "id"), chi.URLParam(r, "key"), q.Get("status"), limit, offset)
	if err != nil {
		s.failErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
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
	waiting, token := false, ""
	for _, n := range run.Nodes {
		if n.Key == key && n.Status == store.StatusWaiting {
			waiting = true
			if out, ok := n.Output.(map[string]any); ok {
				token, _ = out["token"].(string)
			}
		}
	}
	if !waiting {
		fail(w, http.StatusConflict, "not_waiting", "that node is not waiting for approval")
		return
	}
	// The token names the one wait this decision answers. If the request is
	// repeated, the copy carries the same token and the run ignores it.
	sig := runtime.ApprovalSignal{Approved: *req.Approved, Note: req.Note, Token: token, By: who(r).Actor}
	if err := s.Temporal.SignalWorkflow(r.Context(), run.WorkflowID, "", runtime.SignalName(key), sig); err != nil {
		s.Log.Error("signal workflow", "run", id, "error", err.Error())
		fail(w, http.StatusServiceUnavailable, "unavailable", "execution backend unavailable")
		return
	}
	action := "run.reject"
	if *req.Approved {
		action = "run.approve"
	}
	s.audit(r, action, id, map[string]any{"node": key, "wait": token, "note": req.Note})
	writeJSON(w, http.StatusOK, run)
}

/* --------------------------------- schedules ------------------------------- */

func scheduleID(tenantName, id string) string { return "apex-" + tenantName + "-" + id }

func (s *Server) createSchedule(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Cron        string         `json:"cron"`
		Dag         dag            `json:"dag"`
		CatalogIDs  []string       `json:"catalogIds"`
		Input       map[string]any `json:"input"`
		WorkflowID  string         `json:"workflowId"`
		CallbackURL string         `json:"callbackUrl"`
	}
	if !decode(w, r, maxJSONBody, &req) || !validCallback(w, req.CallbackURL) {
		return
	}
	req.Cron = strings.TrimSpace(req.Cron)
	if len(strings.Fields(req.Cron)) != 5 {
		fail(w, http.StatusBadRequest, "bad_request", "cron must have five fields, e.g. 0 9 * * 1")
		return
	}
	var saved *store.Workflow
	if req.WorkflowID != "" {
		wf, err := s.Store.GetWorkflow(r.Context(), tenant(r), req.WorkflowID)
		if err != nil {
			s.failErr(w, err)
			return
		}
		saved = wf
		req.Dag, req.CatalogIDs = dag{Nodes: wf.Nodes, DefaultInput: wf.DefaultInput}, wf.CatalogIDs
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
	params := runtime.RunParams{Tenant: t, Nodes: req.Dag.Nodes, Input: merged(req.Dag.DefaultInput, req.Input), CatalogIDs: req.CatalogIDs, CallbackURL: req.CallbackURL}
	if saved != nil {
		// The tick loads the workflow, so a later edit reaches the schedule.
		sc.WorkflowID = &saved.ID
		params = runtime.RunParams{Tenant: t, Input: req.Input, WorkflowRef: saved.ID, Name: saved.Name}
	}
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
	s.audit(r, "schedule.create", sc.ID, map[string]any{"cron": sc.Cron, "workflowId": req.WorkflowID})
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
	s.audit(r, "schedule.delete", id, nil)
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
