package httpapi

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"go.temporal.io/api/serviceerror"
	"html"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/satyanvm/apex-engine/internal/catalog"
	"github.com/satyanvm/apex-engine/internal/oauth"
	"github.com/satyanvm/apex-engine/internal/store"
)

/* ---------------------------------- API keys -------------------------------- */

func (s *Server) listKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.Store.ListAPIKeys(r.Context(), tenant(r))
	if err != nil {
		s.failErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": keys})
}

// createKey makes a key for a person or a system. The key is in this response
// and nowhere else: only its hash is stored.
func (s *Server) createKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
		Role string `json:"role"`
	}
	if !decode(w, r, maxJSONBody, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len(req.Name) > 80 || req.Name == "root" || req.Name == "webhook" || req.Name == "schedule" {
		fail(w, http.StatusBadRequest, "bad_request", "name is required (at most 80 characters) and cannot be root, webhook or schedule")
		return
	}
	if !store.ValidRole(req.Role) {
		fail(w, http.StatusBadRequest, "bad_request", "role must be viewer, approver, editor, operator or admin")
		return
	}
	k, secret, err := s.Store.CreateAPIKey(r.Context(), tenant(r), req.Name, req.Role, who(r).Actor)
	if err != nil {
		if err == store.ErrConflict {
			fail(w, http.StatusConflict, "conflict", "a key with this name already exists")
			return
		}
		s.failErr(w, err)
		return
	}
	s.audit(r, "key.create", k.ID, map[string]any{"name": k.Name, "role": k.Role})
	writeJSON(w, http.StatusCreated, struct {
		*store.APIKey
		Key string `json:"key"`
	}{k, secret})
}

func (s *Server) revokeKey(w http.ResponseWriter, r *http.Request) {
	k, err := s.Store.RevokeAPIKey(r.Context(), tenant(r), chi.URLParam(r, "id"))
	if err != nil {
		s.failErr(w, err)
		return
	}
	s.audit(r, "key.revoke", k.ID, map[string]any{"name": k.Name})
	writeJSON(w, http.StatusOK, k)
}

/* ---------------------------------- audit log ------------------------------- */

func (s *Server) listAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 100
	if v, err := strconv.Atoi(q.Get("limit")); err == nil && v > 0 && v <= 500 {
		limit = v
	}
	before, _ := strconv.ParseInt(q.Get("before"), 10, 64)
	entries, err := s.Store.ListAudit(r.Context(), tenant(r), limit, before, q.Get("action"), q.Get("actor"), q.Get("target"))
	if err != nil {
		s.failErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

/* ----------------------------------- metrics -------------------------------- */

func (s *Server) countCompile(r *http.Request, err error, rec *recorder) {
	outcome := "ok"
	if err != nil {
		outcome = "failed"
	}
	ctx := context.WithoutCancel(r.Context())
	if berr := s.Store.Bump(ctx, "compiles", tenant(r), outcome, 1); berr != nil {
		s.Log.Warn("count compile", "error", berr.Error())
	}
	_ = s.Store.Bump(ctx, "compile_ms", tenant(r), "", time.Since(rec.begin).Milliseconds())
}

var counterHelp = map[string][2]string{
	"runs_started":  {"apex_runs_started_total", "Runs started, by what started them."},
	"runs_finished": {"apex_runs_finished_total", "Runs finished, by final status."},
	"compiles":      {"apex_compiles_total", "Prompt compiles, by outcome."},
	"compile_ms":    {"apex_compile_milliseconds_total", "Time spent compiling prompts."},
}

var counterLabel = map[string]string{"runs_started": "source", "runs_finished": "status", "compiles": "outcome"}

func promEscape(v string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v)
}

// metrics serves Prometheus text. It covers every tenant, so it has its own
// token rather than accepting a tenant's API key.
func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	if s.MetricsToken == "" {
		fail(w, http.StatusNotFound, "not_found", "metrics are off; set APEX_METRICS_TOKEN")
		return
	}
	given, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if subtle.ConstantTimeCompare([]byte(given), []byte(s.MetricsToken)) != 1 {
		fail(w, http.StatusUnauthorized, "unauthorized", "missing or invalid metrics token")
		return
	}
	counters, err := s.Store.Counters(r.Context())
	if err != nil {
		s.failErr(w, err)
		return
	}
	open, err := s.Store.OpenRuns(r.Context())
	if err != nil {
		s.failErr(w, err)
		return
	}
	waiting, err := s.Store.OldestWaiting(r.Context())
	if err != nil {
		s.failErr(w, err)
		return
	}
	var b strings.Builder
	names := []string{}
	byName := map[string][]store.Counter{}
	for _, c := range counters {
		if _, known := counterHelp[c.Name]; !known {
			continue
		}
		if len(byName[c.Name]) == 0 {
			names = append(names, c.Name)
		}
		byName[c.Name] = append(byName[c.Name], c)
	}
	sort.Strings(names)
	for _, name := range names {
		meta := counterHelp[name]
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s counter\n", meta[0], meta[1], meta[0])
		for _, c := range byName[name] {
			labels := fmt.Sprintf(`tenant="%s"`, promEscape(c.Tenant))
			if l := counterLabel[name]; l != "" {
				labels += fmt.Sprintf(`,%s="%s"`, l, promEscape(c.Label))
			}
			fmt.Fprintf(&b, "%s{%s} %d\n", meta[0], labels, c.Value)
		}
	}
	b.WriteString("# HELP apex_runs_open Runs that have not finished, by status.\n# TYPE apex_runs_open gauge\n")
	for _, g := range open {
		fmt.Fprintf(&b, "apex_runs_open{tenant=\"%s\",status=\"%s\"} %g\n", promEscape(g.Tenant), promEscape(g.Label), g.Value)
	}
	b.WriteString("# HELP apex_oldest_waiting_approval_seconds Age of the oldest run waiting for a person.\n# TYPE apex_oldest_waiting_approval_seconds gauge\n")
	for _, g := range waiting {
		fmt.Fprintf(&b, "apex_oldest_waiting_approval_seconds{tenant=\"%s\"} %g\n", promEscape(g.Tenant), g.Value)
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}

/* -------------------------------- OAuth consent ----------------------------- */

func (s *Server) publicBase(r *http.Request) string {
	if s.PublicURL != "" {
		return strings.TrimSuffix(s.PublicURL, "/")
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// oauthAuthorize starts a consent: it stores the client settings and returns
// the address a person opens to grant access. The provider then sends them to
// /oauth/callback, which stores the tokens.
func (s *Server) oauthAuthorize(w http.ResponseWriter, r *http.Request) {
	var req oauthRequest
	if !decode(w, r, maxJSONBody, &req) {
		return
	}
	req.Grant = store.GrantAuthorizationCode
	o, problem := req.config()
	if problem == "" && !validURL(req.AuthURL) {
		problem = "authUrl must be a valid http(s) url"
	}
	if problem != "" {
		fail(w, http.StatusBadRequest, "bad_request", strings.TrimPrefix(problem, "oauth."))
		return
	}
	o.RedirectURL = s.publicBase(r) + "/oauth/callback"
	id, t := chi.URLParam(r, "id"), tenant(r)
	state := oauth.NewState()
	verifier, challenge := oauth.NewVerifier()
	target, err := oauth.AuthorizeURL(o, state, challenge)
	if err != nil {
		fail(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	// Headers, query parameters and a piece connection the tool already has stay.
	if err := s.Store.UpdateCredentials(r.Context(), t, id, func(c *store.Credentials) error {
		c.OAuth = o
		return nil
	}); err != nil {
		s.failErr(w, err)
		return
	}
	if err := s.Store.SaveOAuthState(r.Context(), state, t, id, verifier, 15*time.Minute); err != nil {
		s.failErr(w, err)
		return
	}
	s.audit(r, "catalog.oauth_start", id, map[string]any{"authUrl": o.AuthURL, "scope": o.Scope})
	writeJSON(w, http.StatusOK, map[string]any{"authorizeUrl": target, "redirectUrl": o.RedirectURL, "expiresInSeconds": 900})
}

func oauthPage(w http.ResponseWriter, status int, title, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>%s</title>
<body style="font:16px/1.5 system-ui,sans-serif;max-width:32rem;margin:15vh auto;padding:0 1rem"><h1 style="font-size:1.4rem">%s</h1><p>%s</p></body>`,
		html.EscapeString(title), html.EscapeString(title), html.EscapeString(body))
}

// oauthCallback finishes a consent. It takes no API key: the state value, which
// only the person who started the consent was given, is the credential, and it
// works once.
func (s *Server) oauthCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state := q.Get("state")
	if state == "" {
		oauthPage(w, http.StatusBadRequest, "Could not connect", "This link is missing its state value.")
		return
	}
	t, id, verifier, err := s.Store.TakeOAuthState(r.Context(), state)
	if err != nil {
		oauthPage(w, http.StatusBadRequest, "Could not connect", "This consent link has expired or was already used. Start again from the engine.")
		return
	}
	if e := q.Get("error"); e != "" {
		oauthPage(w, http.StatusBadRequest, "Access was not granted", "The provider answered: "+e+" "+q.Get("error_description"))
		return
	}
	code := q.Get("code")
	if code == "" {
		oauthPage(w, http.StatusBadRequest, "Could not connect", "The provider sent no authorization code.")
		return
	}
	creds, err := s.Store.Credentials(r.Context(), t, id)
	if err != nil || creds.OAuth == nil {
		oauthPage(w, http.StatusBadRequest, "Could not connect", "The tool this consent was for no longer exists.")
		return
	}
	o := *creds.OAuth
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := oauth.ExchangeCode(ctx, &o, code, verifier); err != nil {
		oauthPage(w, http.StatusBadGateway, "Could not connect", err.Error())
		return
	}
	if err := s.Store.UpdateCredentials(r.Context(), t, id, func(c *store.Credentials) error {
		c.OAuth = &o
		return nil
	}); err != nil {
		oauthPage(w, http.StatusInternalServerError, "Could not connect", "The tokens could not be saved. Try again.")
		return
	}
	detail := map[string]any{"refreshable": o.RefreshToken != ""}
	if aerr := s.Store.Audit(context.WithoutCancel(r.Context()), t, "oauth-callback", "catalog.oauth_connected", id, detail); aerr != nil {
		s.Log.Error("AUDIT WRITE FAILED", "action", "catalog.oauth_connected", "target", id, "error", aerr.Error())
	}
	msg := "The engine can now act on this tool. You can close this tab."
	if o.RefreshToken == "" {
		msg = "Connected, but the provider issued no refresh token, so access ends when this token expires. Many providers need an extra setting such as access_type=offline. You can close this tab."
	}
	oauthPage(w, http.StatusOK, "Connected", msg)
}

/* ----------------------------------- pieces --------------------------------- */

// listPieces returns the Activepieces pieces the runner has installed, to add
// as tools. q filters by name.
func (s *Server) listPieces(w http.ResponseWriter, r *http.Request) {
	if s.PiecesURL == "" {
		fail(w, http.StatusServiceUnavailable, "pieces_unavailable", "this engine has no pieces runner; set APEX_PIECES_URL")
		return
	}
	pieces, err := catalog.ListPieces(r.Context(), s.PiecesURL, s.PiecesToken)
	if err != nil {
		fail(w, http.StatusBadGateway, "pieces_unavailable", err.Error())
		return
	}
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	out := make([]catalog.PieceMeta, 0, len(pieces))
	for _, p := range pieces {
		if q == "" || strings.Contains(strings.ToLower(p.Name), q) || strings.Contains(strings.ToLower(p.DisplayName), q) {
			out = append(out, p)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"pieces": out})
}

// deleteTenant removes everything the calling tenant has. It is how a product
// offboards a customer: running work is cancelled, schedules are removed from
// Temporal, then every row goes. The caller must repeat the tenant's name, so a
// request built for one tenant cannot erase another by mistake.
func (s *Server) deleteTenant(w http.ResponseWriter, r *http.Request) {
	t := tenant(r)
	if r.URL.Query().Get("confirm") != t {
		fail(w, http.StatusBadRequest, "bad_request", "this deletes every tool, workflow, run, schedule and key of the tenant and cannot be undone; repeat the tenant name in ?confirm= to go ahead")
		return
	}
	schedules, err := s.Store.ListSchedules(r.Context(), t)
	if err != nil {
		s.failErr(w, err)
		return
	}
	for _, sc := range schedules {
		err := s.Temporal.ScheduleClient().GetHandle(r.Context(), scheduleID(t, sc.ID)).Delete(r.Context())
		var gone *serviceerror.NotFound
		if err != nil && !errors.As(err, &gone) {
			s.Log.Error("delete tenant: schedule", "tenant", t, "schedule", sc.ID, "error", err.Error())
			fail(w, http.StatusServiceUnavailable, "unavailable", "execution backend unavailable; nothing was deleted")
			return
		}
	}
	open, err := s.Store.OpenRunsOf(r.Context(), t)
	if err != nil {
		s.failErr(w, err)
		return
	}
	for _, run := range open {
		// Terminate, not cancel: a cancelled run would still write its final
		// state to rows that are about to go.
		err := s.Temporal.TerminateWorkflow(r.Context(), run.WorkflowID, "", "tenant deleted")
		var gone *serviceerror.NotFound
		if err != nil && !errors.As(err, &gone) {
			s.Log.Error("delete tenant: run", "tenant", t, "run", run.ID, "error", err.Error())
			fail(w, http.StatusServiceUnavailable, "unavailable", "execution backend unavailable; schedules were removed, nothing else was deleted")
			return
		}
	}
	deleted, err := s.Store.DeleteTenant(r.Context(), t)
	if err != nil {
		s.failErr(w, err)
		return
	}
	// The tenant's own audit log went with it, so the record of this is the
	// engine's log.
	s.Log.Warn("tenant deleted", "tenant", t, "actor", who(r).Actor, "rows", deleted, "runsStopped", len(open), "schedulesRemoved", len(schedules))
	writeJSON(w, http.StatusOK, map[string]any{"tenant": t, "deleted": deleted, "runsStopped": len(open)})
}
